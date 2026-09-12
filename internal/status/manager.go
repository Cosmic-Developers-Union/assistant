package status

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	typeLabelPrefix     = "type/"
	priorityLabelPrefix = "priority/"
	statusLabelPrefix   = "status/"
	awaitingLabelPrefix = "awaiting/"

	triageLabelName           = "status/triage"
	confirmedLabelName        = "status/confirmed"
	inProcessLabelName        = "status/in-process"
	blockedLabelName          = "status/blocked"
	inProgressLabelName       = "status/in-progress"
	reviewLabelName           = "status/review"
	changesRequestedLabelName = "status/changes-requested"
	approvedLabelName         = "status/approved"
	awaitingAuthorLabelName   = "awaiting/author"
	awaitingReviewerLabelName = "awaiting/reviewer"
	awaitingMergeLabelName    = "awaiting/merge"
	duplicateLabelName        = "duplicate"
	wontfixLabelName          = "wontfix"
	needsInfoLabelName        = "needs-info"
	unknownReviewStatus       = "NONE"
)

type LabelDefinition struct {
	Name        string
	Color       string
	Description string
	Exclusive   bool
}

var labelDefinitions = []LabelDefinition{
	{Name: "type/bug", Color: "D73A4A", Description: "需要修复的问题", Exclusive: true},
	{Name: "type/feature", Color: "0075CA", Description: "新功能请求", Exclusive: true},
	{Name: "type/refactor", Color: "FBCA04", Description: "不改变外部行为的内部结构整理", Exclusive: true},
	{Name: "type/task", Color: "0E8A16", Description: "开发或维护任务", Exclusive: true},
	{Name: "priority/high", Color: "B60205", Description: "需要优先处理", Exclusive: true},
	{Name: "priority/low", Color: "D4C5F9", Description: "可延后处理", Exclusive: true},
	{Name: triageLabelName, Color: "E4E669", Description: "Issue 等待分诊", Exclusive: true},
	{Name: confirmedLabelName, Color: "0E8A16", Description: "Issue 已确认有效", Exclusive: true},
	{Name: inProcessLabelName, Color: "1D76DB", Description: "Issue 正在处理", Exclusive: true},
	{Name: blockedLabelName, Color: "B60205", Description: "Issue 处理受阻", Exclusive: true},
	{Name: duplicateLabelName, Color: "CFD3D7", Description: "Issue 与已有记录重复"},
	{Name: wontfixLabelName, Color: "FFFFFF", Description: "Issue 不计划处理"},
	{Name: needsInfoLabelName, Color: "D876E3", Description: "需要报告者补充信息"},
	{Name: inProgressLabelName, Color: "1D76DB", Description: "PR 开发中", Exclusive: true},
	{Name: reviewLabelName, Color: "FBCA04", Description: "PR 等待代码审查", Exclusive: true},
	{Name: changesRequestedLabelName, Color: "D93F0B", Description: "PR 需要修改", Exclusive: true},
	{Name: approvedLabelName, Color: "0E8A16", Description: "PR 已批准合并", Exclusive: true},
	{Name: awaitingAuthorLabelName, Color: "1D76DB", Description: "等待 PR 作者处理", Exclusive: true},
	{Name: awaitingReviewerLabelName, Color: "FBCA04", Description: "等待 reviewer 评审", Exclusive: true},
	{Name: awaitingMergeLabelName, Color: "0E8A16", Description: "等待人类工程师合并", Exclusive: true},
}

type Manager struct {
	api              API
	progressf        func(string, ...any)
	targetRepository *Repository
	// stateReviewer 是状态评审者的账号名（merge 令牌身份）。配置后 sync 按
	// 作者角色区分内容/状态两条 review 通道，automerge 合并前由状态评审者
	// 会签；留空则不做角色区分（历史单通道行为），automerge 也不做会签。
	stateReviewer string
}

type ManagerOption func(*Manager)

func WithProgress(progressf func(string, ...any)) ManagerOption {
	return func(manager *Manager) {
		manager.progressf = progressf
	}
}

