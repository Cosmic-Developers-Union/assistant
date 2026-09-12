// 待办检测：按标签检索待处理的 PR 与 Issue。
//
// dispatcher 只读标签（标签由 Gitea Actions 上的 gitea-assistant 事件驱动维护），
// 不做任何写操作。检测口径与 gitea-assistant check 一致：
//   - PR：awaiting/reviewer（等待评审）
//   - Issue：status/triage（等待分诊）
package dispatcher

import (
	"context"
	"sort"

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
}

// API 是 dispatcher 需要的 Gitea 只读能力（*status.Client 满足）。
type API interface {
	ListReviewPullRequests(context.Context, status.Repository) ([]status.Issue, error)
	ListTriageIssues(context.Context, status.Repository) ([]status.Issue, error)
	GetPullRequest(context.Context, status.Repository, int64) (status.PullRequest, error)
	ListPullReviews(context.Context, status.Repository, int64) ([]status.Review, error)
	GetIssueLabels(context.Context, status.Repository, int64) ([]status.Label, error)
	AuthenticatedUser(context.Context) (string, error)
}

// ListWork 检索全部待办：带 awaiting/reviewer 的 PR + 带 status/triage 的
// Issue，按编号升序。
func ListWork(ctx context.Context, api API, repository status.Repository) ([]WorkItem, error) {
	type result struct {
		items []WorkItem
		err   error
	}
	pullsChannel := make(chan result, 1)
	issuesChannel := make(chan result, 1)
	go func() {
		items, err := api.ListReviewPullRequests(ctx, repository)
		pullsChannel <- result{items: workItems(items, KindPull), err: err}
	}()
	go func() {
		items, err := api.ListTriageIssues(ctx, repository)
		issuesChannel <- result{items: workItems(items, KindIssue), err: err}
	}()
	pulls, issues := <-pullsChannel, <-issuesChannel
	if pulls.err != nil {
		return nil, pulls.err
	}
	if issues.err != nil {
		return nil, issues.err
	}
	work := append(pulls.items, issues.items...)
	sort.Slice(work, func(i, j int) bool { return work[i].Number < work[j].Number })
	return work, nil
}

func workItems(items []status.Issue, kind string) []WorkItem {
	var work []WorkItem
	for _, item := range items {
		// labels 过滤叠加 type 时 Gitea 偶发混入纯 Issue，双保险
		if kind == KindPull && !item.IsPull {
			continue
		}
		work = append(work, WorkItem{Kind: kind, Number: item.Index, Title: item.Title})
	}
	return work
}
