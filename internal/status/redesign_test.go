package status

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestApprovalMustBeLatestAndExplicitlyAnchorCurrentHead(t *testing.T) {
	approved := Review{ID: 1, User: "ai", State: ReviewStateApproved, CommitID: "head", Submitted: time.Unix(1, 0)}
	if !reviewerApprovedOnHead([]Review{approved}, "ai", "head") {
		t.Fatal("当前批准未被识别")
	}
	for _, change := range []func(*Review){func(r *Review) { r.CommitID = "" }, func(r *Review) { r.CommitID = "old" }, func(r *Review) { r.Stale = true }, func(r *Review) { r.Dismissed = true }, func(r *Review) { r.User = "human" }} {
		review := approved
		change(&review)
		if reviewerApprovedOnHead([]Review{review}, "ai", "head") {
			t.Fatal("无效批准被接受", review)
		}
	}
	for _, state := range []ReviewState{ReviewStateComment, ReviewStateRequestChanges} {
		newer := Review{ID: 2, User: "ai", State: state, CommitID: "head", Submitted: time.Unix(2, 0)}
		if reviewerApprovedOnHead([]Review{newer, approved}, "ai", "head") {
			t.Fatal("新结论没有使旧批准失效")
		}
	}
	if reviewerApprovedOnHead([]Review{approved}, "ai", "") {
		t.Fatal("未知 head 被批准")
	}
	manager := NewManager(nil, WithStateReviewer("merge"))
	if _, found := manager.latestContentReview([]Review{{User: "human", State: ReviewStateApproved}, {User: "merge", State: ReviewStateApproved}}); found {
		t.Fatal("其他人的批准被当作内容评审结论")
	}
}
func TestDryRunInterceptsAllWrites(t *testing.T) {
	repo := Repository{Owner: "acme", Name: "repo"}
	api := newFakeAPI(repo, completeLabels())
	var log strings.Builder
	dry := NewDryRunAPI(api, func(format string, args ...any) { log.WriteString(format) })
	ctx := t.Context()
	calls := []func() error{
		func() error { return dry.CloseIssue(ctx, repo, 1) }, func() error { return dry.MergePullRequest(ctx, repo, 1) },
		func() error { _, err := dry.ArmAutoMerge(ctx, repo, 1, "head"); return err }, func() error { _, err := dry.DisarmAutoMerge(ctx, repo, 1); return err },
		func() error { return dry.CreateIssueComment(ctx, repo, 1, "body") }, func() error { return dry.SetLabelExclusive(ctx, repo, 1) },
		func() error { _, err := dry.CreateLabel(ctx, repo, LabelDefinition{Name: "type/bug"}); return err }, func() error { return dry.DeleteLabel(ctx, repo, 1) },
		func() error { return dry.AddLabel(ctx, repo, 1, 1) }, func() error { return dry.RemoveLabel(ctx, repo, 1, 1) },
		func() error { return dry.CreatePullReview(ctx, repo, 1, ReviewInput{State: ReviewStateApproved}) },
		func() error { return dry.CreateReviewRequests(ctx, repo, 1, []string{"ai"}) }, func() error { return dry.DeleteReviewRequests(ctx, repo, 1, []string{"ai"}) },
	}
	for _, call := range calls {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.ops) != 0 || len(api.createdLabels) != 0 || len(api.addedLabels) != 0 || len(api.removedLabels) != 0 || len(api.mergedPulls) != 0 {
		t.Fatal("演练调用了平台写操作")
	}
	if log.Len() == 0 {
		t.Fatal("演练没有输出")
	}
	if err := NewManager(NewDryRunAPI(api, nil), WithRepository(repo)).Sync(ctx); err != nil {
		t.Fatal(err)
	}
}

type failedCandidate struct {
	API
	index int64
}

func (f failedCandidate) GetPullRequest(ctx context.Context, repo Repository, index int64) (PullRequest, error) {
	if index == f.index {
		return PullRequest{}, errors.New("读取候选失败")
	}
	return f.API.GetPullRequest(ctx, repo, index)
}
func TestAutomergeContinuesAfterReadFailure(t *testing.T) {
	api, repo := newAutoMergeAPI(t)
	first := mergeablePullRequest(1, nil)
	second := mergeablePullRequest(2, nil)
	api.pullRequests[repo.FullName()] = []PullRequest{first, second}
	api.current[pullRequestKey(repo, 2)] = second
	withReviewerApproval(api, repo, 2, "head")
	err := NewManager(failedCandidate{API: api, index: 1}, WithRepository(repo)).AutoMerge(t.Context())
	if err == nil || len(api.mergedPulls) != 1 || api.mergedPulls[0] != 2 {
		t.Fatal("单个读取失败阻断了队列", err, api.mergedPulls)
	}
}

func TestCountersignSupersedesEarlierApprovalAndMergeErrorsKeepCause(t *testing.T) {
	api, repo := newAutoMergeAPI(t)
	pull := mergeablePullRequest(1, nil)
	api.reviews[pullRequestKey(repo, 1)] = []Review{
		{ID: 1, User: "merge", State: ReviewStateApproved, CommitID: "head", Submitted: time.Unix(1, 0)},
		{ID: 2, User: "merge", State: ReviewStateRequestChanges, CommitID: "head", Submitted: time.Unix(2, 0)},
	}
	manager := NewManager(api, WithStateReviewer("merge"))
	if err := manager.countersignState(t.Context(), repo, pull, false); err != nil {
		t.Fatal(err)
	}
	if len(api.createdReviews) != 1 {
		t.Fatal("后来的门禁驳回未被新会签覆盖", api.createdReviews)
	}
	cause := errors.New("合并响应丢失")
	if !errors.Is(&mergeAttemptError{cause}, cause) {
		t.Fatal("合并未知结局丢失错误链")
	}
}