func WithRepository(repository Repository) ManagerOption {
	return func(manager *Manager) {
		manager.targetRepository = new(repository)
	}
}

// WithStateReviewer 配置状态评审者账号名（merge 令牌身份）。
func WithStateReviewer(login string) ManagerOption {
	return func(manager *Manager) {
		manager.stateReviewer = login
	}
}

func NewManager(api API, options ...ManagerOption) *Manager {
	manager := &Manager{api: api}
	for _, option := range options {
		option(manager)
	}
	return manager
}

// Sync 对令牌可见的仓库执行一次性 reconcile：补齐标签体系、规范 open Issue 的
// 标签、把每个 open PR 的原生评审状态同步为状态标签。由 Gitea Actions 事件驱动
// （加定时兜底）运行，幂等，可重复执行。
func (m *Manager) Sync(ctx context.Context) error {
	repositories, err := m.visibleRepositories(ctx)
	if err != nil {
		return err
	}
	m.logf("发现 %d 个可访问仓库", len(repositories))

	var runErrors []error
	for _, repository := range repositories {
		m.logf("同步仓库 %s", repository.FullName())
		if err := m.reconcileRepository(ctx, repository); err != nil {
			runErrors = append(runErrors, fmt.Errorf("%s: %w", repository.FullName(), err))
		}
	}
	return errors.Join(runErrors...)
}

func (m *Manager) visibleRepositories(ctx context.Context) ([]Repository, error) {
	repositories, err := m.api.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	if m.targetRepository != nil {
		index := slices.Index(repositories, *m.targetRepository)
		if index < 0 {
			return nil, fmt.Errorf("target repository %s is not visible to the access token", m.targetRepository.FullName())
		}
		repositories = repositories[index : index+1]
	}
	return repositories, nil
}

func (m *Manager) reconcileRepository(ctx context.Context, repository Repository) error {
	labels, err := m.prepareLabels(ctx, repository)
	if err != nil {
		return err
	}

	var reconcileErrors []error
	if err := m.reconcileIssues(ctx, repository, labels); err != nil {
		reconcileErrors = append(reconcileErrors, err)
	}

	// 必要检查门禁依赖分支保护配置；普通协作者令牌（如 Actions 内置令牌）没有
	// 分支保护读权限（HTTP 403），此时回退为「任何失败 context 即阻塞」的严格
	// 模式继续同步——严格模式不需要该配置，且方向更保守，门禁不会因此失效。
	// 其他错误（网络等）宁可本次不同步 PR，也不静默跳过门禁。
	protections, err := m.api.ListBranchProtections(ctx, repository)
	if err != nil {
		if !IsPermissionError(err) {
			return errors.Join(append(reconcileErrors, fmt.Errorf("list branch protections: %w", err))...)
		}
		m.logf("%s: 无权限读取分支保护，必要检查门禁回退为严格模式", repository.FullName())
		protections = nil
	}

	pullRequests, err := m.api.ListOpenPullRequests(ctx, repository)
	if err != nil {
		return errors.Join(append(reconcileErrors, err)...)
	}
	m.logf("%s: 发现 %d 个 open PR", repository.FullName(), len(pullRequests))
	for _, pullRequest := range pullRequests {
		if err := m.reconcilePullRequest(ctx, repository, pullRequest.Index, labels, protections); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("pull request #%d: %w", pullRequest.Index, err))
		}
	}
	return errors.Join(reconcileErrors...)
}

func (m *Manager) prepareLabels(ctx context.Context, repository Repository) (map[string]Label, error) {
	labels, err := m.api.ListRepositoryLabels(ctx, repository)
	if err != nil {
		return nil, err
	}

	byName := make(map[string]Label, len(labels)+len(labelDefinitions))
	for _, label := range labels {
		byName[label.Name] = label
	}
	for _, definition := range labelDefinitions {
		if _, ok := byName[definition.Name]; ok {
			continue
		}
		label, err := m.api.CreateLabel(ctx, repository, definition)
		if err != nil {
			return nil, err
		}
		m.logf("%s: 已创建标签 %s", repository.FullName(), definition.Name)
		byName[definition.Name] = label
	}

	for name, label := range byName {
		if !isScopedLabel(name) || label.Exclusive {
			continue
		}
		if err := m.api.SetLabelExclusive(ctx, repository, label.ID); err != nil {
			return nil, err
		}
		m.logf("%s: 标签 %s 已设为互斥", repository.FullName(), name)
		label.Exclusive = true
		byName[name] = label
	}
	return byName, nil
}

