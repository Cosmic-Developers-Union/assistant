// 待办检测：mention 与标签双通道检索待处理的 PR 与 Issue。
//
// 两个通道是**不同类型的行为**，不只是两条检索路径：
//
//   - mention 通道（@reviewer，Gitea 服务端 mentioned_by 过滤）：mention 是
//     「消息流」。正文或评论里的 @ai 会让条目**持续**留在清单里（直到关闭或
//     mention 被编辑掉），清单本身不携带「哪条 mention 是新的」。因此不能照搬
//     settled：已回应条目改记水位线（最后回应时刻），水位线之后出现他人新评论
//     才重新触发，且以追问轮（同一会话续聊）回应，不重跑全量协议。条目离开
//     mention 清单（关闭）即清除水位线，重新 mention 视为全新请求。
//   - review 请求通道（PR，Gitea 原生 requested_reviewers）：请求即「请求」
//     本体——reviewer 在当前 head 上提交正式回应后待处理状态自然出清（Gitea
//     不消费请求记录，出清判定以 reviews 为准）；作者推进 head 后旧回应失效，
//     条目重新入队。重复信号由 settled 守卫吸收，条目离开清单后解除。
//   - 标签通道（status/triage，仅 Issue）：兼容人工打标签触发分诊的场景。
//     PR 的 status/review 标签与全部状态标签由 action label-sync 继续维护，但**只是观测
//     产物**（看板、报表、既有集成），不再是任何工作的触发源。
package dispatcher

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"assistant/internal/status"
)

// 待办类型
const (
	KindPull  = "pull"
	KindIssue = "issue"
)

// WorkItem 是一个待处理的 PR 或 Issue。
type WorkItem struct {
	Kind   string
	Number int64
	Title  string
	// 通道命中标记，可同时为真（多路都命中时合并为一份待办）。
	// Mention：mention 通道命中——条目在 @reviewer 的 mentioned_by 清单里；
	// Requested：review 请求通道命中——reviewer 被原生请求评审且未在当前
	// head 上回应（仅 PR）；
	// Labeled：标签通道命中——Issue 挂着 status/triage（PR 标签不再是触发源）。
	Mention   bool
	Requested bool
	Labeled   bool
	// Updated 是条目最近活动时刻（Gitea 服务端时钟）：mention 水位线预检用，
	// 未变化条目跳过新评论细查。
	Updated time.Time
	// FollowUp 为真表示以追问轮执行（仅 mention 通道派发）：不重跑全量协议，
	// 把 Since 之后他人新消息喂给同一会话续聊。由检测守卫设置，外部调用方
	// （review/triage 一次性命令）恒为全量模式。
	FollowUp bool
	// Since 是追问轮读取新消息的起点（该条目的 mention 水位线）。
	Since time.Time
}

// key 是待办的唯一键：同一仓库内 kind#number 至多对应一个会话。仓库维度由
// 每个仓库独立的循环/Deps 提供，多实例时不同 instance 各自独立。
func (w WorkItem) key() string {
	return fmt.Sprintf("%s#%d", w.Kind, w.Number)
}

// label 是日志用的待办标识：追问轮追加标注，操作者能区分全量处理与续聊回应。
func (w WorkItem) label() string {
	if w.FollowUp {
		return fmt.Sprintf("%s#%d（追问）", w.Kind, w.Number)
	}
	return w.key()
}

// API 是 dispatcher 需要的 Gitea 只读能力（*status.Client 满足）。
type API interface {
	// ListPullRequestsRequestingReview 是 review 请求通道：Gitea 原生
	// requested_reviewers 含 reviewer 且未在当前 head 上回应的 open PR。
	ListPullRequestsRequestingReview(context.Context, status.Repository, string) ([]status.PullRequest, error)
	ListTriageIssues(context.Context, status.Repository) ([]status.Issue, error)
	// ListIssuesMentioning 是 mention 通道：Gitea 服务端 mentioned_by 过滤，
	// 返回 mention 了 reviewer 的 open 条目（issueType 为 "issues"/"pulls"）。
	ListIssuesMentioning(ctx context.Context, repository status.Repository, user string, issueType string) ([]status.Issue, error)
	GetPullRequest(context.Context, status.Repository, int64) (status.PullRequest, error)
	ListPullReviews(context.Context, status.Repository, int64) ([]status.Review, error)
	GetIssueLabels(context.Context, status.Repository, int64) ([]status.Label, error)
	AuthenticatedUser(context.Context) (string, error)
}

