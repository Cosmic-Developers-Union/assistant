package status

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// AutoMerge 对内容评审者已在当前 head 上正式批准的 open PR 执行自动合并：
// 先以当前身份维护官方评审请求（Gitea 只允许 PR 作者或仓库管理员选择 reviewer，
// 因此合并 job 的 merge 令牌是唯一能登记/撤回请求的自动化身份），再重读最新状态
// 确认批准仍有效、未落后、无冲突、必要检查全绿后，以 squash 方式合并。
//
// 必要检查仍在运行时不空等：「检查从 running 变 success」没有任何 Actions 触发
// 事件，事件唤醒存在结构性盲区，此时武装 Gitea 原生 auto-merge（会签前移到武装
// 之前），检查全绿由 Gitea 核心即时合并。assistant 门禁失效（批准撤销、检查
// 失败、落后）时主动撤销武装——排定不会自灭，不撤会绕过内容门禁。
//
// 一次运行至多发生一笔实际合并（武装不移动 main，不占额度；武装时检查恰好
// 已绿会直接合并并计为该笔）——main 随之前移，其余所有 open PR 因此过期，
// 作者 rebase 并重新获准后才能再次进入合并队列。由 Gitea Actions（事件 +
// schedule）驱动，幂等安全：门禁不满足时本轮跳过。
//
// 新规范：合并不再依赖标签——门禁是内容评审者（默认 ai）对当前 head 的官方
// 批准（approved、未 dismiss、CommitID==head）。标签（status/approved、
// awaiting/merge 等）由 action label-sync 继续维护，但只是观测产物。
func (m *Manager) AutoMerge(ctx context.Context) error {
	if err := m.ReconcileReviewRequests(ctx); err != nil {
		return err
	}
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
	slices.SortFunc(pullRequests, func(a, b PullRequest) int { return cmp.Compare(a.Index, b.Index) })
	for _, candidate := range pullRequests {
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
// 合并门禁 = 内容评审者对当前 head 的官方批准（approved、未 dismiss、
// CommitID==head）+ 未落后 + 可合并 + 必要检查全绿；标签不参与判定。
// 门禁失效时撤销已武装的原生 auto-merge（排定不会自灭，不撤会在检查转绿时
// 绕过内容门禁）。
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
		m.disarmAutoMerge(ctx, repository, pullRequest, "PR 已关闭")
		return skip("PR 已关闭")
	}
	if pullRequest.Draft || workInProgress(pullRequest.Title) {
		m.disarmAutoMerge(ctx, repository, pullRequest, "PR 处于草稿或 WIP 状态")
		return skip("PR 处于草稿或 WIP 状态")
	}
	if !pullRequest.Mergeable {
		m.disarmAutoMerge(ctx, repository, pullRequest, "与目标分支冲突")
		return skip("与目标分支冲突")
	}
	behind, hasBranchState := branchBehind(pullRequest)
	if !hasBranchState {
		return skip("缺少 base 或 merge-base 元数据")
	}
	if behind {
		// 批准只背书评审时的那批提交：main 前移后 PR 过期，必须 rebase 并重新获准
		m.disarmAutoMerge(ctx, repository, pullRequest, "已落后基础分支")
		return skip("已落后基础分支（main 前移使已批准的 PR 过期），需 rebase 后重新评审")
	}
	reviews, err := m.api.ListPullReviews(ctx, repository, pullRequest.Index)
	if err != nil {
		return false, err
	}
	if !reviewerApprovedOnHead(reviews, m.contentReviewer, pullRequest.HeadSHA) {
		m.disarmAutoMerge(ctx, repository, pullRequest, "内容评审者的批准不在当前 head")
		return skip(fmt.Sprintf("内容评审者 @%s 尚未在当前 head（%.10s）上批准", m.contentReviewer, pullRequest.HeadSHA))
	}
	failedChecks, pendingChecks, err := m.checkStates(ctx, repository, pullRequest, protections)
	if err != nil {
		return false, err
	}
	if len(failedChecks) > 0 {
		m.disarmAutoMerge(ctx, repository, pullRequest, "必要检查未通过")
		return skip("必要检查未通过: " + strings.Join(checkContexts(failedChecks), ", "))
	}
	if len(pendingChecks) > 0 {
		return m.armNativeAutoMerge(ctx, repository, pullRequest, protections, pendingChecks)
	}
	// 状态会签：门禁全绿且内容已获批准，状态评审者对当前 head 盖章后立即合并
	// ——会签与合并是同一机械动作，不存在「盖了章等着」的中间态。会签使
	// required approvals 计满（内容批准 + 状态会签），同时以官方 review 覆盖
	// 自己此前的门禁驳回（block_on_rejected_reviews 解除）。未配置状态评审者
	// 时不做会签（历史行为，仅依赖 automerge 门禁本身）。
	if m.stateReviewer != "" {
		if err := m.countersignState(ctx, repository, pullRequest, false); err != nil {
			return false, err
		}
	}
	if err := m.api.MergePullRequest(ctx, repository, pullRequest.Index); err != nil {
		return false, err
	}
	m.logf("%s#%d: 已 squash 合并 %q；main 前移，本轮不再处理其余 PR", repository.FullName(), pullRequest.Index, pullRequest.Title)
	return true, nil
}