func isScopedLabel(name string) bool {
	return strings.HasPrefix(name, typeLabelPrefix) ||
		strings.HasPrefix(name, priorityLabelPrefix) ||
		strings.HasPrefix(name, statusLabelPrefix) ||
		strings.HasPrefix(name, awaitingLabelPrefix)
}

func (m *Manager) reconcilePullRequest(
	ctx context.Context,
	repository Repository,
	index int64,
	repositoryLabels map[string]Label,
	protections []BranchProtection,
) error {
	pullRequest, err := m.api.GetPullRequest(ctx, repository, index)
	if err != nil {
		return err
	}
	if !pullRequest.Open {
		m.logf("跳过 %s#%d: PR 已关闭", repository.FullName(), index)
		return nil
	}

	reviews, err := m.api.ListPullReviews(ctx, repository, index)
	if err != nil {
		return err
	}
	// 双通道角色归属：内容结论（ai 等内容评审者的批准/驳回/COMMENT）驱动状态
	// 标签；状态结论（状态评审者的会签与门禁驳回）只表达门禁健康度，不参与
	// 内容判定——状态批准不等于内容批准，两条通道互不可替代。
	contentLatest, contentFound := m.latestContentReview(reviews)

	// 评审意图：存在尚未回应的待处理评审请求、晚于最新内容结论的评审请求记录
	// （原生按钮对已回应 reviewer 的复审信号），或评论中出现 @ai/@reviewer 提及、
	// 行首 /review 命令。
	// 「是否已回应」按评审者身份判定：提交过正式 review 的 reviewer 名下的请求
	// 视为历史（实测 reviewer 提交 review 后 Gitea 不消费 requested_reviewers，
	// 重复请求同一 reviewer 在字段上也是 no-op，无法与旧请求区分）；从未回应的
	// requested reviewer 与团队请求视为仍在等待回应。这样状态评审者的驳回不会
	// 遮蔽作者对内容 reviewer 的请求，而 reviewer 已回应（含 COMMENT 讨论）后球
	// 确定性回到作者。
	reviewIntent := hasUnansweredReviewRequest(pullRequest, reviews)
	if !reviewIntent {
		// 原生「请求评审」按钮对已回应过的 reviewer 仍有效：点击会生成晚于最新
		// 内容结论的请求记录，而 requested_reviewers 字段对重复请求是 no-op，
		// 表达不了这层复审意图。
		reviewIntent = hasFreshRequestRecord(reviews, contentLatest, contentFound)
	}
	if !reviewIntent {
		// 提及扫描只统计最新内容结论之后的评论，历史提及不会反复触发复审。
		// 这是 reviewer 已回应后作者重新请求评审的评论通道。
		var since time.Time
		if contentFound {
			since = contentLatest.Submitted
		}
		comments, err := m.api.ListIssueCommentsSince(ctx, repository, pullRequest.Index, since)
		if err != nil {
			return err
		}
		reviewIntent = hasReviewerMention(comments) || hasReviewCommand(comments)
	}
	if reviewIntent {
		m.logf("%s#%d: 检测到评审意图（请求、@reviewer 提及或 /review 命令）", repository.FullName(), pullRequest.Index)
	}

	// WIP/draft 的语义是作者仍在开发：Gitea 对 draft 恒报 Mergeable=false——合并
	// 被阻断，与冲突无关（实测 merge-tree 干净的 draft PR 也报 false），冲突/落后/
	// 检查门禁与自动驳回因此不适用。但显式评审意图仍然成立——WIP 不代表不能评审，
	// 检测到意图时进评审队列；否则收敛为 in-progress（也把曾被误标 changes-requested
	// 的 draft PR 拉回）。un-draft 后的下一次事件或 schedule 恢复完整门禁评估。
	if pullRequest.Draft {
		targetLabel := inProgressLabelName
		if reviewIntent {
			targetLabel = reviewLabelName
		}
		m.logf("%s#%d: WIP/draft，收敛到 %s", repository.FullName(), index, targetLabel)
		return m.setPullRequestLabels(ctx, repository, pullRequest, repositoryLabels, targetLabel)
	}

	latestState := unknownReviewStatus
	if contentFound {
		latestState = string(contentLatest.State)
	}
	behind, hasBranchState := branchBehind(pullRequest)
	m.logf(
		"检查 %s#%d: latest-review=%s, mergeable=%t, behind=%s",
		repository.FullName(),
		index,
		latestState,
		pullRequest.Mergeable,
		formatBranchState(behind, hasBranchState),
	)

	targetLabel := inProgressLabelName
	if contentFound || reviewIntent {
		// 有新评审意图时按 REQUEST_REVIEW 处理：无内容结论（评论请求）、
		// REQUEST_CHANGES 后的复审请求、COMMENT 讨论后的再次请求都走同一门禁。
		state := ReviewStateRequestReview
		if contentFound && !reviewIntent {
			state = contentLatest.State
		}
		switch state {
		case ReviewStateRequestReview, ReviewStateApproved:
			if !hasBranchState {
				return fmt.Errorf("missing base or merge-base commit metadata")
			}
			failedChecks, pendingChecks, err := m.checkStates(ctx, repository, pullRequest, protections)
			if err != nil {
				return err
			}
			// 检查仍在运行时本轮跳过：等检查完成再评估，避免基于半成品状态
			// 修改标签或提交驳回 review。由后续事件或 schedule 收敛。
			if len(pendingChecks) > 0 {
				m.logf(
					"%s#%d: 必要检查仍在运行: %v，本轮跳过",
					repository.FullName(),
					pullRequest.Index,
					checkContexts(pendingChecks),
				)
				return nil
			}
			if !pullRequest.Mergeable || behind || len(failedChecks) > 0 {
				return m.requestChangesAndSetStatus(
					ctx,
					repository,
					pullRequest,
					reviews,
					repositoryLabels,
					behind,
					failedChecks,
				)
			}
			if state == ReviewStateRequestReview {
				targetLabel = reviewLabelName
				m.logf("%s#%d: 需要 review", repository.FullName(), pullRequest.Index)
			} else {
				targetLabel = approvedLabelName
			}
		case ReviewStateRequestChanges:
			targetLabel = changesRequestedLabelName
		case ReviewStateComment:
			// reviewer 的 COMMENT review 是「提出了需要作者回应的讨论」：
			// 作者必须回复，因此与驳回一样等待作者处理。
			targetLabel = changesRequestedLabelName
			m.logf("%s#%d: reviewer 留下 COMMENT review，等待作者回应", repository.FullName(), pullRequest.Index)
		}
	}

	// 与目标分支冲突（不可合并）或已落后时，无论评审进展如何都需要作者先处理，
	// 不应停留在「开发中」。仅提升 in-progress，不覆盖已有的评审结论
	// （review/approved 的冲突与落后在上面的门禁里已打回，changes-requested 本就如此）。
	needsAuthorRebase := !pullRequest.Mergeable || (hasBranchState && behind)
	if needsAuthorRebase && targetLabel == inProgressLabelName {
		targetLabel = changesRequestedLabelName
		m.logf("%s#%d: 冲突或落后目标分支，标记 %s", repository.FullName(), pullRequest.Index, changesRequestedLabelName)
	}

	return m.setPullRequestLabels(ctx, repository, pullRequest, repositoryLabels, targetLabel)
}

