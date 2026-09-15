package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	gitea "gitea.dev/sdk"
)

const (
	pageSize          = 50
	httpClientTimeout = 30 * time.Second
)

type ReviewState string

const (
	ReviewStatePending        ReviewState = "PENDING"
	ReviewStateRequestChanges ReviewState = "REQUEST_CHANGES"
	ReviewStateRequestReview  ReviewState = "REQUEST_REVIEW"
	ReviewStateApproved       ReviewState = "APPROVED"
	ReviewStateComment        ReviewState = "COMMENT"
)

type Repository struct {
	Owner string
	Name  string
}

func (r Repository) FullName() string {
	return r.Owner + "/" + r.Name
}

type Label struct {
	ID        int64
	Name      string
	Exclusive bool
}

type Issue struct {
	Index   int64
	Title   string
	HTMLURL string
	// IsPull 表示条目实际是 PR（issues API 的 pull_request 字段非空）。按
	// type=pulls 检索时 Gitea 偶发混入纯 Issue，调用方据此剔除。
	IsPull bool
	Labels []Label
}

type PullRequest struct {
	Index     int64
	Title     string
	HTMLURL   string
	Open      bool
	Mergeable bool
	// Draft 为 Gitea 原生草稿态；标题 WIP 前缀不算草稿，由调用方按仓库约定另行识别。
	// Gitea 对 draft 恒报 Mergeable=false（合并被阻断，与冲突无关），
	// 冲突/落后门禁与标签推导必须先排除 draft。
	Draft     bool
	BaseRef   string
	BaseSHA   string
	HeadSHA   string
	MergeBase string
	// RequestedReviewers 是待处理评审请求的用户名（requested_reviewers）。
	// 实测 reviewer 提交 review 后 Gitea 不消费它，且重复请求同一 reviewer 是
	// no-op，所以「请求是否存在」不等于评审意图——要看该 reviewer 是否已提交过
	// 正式回应（见 hasUnansweredReviewRequest）。
	RequestedReviewers []string
	// RequestedReviewersTeams 为真表示存在团队评审请求。assistant 无法判定团队
	// 成员是否已回应，团队请求始终视为有效评审意图。
	RequestedReviewersTeams bool
	Labels                  []Label
}

type Review struct {
	ID        int64
	State     ReviewState
	Dismissed bool
	Stale     bool
	// Official 表示该 review 是否计入分支保护的批准数（虚拟用户如 gitea-actions
	// 的 review 恒为 false，不被 required approvals 承认）。
	Official bool
	// CommitID 是 review 所针对的 head 提交；状态会签按它去重。
	CommitID  string
	Submitted time.Time
	// User 是提交该 review 的账号名，用于区分内容/状态评审通道与判断待处理
	// 评审请求是否已被本人回应。
	User string
}

// CheckStatus 是 head commit 上某个检查 context 的最新状态。
type CheckStatus struct {
	Context   string
	State     string
	TargetURL string
}

// BranchProtection 是分支保护规则中与状态检查相关的部分。
type BranchProtection struct {
	RuleName          string
	EnableStatusCheck bool
	Contexts          []string
	// RequiredApprovals 是合并所需批准数（setup/e2e 校验用）。
	RequiredApprovals int64
	// 以下字段用于 setup/e2e 校验统一策略。
	EnableMergeWhitelist          bool
	MergeWhitelistUsernames       []string
	BlockOnRejectedReviews        bool
	BlockOnOfficialReviewRequests bool
	BlockAdminMergeOverride       bool
	DismissStaleApprovals         bool
	BlockOnOutdatedBranch         bool
}

type ReviewInput struct {
	State    ReviewState
	Body     string
	CommitID string
}

// Comment 是 Issue/PR 评论中与评审意图识别相关的部分。
type Comment struct {
	ID      int64
	Body    string
	Created time.Time
}

