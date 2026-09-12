package status

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// AutoMerge 对带 status/approved + awaiting/merge 标签的 open PR 执行自动合并：
// 重读最新状态确认批准仍有效、未落后、无冲突、必要检查全绿后，以 squash 方式
// 合并。一次运行至多合并一个 PR——main 随之前移，其余所有 open PR 因此过期，
// 由 sync 打回 changes-requested，作者 rebase 并重新获准后才能再次进入合并队列。
// 由 Gitea Actions schedule（.gitea/workflows/automerge.yml）周期驱动，幂等安全：
// 门禁不满足时本轮跳过，什么都不写。
func (m *Manager) AutoMerge(ctx context.Context) error {
	repositories, err := m.visibleRepositories(ctx)
	if err != nil {
		return err
	}
	var runErrors []error
	for _, repository := range repositories {
		merged, err := m.autoMergeRepository(ctx, repository)
		if err != nil {
			runErrors = append(runErrors, fmt.Errorf("%s: %w", repository.FullName(), err))
		}
		if merged {
			// main 已前移，本轮不再评估任何后续 PR
			break
		}
	}
	return errors.Join(runErrors...)
}

// autoMergeRepository 处理单个仓库的合并队列，返回本仓库是否发生了合并。
func (m *Manager) autoMergeRepository(ctx context.Context, repository Repository) (bool, error) {
	pullRequests, err := m.api.ListOpenPullRequests(ctx, repository)
	if err != nil {
		return false, err
	}
	var candidates []PullRequest
	for _, pullRequest := range pullRequests {
		if !hasLabel(pullRequest.Labels, approvedLabelName) || !hasLabel(pullRequest.Labels, awaitingMergeLabelName) {
			continue
		}
		candidates = append(candidates, pullRequest)
	}
	if len(candidates) == 0 {
		m.logf("%s: 没有 status/approved 的 PR，无合并动作", repository.FullName())
		return false, nil
	}
	protections, err := m.api.ListBranchProtections(ctx, repository)
	if IsPermissionError(err) {
		// 与 sync 的必要检查门禁同款降级：读取分支保护需要 repo admin，普通
		// 协作者令牌被拒时按「任何失败 context 即阻塞」的严格模式继续——门禁
		// 不会失效，只是无法收窄到必要 context。
		m.logf("%s: 分支保护读取被拒，必要检查回退为任何失败即阻塞", repository.FullName())
		protections = nil
	} else if err != nil {
		return false, err
	}
	// 编号升序：先来先合并
	slices.SortFunc(candidates, func(a, b PullRequest) int { return cmp.Compare(a.Index, b.Index) })
	for _, candidate := range candidates {
		merged, err := m.autoMergePullRequest(ctx, repository, candidate, protections)
		if err != nil {
			return false, err
		}
		if merged {
			return true, nil
		}
	}
	return false, nil
}