func (m *Manager) requestChangesAndSetStatus(
	ctx context.Context,
	repository Repository,
	pullRequest PullRequest,
	reviews []Review,
	repositoryLabels map[string]Label,
	behind bool,
	failedChecks []CheckStatus,
) error {
	// 同一状态结论不必重复提交：状态通道的最新 review 已是驳回时，仅收敛标签
	// （状态驳回是门禁健康度的实时表达，标签每轮重推，评论不随每次失败刷屏）。
	if stateLatest, found := m.latestStateReview(reviews); found &&
		stateLatest.State == ReviewStateRequestChanges {
		return m.setPullRequestLabels(
			ctx,
			repository,
			pullRequest,
			repositoryLabels,
			changesRequestedLabelName,
		)
	}
	if pullRequest.HeadSHA == "" {
		return fmt.Errorf("missing head commit metadata")
	}
	m.logf("%s#%d: PR 不满足评审条件，提交状态驳回 review", repository.FullName(), pullRequest.Index)
	if err := m.api.CreatePullReview(ctx, repository, pullRequest.Index, ReviewInput{
		State:    ReviewStateRequestChanges,
		Body:     blockedReviewBody(pullRequest, behind, failedChecks),
		CommitID: pullRequest.HeadSHA,
	}); err != nil {
		return err
	}
	return m.setPullRequestLabels(
		ctx,
		repository,
		pullRequest,
		repositoryLabels,
		changesRequestedLabelName,
	)
}