type API interface {
	ListRepositories(context.Context) ([]Repository, error)
	ListTriageIssues(context.Context, Repository) ([]Issue, error)
	ListReviewPullRequests(context.Context, Repository) ([]Issue, error)
	ListOpenIssues(context.Context, Repository) ([]Issue, error)
	CloseIssue(context.Context, Repository, int64) error
	ListOpenPullRequests(context.Context, Repository) ([]PullRequest, error)
	GetPullRequest(context.Context, Repository, int64) (PullRequest, error)
	MergePullRequest(context.Context, Repository, int64) error
	ListPullReviews(context.Context, Repository, int64) ([]Review, error)
	// ListIssueCommentsSince 返回条目（Issue 或 PR）上 since 之后的评论；since 为零值时返回全部。
	ListIssueCommentsSince(context.Context, Repository, int64, time.Time) ([]Comment, error)
	GetCombinedStatus(context.Context, Repository, string) ([]CheckStatus, error)
	ListBranchProtections(context.Context, Repository) ([]BranchProtection, error)
	ListRepositoryLabels(context.Context, Repository) ([]Label, error)
	SetLabelExclusive(context.Context, Repository, int64) error
	CreateLabel(context.Context, Repository, LabelDefinition) (Label, error)
	// DeleteLabel 删除非规范标签（assistant 强制维护完整标签集）。
	DeleteLabel(context.Context, Repository, int64) error
	AddLabel(context.Context, Repository, int64, int64) error
	RemoveLabel(context.Context, Repository, int64, int64) error
	CreatePullReview(context.Context, Repository, int64, ReviewInput) error
	// CreateReviewRequests / DeleteReviewRequests 维护 PR 的官方评审请求
	// （Gitea 的 requested_reviewers 记录，分支保护的 official review request
	// 门禁按它判定）。
	CreateReviewRequests(context.Context, Repository, int64, []string) error
	DeleteReviewRequests(context.Context, Repository, int64, []string) error
	// AuthenticatedUser 返回当前令牌的账号名，供会签方解析自己的身份。
	AuthenticatedUser(context.Context) (string, error)
}

func (c *Client) ListTriageIssues(ctx context.Context, repository Repository) ([]Issue, error) {
	return c.listIssues(ctx, repository, gitea.IssueTypeIssue, []string{triageLabelName})
}

func (c *Client) ListReviewPullRequests(ctx context.Context, repository Repository) ([]Issue, error) {
	return c.listIssues(ctx, repository, gitea.IssueTypePull, []string{reviewLabelName})
}

func (c *Client) ListOpenIssues(ctx context.Context, repository Repository) ([]Issue, error) {
	return c.listIssues(ctx, repository, gitea.IssueTypeIssue, nil)
}

func (c *Client) listIssues(
	ctx context.Context,
	repository Repository,
	issueType gitea.IssueType,
	labels []string,
) ([]Issue, error) {
	var result []Issue
	for page := 1; ; {
		issues, response, err := c.sdk.Issues.ListRepoIssues(ctx, repository.Owner, repository.Name, gitea.ListIssueOption{
			ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize},
			State:       gitea.StateOpen,
			Type:        issueType,
			Labels:      labels,
		})
		if err != nil {
			return nil, fmt.Errorf("list open issues page %d (type=%s): %w", page, issueType, err)
		}
		for _, issue := range issues {
			result = append(result, issueFromSDK(issue))
		}
		next, ok := nextPage(response, page, len(issues))
		if !ok {
			break
		}
		page = next
	}
	return result, nil
}

func (c *Client) CloseIssue(ctx context.Context, repository Repository, index int64) error {
	_, _, err := c.sdk.Issues.EditIssue(ctx, repository.Owner, repository.Name, index, gitea.EditIssueOption{
		State: new(gitea.StateClosed),
	})
	if err != nil {
		return fmt.Errorf("close issue #%d: %w", index, err)
	}
	return nil
}

// GetIssueLabels 读取单个条目（Issue 或 PR）的当前标签，分页取全量；供完成
// 判定等只读路径使用。
func (c *Client) GetIssueLabels(ctx context.Context, repository Repository, index int64) ([]Label, error) {
	var result []Label
	for page := 1; ; {
		labels, response, err := c.sdk.Issues.GetIssueLabels(
			ctx,
			repository.Owner,
			repository.Name,
			index,
			gitea.ListLabelsOptions{ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize}},
		)
		if err != nil {
			return nil, fmt.Errorf("get labels for #%d page %d: %w", index, page, err)
		}
		for _, label := range labels {
			result = append(result, labelFromSDK(label))
		}
		next, ok := nextPage(response, page, len(labels))
		if !ok {
			break
		}
		page = next
	}
	return result, nil
}

