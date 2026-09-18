package status

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type labelChange struct {
	Item  int64
	Label int64
}

type reviewChange struct {
	PullRequest int64
	Input       ReviewInput
}

type reviewRequestChange struct {
	PullRequest int64
	Reviewer    string
}

type fakeAPI struct {
	repositories    []Repository
	triageIssues    map[string][]Issue
	reviewPulls     map[string][]Issue
	openIssues      map[string][]Issue
	pullRequests    map[string][]PullRequest
	current         map[string]PullRequest
	reviews         map[string][]Review
	comments        map[string][]Comment
	labels          map[string][]Label
	protections     map[string][]BranchProtection
	statuses        map[string][]CheckStatus
	exclusive       []int64
	createdLabels   []LabelDefinition
	addedLabels     []labelChange
	removedLabels   []labelChange
	deletedLabels   []labelChange
	createdReviews  []reviewChange
	createdRequests []reviewRequestChange
	deletedRequests []reviewRequestChange
	closedIssues    []int64
	mergedPulls     []int64
	mergeCalls      int
	mergeError      error
	reviewError     error
	// armedPulls 是 ArmAutoMerge 的服务端排定状态；armResult 覆盖下一次武装
	// 的结果（默认 AutoMergeArmed），armHeads 记录武装钉定的 head。
	armedPulls     map[int64]bool
	armResult      AutoMergeArmResult
	armError       error
	armCalls       []int64
	armHeads       []string
	disarmError    error
	issueListError map[string]error
	pullListError  map[string]error
	protectError   map[string]error
	nextLabelID    int64
	// selfLogin 是 AuthenticatedUser 返回的身份（会签方），默认 "merge"。
	selfLogin string
	// ops 按发生顺序记录 review 提交与合并动作，供时序断言使用。
	ops []string
}

func (f *fakeAPI) ListRepositories(context.Context) ([]Repository, error) {
	return f.repositories, nil
}

func (f *fakeAPI) ListTriageIssues(_ context.Context, repository Repository) ([]Issue, error) {
	if err := f.issueListError[repository.FullName()]; err != nil {
		return nil, err
	}
	return f.triageIssues[repository.FullName()], nil
}

func (f *fakeAPI) ListReviewPullRequests(_ context.Context, repository Repository) ([]Issue, error) {
	return f.reviewPulls[repository.FullName()], nil
}

func (f *fakeAPI) ListOpenIssues(_ context.Context, repository Repository) ([]Issue, error) {
	return f.openIssues[repository.FullName()], nil
}

func (f *fakeAPI) CloseIssue(_ context.Context, _ Repository, index int64) error {
	f.closedIssues = append(f.closedIssues, index)
	return nil
}

func (f *fakeAPI) ListOpenPullRequests(_ context.Context, repository Repository) ([]PullRequest, error) {
	if err := f.pullListError[repository.FullName()]; err != nil {
		return nil, err
	}
	return f.pullRequests[repository.FullName()], nil
}

func (f *fakeAPI) GetPullRequest(_ context.Context, repository Repository, index int64) (PullRequest, error) {
	return f.current[pullRequestKey(repository, index)], nil
}

func (f *fakeAPI) MergePullRequest(_ context.Context, _ Repository, index int64) error {
	// 失败的尝试同样计数：区分「报错即停」与「报错后继续尝试下一候选」
	f.mergeCalls++
	f.ops = append(f.ops, "merge")
	if f.mergeError != nil {
		return f.mergeError
	}
	f.mergedPulls = append(f.mergedPulls, index)
	return nil
}

// ArmAutoMerge 模拟 Gitea merge 端点 merge_when_checks_succeed 的三分支：
// 排定（记状态）、已在排定（幂等）、检查已绿直接合并（计入 mergedPulls）。
func (f *fakeAPI) ArmAutoMerge(_ context.Context, _ Repository, index int64, headSHA string) (AutoMergeArmResult, error) {
	f.armCalls = append(f.armCalls, index)
	f.armHeads = append(f.armHeads, headSHA)
	if f.armError != nil {
		f.ops = append(f.ops, "arm:error")
		return "", f.armError
	}
	result := cmp.Or(f.armResult, AutoMergeArmed)
	switch result {
	case AutoMergeAlreadyArmed:
		f.ops = append(f.ops, "arm:already")
	case AutoMergeMergedNow:
		f.mergedPulls = append(f.mergedPulls, index)
		f.ops = append(f.ops, "arm:merged")
	default:
		f.armedPulls[index] = true
		f.ops = append(f.ops, "arm")
	}
	return result, nil
}

func (f *fakeAPI) DisarmAutoMerge(_ context.Context, _ Repository, index int64) (bool, error) {
	if f.disarmError != nil {
		return false, f.disarmError
	}
	if !f.armedPulls[index] {
		return false, nil
	}
	delete(f.armedPulls, index)
	return true, nil
}

func (f *fakeAPI) ListPullReviews(_ context.Context, repository Repository, index int64) ([]Review, error) {
	return f.reviews[pullRequestKey(repository, index)], nil
}

func (f *fakeAPI) ListIssueCommentsSince(
	_ context.Context,
	repository Repository,
	index int64,
	since time.Time,
) ([]Comment, error) {
	var result []Comment
	for _, comment := range f.comments[pullRequestKey(repository, index)] {
		// 模拟 Gitea 的 since 过滤：>=（秒级），边界评论由调用方再次严格过滤
		if since.IsZero() || !comment.Created.Before(since) {
			result = append(result, comment)
		}
	}
	return result, nil
}

func (f *fakeAPI) CreateIssueComment(_ context.Context, _ Repository, _ int64, _ string) error {
	return nil
}

func (f *fakeAPI) ListIssuesMentioning(_ context.Context, _ Repository, _ string, _ string) ([]Issue, error) {
	return nil, nil
}

func (f *fakeAPI) GetCombinedStatus(_ context.Context, _ Repository, sha string) ([]CheckStatus, error) {
	return f.statuses[sha], nil
}