func blockedReviewBody(pullRequest PullRequest, behind bool, failedChecks []CheckStatus) string {
	reasons := make([]string, 0, 2+len(failedChecks))
	if !pullRequest.Mergeable {
		reasons = append(reasons, "Gitea 当前判定该 PR 无法合并，请解决冲突或其他合并阻塞项。")
	}
	if behind {
		reasons = append(
			reasons,
			fmt.Sprintf("当前分支落后基础分支 %s，请先 rebase 到最新基础分支。", pullRequest.BaseRef),
		)
	}
	reasons = append(reasons, checkFailureReasons(failedChecks)...)
	return "该 PR 暂不满足评审条件：\n\n- " + strings.Join(reasons, "\n- ") +
		"\n\n请处理上述问题并更新 PR。本驳回来自 assistant，不影响你对 reviewer 的评审请求：" +
		"门禁通过后 PR 会自动回到评审队列。"
}

func branchBehind(pullRequest PullRequest) (bool, bool) {
	if pullRequest.BaseSHA == "" || pullRequest.MergeBase == "" {
		return false, false
	}
	return pullRequest.MergeBase != pullRequest.BaseSHA, true
}

func formatBranchState(behind, known bool) string {
	if !known {
		return "unknown"
	}
	return fmt.Sprint(behind)
}

func hasLabel(labels []Label, name string) bool {
	return slices.ContainsFunc(labels, func(label Label) bool {
		return label.Name == name
	})
}

// awaitingLabelForStatus 把 PR 的 status/* 状态映射为「轮到谁行动」的 awaiting/* 标签。
// 映射是确定性的：in-progress/changes-requested 都归作者，review 归 reviewer，
// approved 归合并人（人类工程师）。
func awaitingLabelForStatus(statusLabel string) string {
	switch statusLabel {
	case reviewLabelName:
		return awaitingReviewerLabelName
	case approvedLabelName:
		return awaitingMergeLabelName
	default:
		return awaitingAuthorLabelName
	}
}

// setPullRequestLabels 同时维护 PR 的两个标签维度：status/*（review 状态）与
// awaiting/*（行动方），两者由同一判断导出，必须同事务口径更新。
func (m *Manager) setPullRequestLabels(
	ctx context.Context,
	repository Repository,
	pullRequest PullRequest,
	repositoryLabels map[string]Label,
	statusLabel string,
) error {
	if err := m.keepOnlyPrefixedLabel(
		ctx, repository, pullRequest, repositoryLabels, statusLabelPrefix, statusLabel,
	); err != nil {
		return err
	}
	return m.keepOnlyPrefixedLabel(
		ctx, repository, pullRequest, repositoryLabels, awaitingLabelPrefix, awaitingLabelForStatus(statusLabel),
	)
}