func issueFromSDK(issue *gitea.Issue) Issue {
	result := Issue{
		Index:   issue.Index,
		Title:   issue.Title,
		HTMLURL: issue.HTMLURL,
		IsPull:  issue.PullRequest != nil,
	}
	for _, label := range issue.Labels {
		result.Labels = append(result.Labels, labelFromSDK(label))
	}
	return result
}

type Client struct {
	sdk         *gitea.Client
	host        string
	accessToken string
	httpClient  *http.Client

	// branchProtectionSDK 仅在配置了 GITEA_BRANCH_PROTECTION_TOKEN 时非空：
	// 分支保护端点要求 repo admin（Gitea Actions 内置令牌无法授予该权限），
	// 这一处读取改用独立的管理员令牌，其余调用仍走 accessToken。
	branchProtectionSDK *gitea.Client
	// stateSDK 仅在配置了 GITEA_STATE_TOKEN 时非空：状态评审（门禁驳回）以
	// 状态评审者账号提交才是 official review，能被分支保护的
	// block_on_rejected_reviews / required approvals 承认。
	stateSDK *gitea.Client
}

func NewClient(host, accessToken string) (*Client, error) {
	httpClient := &http.Client{Timeout: httpClientTimeout}
	sdk, err := gitea.NewClient(
		host,
		gitea.SetToken(accessToken),
		gitea.SetUserAgent("assistant/1"),
		gitea.SetHTTPClient(httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("create Gitea client: %w", err)
	}
	return &Client{sdk: sdk, host: strings.TrimRight(host, "/"), accessToken: accessToken, httpClient: httpClient}, nil
}

// UseBranchProtectionToken 为分支保护读取配置独立令牌。token 为空时保持现状
// （继续用基础令牌读取，被拒后按严格模式降级）。
func (c *Client) UseBranchProtectionToken(token string) error {
	if token == "" {
		return nil
	}
	sdk, err := gitea.NewClient(
		c.host,
		gitea.SetToken(token),
		gitea.SetUserAgent("assistant/1"),
		gitea.SetHTTPClient(c.httpClient),
	)
	if err != nil {
		return fmt.Errorf("create branch-protection Gitea client: %w", err)
	}
	c.branchProtectionSDK = sdk
	return nil
}

// UseStateReviewerToken 为状态评审（门禁驳回）配置独立令牌。token 为空时保持
// 现状：状态驳回以基础令牌身份提交（gitea-actions 虚拟用户，official=false，
// 不被分支保护承认，仅起时间线记录与状态机驱动作用）。
func (c *Client) UseStateReviewerToken(token string) error {
	if token == "" {
		return nil
	}
	sdk, err := gitea.NewClient(
		c.host,
		gitea.SetToken(token),
		gitea.SetUserAgent("assistant/1"),
		gitea.SetHTTPClient(c.httpClient),
	)
	if err != nil {
		return fmt.Errorf("create state-reviewer Gitea client: %w", err)
	}
	c.stateSDK = sdk
	return nil
}

// FatalError 表示重试无法好转的配置类错误：访问令牌被 Gitea 拒绝（HTTP 401/403），
// 或 GITEA_HOST 没有指向 Gitea API。调用方应当停止并提示用户修正配置。
type FatalError struct {
	Reason string
}

func (e *FatalError) Error() string {
	return e.Reason
}

// IsFatalError 判断错误是否为配置类致命错误。
func IsFatalError(err error) bool {
	var fatalError *FatalError
	return errors.As(err, &fatalError)
}

// PermissionError 表示令牌缺少该接口所需权限（HTTP 403），例如 Actions 内置令牌
// 无法读取分支保护（需要仓库 owner 或 admin 协作者）。与 FatalError 不同，这不是
// 配置错误：调用方应降级到不需要该权限的路径继续工作。
type PermissionError struct {
	Operation string
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("%s: 令牌权限不足 (HTTP 403)", e.Operation)
}

// IsPermissionError 判断错误是否为权限不足。
func IsPermissionError(err error) bool {
	var permissionError *PermissionError
	return errors.As(err, &permissionError)
}

// VerifyAuthentication 通过访问当前用户接口校验配置。返回 *FatalError 表示认证失败
// 或 Gitea 地址配置错误，其他错误（网络不通、Gitea 暂不可用等）视为瞬时故障。
func (c *Client) VerifyAuthentication(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.host+"/api/v1/user", nil)
	if err != nil {
		return fmt.Errorf("verify authentication: %w", err)
	}
	request.Header.Set("Authorization", "token "+c.accessToken)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("verify authentication: %w", err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 512))
	if readErr != nil {
		body = nil
	}
	switch {
	case response.StatusCode/100 == 2:
		return nil
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return &FatalError{Reason: authFailureReason(response.StatusCode, body)}
	case response.StatusCode == http.StatusNotFound:
		return &FatalError{
			Reason: fmt.Sprintf("GITEA_HOST 未指向 Gitea API (HTTP 404): %s", c.host),
		}
	}
	return fmt.Errorf("verify authentication: unexpected HTTP %d", response.StatusCode)
}

