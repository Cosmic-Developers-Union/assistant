package status

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func newAutoMergeAPI(t *testing.T) (*fakeAPI, Repository) {
	t.Helper()
	repository := Repository{Owner: "acme", Name: "video"}
	return newFakeAPI(repository, completeLabels()), repository
}

// withReviewerApproval 以内容评审者（ai）身份对 PR 当前 head 提交官方批准——
// automerge 的合并门禁（新规范：不依赖标签，批准锚定 head）。
func withReviewerApproval(api *fakeAPI, repository Repository, index int64, headSHA string) {
	key := pullRequestKey(repository, index)
	api.reviews[key] = append(api.reviews[key], Review{
		ID: int64(len(api.reviews[key]) + 1), State: ReviewStateApproved,
		Submitted: time.Unix(10, 0), User: "ai", CommitID: headSHA, Official: true,
	})
}

func TestAutoMergeMergesApprovedFreshPullRequest(t *testing.T) {
	api, repository := newAutoMergeAPI(t)
	candidate := mergeablePullRequest(21, nil)
	api.pullRequests[repository.FullName()] = []PullRequest{candidate}
	api.current[pullRequestKey(repository, 21)] = candidate
	withReviewerApproval(api, repository, 21, "head")

	if err := NewManager(api).AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if !slices.Equal(api.mergedPulls, []int64{21}) {
		t.Errorf("merged pulls = %v, want [21]", api.mergedPulls)
	}
}

// 配置状态评审者后，合并前先以状态评审者身份对 head 会签（required approvals
// 的第二票），随后立即合并——会签与合并是同一机械动作，时序严格先后。
func TestAutoMergeCountersignsThenMerges(t *testing.T) {
	api, repository := newAutoMergeAPI(t)
	candidate := mergeablePullRequest(21, nil)
	api.pullRequests[repository.FullName()] = []PullRequest{candidate}
	api.current[pullRequestKey(repository, 21)] = candidate
	withReviewerApproval(api, repository, 21, "head")

	manager := NewManager(api, WithStateReviewer("merge"))
	if err := manager.AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if !slices.Equal(api.mergedPulls, []int64{21}) {
		t.Errorf("merged pulls = %v, want [21]", api.mergedPulls)
	}
	if len(api.createdReviews) != 1 {
		t.Fatalf("created reviews = %+v, want exactly one countersign", api.createdReviews)
	}
	countersign := api.createdReviews[0]
	if countersign.Input.State != ReviewStateApproved {
		t.Errorf("countersign state = %q, want APPROVED", countersign.Input.State)
	}
	if countersign.Input.CommitID != candidate.HeadSHA {
		t.Errorf("countersign commit = %q, want head %q", countersign.Input.CommitID, candidate.HeadSHA)
	}
	if !slices.Equal(api.ops, []string{"review:APPROVED", "merge"}) {
		t.Errorf("ops = %v, want [review:APPROVED merge]（先会签后合并）", api.ops)
	}
}

// 会签按 head 去重：同一 head 已有本人的状态批准（上次合并调用失败的残留）
// 时不重复盖章，直接重试合并。
func TestAutoMergeCountersignDedupedPerHead(t *testing.T) {
	api, repository := newAutoMergeAPI(t)
	candidate := mergeablePullRequest(21, nil)
	api.pullRequests[repository.FullName()] = []PullRequest{candidate}
	api.current[pullRequestKey(repository, 21)] = candidate
	withReviewerApproval(api, repository, 21, "head")
	api.reviews[pullRequestKey(repository, 21)] = append(api.reviews[pullRequestKey(repository, 21)], Review{
		ID: 1, State: ReviewStateApproved, Submitted: time.Unix(10, 0),
		User: "merge", CommitID: candidate.HeadSHA, Official: true,
	})

	manager := NewManager(api, WithStateReviewer("merge"))
	if err := manager.AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if !slices.Equal(api.mergedPulls, []int64{21}) {
		t.Errorf("merged pulls = %v, want [21]", api.mergedPulls)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none（同 head 不重复会签）", api.createdReviews)
	}
	if !slices.Equal(api.ops, []string{"merge"}) {
		t.Errorf("ops = %v, want [merge]", api.ops)
	}
}