func (f *fakeAPI) ListBranchProtections(_ context.Context, repository Repository) ([]BranchProtection, error) {
	if err := f.protectError[repository.FullName()]; err != nil {
		return nil, err
	}
	return f.protections[repository.FullName()], nil
}

func (f *fakeAPI) ListRepositoryLabels(_ context.Context, repository Repository) ([]Label, error) {
	return f.labels[repository.FullName()], nil
}

func (f *fakeAPI) SetLabelExclusive(_ context.Context, _ Repository, labelID int64) error {
	f.exclusive = append(f.exclusive, labelID)
	return nil
}

func (f *fakeAPI) DeleteLabel(_ context.Context, _ Repository, labelID int64) error {
	f.deletedLabels = append(f.deletedLabels, labelChange{Label: labelID})
	return nil
}

func (f *fakeAPI) CreateLabel(
	_ context.Context,
	_ Repository,
	definition LabelDefinition,
) (Label, error) {
	f.createdLabels = append(f.createdLabels, definition)
	f.nextLabelID++
	return Label{ID: f.nextLabelID, Name: definition.Name, Exclusive: definition.Exclusive}, nil
}

func (f *fakeAPI) AddLabel(_ context.Context, _ Repository, index, labelID int64) error {
	f.addedLabels = append(f.addedLabels, labelChange{Item: index, Label: labelID})
	return nil
}

func (f *fakeAPI) RemoveLabel(_ context.Context, _ Repository, index, labelID int64) error {
	f.removedLabels = append(f.removedLabels, labelChange{Item: index, Label: labelID})
	return nil
}

func (f *fakeAPI) CreatePullReview(_ context.Context, _ Repository, index int64, input ReviewInput) error {
	f.createdReviews = append(f.createdReviews, reviewChange{PullRequest: index, Input: input})
	f.ops = append(f.ops, "review:"+string(input.State))
	return f.reviewError
}

func (f *fakeAPI) CreateReviewRequests(_ context.Context, _ Repository, index int64, reviewers []string) error {
	for _, reviewer := range reviewers {
		f.createdRequests = append(f.createdRequests, reviewRequestChange{PullRequest: index, Reviewer: reviewer})
	}
	return nil
}

func (f *fakeAPI) DeleteReviewRequests(_ context.Context, _ Repository, index int64, reviewers []string) error {
	for _, reviewer := range reviewers {
		f.deletedRequests = append(f.deletedRequests, reviewRequestChange{PullRequest: index, Reviewer: reviewer})
	}
	return nil
}

func (f *fakeAPI) AuthenticatedUser(context.Context) (string, error) {
	if f.selfLogin == "" {
		return "merge", nil
	}
	return f.selfLogin, nil
}

func TestManagerCreatesCompleteLabelSchema(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	api := newFakeAPI(repository, nil)

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if !slices.Equal(api.createdLabels, labelDefinitions) {
		t.Errorf("created labels = %+v, want %+v", api.createdLabels, labelDefinitions)
	}
	if len(api.exclusive) != 0 {
		t.Errorf("exclusive labels = %v", api.exclusive)
	}
}

func TestManagerValidatesTargetRepositoryBeforeWriting(t *testing.T) {
	visible := Repository{Owner: "acme", Name: "visible"}
	target := Repository{Owner: "acme", Name: "target"}
	api := newFakeAPI(visible, nil)

	err := NewManager(api, WithRepository(target)).Sync(t.Context())
	if err == nil || !strings.Contains(err.Error(), target.FullName()) {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdLabels) != 0 || len(api.exclusive) != 0 {
		t.Fatalf("created labels = %+v, exclusive = %v", api.createdLabels, api.exclusive)
	}
}

func TestManagerMakesEveryScopedLabelExclusive(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := []Label{
		{ID: 1, Name: "type/bug"},
		{ID: 2, Name: "priority/high"},
		{ID: 3, Name: reviewLabelName},
		{ID: 4, Name: "needs-info"},
		{ID: 5, Name: "status/legacy"},
		{ID: 6, Name: awaitingAuthorLabelName},
	}
	api := newFakeAPI(repository, labels)

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	slices.Sort(api.exclusive)
	if !slices.Equal(api.exclusive, []int64{1, 2, 3, 5, 6}) {
		t.Errorf("exclusive labels = %v", api.exclusive)
	}
}

