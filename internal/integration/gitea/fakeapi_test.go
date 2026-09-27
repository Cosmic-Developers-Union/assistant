package gitea

import (
	"context"
	"sync/atomic"

	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// fakeAPI 是 API 的测试桩：各方法返回预置结果并按调用计数。
type fakeAPI struct {
	issues  []status.Issue
	pull    status.PullRequest
	pullErr error
	reviews []status.Review
	// reviewsErr 非空时作为 review 列表失败（完成判定的错误路径）
	reviewsErr error
	labels     []status.Label
	// 非空时优先作为标签错误
	labelsErr error
	login     string
	loginErr  error
	listCalls atomic.Int32
	// mention 通道：mentioned_by 过滤的返回（mentionPulls / mentionIssues）
	mentionPulls  []status.Issue
	mentionIssues []status.Issue
	// mentionUser 记录 mention 通道的账号参数；两路并发调用，用原子量留痕
	mentionUser atomic.Pointer[string]
	// review 请求通道：requested_reviewers 命中且未在当前 head 上回应的 PR
	requestedPulls    []status.PullRequest
	requestedReviewer atomic.Pointer[string]
}

func (f *fakeAPI) ListIssuesMentioning(_ context.Context, _ status.Repository, user, issueType string) ([]status.Issue, error) {
	f.listCalls.Add(1)
	f.mentionUser.Store(&user)
	if issueType == "pulls" {
		return f.mentionPulls, nil
	}
	return f.mentionIssues, nil
}

func (f *fakeAPI) ListPullRequestsRequestingReview(_ context.Context, _ status.Repository, reviewer string) ([]status.PullRequest, error) {
	f.listCalls.Add(1)
	f.requestedReviewer.Store(&reviewer)
	return f.requestedPulls, nil
}

func (f *fakeAPI) ListTriageIssues(context.Context, status.Repository) ([]status.Issue, error) {
	f.listCalls.Add(1)
	return f.issues, nil
}

func (f *fakeAPI) GetPullRequest(context.Context, status.Repository, int64) (status.PullRequest, error) {
	return f.pull, f.pullErr
}

func (f *fakeAPI) ListPullReviews(context.Context, status.Repository, int64) ([]status.Review, error) {
	return f.reviews, f.reviewsErr
}

func (f *fakeAPI) GetIssueLabels(context.Context, status.Repository, int64) ([]status.Label, error) {
	return f.labels, f.labelsErr
}

func (f *fakeAPI) AuthenticatedUser(context.Context) (string, error) {
	return f.login, f.loginErr
}
