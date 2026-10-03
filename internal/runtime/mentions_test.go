package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

type mentionPlatform struct {
	platformFake
	repositories                            []status.Repository
	mentioned                               []status.PullRequest
	reviews                                 []status.Review
	comments                                []status.Comment
	mentionError, reviewError, commentError error
	identity                                string
}

func (p *mentionPlatform) ListRepositories(context.Context) ([]status.Repository, error) {
	return p.repositories, p.err
}
func (p *mentionPlatform) ListMentionedPullRequests(_ context.Context, user string) ([]status.PullRequest, error) {
	p.identity = user
	return p.mentioned, p.mentionError
}
func (p *mentionPlatform) ListPullReviews(context.Context, status.Repository, int64) ([]status.Review, error) {
	return p.reviews, p.reviewError
}
func (p *mentionPlatform) ListIssueCommentsSince(context.Context, status.Repository, int64, time.Time) ([]status.Comment, error) {
	return p.comments, p.commentError
}
func TestSourceFindsMentionsAcrossRepositoriesWithoutWorkflowOrCollaboration(t *testing.T) {
	repo := status.Repository{Owner: "outside", Name: "public"}
	api := &mentionPlatform{mentioned: []status.PullRequest{{Repository: repo, Index: 4, Open: true, HeadSHA: "head"}, {Repository: repo, Index: 5, Open: true, Draft: true}, {Repository: repo, Index: 6, Open: true, Title: "WIP: work"}, {Repository: repo, Index: 7, Open: false}}}
	source := GiteaSource{API: api, Host: "https://gitea", Identity: "custom-bot", ReviewBot: "review"}
	events, err := source.Poll(t.Context())
	if err != nil || len(events) != 1 || events[0].Repo != "outside/public" || events[0].Head != "head" || api.identity != "custom-bot" {
		t.Fatal(events, err, api.identity)
	}
	// 标签与全站搜索发现同一个 PR 时只入队一次。
	api.repositories = []status.Repository{repo}
	api.pulls = []status.PullRequest{{Index: 4, Open: true, HeadSHA: "head", Labels: []status.Label{{Name: status.LabelReview}}}}
	if events, err := source.Poll(t.Context()); err != nil || len(events) != 1 {
		t.Fatal(events, err)
	}
}
func TestSourceUsesNewMentionsAfterReviewAndKeepsErrorsVisible(t *testing.T) {
	now := time.Unix(100, 0)
	api := &mentionPlatform{mentioned: []status.PullRequest{{Repository: status.Repository{Owner: "team", Name: "repo"}, Index: 1, Open: true}}, reviews: []status.Review{{User: "ai", State: status.ReviewStateApproved, Submitted: now}, {User: "other", State: status.ReviewStateApproved, Submitted: now.Add(time.Hour)}}}
	source := GiteaSource{API: api, Identity: "ai", ReviewBot: "review"}
	for _, comment := range []status.Comment{{User: "author", Body: "@ai", Created: now}, {User: "author", Body: "@ai-extra", Created: now.Add(time.Second)}, {User: "ai", Body: "@ai", Created: now.Add(time.Second)}} {
		api.comments = []status.Comment{comment}
		if events, err := source.Poll(t.Context()); err != nil || len(events) != 0 {
			t.Fatal("历史/无关/自身提及触发评审", events, err)
		}
	}
	api.comments = []status.Comment{{User: "author", Body: "请 @AI 重新评审", Created: now.Add(time.Second)}}
	if events, err := source.Poll(t.Context()); err != nil || len(events) != 1 {
		t.Fatal(events, err)
	}
	for _, which := range []string{"mention", "review", "comment", "repository"} {
		cause := errors.New(which)
		api.mentionError = nil
		api.reviewError = nil
		api.commentError = nil
		api.err = nil
		switch which {
		case "mention":
			api.mentionError = cause
		case "review":
			api.reviewError = cause
		case "comment":
			api.commentError = cause
		case "repository":
			api.err = cause
		}
		if _, err := source.Poll(t.Context()); !errors.Is(err, cause) {
			t.Fatal("发现失败被吞掉", err)
		}
	}
}
func TestMentionAccountBoundaries(t *testing.T) {
	for _, text := range []string{"@ai", "请 @AI", "(@ai)", "@ai-extra 然后 @ai"} {
		if !mentionsAccount(text, "ai") {
			t.Fatal(text)
		}
	}
	for _, text := range []string{"email@ai.com", "@aibot", "@ai-extra", "none"} {
		if mentionsAccount(text, "ai") {
			t.Fatal(text)
		}
	}
}
