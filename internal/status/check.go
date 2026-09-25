package status

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

type IssueSummary struct {
	Repository Repository
	Index      int64
	Title      string
	HTMLURL    string
}

type PullRequestSummary struct {
	Repository Repository
	Index      int64
	Title      string
	HTMLURL    string
}

type Report struct {
	NeedsTriage []IssueSummary
	NeedsReview []PullRequestSummary
}

func (r Report) HasWork() bool {
	return len(r.NeedsTriage) > 0 || len(r.NeedsReview) > 0
}

// Check 只读检索待处理项：带 status/triage 的 open Issue 和带 status/review 的
// open PR。标签由 action label-sync（CI 事件驱动）维护，check 不做任何写操作，供调用方
// （如 Claude loop）立即获取待办列表。
func (m *Manager) Check(ctx context.Context) (Report, error) {
	repositories, err := m.visibleRepositories(ctx)
	if err != nil {
		return Report{}, err
	}
	m.logf("发现 %d 个可访问仓库", len(repositories))

	var report Report
	var checkErrors []error
	for _, repository := range repositories {
		m.logf("检索仓库 %s", repository.FullName())
		if err := m.checkRepository(ctx, repository, &report); err != nil {
			checkErrors = append(checkErrors, fmt.Errorf("%s: %w", repository.FullName(), err))
		}
	}

	slices.SortFunc(report.NeedsTriage, func(a, b IssueSummary) int {
		if result := strings.Compare(a.Repository.FullName(), b.Repository.FullName()); result != 0 {
			return result
		}
		return cmp.Compare(a.Index, b.Index)
	})
	slices.SortFunc(report.NeedsReview, func(a, b PullRequestSummary) int {
		if result := strings.Compare(a.Repository.FullName(), b.Repository.FullName()); result != 0 {
			return result
		}
		return cmp.Compare(a.Index, b.Index)
	})
	return report, errors.Join(checkErrors...)
}

func (m *Manager) checkRepository(ctx context.Context, repository Repository, report *Report) error {
	var checkErrors []error

	issues, err := m.api.ListTriageIssues(ctx, repository)
	if err != nil {
		checkErrors = append(checkErrors, fmt.Errorf("list triage issues: %w", err))
	} else {
		m.logf("%s: 发现 %d 个待 triage Issue", repository.FullName(), len(issues))
		for _, issue := range issues {
			report.NeedsTriage = append(report.NeedsTriage, IssueSummary{
				Repository: repository,
				Index:      issue.Index,
				Title:      issue.Title,
				HTMLURL:    issue.HTMLURL,
			})
		}
	}

	pullRequests, err := m.api.ListReviewPullRequests(ctx, repository)
	if err != nil {
		checkErrors = append(checkErrors, fmt.Errorf("list review pull requests: %w", err))
	} else {
		m.logf("%s: 发现 %d 个待 review PR", repository.FullName(), len(pullRequests))
		for _, pullRequest := range pullRequests {
			report.NeedsReview = append(report.NeedsReview, PullRequestSummary{
				Repository: repository,
				Index:      pullRequest.Index,
				Title:      pullRequest.Title,
				HTMLURL:    pullRequest.HTMLURL,
			})
		}
	}
	return errors.Join(checkErrors...)
}

func WriteReport(writer io.Writer, report Report) error {
	if !report.HasWork() {
		_, err := fmt.Fprintln(writer, "没有 Issue 或 PR 需要处理")
		return err
	}

	if len(report.NeedsTriage) > 0 {
		if _, err := fmt.Fprintln(writer, "需要 triage 的 Issue:"); err != nil {
			return err
		}
		for _, issue := range report.NeedsTriage {
			if _, err := fmt.Fprintf(
				writer,
				"- %s#%d %s %s\n",
				issue.Repository.FullName(),
				issue.Index,
				issue.Title,
				issue.HTMLURL,
			); err != nil {
				return err
			}
		}
	}
	if len(report.NeedsReview) > 0 {
		if _, err := fmt.Fprintln(writer, "需要 review 的 PR:"); err != nil {
			return err
		}
		for _, pullRequest := range report.NeedsReview {
			if _, err := fmt.Fprintf(
				writer,
				"- %s#%d %s %s\n",
				pullRequest.Repository.FullName(),
				pullRequest.Index,
				pullRequest.Title,
				pullRequest.HTMLURL,
			); err != nil {
				return err
			}
		}
	}
	return nil
}
