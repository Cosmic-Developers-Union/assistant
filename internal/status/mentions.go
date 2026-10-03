package status

import (
	"context"
	"errors"
	"fmt"

	gitea "gitea.dev/sdk"
	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
)

// ListMentionedPullRequests 用全站 mention 搜索发现召唤，包括尚未成为协作者的公开仓库。
func (c *Client) ListMentionedPullRequests(ctx context.Context, identity string) ([]PullRequest, error) {
	var result []PullRequest
	var failures []error
	for page := 1; ; {
		issues, response, err := c.sdk.Issues.ListIssues(ctx, gitea.ListIssueOption{ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize}, State: gitea.StateOpen, Type: gitea.IssueTypePull, MentionedBy: identity})
		if err != nil {
			return result, errors.Join(append(failures, fmt.Errorf("搜索 @%s 的 PR 召唤: %w", identity, err))...)
		}
		for _, issue := range issues {
			if issue.PullRequest == nil {
				continue
			}
			if issue.Repository == nil {
				failures = append(failures, fmt.Errorf("召唤搜索结果缺少仓库信息"))
				continue
			}
			owner, name, err := credentials.ParseRepoName(issue.Repository.FullName)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			repo := Repository{Owner: owner, Name: name}
			pr, err := c.GetPullRequest(ctx, repo, issue.Index)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			pr.Repository = repo
			result = append(result, pr)
		}
		next, ok := nextPage(response, page, len(issues))
		if !ok {
			break
		}
		page = next
	}
	return result, errors.Join(failures...)
}