// 与目标分支冲突（不可合并）的 PR 无论是否有正式 review，都标记
// status/changes-requested（等待作者解决冲突），而不是 status/in-progress；
// 不自动提交驳回 review。
func TestManagerMarksConflictedPullRequestChangesRequested(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 20}}
	api.current[pullRequestKey(repository, 20)] = unmergeablePullRequest(20, nil)

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 20, Label: labelByName(t, labels, changesRequestedLabelName).ID},
		{Item: 20, Label: labelByName(t, labels, awaitingAuthorLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
}

// 落后基础分支（无冲突、可合并）的 PR 同样标记 status/changes-requested：
// 提醒作者该 rebase 了，而不是看似正常开发中。
func TestManagerMarksBehindPullRequestChangesRequested(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 22}}
	api.current[pullRequestKey(repository, 22)] = stalePullRequest(22, nil)

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 22, Label: labelByName(t, labels, changesRequestedLabelName).ID},
		{Item: 22, Label: labelByName(t, labels, awaitingAuthorLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
}

// 无评审意图的 WIP/draft PR 保持 status/in-progress：draft 恒报 mergeable=false
// （合并被阻断，与冲突无关），不应进冲突门禁；曾被误标 changes-requested 的
// draft 收敛回 in-progress（移除误标、不提交驳回 review）。
func TestManagerKeepsDraftPullRequestInProgress(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	changesLabel := labelByName(t, labels, changesRequestedLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 26}}
	api.current[pullRequestKey(repository, 26)] = draftPullRequest(26, []Label{changesLabel})

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 26, Label: labelByName(t, labels, inProgressLabelName).ID},
		{Item: 26, Label: labelByName(t, labels, awaitingAuthorLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	wantRemoved := []labelChange{{Item: 26, Label: changesLabel.ID}}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
}

// WIP/draft PR 存在评审意图时进评审队列：作者对 draft 明确请求评审（/review
// 命令、@ai 提及）应得到 status/review + awaiting/reviewer，而不是被 draft 语义
// 吞掉。门禁对 draft 不适用，也绝不自动提交驳回 review——draft 恒报不可合并，
// 走门禁会被误驳回。
func TestManagerMarksDraftPullRequestWithReviewCommandAsReview(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	changesLabel := labelByName(t, labels, changesRequestedLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 26}}
	api.current[pullRequestKey(repository, 26)] = draftPullRequest(26, []Label{changesLabel})
	api.comments[pullRequestKey(repository, 26)] = []Comment{
		{ID: 1, Body: "/review 麻烦先看下方向", Created: time.Unix(10, 0)},
		{ID: 2, Body: "@ai 请评审", Created: time.Unix(11, 0)},
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 26, Label: labelByName(t, labels, reviewLabelName).ID},
		{Item: 26, Label: labelByName(t, labels, awaitingReviewerLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	wantRemoved := []labelChange{{Item: 26, Label: changesLabel.ID}}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
}

// draft PR 的原生评审请求同样有效：requested_reviewers 中有从未回应的 reviewer
// 即视为评审意图，无需 un-draft。
func TestManagerMarksDraftPullRequestWithPendingRequestAsReview(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 27}}
	draft := draftPullRequest(27, nil)
	draft.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 27)] = draft

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 27, Label: labelByName(t, labels, reviewLabelName).ID},
		{Item: 27, Label: labelByName(t, labels, awaitingReviewerLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
}

// 评论中 @ai/@reviewer 提及视为评审请求：REQUEST_CHANGES 之后作者评论
// 「@ai 已处理」，门禁通过则进入 review，且不重复提交驳回 review。
func TestManagerTreatsReviewerMentionAsReviewRequest(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	changesLabel := labelByName(t, labels, changesRequestedLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 24}}
	api.current[pullRequestKey(repository, 24)] = mergeablePullRequest(24, []Label{changesLabel})
	api.reviews[pullRequestKey(repository, 24)] = []Review{{
		ID: 1, State: ReviewStateRequestChanges, Submitted: time.Unix(10, 0),
	}}
	api.comments[pullRequestKey(repository, 24)] = []Comment{
		{ID: 1, Body: "收到，我来改", Created: time.Unix(11, 0)},
		{ID: 2, Body: "@AI 已处理，麻烦再看下", Created: time.Unix(12, 0)},
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none (已有驳回记录)", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 24, Label: labelByName(t, labels, reviewLabelName).ID},
		{Item: 24, Label: labelByName(t, labels, awaitingReviewerLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	wantRemoved := []labelChange{{Item: 24, Label: changesLabel.ID}}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
}

// 没有任何正式 review 时，评论中的 @reviewer 提及同样走评审门禁。
func TestManagerTreatsMentionAsReviewRequestWithoutReviews(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 26}}
	api.current[pullRequestKey(repository, 26)] = mergeablePullRequest(26, nil)
	api.comments[pullRequestKey(repository, 26)] = []Comment{
		{ID: 1, Body: "@reviewer 初版完成，请评审", Created: time.Unix(10, 0)},
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wantAdded := []labelChange{
		{Item: 26, Label: labelByName(t, labels, reviewLabelName).ID},
		{Item: 26, Label: labelByName(t, labels, awaitingReviewerLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
}

// 提及早于最新驳回（历史评论）不构成新的评审意图；@aim 之类相似前缀不误命中。
func TestHasReviewerMention(t *testing.T) {
	for _, test := range []struct {
		body string
		want bool
	}{
		{"@ai 请看", true},
		{"@reviewer 麻烦评审", true},
		{"cc @AI", true},
		{"@ai-fix 已修", false},
		{"@aim 你好", false},
		{"@ai小明 不算提及", false},
		{"普通评论", false},
		{"at 符号 ai 没有提及", false},
	} {
		comments := []Comment{{Body: test.body}}
		if got := hasReviewerMention(comments); got != test.want {
			t.Errorf("hasReviewerMention(%q) = %t, want %t", test.body, got, test.want)
		}
	}
}

func TestHasReviewCommand(t *testing.T) {
	for _, test := range []struct {
		body string
		want bool
	}{
		{"/review", true},
		{"/review 麻烦再看一遍", true},
		{"/REVIEW", true},
		{"已按意见修改\n/review", true},
		{"已按意见修改\n/Review\t请复审", true},
		{"已按意见修改\r\n/review\r\n", true}, // CRLF 行尾（Windows 编辑的评论）
		{"请看 /review 这个命令", false},
		{"   /review", false}, // 必须独占行首，前导空白不算
		{"\t/review", false},
		{"/reviewer 不是命令", false},
		{"/review。直接跟标点不算", false},
		{"普通评论", false},
	} {
		comments := []Comment{{Body: test.body}}
		if got := hasReviewCommand(comments); got != test.want {
			t.Errorf("hasReviewCommand(%q) = %t, want %t", test.body, got, test.want)
		}
	}
}

func TestManagerSynchronizesPullRequestReviewStates(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	oldReviewLabel := labelByName(t, labels, reviewLabelName)
	approvedLabel := labelByName(t, labels, approvedLabelName)
	staleAwaiting := labelByName(t, labels, awaitingReviewerLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 1}, {Index: 2}, {Index: 3}, {Index: 4}}
	pr2 := mergeablePullRequest(2, nil)
	pr2.RequestedReviewers = []string{"ai"}
	api.current = map[string]PullRequest{
		pullRequestKey(repository, 1): mergeablePullRequest(1, nil),
		pullRequestKey(repository, 2): pr2,
		pullRequestKey(repository, 3): mergeablePullRequest(3, []Label{oldReviewLabel}),
		pullRequestKey(repository, 4): mergeablePullRequest(4, []Label{approvedLabel, staleAwaiting}),
	}
	api.reviews = map[string][]Review{
		pullRequestKey(repository, 3): {{ID: 3, State: ReviewStateRequestChanges, Submitted: time.Unix(30, 0), User: "ai"}},
		pullRequestKey(repository, 4): {{ID: 4, State: ReviewStateApproved, Submitted: time.Unix(40, 0), User: "ai"}},
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wantAdded := []labelChange{
		{Item: 1, Label: labelByName(t, labels, inProgressLabelName).ID},
		{Item: 1, Label: labelByName(t, labels, awaitingAuthorLabelName).ID},
		{Item: 2, Label: labelByName(t, labels, reviewLabelName).ID},
		{Item: 2, Label: labelByName(t, labels, awaitingReviewerLabelName).ID},
		{Item: 3, Label: labelByName(t, labels, changesRequestedLabelName).ID},
		{Item: 3, Label: labelByName(t, labels, awaitingAuthorLabelName).ID},
		{Item: 4, Label: labelByName(t, labels, awaitingMergeLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	wantRemoved := []labelChange{
		{Item: 3, Label: oldReviewLabel.ID},
		{Item: 4, Label: staleAwaiting.ID},
	}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
}

// 落后（不可合并）的 PR 上出现评审请求：进入 review 队列而不是被自动驳回—
// 门禁只在 automerge 合并时校验，评审本身照常拉起。
func TestManagerQueuesReviewRequestOnStalePullRequest(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	reviewLabel := labelByName(t, labels, reviewLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 12}}
	pr := stalePullRequest(12, []Label{reviewLabel})
	pr.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 12)] = pr

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Fatalf("created reviews = %+v, want none", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 12, Label: labelByName(t, labels, awaitingReviewerLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	if len(api.removedLabels) != 0 {
		t.Errorf("removed labels = %+v, want none", api.removedLabels)
	}
}

// assistant（gitea-actions）曾自动驳回，作者随后重新请求评审：请求优先，回到
// review 队列，旧驳回标签被替换。
func TestManagerQueuesReviewRequestOverOldRejection(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	changesLabel := labelByName(t, labels, changesRequestedLabelName)
	awaitingLabel := labelByName(t, labels, awaitingAuthorLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 18}}
	pr := stalePullRequest(18, []Label{changesLabel, awaitingLabel})
	pr.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 18)] = pr
	api.reviews[pullRequestKey(repository, 18)] = []Review{{
		ID: 1, State: ReviewStateRequestChanges, Submitted: time.Unix(10, 0), User: "gitea-actions",
	}}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 18, Label: labelByName(t, labels, reviewLabelName).ID},
		{Item: 18, Label: labelByName(t, labels, awaitingReviewerLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	wantRemoved := []labelChange{
		{Item: 18, Label: changesLabel.ID},
		{Item: 18, Label: awaitingLabel.ID},
	}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
}

// 与最新内容结论同秒的旧 /review 评论不算新意图（Gitea since 过滤是 >=，
// 本地必须严格过滤，否则会在批准后错误地重新登记评审请求）。
func TestManagerIgnoresCommentAtSameSecondAsReview(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	approvedLabel := labelByName(t, labels, approvedLabelName)
	awaitingMergeLabel := labelByName(t, labels, awaitingMergeLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 44}}
	pr := mergeablePullRequest(44, []Label{approvedLabel, awaitingMergeLabel})
	pr.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 44)] = pr
	api.reviews[pullRequestKey(repository, 44)] = []Review{
		{ID: 1, State: ReviewStateApproved, Submitted: time.Unix(20, 0), User: "ai"},
	}
	api.comments[pullRequestKey(repository, 44)] = []Comment{
		{ID: 1, Body: "/review", Created: time.Unix(20, 0)},
	}

	manager := NewManager(api)
	if err := manager.Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if err := manager.ReconcileReviewRequests(t.Context()); err != nil {
		t.Fatalf("ReconcileReviewRequests() error = %v", err)
	}
	if len(api.createdRequests) != 0 {
		t.Errorf("created requests = %+v, want none（同秒旧评论不是新意图）", api.createdRequests)
	}
	if len(api.addedLabels) != 0 || len(api.removedLabels) != 0 {
		t.Errorf("added = %+v removed = %+v, want none", api.addedLabels, api.removedLabels)
	}
	wantWithdrawn := []reviewRequestChange{{PullRequest: 44, Reviewer: "ai"}}
	if !slices.Equal(api.deletedRequests, wantWithdrawn) {
		t.Errorf("deleted requests = %+v, want %+v", api.deletedRequests, wantWithdrawn)
	}
}

// assistant（gitea-actions）自动驳回后，作者对 reviewer 的评审请求仍然有效：
// 请求未被回应，进入 review 队列。
func TestManagerHonorsPendingReviewRequestAfterRejection(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	changesLabel := labelByName(t, labels, changesRequestedLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 16}}
	pr := mergeablePullRequest(16, []Label{changesLabel})
	pr.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 16)] = pr
	api.reviews[pullRequestKey(repository, 16)] = []Review{{
		ID: 1, State: ReviewStateRequestChanges, Submitted: time.Unix(10, 0), User: "gitea-actions",
	}}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none (已有驳回记录)", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 16, Label: labelByName(t, labels, reviewLabelName).ID},
		{Item: 16, Label: labelByName(t, labels, awaitingReviewerLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	wantRemoved := []labelChange{{Item: 16, Label: changesLabel.ID}}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
}

// reviewer 本人批准时，其名下残留的待处理评审请求（Gitea 不消费
// requested_reviewers）视为已回应：批准成立，标记 approved。
func TestManagerKeepsApprovedDespiteStalePendingRequest(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	reviewLabel := labelByName(t, labels, reviewLabelName)
	awaitingReviewerLabel := labelByName(t, labels, awaitingReviewerLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 19}}
	pr := mergeablePullRequest(19, []Label{reviewLabel, awaitingReviewerLabel})
	pr.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 19)] = pr
	api.reviews[pullRequestKey(repository, 19)] = []Review{{
		ID: 1, State: ReviewStateApproved, Submitted: time.Unix(10, 0), User: "ai",
	}}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 19, Label: labelByName(t, labels, approvedLabelName).ID},
		{Item: 19, Label: labelByName(t, labels, awaitingMergeLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	wantRemoved := []labelChange{
		{Item: 19, Label: reviewLabel.ID},
		{Item: 19, Label: awaitingReviewerLabel.ID},
	}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
}

// 「已回应」判定看全部正式 review 历史而非仅最新一条：assistant（gitea-actions）
// 自动驳回过、reviewer 随后批准时，两个账号名下的残留请求都已被各自吸收——
// 批准成立，标记 approved。若只看最新 review 作者，bot 的残留请求会把已批准的
// PR 永远卡在 review（#47 实测场景：requested_reviewers 同时残留双方）。
func TestManagerApprovalAbsorbsBotRequestAfterBotResponded(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	reviewLabel := labelByName(t, labels, reviewLabelName)
	awaitingReviewerLabel := labelByName(t, labels, awaitingReviewerLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 37}}
	pr := mergeablePullRequest(37, []Label{reviewLabel, awaitingReviewerLabel})
	pr.RequestedReviewers = []string{"gitea-actions", "ai"}
	api.current[pullRequestKey(repository, 37)] = pr
	api.reviews[pullRequestKey(repository, 37)] = []Review{
		{ID: 1, State: ReviewStateRequestChanges, Submitted: time.Unix(10, 0), User: "gitea-actions"},
		{ID: 2, State: ReviewStateApproved, Submitted: time.Unix(20, 0), User: "ai"},
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 37, Label: labelByName(t, labels, approvedLabelName).ID},
		{Item: 37, Label: labelByName(t, labels, awaitingMergeLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	wantRemoved := []labelChange{
		{Item: 37, Label: reviewLabel.ID},
		{Item: 37, Label: awaitingReviewerLabel.ID},
	}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
}

// reviewer 的 COMMENT review 代表提出了需要作者回应的讨论：标记
// status/changes-requested（等待作者回复），而不是看似开发中的 in-progress；
// 不自动提交驳回 review。
func TestManagerMarksCommentReviewChangesRequested(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	inProgressLabel := labelByName(t, labels, inProgressLabelName)
	awaitingLabel := labelByName(t, labels, awaitingAuthorLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 23}}
	api.current[pullRequestKey(repository, 23)] = mergeablePullRequest(23, []Label{inProgressLabel, awaitingLabel})
	api.reviews[pullRequestKey(repository, 23)] = []Review{{
		ID: 1, State: ReviewStateComment, Submitted: time.Unix(10, 0),
	}}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	wantAdded := []labelChange{{Item: 23, Label: labelByName(t, labels, changesRequestedLabelName).ID}}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	wantRemoved := []labelChange{{Item: 23, Label: inProgressLabel.ID}}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
}

// reviewer 本人的 COMMENT 意味着等待作者回应，其名下残留的待处理评审请求
// 视为已回应：标记 changes-requested。否则「已有意图」会短路作者的回复与
// @ai 提及，PR 死锁在 review。
func TestManagerCommentReviewWinsOverStalePendingRequest(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	reviewLabel := labelByName(t, labels, reviewLabelName)
	awaitingReviewerLabel := labelByName(t, labels, awaitingReviewerLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 29}}
	pr := mergeablePullRequest(29, []Label{reviewLabel, awaitingReviewerLabel})
	pr.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 29)] = pr
	api.reviews[pullRequestKey(repository, 29)] = []Review{{
		ID: 1, State: ReviewStateComment, Submitted: time.Unix(10, 0), User: "ai",
	}}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 29, Label: labelByName(t, labels, changesRequestedLabelName).ID},
		{Item: 29, Label: labelByName(t, labels, awaitingAuthorLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	wantRemoved := []labelChange{
		{Item: 29, Label: reviewLabel.ID},
		{Item: 29, Label: awaitingReviewerLabel.ID},
	}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
}

// reviewer 本人驳回后，其名下残留的待处理评审请求视为已回应：PR 保持
// changes-requested（球在作者），不会被残留请求拉回 review。作者修复后
// 通过 @ai/@reviewer 提及重新表达评审意图。
func TestManagerKeepsRejectionFromRequestedReviewer(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	changesLabel := labelByName(t, labels, changesRequestedLabelName)
	awaitingLabel := labelByName(t, labels, awaitingAuthorLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 31}}
	pr := mergeablePullRequest(31, []Label{changesLabel, awaitingLabel})
	pr.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 31)] = pr
	api.reviews[pullRequestKey(repository, 31)] = []Review{{
		ID: 1, State: ReviewStateRequestChanges, Submitted: time.Unix(10, 0), User: "ai",
	}}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	if len(api.addedLabels) != 0 || len(api.removedLabels) != 0 {
		t.Errorf("added = %+v, removed = %+v, want 标签不变", api.addedLabels, api.removedLabels)
	}
}