// 未配置状态评审者时保持历史行为：不产生会签，仅依赖 automerge 自身的门禁。
func TestAutoMergeWithoutStateReviewerSkipsCountersign(t *testing.T) {
	api, repository := newAutoMergeAPI(t)
	candidate := mergeablePullRequest(21, nil)
	api.pullRequests[repository.FullName()] = []PullRequest{candidate}
	api.current[pullRequestKey(repository, 21)] = candidate
	withReviewerApproval(api, repository, 21, "head")

	if err := NewManager(api).AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if !slices.Equal(api.mergedPulls, []int64{21}) {
		t.Errorf("merged pulls = %v, want [21]", api.mergedPulls)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none（未配置状态评审者）", api.createdReviews)
	}
}

// 一次运行至多合并一个 PR：main 随第一笔合并前移，其余 PR 全部过期，
// 必须留给下一轮（sync 会把他们打回 changes-requested）。
func TestAutoMergeMergesAtMostOnePullRequestPerRun(t *testing.T) {
	api, repository := newAutoMergeAPI(t)
	first := mergeablePullRequest(21, nil)
	second := mergeablePullRequest(33, nil)
	api.pullRequests[repository.FullName()] = []PullRequest{second, first}
	api.current[pullRequestKey(repository, 21)] = first
	withReviewerApproval(api, repository, 21, "head")
	api.current[pullRequestKey(repository, 33)] = second
	withReviewerApproval(api, repository, 33, "head")

	if err := NewManager(api).AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if !slices.Equal(api.mergedPulls, []int64{21}) {
		t.Errorf("merged pulls = %v, want [21]（编号最小者，且只此一笔）", api.mergedPulls)
	}
}

func TestAutoMergeSkipsStalePullRequest(t *testing.T) {
	api, repository := newAutoMergeAPI(t)
	candidate := stalePullRequest(21, nil)
	api.pullRequests[repository.FullName()] = []PullRequest{candidate}
	api.current[pullRequestKey(repository, 21)] = candidate
	withReviewerApproval(api, repository, 21, "head")

	if err := NewManager(api).AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if len(api.mergedPulls) != 0 {
		t.Errorf("merged pulls = %v, want none（落后即过期，不得合并）", api.mergedPulls)
	}
}

func TestAutoMergeSkipsUnmergeablePullRequest(t *testing.T) {
	api, repository := newAutoMergeAPI(t)
	candidate := unmergeablePullRequest(21, nil)
	api.pullRequests[repository.FullName()] = []PullRequest{candidate}
	api.current[pullRequestKey(repository, 21)] = candidate
	withReviewerApproval(api, repository, 21, "head")

	if err := NewManager(api).AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if len(api.mergedPulls) != 0 {
		t.Errorf("merged pulls = %v, want none", api.mergedPulls)
	}
}

// 合并 API 报错必须上抛并立即停止该仓库的候选队列：错误被吞掉继续合并的话，
// main 前移后可能误合过期 PR，「一次一合」的保证被绕过。mergeCalls 计数区分
// 「第一笔报错即停」与「后续候选仍被尝试（即使也失败）」。
func TestAutoMergeStopsRepositoryQueueOnMergeError(t *testing.T) {
	api, repository := newAutoMergeAPI(t)
	first := mergeablePullRequest(21, nil)
	second := mergeablePullRequest(33, nil)
	api.pullRequests[repository.FullName()] = []PullRequest{first, second}
	api.current[pullRequestKey(repository, 21)] = first
	withReviewerApproval(api, repository, 21, "head")
	api.current[pullRequestKey(repository, 33)] = second
	withReviewerApproval(api, repository, 33, "head")
	api.mergeError = errors.New("merge rejected")

	err := NewManager(api).AutoMerge(t.Context())
	if err == nil {
		t.Fatal("AutoMerge() error = nil, want merge failure to surface")
	}
	if !strings.Contains(err.Error(), "merge rejected") {
		t.Errorf("error = %v, want wrapped merge failure", err)
	}
	if api.mergeCalls != 1 {
		t.Errorf("merge calls = %d, want 1（报错即停，不尝试后续候选）", api.mergeCalls)
	}
	if len(api.mergedPulls) != 0 {
		t.Errorf("merged pulls = %v, want none", api.mergedPulls)
	}
}

