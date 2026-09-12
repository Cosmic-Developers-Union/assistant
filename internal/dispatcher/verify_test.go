package dispatcher

import (
	"context"
	"strings"
	"testing"
	"time"

	"assistant/internal/status"
)

var since = time.Date(2026, 9, 6, 5, 0, 0, 0, time.UTC)

func freshReviews() []status.Review {
	return []status.Review{{
		User:      "ai",
		Submitted: time.Date(2026, 9, 6, 5, 10, 0, 0, time.UTC),
	}}
}

func TestVerifyPullReviewCompletedOnFreshReview(t *testing.T) {
	api := &fakeAPI{
		pull:    status.PullRequest{HeadSHA: "sha-1"},
		reviews: freshReviews(),
	}
	verdict, err := VerifyPullReview(context.Background(), api, testRepo, "ai", 58, since, "sha-1")
	if err != nil {
		t.Fatalf("VerifyPullReview() error = %v", err)
	}
	if !verdict.Completed || verdict.HeadMoved {
		t.Errorf("verdict = %+v, want completed", verdict)
	}
}

func TestVerifyPullReviewHeadDriftInvalidatesRound(t *testing.T) {
	api := &fakeAPI{
		pull:    status.PullRequest{HeadSHA: "sha-2"},
		reviews: freshReviews(),
	}
	verdict, err := VerifyPullReview(context.Background(), api, testRepo, "ai", 58, since, "sha-1")
	if err != nil {
		t.Fatalf("VerifyPullReview() error = %v", err)
	}
	if verdict.Completed || !verdict.HeadMoved {
		t.Errorf("verdict = %+v, want head moved", verdict)
	}
	if !strings.Contains(verdict.Reason, "sha-1 → sha-2") {
		t.Errorf("reason = %q, want sha transition", verdict.Reason)
	}
}

func TestVerifyPullReviewIgnoresStaleReviews(t *testing.T) {
	api := &fakeAPI{
		pull: status.PullRequest{HeadSHA: "sha-1"},
		reviews: []status.Review{{
			User:      "ai",
			Submitted: since.Add(-time.Second),
		}},
	}
	verdict, err := VerifyPullReview(context.Background(), api, testRepo, "ai", 58, since, "sha-1")
	if err != nil {
		t.Fatalf("VerifyPullReview() error = %v", err)
	}
	if verdict.Completed || verdict.HeadMoved {
		t.Errorf("verdict = %+v, want not completed", verdict)
	}
}

func TestVerifyPullReviewRejectsMissingHead(t *testing.T) {
	api := &fakeAPI{pull: status.PullRequest{}}
	if _, err := VerifyPullReview(context.Background(), api, testRepo, "ai", 58, since, "sha-1"); err == nil ||
		!strings.Contains(err.Error(), "head.sha") {
		t.Errorf("error = %v, want head.sha", err)
	}
}

func TestVerifyIssueTriage(t *testing.T) {
	done := &fakeAPI{labels: []status.Label{{Name: "status/confirmed"}}}
	verdict, err := VerifyIssueTriage(context.Background(), done, testRepo, 62)
	if err != nil {
		t.Fatalf("VerifyIssueTriage() error = %v", err)
	}
	if !verdict.Completed {
		t.Errorf("verdict = %+v, want completed", verdict)
	}

	pending := &fakeAPI{labels: []status.Label{{Name: triageLabel}, {Name: "type/bug"}}}
	verdict, err = VerifyIssueTriage(context.Background(), pending, testRepo, 62)
	if err != nil {
		t.Fatalf("VerifyIssueTriage() error = %v", err)
	}
	if verdict.Completed {
		t.Errorf("verdict = %+v, want pending", verdict)
	}
}