// 多个 requested reviewer 时，未回应者的请求仍然是有效评审意图：
// 最新评审者回应过不代表其他人也回应了。
func TestManagerHonorsRequestFromOtherReviewer(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	changesLabel := labelByName(t, labels, changesRequestedLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 33}}
	pr := mergeablePullRequest(33, []Label{changesLabel})
	pr.RequestedReviewers = []string{"ai", "bob"}
	api.current[pullRequestKey(repository, 33)] = pr
	api.reviews[pullRequestKey(repository, 33)] = []Review{{
		ID: 1, State: ReviewStateComment, Submitted: time.Unix(10, 0), User: "ai",
	}}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 33, Label: labelByName(t, labels, reviewLabelName).ID},
		{Item: 33, Label: labelByName(t, labels, awaitingReviewerLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	wantRemoved := []labelChange{{Item: 33, Label: changesLabel.ID}}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
}

// REQUEST_REVIEW 记录晚于最新内容结论时是一次明确的复审请求（原生按钮的
// 新鲜信号——requested_reviewers 字段对重复请求是 no-op）：无内容结论的纯
// 记录进评审队列，批准之后的新记录把 PR 拉回评审。
func TestManagerTreatsFreshRequestRecordAsIntent(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	reviewLabel := labelByName(t, labels, reviewLabelName)
	awaitingReviewerLabel := labelByName(t, labels, awaitingReviewerLabelName)
	approvedLabel := labelByName(t, labels, approvedLabelName)
	awaitingMergeLabel := labelByName(t, labels, awaitingMergeLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 35}, {Index: 36}}
	// #35：只有一条请求记录，无内容结论——按钮请求从未被回应
	api.current[pullRequestKey(repository, 35)] = mergeablePullRequest(35, nil)
	api.reviews[pullRequestKey(repository, 35)] = []Review{{
		ID: 1, State: ReviewStateRequestReview, Submitted: time.Unix(10, 0), User: "ai",
	}}
	// #36：批准之后出现更晚的请求记录——reviewer 已回应，按钮是唯一的复审信号
	approved := mergeablePullRequest(36, []Label{approvedLabel, awaitingMergeLabel})
	approved.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 36)] = approved
	api.reviews[pullRequestKey(repository, 36)] = []Review{
		{ID: 2, State: ReviewStateApproved, Submitted: time.Unix(10, 0), User: "ai"},
		{ID: 3, State: ReviewStateRequestReview, Submitted: time.Unix(20, 0), User: "ai"},
	}

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	wantAdded := []labelChange{
		{Item: 35, Label: reviewLabel.ID},
		{Item: 35, Label: awaitingReviewerLabel.ID},
		{Item: 36, Label: reviewLabel.ID},
		{Item: 36, Label: awaitingReviewerLabel.ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	wantRemoved := []labelChange{
		{Item: 36, Label: approvedLabel.ID},
		{Item: 36, Label: awaitingMergeLabel.ID},
	}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
}

// 评论意图（/review、@提及）会被登记为官方评审请求；可用自定义评审者名。
func TestManagerRegistersCommentIntentAsReviewRequest(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	reviewLabel := labelByName(t, labels, reviewLabelName)
	awaitingReviewerLabel := labelByName(t, labels, awaitingReviewerLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 40}, {Index: 41}}
	api.current[pullRequestKey(repository, 40)] = mergeablePullRequest(40, nil)
	api.comments[pullRequestKey(repository, 40)] = []Comment{{Body: "/review"}}
	api.current[pullRequestKey(repository, 41)] = mergeablePullRequest(41, nil)
	api.comments[pullRequestKey(repository, 41)] = []Comment{{Body: "麻烦 @ai 看一下"}}

	manager := NewManager(api)
	if err := manager.Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if err := manager.ReconcileReviewRequests(t.Context()); err != nil {
		t.Fatalf("ReconcileReviewRequests() error = %v", err)
	}
	wantRequests := []reviewRequestChange{
		{PullRequest: 40, Reviewer: "ai"},
		{PullRequest: 41, Reviewer: "ai"},
	}
	if !slices.Equal(api.createdRequests, wantRequests) {
		t.Errorf("created requests = %+v, want %+v", api.createdRequests, wantRequests)
	}
	wantAdded := []labelChange{
		{Item: 40, Label: reviewLabel.ID},
		{Item: 40, Label: awaitingReviewerLabel.ID},
		{Item: 41, Label: reviewLabel.ID},
		{Item: 41, Label: awaitingReviewerLabel.ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
}

func TestManagerRegistersRequestForConfiguredReviewer(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	api := newFakeAPI(repository, completeLabels())
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 42}}
	api.current[pullRequestKey(repository, 42)] = mergeablePullRequest(42, nil)
	api.comments[pullRequestKey(repository, 42)] = []Comment{{Body: "/review"}}

	manager := NewManager(api, WithContentReviewer("bot"))
	if err := manager.ReconcileReviewRequests(t.Context()); err != nil {
		t.Fatalf("ReconcileReviewRequests() error = %v", err)
	}
	if len(api.createdRequests) != 1 || api.createdRequests[0].Reviewer != "bot" {
		t.Errorf("created requests = %+v, want bot", api.createdRequests)
	}
}