func (m *Manager) keepOnlyPrefixedLabel(
	ctx context.Context,
	repository Repository,
	pullRequest PullRequest,
	repositoryLabels map[string]Label,
	prefix string,
	keepName string,
) error {
	keepLabel, ok := repositoryLabels[keepName]
	if !ok {
		return fmt.Errorf("required label %q is missing", keepName)
	}

	for _, label := range pullRequest.Labels {
		if !strings.HasPrefix(label.Name, prefix) || label.Name == keepName {
			continue
		}
		if err := m.api.RemoveLabel(ctx, repository, pullRequest.Index, label.ID); err != nil {
			return err
		}
		m.logf("%s#%d: 移除标签 %s", repository.FullName(), pullRequest.Index, label.Name)
	}
	if hasLabel(pullRequest.Labels, keepName) {
		return nil
	}
	if err := m.api.AddLabel(ctx, repository, pullRequest.Index, keepLabel.ID); err != nil {
		return err
	}
	m.logf("%s#%d: 添加标签 %s", repository.FullName(), pullRequest.Index, keepName)
	return nil
}

func (m *Manager) logf(format string, arguments ...any) {
	if m.progressf != nil {
		m.progressf(format, arguments...)
	}
}

// hasUnansweredReviewRequest 判断是否存在「尚未回应」的待处理评审请求：
// 提交过正式 review（APPROVED/REQUEST_CHANGES/COMMENT，含被 dismiss 的——
// dismiss 是批准的时效性，不代表未回应）的 reviewer，其名下请求视为已回应；
// 从未提交过正式 review 的 requested reviewer 与任何团队请求都视为仍在等待
// 回应。判定看全部历史而非仅最新 review：assistant 的自动驳回（gitea-actions
// 名下）在被 reviewer 的更新 review 挤出最新位置后，仍吸收 bot 自己的残留
// 请求，已批准的 PR 不会被打回 review 空等。
func hasUnansweredReviewRequest(pullRequest PullRequest, reviews []Review) bool {
	if pullRequest.RequestedReviewersTeams {
		return true
	}
	responded := make(map[string]bool, len(reviews))
	for _, review := range reviews {
		switch review.State {
		case ReviewStateApproved, ReviewStateRequestChanges, ReviewStateComment:
			responded[review.User] = true
		}
	}
	for _, reviewer := range pullRequest.RequestedReviewers {
		if !responded[reviewer] {
			return true
		}
	}
	return false
}

// reviewerMentions 是评论中视为「请求 reviewer 处理」的提及（不区分大小写）。
// reviewer 人员账号为 ai，别名 reviewer（见 AGENTS.md）。
var reviewerMentions = []string{"@ai", "@reviewer"}

// hasReviewerMention 判断评论中是否存在 reviewer 提及。提及后紧跟字母/数字/_/-
// 的不算（避免 @aim、@aisha 之类误命中）。
func hasReviewerMention(comments []Comment) bool {
	for _, comment := range comments {
		body := strings.ToLower(comment.Body)
		for _, mention := range reviewerMentions {
			if containsMention(body, mention) {
				return true
			}
		}
	}
	return false
}

func containsMention(body, mention string) bool {
	for offset := 0; offset <= len(body)-len(mention); {
		index := strings.Index(body[offset:], mention)
		if index < 0 {
			return false
		}
		end := offset + index + len(mention)
		if end == len(body) || !isNameChar(body[end]) {
			return true
		}
		offset = end
	}
	return false
}

// reviewCommand 是评论中拉起评审的 slash 命令（不区分大小写）。与 @ai 提及
// 不同，命令是刻意的祈使动作：必须独占行首，且后面只能跟空白或行尾——
// /reviewer、/review。等写法都不算，避免正文引用误触发。
const reviewCommand = "/review"