// armNativeAutoMerge 在必要检查仍在运行时武装 Gitea 原生 auto-merge：检查转绿
// 没有任何 Actions 触发事件，事件唤醒够不到这一转换，武装后由 Gitea 核心在
// 检查全绿时即时合并（squash、删 head 分支），盲区消失。会签必须前移到武装
// 之前——Gitea 在检查变绿后尝试合并时会重新校验分支保护（required approvals、
// block_on_rejected_reviews），武装那一刻批准门禁就得满足；会签提交的 review
// 事件也会再次触发服务端的排定检查，形成自然重试。
func (m *Manager) armNativeAutoMerge(
	ctx context.Context,
	repository Repository,
	pullRequest PullRequest,
	protections []BranchProtection,
	pendingChecks []CheckStatus,
) (bool, error) {
	pending := strings.Join(checkContexts(pendingChecks), ", ")
	if requiredContexts(protections, pullRequest.BaseRef) == nil {
		// 分支保护没有配置必要检查（或不可读而回退）时不能武装：原生合并只认
		// Gitea 自己的必要检查配置，检查已绿时会直接合并，绕过回退模式的严格
		// 门禁（任何 context 未完成即阻塞）。维持等 schedule 的现状。
		m.logf("%s#%d: 跳过武装（保护规则未配置必要检查，严格门禁维持现状）: 检查仍在运行: %s",
			repository.FullName(), pullRequest.Index, pending)
		return false, nil
	}
	if m.stateReviewer != "" {
		if err := m.countersignState(ctx, repository, pullRequest, true); err != nil {
			return false, err
		}
	}
	result, err := m.api.ArmAutoMerge(ctx, repository, pullRequest.Index, pullRequest.HeadSHA)
	if err != nil {
		// 武装失败不上抛：405「Please try again later」在合并性异步计算未完成时
		// 属常态，其余失败也有 schedule 兜底，不该挡住本轮的其他候选与仓库。
		m.logf("%s#%d: 武装原生 auto-merge 失败（回退 schedule 兜底）: %v",
			repository.FullName(), pullRequest.Index, err)
		return false, nil
	}
	switch result {
	case AutoMergeMergedNow:
		m.logf("%s#%d: 武装时必要检查已全绿，直接 squash 合并 %q；main 前移，本轮不再处理其余 PR",
			repository.FullName(), pullRequest.Index, pullRequest.Title)
		return true, nil
	case AutoMergeAlreadyArmed:
		m.logf("%s#%d: 原生 auto-merge 已在排定（必要检查运行中: %s），等待检查变绿",
			repository.FullName(), pullRequest.Index, pending)
	default:
		m.logf("%s#%d: 必要检查仍在运行（%s），已武装原生 auto-merge：检查全绿即由 Gitea 即时合并",
			repository.FullName(), pullRequest.Index, pending)
	}
	return false, nil
}

// disarmAutoMerge 在 assistant 门禁失效时撤销已武装的原生 auto-merge：Gitea
// 1.27 的排定不会因批准撤销、head 前移或检查失败自动失效，不撤的话检查转绿
// 即合并，会绕过内容门禁。幂等：本无排定时静默跳过（Gitea 回 404）。
func (m *Manager) disarmAutoMerge(ctx context.Context, repository Repository, pullRequest PullRequest, reason string) {
	removed, err := m.api.DisarmAutoMerge(ctx, repository, pullRequest.Index)
	if err != nil {
		m.logf("%s#%d: 撤销原生 auto-merge 失败（下一轮重试）: %v",
			repository.FullName(), pullRequest.Index, err)
		return
	}
	if removed {
		m.logf("%s#%d: 门禁失效（%s），已撤销武装的原生 auto-merge",
			repository.FullName(), pullRequest.Index, reason)
	}
}

// reviewerApprovedOnHead 报告内容评审者是否在 headSHA 上提交过未被 dismiss 的
// approved review。作者推进 head 后旧批准失效（dismiss_stale_approvals 会由
// Gitea 落实，这里再按 CommitID 双保险），需要重新评审后才能合并。
func reviewerApprovedOnHead(reviews []Review, reviewer, headSHA string) bool {
	if reviewer == "" {
		return false
	}
	for _, review := range reviews {
		if review.User != reviewer || review.Dismissed || review.State != ReviewStateApproved {
			continue
		}
		if review.CommitID == "" || review.CommitID == headSHA {
			return true
		}
	}
	return false
}

// countersignState 以当前令牌身份（状态评审者）对 head 提交状态批准。pending
// 为真时是武装前的会签（必要检查仍在运行）：Gitea 在检查变绿后尝试合并时会
// 重新校验 required approvals，批准门禁必须在武装那一刻满足。同一 head 已
// 会签过（上一轮武装或上次合并失败的残留）时不重复盖章。
func (m *Manager) countersignState(
	ctx context.Context,
	repository Repository,
	pullRequest PullRequest,
	pending bool,
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
			m.logf("%s#%d: 状态会签已在（head %.10s），不重复盖章", repository.FullName(), pullRequest.Index, pullRequest.HeadSHA)
			return nil
		}
	}
	if pending {
		m.logf("%s#%d: 内容已批准、检查运行中，先会签（head %.10s）满足批准门禁，再武装原生 auto-merge",
			repository.FullName(), pullRequest.Index, pullRequest.HeadSHA)
	} else {
		m.logf("%s#%d: 状态门禁通过，会签（head %.10s）后立即合并", repository.FullName(), pullRequest.Index, pullRequest.HeadSHA)
	}
	return m.api.CreatePullReview(ctx, repository, pullRequest.Index, ReviewInput{
		State:    ReviewStateApproved,
		Body:     countersignBody(pullRequest, pending),
		CommitID: pullRequest.HeadSHA,
	})
}

func countersignBody(pullRequest PullRequest, pending bool) string {
	short := pullRequest.HeadSHA
	if len(short) > 10 {
		short = short[:10]
	}
	if pending {
		return "状态门禁会签：内容已获批准、未落后 main、可合并，必要检查运行中。" +
			"机械核验通过（head " + short + "），武装原生 auto-merge，检查全绿即自动合并。"
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