// 身份边界：sync（Actions 内置令牌 gitea-actions）不得触碰官方评审请求——
// Gitea 只允许 PR 作者或仓库管理员选择 reviewer（gitea-actions 会被拒），
// 请求维护由 merge 身份的 ReconcileReviewRequests 承担。
func TestManagerSyncLeavesReviewRequestsToMergeIdentity(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	api := newFakeAPI(repository, completeLabels())
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 45}}
	pr := mergeablePullRequest(45, nil)
	pr.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 45)] = pr
	api.comments[pullRequestKey(repository, 45)] = []Comment{{Body: "/review"}}
	api.reviews[pullRequestKey(repository, 45)] = []Review{
		{ID: 1, State: ReviewStateApproved, Submitted: time.Unix(20, 0), User: "ai"},
	}

	manager := NewManager(api)
	if err := manager.Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(api.createdRequests) != 0 || len(api.deletedRequests) != 0 {
		t.Fatalf("sync 不应触碰评审请求：created=%+v deleted=%+v", api.createdRequests, api.deletedRequests)
	}
	if err := manager.ReconcileReviewRequests(t.Context()); err != nil {
		t.Fatalf("ReconcileReviewRequests() error = %v", err)
	}
	wantWithdrawn := []reviewRequestChange{{PullRequest: 45, Reviewer: "ai"}}
	if !slices.Equal(api.deletedRequests, wantWithdrawn) {
		t.Errorf("deleted requests = %+v, want %+v", api.deletedRequests, wantWithdrawn)
	}
}