// hasReviewCommand 判断评论中是否存在行首 /review 命令。与 hasReviewerMention
// 同一调用点、同一扫描窗口：只统计最新正式 review 之后的评论。
func hasReviewCommand(comments []Comment) bool {
	for _, comment := range comments {
		for line := range strings.Lines(strings.ToLower(comment.Body)) {
			// strings.Lines 保留行终止符（含 \n，CRLF 输入还会带 \r）；不
			// TrimRight 则 "/review\r\n" != "/review"，CRLF 评论直接漏判。
			// （会剥掉 \r 的是 bufio.Scanner 一类按行读取器；strings.Split
			// 与 strings.Lines 一样保留它。）
			line = strings.TrimRight(line, "\r\n")
			if line == reviewCommand ||
				strings.HasPrefix(line, reviewCommand+" ") || strings.HasPrefix(line, reviewCommand+"\t") {
				return true
			}
		}
	}
	return false
}

// isNameChar 判断提及后紧跟的字符是否属于用户名的一部分。非 ASCII 字节
// （>=0x80，多字节 UTF-8 的组成部分）保守地视为名字字符，避免 @ai小明 这类
// 提及误命中。
func isNameChar(b byte) bool {
	return b >= 0x80 || b == '_' || b == '-' ||
		'0' <= b && b <= '9' || 'a' <= b && b <= 'z'
}

// legacyStateReviewer 是 sync 以 Actions 内置令牌提交门禁驳回的历史身份。
// sync 只提交状态类 review（门禁驳回），因此该账号名下的 review 一律归入
// 状态通道，不参与内容判定。
const legacyStateReviewer = "gitea-actions"

// isStateAuthor 判断 review 作者是否属于状态评审通道：配置的状态评审账号
// （merge 令牌身份），以及 sync 自身的历史驳回身份。
func (m *Manager) isStateAuthor(user string) bool {
	return user != "" && (user == m.stateReviewer || user == legacyStateReviewer)
}

// latestContentReview 返回内容通道的最新正式 review：内容结论只能是内容评审者
// 的判断，状态评审者的会签与门禁驳回不参与内容判定。
func (m *Manager) latestContentReview(reviews []Review) (Review, bool) {
	return latestReviewBy(reviews, func(user string) bool { return !m.isStateAuthor(user) })
}

// latestStateReview 返回状态通道的最新正式 review（状态评审者的会签或门禁
// 驳回）。未配置状态评审者时通道只含 sync 的历史驳回（gitea-actions）。
func (m *Manager) latestStateReview(reviews []Review) (Review, bool) {
	return latestReviewBy(reviews, m.isStateAuthor)
}

func latestReviewBy(reviews []Review, include func(string) bool) (Review, bool) {
	var latest Review
	found := false
	for _, review := range reviews {
		// REQUEST_REVIEW 是「评审请求」的时间线记录而非评审结论（其作者字段还是
		// 被请求的 reviewer 而非请求发起者），且它与 requested_reviewers 的出现
		// 并不同步（实测有的请求有记录、有的没有）——不能当作最新回应参与判定，
		// 请求意图统一由 requested_reviewers 表达。
		if review.Dismissed || review.State == ReviewStatePending || review.State == ReviewStateRequestReview {
			continue
		}
		if !include(review.User) {
			continue
		}
		if !found || review.Submitted.After(latest.Submitted) ||
			(review.Submitted.Equal(latest.Submitted) && review.ID > latest.ID) {
			latest = review
			found = true
		}
	}
	return latest, found
}

// hasFreshRequestRecord 判断是否存在晚于最新内容结论的评审请求记录。
// REQUEST_REVIEW 记录的「有无」并不可靠（实测有的请求有记录、有的没有），因此
// 只作补充信号：记录缺失时退化为 requested_reviewers 与评论扫描的原有判定。
// 记录晚于最新内容结论则是一次明确的复审请求——reviewer 已回应后原生按钮
// 无法通过 requested_reviewers 表达新意图（重复请求在字段上是 no-op），这条
// 时间线记录是按钮留下的唯一新鲜证据。请求面向内容评审者，时间基准取内容
// 通道的最新结论，状态评审者的会签不重置该窗口。
func hasFreshRequestRecord(reviews []Review, contentLatest Review, contentFound bool) bool {
	for _, review := range reviews {
		if review.State != ReviewStateRequestReview {
			continue
		}
		if !contentFound || review.Submitted.After(contentLatest.Submitted) {
			return true
		}
	}
	return false
}