func authFailureReason(statusCode int, body []byte) string {
	var payload struct {
		Message string `json:"message"`
	}
	message := ""
	if err := json.Unmarshal(body, &payload); err == nil {
		message = strings.TrimSpace(payload.Message)
	}
	if message == "" {
		return fmt.Sprintf("Gitea 认证失败 (HTTP %d)", statusCode)
	}
	return fmt.Sprintf("Gitea 认证失败 (HTTP %d): %s", statusCode, message)
}

func (c *Client) ListRepositories(ctx context.Context) ([]Repository, error) {
	var result []Repository
	for page := 1; ; {
		repositories, response, err := c.sdk.Repositories.ListMyRepos(ctx, gitea.ListReposOptions{
			ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize},
		})
		if err != nil {
			return nil, fmt.Errorf("list repositories page %d: %w", page, err)
		}
		for _, repository := range repositories {
			if repository.Owner == nil || repository.Owner.UserName == "" || repository.Name == "" {
				return nil, fmt.Errorf("Gitea returned a repository without an owner or name")
			}
			result = append(result, Repository{Owner: repository.Owner.UserName, Name: repository.Name})
		}
		next, ok := nextPage(response, page, len(repositories))
		if !ok {
			break
		}
		page = next
	}
	return result, nil
}

func (c *Client) ListOpenPullRequests(ctx context.Context, repository Repository) ([]PullRequest, error) {
	var result []PullRequest
	for page := 1; ; {
		pullRequests, response, err := c.sdk.PullRequests.ListRepoPullRequests(
			ctx,
			repository.Owner,
			repository.Name,
			gitea.ListPullRequestsOptions{
				ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize},
				State:       gitea.StateOpen,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("list open pull requests page %d: %w", page, err)
		}
		for _, pullRequest := range pullRequests {
			result = append(result, pullRequestFromSDK(pullRequest))
		}
		next, ok := nextPage(response, page, len(pullRequests))
		if !ok {
			break
		}
		page = next
	}
	return result, nil
}

func (c *Client) GetPullRequest(ctx context.Context, repository Repository, index int64) (PullRequest, error) {
	pullRequest, _, err := c.sdk.PullRequests.GetPullRequest(ctx, repository.Owner, repository.Name, index)
	if err != nil {
		return PullRequest{}, fmt.Errorf("get pull request #%d: %w", index, err)
	}
	return pullRequestFromSDK(pullRequest), nil
}

// MergePullRequest 以 squash 方式合并 PR——main 的历史是每个 PR 一个 squash
// 提交（"标题 (#编号)"）。合并后删除 head 分支：feature 分支随合并完成使命，
// 不留已合并分支。能否合并（新鲜度、必要检查、权限）由调用方门禁与 Gitea
// 分支保护共同把关，这里不做前置判断。
//
// 注意 SDK 的 MergePullRequest 对 405 等拒绝只回 bool=false 而不回 error，
// 必须检查 success 标志，否则门禁拒绝会被当成合并成功。
func (c *Client) MergePullRequest(ctx context.Context, repository Repository, index int64) error {
	success, _, err := c.sdk.PullRequests.MergePullRequest(ctx, repository.Owner, repository.Name, index, gitea.MergePullRequestOption{
		Style:                  gitea.MergeStyleSquash,
		DeleteBranchAfterMerge: new(true),
	})
	if err != nil {
		return fmt.Errorf("merge pull request #%d: %w", index, err)
	}
	if !success {
		return fmt.Errorf("merge pull request #%d: Gitea 拒绝合并（分支保护/权限/状态不满足）", index)
	}
	return nil
}