// autoMergePullRequest 对单个候选执行合并门禁并尝试合并。合并前重读最新状态：
// 候选来自列表快照，等待期间评审状态可能已被 sync 更新（批准撤销、打回、落后）。
func (m *Manager) autoMergePullRequest(
	ctx context.Context,
	repository Repository,
	candidate PullRequest,
	protections []BranchProtection,
) (bool, error) {
	pullRequest, err := m.api.GetPullRequest(ctx, repository, candidate.Index)
	if err != nil {
		return false, err
	}
	skip := func(reason string) (bool, error) {
		m.logf("%s#%d: 跳过合并: %s", repository.FullName(), pullRequest.Index, reason)
		return false, nil
	}
	if !pullRequest.Open {
		return skip("PR 已关闭")
	}
	if !hasLabel(pullRequest.Labels, approvedLabelName) || !hasLabel(pullRequest.Labels, awaitingMergeLabelName) {
		return skip("status/approved 标签已不在（评审状态在等待期间被更新）")
	}
	if pullRequest.Draft || workInProgress(pullRequest.Title) {
		return skip("PR 处于草稿或 WIP 状态")
	}
	if !pullRequest.Mergeable {
		return skip("与目标分支冲突")
	}
	behind, hasBranchState := branchBehind(pullRequest)
	if !hasBranchState {
		return skip("缺少 base 或 merge-base 元数据")
	}
	if behind {
		// 批准只背书评审时的那批提交：main 前移后 PR 过期，必须 rebase 并重新获准
		return skip("已落后基础分支（main 前移使已批准的 PR 过期），需 rebase 后重新评审")
	}
	failedChecks, pendingChecks, err := m.checkStates(ctx, repository, pullRequest, protections)
	if err != nil {
		return false, err
	}
	if len(pendingChecks) > 0 {
		return skip("必要检查仍在运行: " + strings.Join(checkContexts(pendingChecks), ", "))
	}
	if len(failedChecks) > 0 {
		return skip("必要检查未通过: " + strings.Join(checkContexts(failedChecks), ", "))
	}
	// 状态会签：门禁全绿且内容已获批准，状态评审者对当前 head 盖章后立即合并
	// ——会签与合并是同一机械动作，不存在「盖了章等着」的中间态。会签使
	// required approvals 计满（内容批准 + 状态会签），同时以官方 review 覆盖
	// 自己此前的门禁驳回（block_on_rejected_reviews 解除）。未配置状态评审者
	// 时不做会签（历史行为，仅依赖 automerge 门禁本身）。
	if m.stateReviewer != "" {
		if err := m.countersignState(ctx, repository, pullRequest); err != nil {
			return false, err
		}
	}
	if err := m.api.MergePullRequest(ctx, repository, pullRequest.Index); err != nil {
		return false, err
	}
	m.logf("%s#%d: 已 squash 合并 %q；main 前移，本轮不再处理其余 PR", repository.FullName(), pullRequest.Index, pullRequest.Title)
	return true, nil
}

// countersignState 以当前令牌身份（状态评审者）对 head 提交状态批准：核验
// 内容已获批准、必要检查全绿、未落后、可合并之后，会签即合并。同一 head 已
// 会签过（上次合并调用失败的残留）时不重复盖章。
func (m *Manager) countersignState(
	ctx context.Context,
	repository Repository,
	pullRequest PullRequest,
) error {
	if pullRequest.HeadSHA == "" {
		// 缺少 head 元数据的遗留路径无从按 head 去重，直接合并（不盖章）。
		return nil
	}
	identity, err := m.api.AuthenticatedUser(ctx)
	if err != nil {
		return err
	}
	reviews, err := m.api.ListPullReviews(ctx, repository, pullRequest.Index)
	if err != nil {
		return err
	}
	for _, review := range reviews {
		if review.User == identity && !review.Dismissed &&
			review.State == ReviewStateApproved && review.CommitID == pullRequest.HeadSHA {
			m.logf("%s#%d: 状态会签已在（head %.10s），直接合并", repository.FullName(), pullRequest.Index, pullRequest.HeadSHA)
			return nil
		}
	}
	m.logf("%s#%d: 状态门禁通过，会签（head %.10s）后立即合并", repository.FullName(), pullRequest.Index, pullRequest.HeadSHA)
	return m.api.CreatePullReview(ctx, repository, pullRequest.Index, ReviewInput{
		State:    ReviewStateApproved,
		Body:     countersignBody(pullRequest),
		CommitID: pullRequest.HeadSHA,
	})
}

func countersignBody(pullRequest PullRequest) string {
	short := pullRequest.HeadSHA
	if len(short) > 10 {
		short = short[:10]
	}
	return "状态门禁会签：必要检查全绿、未落后 main、可合并；内容已获批准。" +
		"机械核验通过（head " + short + "），随即合并。"
}

// workInProgress 按标题前缀识别 WIP PR（Gitea 原生草稿见 PullRequest.Draft）。
// 仓库约定 WIP PR 以「WIP: 」开头，一并列出 Gitea 常见的等价写法。
func workInProgress(title string) bool {
	for _, prefix := range []string{"WIP:", "WIP ", "[WIP]", "[wip]"} {
		if strings.HasPrefix(title, prefix) {
			return true
		}
	}
	return false
}