func TestAutoMergeWaitsForPendingChecks(t *testing.T) {
	api, repository := newAutoMergeAPI(t)
	candidate := mergeablePullRequest(21, nil)
	api.pullRequests[repository.FullName()] = []PullRequest{candidate}
	api.current[pullRequestKey(repository, 21)] = candidate
	withReviewerApproval(api, repository, 21, "head")
	api.statuses["head"] = []CheckStatus{{Context: "build / test", State: "pending"}}

	if err := NewManager(api).AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if len(api.mergedPulls) != 0 {
		t.Errorf("merged pulls = %v, want none（检查未完成不合并，下一轮再看）", api.mergedPulls)
	}
	if len(api.armCalls) != 0 {
		t.Errorf("arm calls = %v, want none（未配置必要检查保护时不得武装）", api.armCalls)
	}
}

// armedPullRequestAPI 构造「内容已批准 + 必要检查运行中 + 保护规则配置了必要
// 检查」的武装场景。
func armedPullRequestAPI(t *testing.T) (*fakeAPI, Repository, PullRequest) {
	t.Helper()
	api, repository := newAutoMergeAPI(t)
	candidate := mergeablePullRequest(21, nil)
	api.pullRequests[repository.FullName()] = []PullRequest{candidate}
	api.current[pullRequestKey(repository, 21)] = candidate
	withReviewerApproval(api, repository, 21, "head")
	api.protections[repository.FullName()] = []BranchProtection{
		{RuleName: "main", EnableStatusCheck: true, Contexts: []string{"e2e/build"}},
	}
	api.statuses["head"] = []CheckStatus{{Context: "e2e/build", State: "pending"}}
	return api, repository, candidate
}

// 必要检查运行中时武装 Gitea 原生 auto-merge：会签前移到武装之前（检查变绿后
// Gitea 重新校验 required approvals，批准门禁必须在武装时满足），本轮不实际
// 合并——main 不动，盲区由服务端在检查转绿时即时补上。
func TestAutoMergeArmsNativeAutoMergeWhileChecksPending(t *testing.T) {
	api, _, candidate := armedPullRequestAPI(t)

	manager := NewManager(api, WithStateReviewer("merge"))
	if err := manager.AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if len(api.mergedPulls) != 0 {
		t.Errorf("merged pulls = %v, want none（检查运行中只武装不合并）", api.mergedPulls)
	}
	if !slices.Equal(api.ops, []string{"review:APPROVED", "arm"}) {
		t.Errorf("ops = %v, want [review:APPROVED arm]（先会签后武装）", api.ops)
	}
	if !slices.Equal(api.armHeads, []string{candidate.HeadSHA}) {
		t.Errorf("arm heads = %v, want [%s]（武装钉定当前 head）", api.armHeads, candidate.HeadSHA)
	}
	if !api.armedPulls[21] {
		t.Error("armed pulls 不含 #21，want 武装成功")
	}
	if len(api.createdReviews) != 1 || !strings.Contains(api.createdReviews[0].Input.Body, "武装原生 auto-merge") {
		t.Errorf("created reviews = %+v, want 武装形态的会签文案", api.createdReviews)
	}
}

// 未配置状态评审者时武装保持历史单通道行为：不盖章，直接武装。
func TestAutoMergeArmsWithoutStateReviewer(t *testing.T) {
	api, _, _ := armedPullRequestAPI(t)

	if err := NewManager(api).AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if !slices.Equal(api.ops, []string{"arm"}) {
		t.Errorf("ops = %v, want [arm]", api.ops)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none（未配置状态评审者）", api.createdReviews)
	}
}

