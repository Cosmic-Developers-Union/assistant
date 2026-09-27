// API 是 gitea 集成所需的站台只读能力（*status.Client 满足）。
//
// 放在本包而不是调度引擎：它是「Gitea 能读什么」的定义，是集成自己的脸面；
// 调度引擎只把它当依赖注入使用。
package gitea

import (
	"context"

	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// API 是 gitea 集成所需的站台只读能力（*status.Client 满足）。
type API interface {
	// ListPullRequestsRequestingReview 是 review 请求通道：Gitea 原生
	// requested_reviewers 含 reviewer 且未在当前 head 上回应的 open PR。
	ListPullRequestsRequestingReview(context.Context, status.Repository, string) ([]status.PullRequest, error)
	ListTriageIssues(context.Context, status.Repository) ([]status.Issue, error)
	// ListIssuesMentioning 是 mention 通道：Gitea 服务端 mentioned_by 过滤，
	// 返回 mention 了 reviewer 的 open 条目（issueType 为 "issues"/"pulls"）。
	ListIssuesMentioning(ctx context.Context, repository status.Repository, user string, issueType string) ([]status.Issue, error)
	GetPullRequest(context.Context, status.Repository, int64) (status.PullRequest, error)
	ListPullReviews(context.Context, status.Repository, int64) ([]status.Review, error)
	GetIssueLabels(context.Context, status.Repository, int64) ([]status.Label, error)
	AuthenticatedUser(context.Context) (string, error)
}