// ListWork 检索全部待办：mention 通道（@reviewer 提及）、review 请求通道
// （原生 requested_reviewers）与标签通道（status/triage，仅 Issue）三路并集，
// 通道命中合并进同一条待办（WorkItem 通道标记），按编号**倒序**（新的待办优先
// 处理；用户刚 @ai 的条目排在最前）。
func ListWork(ctx context.Context, api API, repository status.Repository, reviewer string) ([]WorkItem, error) {
	type result struct {
		items []status.Issue
		err   error
	}
	issuesType := "issues"
	const channels = 4
	results := make([]result, channels)
	start := func(index int, fetch func() ([]status.Issue, error)) {
		items, err := fetch()
		results[index] = result{items: items, err: err}
	}
	requested := func() ([]status.Issue, error) {
		// review 请求通道按 PullRequest 检索，这里折成统一视图
		pulls, err := api.ListPullRequestsRequestingReview(ctx, repository, reviewer)
		if err != nil {
			return nil, err
		}
		issues := make([]status.Issue, 0, len(pulls))
		for _, pull := range pulls {
			issues = append(issues, status.Issue{Index: pull.Index, Title: pull.Title, IsPull: true})
		}
		return issues, nil
	}
	var waitGroup sync.WaitGroup
	waitGroup.Add(channels)
	go func() {
		defer waitGroup.Done()
		start(0, func() ([]status.Issue, error) { return api.ListIssuesMentioning(ctx, repository, reviewer, "pulls") })
	}()
	go func() {
		defer waitGroup.Done()
		start(1, func() ([]status.Issue, error) { return api.ListIssuesMentioning(ctx, repository, reviewer, issuesType) })
	}()
	go func() { defer waitGroup.Done(); start(2, requested) }()
	go func() {
		defer waitGroup.Done()
		start(3, func() ([]status.Issue, error) { return api.ListTriageIssues(ctx, repository) })
	}()
	waitGroup.Wait()
	for _, result := range results {
		if result.err != nil {
			return nil, result.err
		}
	}
	work := make([]WorkItem, 0, len(results[0].items)+len(results[1].items)+len(results[2].items)+len(results[3].items))
	// mention 通道由 Gitea 的 IsPull 定型；标签通道仅剩 Issue 分诊
	work = append(work, workItems(results[0].items, mentionChannel)...)
	work = append(work, workItems(results[1].items, mentionChannel)...)
	work = append(work, workItems(results[2].items, requestedChannel)...)
	work = append(work, workItems(results[3].items, labelChannel, KindIssue)...)
	slices.SortFunc(work, func(a, b WorkItem) int { return cmp.Compare(b.Number, a.Number) })
	return dedupeWork(work), nil
}

// dedupeWork 保证同一仓库内 (kind, number) 唯一：请求的初始提示词与完成判定
// 都以它为键，重复条目会导致同一待办被并发拉起多个会话。多路命中同一待办时
// 合并为一份（通道标记按或合并，守卫各自独立生效）。
func dedupeWork(work []WorkItem) []WorkItem {
	if len(work) < 2 {
		return work
	}
	result := work[:1]
	for _, item := range work[1:] {
		last := &result[len(result)-1]
		if last.Kind == item.Kind && last.Number == item.Number {
			last.Mention = last.Mention || item.Mention
			last.Requested = last.Requested || item.Requested
			last.Labeled = last.Labeled || item.Labeled
			continue
		}
		result = append(result, item)
	}
	return result
}

// channel 标记 workItems 折叠条目时归入的检测通道。
type channel int

const (
	mentionChannel channel = iota
	requestedChannel
	labelChannel
)

// workItems 把条目列表折成待办并打上通道标记。标签通道传 kind 按来源定型
// （status/triage 只可能是 Issue；叠加 type 时 Gitea 偶发混入条目，双保险
// 过滤）；mention 通道按条目自身的 IsPull 区分；review 请求通道按构造已是 PR。
func workItems(items []status.Issue, ch channel, kind ...string) []WorkItem {
	forced := ""
	if len(kind) > 0 {
		forced = kind[0]
	}
	var work []WorkItem
	for _, item := range items {
		kindValue := forced
		if kindValue == "" {
			if item.IsPull {
				kindValue = KindPull
			} else {
				kindValue = KindIssue
			}
		} else if kindValue == KindPull && !item.IsPull {
			continue
		}
		work = append(work, WorkItem{
			Kind:      kindValue,
			Number:    item.Index,
			Title:     item.Title,
			Updated:   item.Updated,
			Mention:   ch == mentionChannel,
			Requested: ch == requestedChannel,
			Labeled:   ch == labelChannel,
		})
	}
	return work
}