func (c *Client) ListIssueCommentsSince(
	ctx context.Context,
	repository Repository,
	index int64,
	since time.Time,
) ([]Comment, error) {
	var result []Comment
	for page := 1; ; {
		comments, response, err := c.sdk.Issues.ListIssueComments(
			ctx,
			repository.Owner,
			repository.Name,
			index,
			gitea.ListIssueCommentOptions{
				ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize},
				Since:       since,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("list comments for #%d page %d: %w", index, page, err)
		}
		for _, comment := range comments {
			result = append(result, Comment{ID: comment.ID, Body: comment.Body, Created: comment.Created})
		}
		next, ok := nextPage(response, page, len(comments))
		if !ok {
			break
		}
		page = next
	}
	return result, nil
}

func (c *Client) ListPullReviews(ctx context.Context, repository Repository, index int64) ([]Review, error) {
	var result []Review
	for page := 1; ; {
		reviews, response, err := c.sdk.PullRequests.ListPullReviews(
			ctx,
			repository.Owner,
			repository.Name,
			index,
			gitea.ListPullReviewsOptions{ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize}},
		)
		if err != nil {
			return nil, fmt.Errorf("list reviews for pull request #%d page %d: %w", index, page, err)
		}
		for _, review := range reviews {
			item := Review{
				ID:        review.ID,
				State:     ReviewState(review.State),
				Dismissed: review.Dismissed,
				Stale:     review.Stale,
				Official:  review.Official,
				CommitID:  review.CommitID,
				Submitted: review.Submitted,
			}
			if review.Reviewer != nil {
				item.User = review.Reviewer.UserName
			}
			result = append(result, item)
		}
		next, ok := nextPage(response, page, len(reviews))
		if !ok {
			break
		}
		page = next
	}
	return result, nil
}

func (c *Client) GetCombinedStatus(ctx context.Context, repository Repository, sha string) ([]CheckStatus, error) {
	combined, _, err := c.sdk.Repositories.GetCombinedStatus(ctx, repository.Owner, repository.Name, sha)
	if err != nil {
		return nil, fmt.Errorf("get combined status for %s: %w", sha, err)
	}
	// CombinedStatus.Statuses 已是每个 context 的最新状态
	var result []CheckStatus
	for _, s := range combined.Statuses {
		result = append(result, CheckStatus{
			Context:   s.Context,
			State:     string(s.State),
			TargetURL: s.TargetURL,
		})
	}
	return result, nil
}

func (c *Client) ListBranchProtections(ctx context.Context, repository Repository) ([]BranchProtection, error) {
	sdk := c.sdk
	if c.branchProtectionSDK != nil {
		sdk = c.branchProtectionSDK
	}
	var result []BranchProtection
	for page := 1; ; {
		protections, response, err := sdk.Repositories.ListBranchProtections(
			ctx,
			repository.Owner,
			repository.Name,
			gitea.ListBranchProtectionsOptions{ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize}},
		)
		if err != nil {
			if response != nil && response.StatusCode == http.StatusForbidden {
				return nil, &PermissionError{Operation: "list branch protections"}
			}
			return nil, fmt.Errorf("list branch protections page %d: %w", page, err)
		}
		for _, protection := range protections {
			result = append(result, BranchProtection{
				RuleName:                      protection.RuleName,
				EnableStatusCheck:             protection.EnableStatusCheck,
				Contexts:                      protection.StatusCheckContexts,
				RequiredApprovals:             protection.RequiredApprovals,
				EnableMergeWhitelist:          protection.EnableMergeWhitelist,
				MergeWhitelistUsernames:       protection.MergeWhitelistUsernames,
				BlockOnRejectedReviews:        protection.BlockOnRejectedReviews,
				BlockOnOfficialReviewRequests: protection.BlockOnOfficialReviewRequests,
				BlockAdminMergeOverride:       protection.BlockAdminMergeOverride,
				DismissStaleApprovals:         protection.DismissStaleApprovals,
				BlockOnOutdatedBranch:         protection.BlockOnOutdatedBranch,
			})
		}
		next, ok := nextPage(response, page, len(protections))
		if !ok {
			break
		}
		page = next
	}
	return result, nil
}

