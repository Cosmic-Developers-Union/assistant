package status

import (
	"slices"
	"testing"
	"time"
)

// 准备一个带评审请求（requested_reviewers）的待评审 PR。
// 评审意图由 requested_reviewers 表达；REQUEST_REVIEW 时间线记录保留在 fixture
// 中，用于同时验证它不会被当作评审结论。
func newReviewCandidateAPI(t *testing.T) (*fakeAPI, Repository, []Label) {
	t.Helper()
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 21}}
	pr := mergeablePullRequest(21, nil)
	pr.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 21)] = pr
	api.reviews[pullRequestKey(repository, 21)] = []Review{{
		ID: 1, State: ReviewStateRequestReview, Submitted: time.Unix(10, 0), User: "ai",
	}}
	return api, repository, labels
}

// 评审请求不被门禁阻断：必要检查失败时照常进入 review 队列，不自动驳回、不
// 改变球权。status/review 是「评审请求中」标记，门禁只在 automerge 合并时校验。
func TestSyncQueuesReviewWhenCheckFails(t *testing.T) {
	api, _, labels := newReviewCandidateAPI(t)
	api.statuses["head"] = []CheckStatus{
		{Context: "build / test", State: "failure", TargetURL: "https://ci.example.com/runs/1"},
		{Context: "lint", State: "success"},
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none（门禁不阻断评审）", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 21, Label: labelByName(t, labels, reviewLabelName).ID},
		{Item: 21, Label: labelByName(t, labels, awaitingReviewerLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
}

// 检查仍在运行（pending）同样不跳过、不阻断：请求照常排队。
func TestSyncQueuesReviewWhileChecksPending(t *testing.T) {
	api, _, labels := newReviewCandidateAPI(t)
	api.statuses["head"] = []CheckStatus{
		{Context: "build / test", State: "pending"},
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 21, Label: labelByName(t, labels, reviewLabelName).ID},
		{Item: 21, Label: labelByName(t, labels, awaitingReviewerLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
}

// 落后基础分支同样不阻断评审请求（此前会被自动驳回为 changes-requested）。
func TestSyncQueuesStaleReviewRequest(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 12}}
	pr := stalePullRequest(12, nil)
	pr.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 12)] = pr

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 12, Label: labelByName(t, labels, reviewLabelName).ID},
		{Item: 12, Label: labelByName(t, labels, awaitingReviewerLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
}

// checkStates 是 automerge 的门禁读取：配置了分支保护时只统计必要 context。
func TestCheckStatesRespectsRequiredContexts(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	api := newFakeAPI(repository, nil)
	api.statuses["head"] = []CheckStatus{
		{Context: "build / test", State: "failure"},
		{Context: "optional / lint", State: "failure"},
		{Context: "slow / integration", State: "pending"},
	}
	manager := NewManager(api)
	failed, pending, err := manager.checkStates(t.Context(), repository, mergeablePullRequest(21, nil), []BranchProtection{{
		RuleName:          "main",
		EnableStatusCheck: true,
		Contexts:          []string{"build / test", "slow / integration"},
	}})
	if err != nil {
		t.Fatalf("checkStates() error = %v", err)
	}
	if len(failed) != 1 || failed[0].Context != "build / test" {
		t.Errorf("failed = %+v, want only build / test", failed)
	}
	if len(pending) != 1 || pending[0].Context != "slow / integration" {
		t.Errorf("pending = %+v, want only slow / integration", pending)
	}
}

// 未配置分支保护时回退为「任何失败 context 即阻塞」，但排除 assistant 自己的
// sync 工作流 context：排队运行被取消会留下 failure 状态（"Canceled after 0s"），
// bot 自身的运行结果不是 PR 的质量信号。
func TestCheckStatesFallbackExcludesSelfWorkflow(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	api := newFakeAPI(repository, nil)
	api.statuses["head"] = []CheckStatus{
		{Context: "Gitea Assistant / run (pull_request)", State: "failure"},
		{Context: "Gitea Assistant Build / build (pull_request)", State: "failure"},
	}
	manager := NewManager(api)
	failed, _, err := manager.checkStates(t.Context(), repository, mergeablePullRequest(21, nil), nil)
	if err != nil {
		t.Fatalf("checkStates() error = %v", err)
	}
	if len(failed) != 1 || failed[0].Context != "Gitea Assistant Build / build (pull_request)" {
		t.Errorf("failed = %+v, want only build context", failed)
	}
}

func TestBranchRuleMatches(t *testing.T) {
	for _, test := range []struct {
		rule, branch string
		want         bool
	}{
		{"main", "main", true},
		{"main", "develop", false},
		{"release/*", "release/1.2", true},
		{"*", "main", true},
		{"[", "main", false},
	} {
		if got := branchRuleMatches(test.rule, test.branch); got != test.want {
			t.Errorf("branchRuleMatches(%q, %q) = %t, want %t", test.rule, test.branch, got, test.want)
		}
	}
}
