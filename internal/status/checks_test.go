package status

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// 准备一个没有分支保护（回退模式：任何失败 context 阻塞评审）的待评审 PR。
// 评审意图由 requested_reviewers 表达；REQUEST_REVIEW 时间线记录保留在 fixture
// 中，用于同时验证它不会被当作评审结论（见 latestActiveReview）。
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

func TestSyncBlocksReviewWhenCheckFails(t *testing.T) {
	api, _, labels := newReviewCandidateAPI(t)
	api.statuses["head"] = []CheckStatus{
		{Context: "build / test", State: "failure", TargetURL: "https://ci.example.com/runs/1"},
		{Context: "lint", State: "success"},
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 1 {
		t.Fatalf("created reviews = %+v", api.createdReviews)
	}
	created := api.createdReviews[0]
	if created.Input.State != ReviewStateRequestChanges {
		t.Errorf("review state = %s", created.Input.State)
	}
	if !strings.Contains(created.Input.Body, "build / test") ||
		!strings.Contains(created.Input.Body, "https://ci.example.com/runs/1") ||
		strings.Contains(created.Input.Body, "lint") {
		t.Errorf("review body = %q", created.Input.Body)
	}
	wantAdded := []labelChange{
		{Item: 21, Label: labelByName(t, labels, changesRequestedLabelName).ID},
		{Item: 21, Label: labelByName(t, labels, awaitingAuthorLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
}

func TestSyncAllowsReviewWhenChecksPassOrPending(t *testing.T) {
	for name, statuses := range map[string][]CheckStatus{
		"all success": {
			{Context: "build / test", State: "success"},
			{Context: "lint", State: "success"},
		},
		"no checks at all": {},
	} {
		t.Run(name, func(t *testing.T) {
			api, _, labels := newReviewCandidateAPI(t)
			api.statuses["head"] = statuses

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
		})
	}
}

// 检查仍在运行（pending）时本轮跳过：不改标签、不提交 review，
// 等检查完成后由后续事件或 schedule 重新评估。
func TestSyncSkipsWhileChecksPending(t *testing.T) {
	api, _, _ := newReviewCandidateAPI(t)
	api.statuses["head"] = []CheckStatus{
		{Context: "build / test", State: "pending"},
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v", api.createdReviews)
	}
	if len(api.addedLabels) != 0 || len(api.removedLabels) != 0 {
		t.Errorf("added = %+v, removed = %+v, want 本轮跳过", api.addedLabels, api.removedLabels)
	}
}

func TestSyncBlocksOnlyRequiredContextsWhenProtectionConfigured(t *testing.T) {
	for name, test := range map[string]struct {
		statuses    []CheckStatus
		wantBlocked bool
	}{
		"required context failed": {
			statuses: []CheckStatus{
				{Context: "build / test", State: "failure"},
				{Context: "optional / lint", State: "failure"},
			},
			wantBlocked: true,
		},
		"non-required context failed": {
			statuses: []CheckStatus{
				{Context: "optional / lint", State: "failure"},
			},
			wantBlocked: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			api, repository, labels := newReviewCandidateAPI(t)
			api.protections[repository.FullName()] = []BranchProtection{{
				RuleName:          "main",
				EnableStatusCheck: true,
				Contexts:          []string{"build / test"},
			}}
			api.statuses["head"] = test.statuses

			if err := NewManager(api).Sync(t.Context()); err != nil {
				t.Fatalf("Sync() error = %v", err)
			}
			if test.wantBlocked {
				if len(api.createdReviews) != 1 {
					t.Fatalf("created reviews = %+v, want one REQUEST_CHANGES", api.createdReviews)
				}
				wantAdded := []labelChange{
					{Item: 21, Label: labelByName(t, labels, changesRequestedLabelName).ID},
					{Item: 21, Label: labelByName(t, labels, awaitingAuthorLabelName).ID},
				}
				if !slices.Equal(api.addedLabels, wantAdded) {
					t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
				}
				return
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
		})
	}
}

// 严格回退模式下排除 assistant 自己的 sync 工作流 context：排队运行被取消会
// 留下 failure 状态（"Canceled after 0s"），bot 自身的运行结果不是 PR 的质量
// 信号，不排除则一次取消就触发自我驳回循环。构建工作流（Gitea Assistant Build）
// 的 context 不受影响。
func TestSyncIgnoresOwnWorkflowContextInFallback(t *testing.T) {
	api, _, labels := newReviewCandidateAPI(t)
	api.statuses["head"] = []CheckStatus{
		{Context: "Gitea Assistant / run (pull_request)", State: "failure"},
		{Context: "Gitea Assistant Build / build (pull_request)", State: "success"},
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none（自身 context 不阻塞）", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 21, Label: labelByName(t, labels, reviewLabelName).ID},
		{Item: 21, Label: labelByName(t, labels, awaitingReviewerLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
}

func TestSyncFallsBackToStrictModeWhenProtectionsForbidden(t *testing.T) {
	api, repository, labels := newReviewCandidateAPI(t)
	api.protectError[repository.FullName()] = &PermissionError{Operation: "list branch protections"}
	api.statuses["head"] = []CheckStatus{
		{Context: "optional / lint", State: "failure"},
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 1 {
		t.Fatalf("created reviews = %+v, want one REQUEST_CHANGES", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 21, Label: labelByName(t, labels, changesRequestedLabelName).ID},
		{Item: 21, Label: labelByName(t, labels, awaitingAuthorLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
}

func TestSyncSkipsPullRequestsWhenProtectionsUnreadable(t *testing.T) {
	api, repository, _ := newReviewCandidateAPI(t)
	api.protectError[repository.FullName()] = errors.New("connection reset")

	err := NewManager(api).Sync(t.Context())
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 || len(api.addedLabels) != 0 || len(api.removedLabels) != 0 {
		t.Errorf("reviews = %+v, added = %+v, removed = %+v", api.createdReviews, api.addedLabels, api.removedLabels)
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