func (c *Client) ListRepositoryLabels(ctx context.Context, repository Repository) ([]Label, error) {
	var result []Label
	for page := 1; ; {
		labels, response, err := c.sdk.Repositories.ListRepoLabels(
			ctx,
			repository.Owner,
			repository.Name,
			gitea.ListLabelsOptions{ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize}},
		)
		if err != nil {
			if response != nil && response.StatusCode == http.StatusForbidden {
				return nil, &PermissionError{Operation: "list repository labels"}
			}
			return nil, fmt.Errorf("list repository labels page %d: %w", page, err)
		}
		for _, label := range labels {
			result = append(result, labelFromSDK(label))
		}
		next, ok := nextPage(response, page, len(labels))
		if !ok {
			break
		}
		page = next
	}
	return result, nil
}

// ListCollaboratorLogins 返回仓库协作者的登录名（setup/e2e 校验用）。
func (c *Client) ListCollaboratorLogins(ctx context.Context, repository Repository) ([]string, error) {
	var result []string
	for page := 1; ; {
		collaborators, response, err := c.sdk.Repositories.ListCollaborators(
			ctx,
			repository.Owner,
			repository.Name,
			gitea.ListCollaboratorsOptions{ListOptions: gitea.ListOptions{Page: page, PageSize: pageSize}},
		)
		if err != nil {
			return nil, fmt.Errorf("list collaborators for %s page %d: %w", repository.FullName(), page, err)
		}
		for _, collaborator := range collaborators {
			result = append(result, collaborator.UserName)
		}
		next, ok := nextPage(response, page, len(collaborators))
		if !ok {
			break
		}
		page = next
	}
	return result, nil
}

func (c *Client) SetLabelExclusive(ctx context.Context, repository Repository, labelID int64) error {
	_, _, err := c.sdk.Repositories.EditLabel(ctx, repository.Owner, repository.Name, labelID, gitea.EditLabelOption{
		Exclusive: new(true),
	})
	if err != nil {
		return fmt.Errorf("make label %d exclusive: %w", labelID, err)
	}
	return nil
}

func (c *Client) CreateLabel(
	ctx context.Context,
	repository Repository,
	definition LabelDefinition,
) (Label, error) {
	label, _, err := c.sdk.Repositories.CreateLabel(ctx, repository.Owner, repository.Name, gitea.CreateLabelOption{
		Name:        definition.Name,
		Color:       definition.Color,
		Description: definition.Description,
		Exclusive:   definition.Exclusive,
	})
	if err != nil {
		return Label{}, fmt.Errorf("create label %q: %w", definition.Name, err)
	}
	return labelFromSDK(label), nil
}

// DeleteLabel 删除仓库标签（清理不在规范体系内的标签用）。
func (c *Client) DeleteLabel(ctx context.Context, repository Repository, labelID int64) error {
	if _, err := c.sdk.Repositories.DeleteLabel(ctx, repository.Owner, repository.Name, labelID); err != nil {
		return fmt.Errorf("delete label %d: %w", labelID, err)
	}
	return nil
}

func (c *Client) AddLabel(ctx context.Context, repository Repository, index, labelID int64) error {
	_, _, err := c.sdk.Issues.AddIssueLabels(ctx, repository.Owner, repository.Name, index, gitea.IssueLabelsOption{
		Labels: []int64{labelID},
	})
	if err != nil {
		return fmt.Errorf("add label %d to item #%d: %w", labelID, index, err)
	}
	return nil
}

func (c *Client) RemoveLabel(ctx context.Context, repository Repository, index, labelID int64) error {
	_, err := c.sdk.Issues.DeleteIssueLabel(ctx, repository.Owner, repository.Name, index, labelID)
	if err != nil {
		return fmt.Errorf("remove label %d from item #%d: %w", labelID, index, err)
	}
	return nil
}

func (c *Client) CreatePullReview(ctx context.Context, repository Repository, index int64, input ReviewInput) error {
	sdk := c.sdk
	if c.stateSDK != nil {
		// 状态评审（门禁驳回/会签）以状态评审者身份提交，使其成为 official
		// review，被分支保护的 block_on_rejected_reviews / required approvals
		// 承认。
		sdk = c.stateSDK
	}
	_, _, err := sdk.PullRequests.CreatePullReview(ctx, repository.Owner, repository.Name, index, gitea.CreatePullReviewOptions{
		State:    gitea.ReviewStateType(input.State),
		Body:     input.Body,
		CommitID: input.CommitID,
	})
	if err != nil {
		return fmt.Errorf("create review for pull request #%d: %w", index, err)
	}
	return nil
}

