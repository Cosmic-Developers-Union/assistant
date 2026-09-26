package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/status"
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

	pending := &fakeAPI{labels: []status.Label{{Name: status.LabelTriage}, {Name: "type/bug"}}}
	verdict, err = VerifyIssueTriage(context.Background(), pending, testRepo, 62)
	if err != nil {
		t.Fatalf("VerifyIssueTriage() error = %v", err)
	}
	if verdict.Completed {
		t.Errorf("verdict = %+v, want pending", verdict)
	}
}

// VerifyPullReview 的失败路径与边界：API 报错原样上抛（不得当成「未完成」，
// 否则一次网络抖动会伪装成「本轮无新 review」），review 作者与时刻边界严格。
func TestVerifyPullReviewErrorsAndBoundaries(t *testing.T) {
	t.Run("取 PR 失败", func(t *testing.T) {
		api := &fakeAPI{pullErr: errors.New("gitea 500")}
		if _, err := VerifyPullReview(context.Background(), api, testRepo, "ai", 58, since, "sha-1"); err == nil ||
			!strings.Contains(err.Error(), "gitea 500") {
			t.Errorf("error = %v, want 原样上抛", err)
		}
	})

	t.Run("取 review 列表失败", func(t *testing.T) {
		api := &fakeAPI{pull: status.PullRequest{HeadSHA: "sha-1"}, reviewsErr: errors.New("gitea 502")}
		if _, err := VerifyPullReview(context.Background(), api, testRepo, "ai", 58, since, "sha-1"); err == nil ||
			!strings.Contains(err.Error(), "gitea 502") {
			t.Errorf("error = %v, want 原样上抛", err)
		}
	})

	t.Run("他人 review 不计入", func(t *testing.T) {
		api := &fakeAPI{
			pull: status.PullRequest{HeadSHA: "sha-1"},
			reviews: []status.Review{{
				User:      "Ge",
				Submitted: since.Add(time.Hour),
			}},
		}
		verdict, err := VerifyPullReview(context.Background(), api, testRepo, "ai", 58, since, "sha-1")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if verdict.Completed {
			t.Errorf("verdict = %+v, want 未完成（他人 review 不代表 reviewer 已回应）", verdict)
		}
	})

	t.Run("缺少提交时刻的 review 跳过", func(t *testing.T) {
		api := &fakeAPI{
			pull:    status.PullRequest{HeadSHA: "sha-1"},
			reviews: []status.Review{{User: "ai"}},
		}
		verdict, err := VerifyPullReview(context.Background(), api, testRepo, "ai", 58, since, "sha-1")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if verdict.Completed {
			t.Errorf("verdict = %+v, want 未完成（零值时刻不可比，不能当作新 review）", verdict)
		}
	})

	t.Run("review 时刻恰等于起点计入", func(t *testing.T) {
		api := &fakeAPI{
			pull:    status.PullRequest{HeadSHA: "sha-1"},
			reviews: []status.Review{{User: "ai", Submitted: since}},
		}
		verdict, err := VerifyPullReview(context.Background(), api, testRepo, "ai", 58, since, "sha-1")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if !verdict.Completed {
			t.Errorf("verdict = %+v, want 完成（!Before(since) 含等于）", verdict)
		}
	})
}

// head 漂移判定先于 review 检索：漂移即放行，不必再打一次 reviews 接口。
func TestVerifyPullReviewHeadDriftShortCircuits(t *testing.T) {
	api := &fakeAPI{
		pull:       status.PullRequest{HeadSHA: "sha-2"},
		reviewsErr: errors.New("不该被调用"),
	}
	if _, err := VerifyPullReview(context.Background(), api, testRepo, "ai", 58, since, "sha-1"); err != nil {
		t.Errorf("error = %v, want nil（漂移即短路，不应触碰 reviews）", err)
	}
}

func TestVerifyIssueTriageError(t *testing.T) {
	api := &fakeAPI{labelsErr: errors.New("gitea 500")}
	if _, err := VerifyIssueTriage(context.Background(), api, testRepo, 62); err == nil ||
		!strings.Contains(err.Error(), "gitea 500") {
		t.Errorf("error = %v, want 原样上抛", err)
	}
}

func TestShortSHA(t *testing.T) {
	if got := shortSHA("abcdef0123456"); got != "abcdef0" {
		t.Errorf("shortSHA(长 sha) = %q, want abcdef0", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("shortSHA(短 sha) = %q, want 原样", got)
	}
	if got := shortSHA(""); got != "" {
		t.Errorf("shortSHA(空) = %q, want 空", got)
	}
}