// 重复运行幂等：同 head 会签已在、排定已在（409），不重复盖章也不产生副作用。
func TestAutoMergeArmIdempotentAcrossRuns(t *testing.T) {
	api, repository, candidate := armedPullRequestAPI(t)
	api.armResult = AutoMergeAlreadyArmed
	api.reviews[pullRequestKey(repository, 21)] = append(api.reviews[pullRequestKey(repository, 21)], Review{
		ID: 2, State: ReviewStateApproved, Submitted: time.Unix(10, 0),
		User: "merge", CommitID: candidate.HeadSHA, Official: true,
	})

	manager := NewManager(api, WithStateReviewer("merge"))
	if err := manager.AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if !slices.Equal(api.ops, []string{"arm:already"}) {
		t.Errorf("ops = %v, want [arm:already]（会签已在不重盖，排定已在幂等）", api.ops)
	}
	if len(api.mergedPulls) != 0 {
		t.Errorf("merged pulls = %v, want none", api.mergedPulls)
	}
}

// 武装时检查恰好已全绿（快照 pending、服务端已绿）：直接完成合并，等价于本轮
// 的那一笔实际合并——一次运行至多一笔的纪律不变，后续候选不再处理。
func TestAutoMergeArmMergesDirectlyWhenChecksTurnedGreen(t *testing.T) {
	api, repository, _ := armedPullRequestAPI(t)
	api.armResult = AutoMergeMergedNow
	second := mergeablePullRequest(33, nil)
	api.pullRequests[repository.FullName()] = append(api.pullRequests[repository.FullName()], second)
	api.current[pullRequestKey(repository, 33)] = second
	withReviewerApproval(api, repository, 33, "head")

	manager := NewManager(api, WithStateReviewer("merge"))
	if err := manager.AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if !slices.Equal(api.mergedPulls, []int64{21}) {
		t.Errorf("merged pulls = %v, want [21]（武装即合并后本轮不再处理 #33）", api.mergedPulls)
	}
	if !slices.Equal(api.ops, []string{"review:APPROVED", "arm:merged"}) {
		t.Errorf("ops = %v, want [review:APPROVED arm:merged]", api.ops)
	}
}

// 武装失败不上抛（405「Please try again later」是合并性异步计算的常态），不
// 挡住本轮后续候选——schedule 兜底仍然在。
func TestAutoMergeArmFailureFallsBackToSchedule(t *testing.T) {
	api, repository, _ := armedPullRequestAPI(t)
	api.armError = errors.New("HTTP 405")
	second := mergeablePullRequest(33, nil)
	second.HeadSHA = "head-33"
	api.pullRequests[repository.FullName()] = append(api.pullRequests[repository.FullName()], second)
	api.current[pullRequestKey(repository, 33)] = second
	withReviewerApproval(api, repository, 33, "head-33")

	manager := NewManager(api, WithStateReviewer("merge"))
	if err := manager.AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v, want nil（武装失败回退 schedule，不上抛）", err)
	}
	if !slices.Equal(api.mergedPulls, []int64{33}) {
		t.Errorf("merged pulls = %v, want [33]（武装失败不挡后续候选）", api.mergedPulls)
	}
	if !slices.Equal(api.ops, []string{"review:APPROVED", "arm:error", "review:APPROVED", "merge"}) {
		t.Errorf("ops = %v", api.ops)
	}
}

// 内容批准不在当前 head 时必须撤武：Gitea 1.27 的排定不会随批准撤销自灭，
// 不撤的话检查转绿即合并，绕过内容门禁。
func TestAutoMergeDisarmsWhenApprovalGone(t *testing.T) {
	api, repository, _ := armedPullRequestAPI(t)
	api.armedPulls[21] = true
	// 批准消失：内容评审者的 review 已不在
	delete(api.reviews, pullRequestKey(repository, 21))

	if err := NewManager(api).AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if api.armedPulls[21] {
		t.Error("armed pulls 仍含 #21，want 已撤销（批准消失不得保持武装）")
	}
	if len(api.armCalls) != 0 {
		t.Errorf("arm calls = %v, want none", api.armCalls)
	}
}

// 必要检查失败同样撤武：检查转绿的合并尝试不该由旧排定代发。
func TestAutoMergeDisarmsOnFailedChecks(t *testing.T) {
	api, _, _ := armedPullRequestAPI(t)
	api.statuses["head"] = []CheckStatus{{Context: "e2e/build", State: "failure"}}
	api.armedPulls[21] = true

	if err := NewManager(api).AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if api.armedPulls[21] {
		t.Error("armed pulls 仍含 #21，want 已撤销（检查失败不得保持武装）")
	}
	if len(api.mergedPulls) != 0 || len(api.armCalls) != 0 {
		t.Errorf("merged = %v, arm calls = %v, want none/none", api.mergedPulls, api.armCalls)
	}
}