// 早于最新内容结论的请求记录是历史残留：不覆盖已有的评审结论——批准仍是
// 批准、驳回仍是驳回；同时撤回遗留请求（Gitea 不消费请求记录，不撤回会让
// official review request 门禁永久阻塞合并）。
func TestManagerIgnoresStaleRequestRecord(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	approvedLabel := labelByName(t, labels, approvedLabelName)
	awaitingMergeLabel := labelByName(t, labels, awaitingMergeLabelName)
	changesLabel := labelByName(t, labels, changesRequestedLabelName)
	awaitingAuthorLabel := labelByName(t, labels, awaitingAuthorLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 37}, {Index: 38}}
	// #37：请求记录在批准之前，最新结论是 APPROVED
	pr37 := mergeablePullRequest(37, []Label{approvedLabel, awaitingMergeLabel})
	pr37.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 37)] = pr37
	api.reviews[pullRequestKey(repository, 37)] = []Review{
		{ID: 1, State: ReviewStateRequestReview, Submitted: time.Unix(10, 0), User: "ai"},
		{ID: 2, State: ReviewStateApproved, Submitted: time.Unix(20, 0), User: "ai"},
	}
	// #38：请求记录在驳回之前，最新结论是 REQUEST_CHANGES
	pr38 := mergeablePullRequest(38, []Label{changesLabel, awaitingAuthorLabel})
	pr38.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 38)] = pr38
	api.reviews[pullRequestKey(repository, 38)] = []Review{
		{ID: 3, State: ReviewStateRequestReview, Submitted: time.Unix(10, 0), User: "ai"},
		{ID: 4, State: ReviewStateRequestChanges, Submitted: time.Unix(20, 0), User: "ai"},
	}

	manager := NewManager(api)
	if err := manager.Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if err := manager.ReconcileReviewRequests(t.Context()); err != nil {
		t.Fatalf("ReconcileReviewRequests() error = %v", err)
	}
	if len(api.createdReviews) != 0 {
		t.Errorf("created reviews = %+v, want none", api.createdReviews)
	}
	if len(api.addedLabels) != 0 {
		t.Errorf("added labels = %+v, want none", api.addedLabels)
	}
	if len(api.removedLabels) != 0 {
		t.Errorf("removed labels = %+v, want none", api.removedLabels)
	}
	wantWithdrawn := []reviewRequestChange{
		{PullRequest: 37, Reviewer: "ai"},
		{PullRequest: 38, Reviewer: "ai"},
	}
	if !slices.Equal(api.deletedRequests, wantWithdrawn) {
		t.Errorf("deleted requests = %+v, want %+v", api.deletedRequests, wantWithdrawn)
	}
	if len(api.createdRequests) != 0 {
		t.Errorf("created requests = %+v, want none", api.createdRequests)
	}
}