// CreateReviewRequests 为 PR 添加官方评审请求（幂等；已存在时为 no-op）。
func (c *Client) CreateReviewRequests(ctx context.Context, repository Repository, index int64, reviewers []string) error {
	if len(reviewers) == 0 {
		return nil
	}
	if _, err := c.sdk.PullRequests.CreateReviewRequests(ctx, repository.Owner, repository.Name, index, gitea.PullReviewRequestOptions{
		Reviewers: reviewers,
	}); err != nil {
		return fmt.Errorf("create review requests for pull request #%d: %w", index, err)
	}
	return nil
}

// DeleteReviewRequests 撤回 PR 的官方评审请求（已不存在时为 no-op）。
func (c *Client) DeleteReviewRequests(ctx context.Context, repository Repository, index int64, reviewers []string) error {
	if len(reviewers) == 0 {
		return nil
	}
	if _, err := c.sdk.PullRequests.DeleteReviewRequests(ctx, repository.Owner, repository.Name, index, gitea.PullReviewRequestOptions{
		Reviewers: reviewers,
	}); err != nil {
		return fmt.Errorf("delete review requests for pull request #%d: %w", index, err)
	}
	return nil
}

// AuthenticatedUser 返回当前令牌的账号名。automerge 以状态评审者令牌运行，
// 会签前用它解析自己的身份以便按 head 去重。
func (c *Client) AuthenticatedUser(ctx context.Context) (string, error) {
	user, _, err := c.sdk.GetMyUserInfo(ctx)
	if err != nil {
		return "", fmt.Errorf("get authenticated user: %w", err)
	}
	if user == nil || user.UserName == "" {
		return "", fmt.Errorf("authenticated user has no username")
	}
	return user.UserName, nil
}

// AuthenticatedIdentity 返回当前令牌的账号名与实例管理员身份：登录时用它把
// 「以谁的身份、有没有管理员权限」落盘（凭据库的身份事实）。
func (c *Client) AuthenticatedIdentity(ctx context.Context) (string, bool, error) {
	user, _, err := c.sdk.GetMyUserInfo(ctx)
	if err != nil {
		return "", false, fmt.Errorf("get authenticated user: %w", err)
	}
	if user == nil || user.UserName == "" {
		return "", false, fmt.Errorf("authenticated user has no username")
	}
	return user.UserName, user.IsAdmin, nil
}

func pullRequestFromSDK(pullRequest *gitea.PullRequest) PullRequest {
	result := PullRequest{
		Index:                   pullRequest.Index,
		Title:                   pullRequest.Title,
		HTMLURL:                 pullRequest.HTMLURL,
		Open:                    pullRequest.State == gitea.StateOpen,
		Mergeable:               pullRequest.Mergeable,
		Draft:                   pullRequest.Draft,
		MergeBase:               pullRequest.MergeBase,
		RequestedReviewersTeams: len(pullRequest.RequestedReviewersTeams) > 0,
	}
	for _, reviewer := range pullRequest.RequestedReviewers {
		if reviewer != nil && reviewer.UserName != "" {
			result.RequestedReviewers = append(result.RequestedReviewers, reviewer.UserName)
		}
	}
	if pullRequest.Base != nil {
		result.BaseRef = pullRequest.Base.Ref
		result.BaseSHA = pullRequest.Base.Sha
	}
	if pullRequest.Head != nil {
		result.HeadSHA = pullRequest.Head.Sha
	}
	for _, label := range pullRequest.Labels {
		result.Labels = append(result.Labels, labelFromSDK(label))
	}
	return result
}

func labelFromSDK(label *gitea.Label) Label {
	return Label{ID: label.ID, Name: label.Name, Exclusive: label.Exclusive}
}

func nextPage(response *gitea.Response, currentPage, itemCount int) (int, bool) {
	if response != nil {
		if response.NextPage > currentPage {
			return response.NextPage, true
		}
		if response.LastPage > currentPage {
			return currentPage + 1, true
		}
	}
	return currentPage + 1, itemCount == pageSize
}
