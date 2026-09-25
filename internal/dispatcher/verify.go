// 完成判定（具体动作 + 简单规则，不做 goal 驱动编排）：
//   - PR：会话起点之后 reviewer 账号提交过新 review，且当前 head 仍是会话
//     开工时钉定的那个。作者在会话期间推送会让本轮评审作废——评审锚定的是
//     旧 head，不能当作对现状的完成；作废即放行，下一轮以新 head 重开。
//   - Issue：status/triage 标签已被移除（分诊结论会换标签或关闭）即视为完成。
package dispatcher

import (
	"context"
	"fmt"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// PullVerdict 是 PR 完成判定结论：completed 之外给出依据；HeadMoved=true 时
// 本轮直接放行，不烧重试会话。
type PullVerdict struct {
	Completed bool
	HeadMoved bool
	Reason    string
}

// VerifyPullReview 完成判定：head 未漂移且 reviewer 在 since 之后有新 review。
func VerifyPullReview(
	ctx context.Context,
	api API,
	repository status.Repository,
	reviewer string,
	pullNumber int64,
	since time.Time,
	expectedHead string,
) (PullVerdict, error) {
	pull, err := api.GetPullRequest(ctx, repository, pullNumber)
	if err != nil {
		return PullVerdict{}, err
	}
	if pull.HeadSHA == "" {
		return PullVerdict{}, fmt.Errorf("Gitea 返回缺 head.sha：pulls/%d", pullNumber)
	}
	if pull.HeadSHA != expectedHead {
		return PullVerdict{
			HeadMoved: true,
			Reason: fmt.Sprintf(
				"head 已推进 %s → %s，本轮评审作废",
				shortSHA(expectedHead),
				shortSHA(pull.HeadSHA),
			),
		}, nil
	}
	reviews, err := api.ListPullReviews(ctx, repository, pullNumber)
	if err != nil {
		return PullVerdict{}, err
	}
	for _, review := range reviews {
		if review.User != reviewer || review.Submitted.IsZero() {
			continue
		}
		if !review.Submitted.Before(since) {
			return PullVerdict{
				Completed: true,
				Reason:    fmt.Sprintf("reviewer「%s」已提交新 review", reviewer),
			}, nil
		}
	}
	return PullVerdict{Reason: "未见 reviewer 新 review"}, nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// IssueVerdict 是 Issue 完成判定结论。
type IssueVerdict struct {
	Completed bool
	Reason    string
}

// VerifyIssueTriage 完成判定：status/triage 是否已从 Issue 上移除。
func VerifyIssueTriage(
	ctx context.Context,
	api API,
	repository status.Repository,
	issueNumber int64,
) (IssueVerdict, error) {
	labels, err := api.GetIssueLabels(ctx, repository, issueNumber)
	if err != nil {
		return IssueVerdict{}, err
	}
	stillThere := false
	for _, label := range labels {
		if label.Name == status.LabelTriage {
			stillThere = true
			break
		}
	}
	if stillThere {
		return IssueVerdict{Reason: status.LabelTriage + " 仍在"}, nil
	}
	return IssueVerdict{Completed: true, Reason: status.LabelTriage + " 已移除"}, nil
}