// 按钮复审记录晚于最新内容结论时不撤回请求：那是明确的新一轮复审意图。
func TestManagerKeepsFreshRequestRecordWithoutWithdrawal(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	api := newFakeAPI(repository, completeLabels())
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 43}}
	pr := mergeablePullRequest(43, nil)
	pr.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 43)] = pr
	api.reviews[pullRequestKey(repository, 43)] = []Review{
		{ID: 1, State: ReviewStateApproved, Submitted: time.Unix(10, 0), User: "ai"},
		{ID: 2, State: ReviewStateRequestReview, Submitted: time.Unix(20, 0), User: "ai"},
	}

	manager := NewManager(api)
	if err := manager.ReconcileReviewRequests(t.Context()); err != nil {
		t.Fatalf("ReconcileReviewRequests() error = %v", err)
	}
	if len(api.deletedRequests) != 0 {
		t.Errorf("deleted requests = %+v, want none（复审请求保留）", api.deletedRequests)
	}
	if len(api.createdRequests) != 0 {
		t.Errorf("created requests = %+v, want none（已有请求）", api.createdRequests)
	}
}

// 状态评审者的会签（merge 账号的 APPROVED）属于状态通道，不参与内容判定：
// 只有状态会签而没有内容批准的 PR 仍是开发中，绝不会被误标为可合并。
func TestManagerStateApprovalDoesNotDriveContentLabels(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 39}}
	api.current[pullRequestKey(repository, 39)] = mergeablePullRequest(39, nil)
	api.reviews[pullRequestKey(repository, 39)] = []Review{
		{ID: 1, State: ReviewStateApproved, Submitted: time.Unix(10, 0), User: "merge", CommitID: "head", Official: true},
	}

	manager := NewManager(api, WithStateReviewer("merge"))
	if err := manager.Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wantAdded := []labelChange{
		{Item: 39, Label: labelByName(t, labels, inProgressLabelName).ID},
		{Item: 39, Label: labelByName(t, labels, awaitingAuthorLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	if slices.ContainsFunc(api.addedLabels, func(change labelChange) bool {
		return change.Label == labelByName(t, labels, approvedLabelName).ID
	}) {
		t.Errorf("状态会签被误当内容批准: %v", api.addedLabels)
	}
}

// 状态评审者的门禁驳回属于状态通道：内容批准不受其遮蔽，门禁恢复后 PR 直接
// 回到 approved，不需要内容评审者重新批准。
func TestManagerStateRejectionDoesNotShadowContentApproval(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := completeLabels()
	approvedLabel := labelByName(t, labels, approvedLabelName)
	awaitingMergeLabel := labelByName(t, labels, awaitingMergeLabelName)
	changesLabel := labelByName(t, labels, changesRequestedLabelName)
	awaitingAuthorLabel := labelByName(t, labels, awaitingAuthorLabelName)
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 40}}
	pr := mergeablePullRequest(40, []Label{changesLabel, awaitingAuthorLabel})
	pr.RequestedReviewers = []string{"ai"}
	api.current[pullRequestKey(repository, 40)] = pr
	api.reviews[pullRequestKey(repository, 40)] = []Review{
		{ID: 1, State: ReviewStateApproved, Submitted: time.Unix(10, 0), User: "ai", Official: true},
		{ID: 2, State: ReviewStateRequestChanges, Submitted: time.Unix(20, 0), User: "merge", CommitID: "head", Official: true},
	}

	manager := NewManager(api, WithStateReviewer("merge"))
	if err := manager.Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wantAdded := []labelChange{
		{Item: 40, Label: approvedLabel.ID},
		{Item: 40, Label: awaitingMergeLabel.ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
	wantRemoved := []labelChange{
		{Item: 40, Label: changesLabel.ID},
		{Item: 40, Label: awaitingAuthorLabel.ID},
	}
	if !slices.Equal(api.removedLabels, wantRemoved) {
		t.Errorf("removed labels = %+v, want %+v", api.removedLabels, wantRemoved)
	}
}

func TestManagerContinuesAfterRepositoryError(t *testing.T) {
	broken := Repository{Owner: "acme", Name: "broken"}
	healthy := Repository{Owner: "acme", Name: "healthy"}
	api := newFakeAPI(healthy, completeLabels())
	api.repositories = []Repository{broken, healthy}
	api.labels[broken.FullName()] = completeLabels()
	api.pullListError[broken.FullName()] = errors.New("unavailable")
	api.pullRequests[healthy.FullName()] = []PullRequest{{Index: 3}}
	api.current[pullRequestKey(healthy, 3)] = mergeablePullRequest(3, nil)

	err := NewManager(api).Sync(t.Context())
	if err == nil || !strings.Contains(err.Error(), broken.FullName()) {
		t.Fatalf("Sync() error = %v", err)
	}
	wantAdded := []labelChange{
		{Item: 3, Label: labelByName(t, api.labels[healthy.FullName()], inProgressLabelName).ID},
		{Item: 3, Label: labelByName(t, api.labels[healthy.FullName()], awaitingAuthorLabelName).ID},
	}
	if !slices.Equal(api.addedLabels, wantAdded) {
		t.Errorf("added labels = %+v, want %+v", api.addedLabels, wantAdded)
	}
}

func TestManagerReportsProgress(t *testing.T) {
	api := newFakeAPI(Repository{Owner: "acme", Name: "video"}, nil)
	api.repositories = []Repository{}
	var messages []string
	err := NewManager(api, WithProgress(func(message string, _ ...any) {
		messages = append(messages, message)
	})).Sync(t.Context())
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if len(messages) == 0 || !strings.Contains(messages[0], "可访问仓库") {
		t.Errorf("progress messages = %v", messages)
	}
}

func newFakeAPI(repository Repository, labels []Label) *fakeAPI {
	return &fakeAPI{
		repositories:   []Repository{repository},
		triageIssues:   map[string][]Issue{},
		reviewPulls:    map[string][]Issue{},
		openIssues:     map[string][]Issue{},
		pullRequests:   map[string][]PullRequest{},
		current:        map[string]PullRequest{},
		reviews:        map[string][]Review{},
		comments:       map[string][]Comment{},
		labels:         map[string][]Label{repository.FullName(): labels},
		protections:    map[string][]BranchProtection{},
		statuses:       map[string][]CheckStatus{},
		armedPulls:     map[int64]bool{},
		issueListError: map[string]error{},
		pullListError:  map[string]error{},
		protectError:   map[string]error{},
		nextLabelID:    100,
	}
}

func completeLabels() []Label {
	labels := make([]Label, 0, len(labelDefinitions))
	for index, definition := range labelDefinitions {
		labels = append(labels, Label{
			ID:        int64(index + 1),
			Name:      definition.Name,
			Exclusive: definition.Exclusive,
		})
	}
	return labels
}

func labelByName(t *testing.T, labels []Label, name string) Label {
	t.Helper()
	index := slices.IndexFunc(labels, func(label Label) bool {
		return label.Name == name
	})
	if index < 0 {
		t.Fatalf("label %q not found", name)
	}
	return labels[index]
}

func mergeablePullRequest(index int64, labels []Label) PullRequest {
	return PullRequest{
		Index:     index,
		Title:     "Ready",
		HTMLURL:   "https://gitea.example.com/acme/video/pulls/1",
		Open:      true,
		Mergeable: true,
		BaseRef:   "main",
		BaseSHA:   "base",
		HeadSHA:   "head",
		MergeBase: "base",
		Labels:    labels,
	}
}

func stalePullRequest(index int64, labels []Label) PullRequest {
	pullRequest := mergeablePullRequest(index, labels)
	pullRequest.MergeBase = "old-base"
	return pullRequest
}

func unmergeablePullRequest(index int64, labels []Label) PullRequest {
	pullRequest := mergeablePullRequest(index, labels)
	pullRequest.Mergeable = false
	return pullRequest
}

// draftPullRequest 模拟 WIP PR：Gitea 对 draft 恒报 Mergeable=false
// （合并被阻断，与冲突无关）。
func draftPullRequest(index int64, labels []Label) PullRequest {
	pullRequest := unmergeablePullRequest(index, labels)
	pullRequest.Draft = true
	return pullRequest
}

func pullRequestKey(repository Repository, index int64) string {
	return repository.FullName() + "#" + strconv.FormatInt(index, 10)
}

// 不在规范体系内的标签会被删除：assistant 强制维护完整标签集，避免历史/手改
// 标签让状态机分叉（doctor 与 sync 同口径）。
func TestManagerDeletesUnexpectedLabels(t *testing.T) {
	repository := Repository{Owner: "acme", Name: "video"}
	labels := append(completeLabels(), Label{ID: 99, Name: "stale/topic"})
	api := newFakeAPI(repository, labels)
	api.pullRequests[repository.FullName()] = []PullRequest{{Index: 50}}
	api.current[pullRequestKey(repository, 50)] = mergeablePullRequest(50, nil)

	if err := NewManager(api).Sync(t.Context()); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wantDeleted := []labelChange{{Label: 99}}
	if !slices.Equal(api.deletedLabels, wantDeleted) {
		t.Errorf("deleted labels = %+v, want %+v", api.deletedLabels, wantDeleted)
	}
}