func TestAutoMergeSkipsFailedChecks(t *testing.T) {
	api, repository := newAutoMergeAPI(t)
	candidate := mergeablePullRequest(21, nil)
	api.pullRequests[repository.FullName()] = []PullRequest{candidate}
	api.current[pullRequestKey(repository, 21)] = candidate
	withReviewerApproval(api, repository, 21, "head")
	api.statuses["head"] = []CheckStatus{{Context: "build / test", State: "failure"}}

	if err := NewManager(api).AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if len(api.mergedPulls) != 0 {
		t.Errorf("merged pulls = %v, want none", api.mergedPulls)
	}
}

func TestAutoMergeSkipsDraftAndWIPTitles(t *testing.T) {
	for name, mutate := range map[string]func(*PullRequest){
		"native draft": func(p *PullRequest) { p.Draft = true },
		"WIP: prefix":  func(p *PullRequest) { p.Title = "WIP: feat(x)" },
		"[WIP] prefix": func(p *PullRequest) { p.Title = "[WIP] feat(x)" },
	} {
		t.Run(name, func(t *testing.T) {
			api, repository := newAutoMergeAPI(t)
			candidate := mergeablePullRequest(21, nil)
			mutate(&candidate)
			api.pullRequests[repository.FullName()] = []PullRequest{candidate}
			api.current[pullRequestKey(repository, 21)] = candidate
			withReviewerApproval(api, repository, 21, "head")

			if err := NewManager(api).AutoMerge(t.Context()); err != nil {
				t.Fatalf("AutoMerge() error = %v", err)
			}
			if len(api.mergedPulls) != 0 {
				t.Errorf("merged pulls = %v, want none", api.mergedPulls)
			}
		})
	}
}

// 候选来自列表快照；合并前重读发现标签已被 sync 更新（批准撤销、打回）时放弃合并。
func TestAutoMergeSkipsWhenApprovalGoneAfterSnapshot(t *testing.T) {
	api, repository := newAutoMergeAPI(t)
	candidate := mergeablePullRequest(21, nil)
	api.pullRequests[repository.FullName()] = []PullRequest{candidate}
	api.current[pullRequestKey(repository, 21)] = mergeablePullRequest(21, nil)

	if err := NewManager(api).AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if len(api.mergedPulls) != 0 {
		t.Errorf("merged pulls = %v, want none", api.mergedPulls)
	}
}

func TestAutoMergeWithoutCandidatesDoesNothing(t *testing.T) {
	api, repository := newAutoMergeAPI(t)
	api.pullRequests[repository.FullName()] = []PullRequest{
		mergeablePullRequest(21, nil), // 无内容评审者批准
	}

	if err := NewManager(api).AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if len(api.mergedPulls) != 0 {
		t.Errorf("merged pulls = %v, want none", api.mergedPulls)
	}
}

// 批准必须来自内容评审者本人：其他账号（包括状态评审者）的批准不构成合并门禁。
func TestAutoMergeIgnoresApprovalFromOthers(t *testing.T) {
	api, repository := newAutoMergeAPI(t)
	candidate := mergeablePullRequest(21, nil)
	api.pullRequests[repository.FullName()] = []PullRequest{candidate}
	api.current[pullRequestKey(repository, 21)] = candidate
	withReviewerApproval(api, repository, 21, "head")
	api.reviews[pullRequestKey(repository, 21)] = append(api.reviews[pullRequestKey(repository, 21)], Review{
		ID: 99, State: ReviewStateApproved, Submitted: time.Unix(20, 0),
		User: "someone-else", CommitID: "head", Official: true,
	})

	manager := NewManager(api, WithContentReviewer("alice"))
	if err := manager.AutoMerge(t.Context()); err != nil {
		t.Fatalf("AutoMerge() error = %v", err)
	}
	if len(api.mergedPulls) != 0 {
		t.Errorf("merged pulls = %v, want none（alice 未批准，他人批准不算）", api.mergedPulls)
	}
}
