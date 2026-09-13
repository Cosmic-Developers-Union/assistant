package status

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// 服务端体检状态（与本地 repoinstall 的状态词表一致；两者不互相依赖）。
const (
	AuditStatusOK       = "ok"
	AuditStatusMissing  = "missing"
	AuditStatusOutdated = "outdated"
	AuditStatusSkipped  = "skipped"
)

// AuditFinding 是一处服务端配置检查结果；Path 是配置项名称。
type AuditFinding struct {
	Path   string
	Status string
	Detail string
}

// OK 表示该处无需处理（skipped 表示因权限不足跳过，不算问题）。
func (f AuditFinding) OK() bool {
	return f.Status == AuditStatusOK || f.Status == AuditStatusSkipped
}

// AuditOptions 描述仓库服务端应满足的统一配置（与 setup 写入的策略同口径）。
type AuditOptions struct {
	// Branch 是默认分支（缺省 main）。
	Branch string
	// Reviewer / Merger 是约定身份（缺省 ai / merge）。
	Reviewer string
	Merger   string
	// RequiredApprovals 是合并所需批准数（缺省 0 表示不检查）。
	RequiredApprovals int64
	// AllowAdminOverride 为真时不要求 block_admin_merge_override。
	AllowAdminOverride bool
	// SkipSecrets 为真时不检查 Actions secret（无管理员权限时）。
	SkipSecrets bool
}

// AuditRepository 对照 setup 的统一策略检查仓库服务端配置：分支保护、标签
// 体系、协作者权限与 merge 令牌 secret。只读。
func (c *Client) AuditRepository(ctx context.Context, repository Repository, options AuditOptions) ([]AuditFinding, error) {
	if options.Branch == "" {
		options.Branch = "main"
	}
	if options.Reviewer == "" {
		options.Reviewer = "ai"
	}
	if options.Merger == "" {
		options.Merger = "merge"
	}
	var findings []AuditFinding
	add := func(path, status, detail string) {
		findings = append(findings, AuditFinding{Path: path, Status: status, Detail: detail})
	}

	// 分支保护：统一策略（读取需要仓库管理员；权限不足时跳过其余检查）
	protections, err := c.ListBranchProtections(ctx, repository)
	if IsPermissionError(err) {
		add("branch protection", AuditStatusSkipped, "读取被拒（需要仓库管理员）")
		return findings, nil
	}
	if err != nil {
		return nil, err
	}
	var protection *BranchProtection
	for index := range protections {
		if protections[index].RuleName == options.Branch {
			protection = &protections[index]
			break
		}
	}
	if protection == nil {
		add("branch protection "+options.Branch, AuditStatusMissing, "未配置分支保护")
	} else {
		if options.RequiredApprovals > 0 && protection.RequiredApprovals != options.RequiredApprovals {
			add("branch protection approvals", AuditStatusOutdated,
				fmt.Sprintf("required approvals=%d, want %d", protection.RequiredApprovals, options.RequiredApprovals))
		}
		if !protection.EnableMergeWhitelist || !slices.Equal(protection.MergeWhitelistUsernames, []string{options.Merger}) {
			add("branch protection merge whitelist", AuditStatusOutdated,
				fmt.Sprintf("合并白名单=%v, want 仅 %s", protection.MergeWhitelistUsernames, options.Merger))
		}
		if !protection.BlockOnRejectedReviews {
			add("branch protection rejected reviews", AuditStatusOutdated, "应阻塞被驳回的评审")
		}
		if !protection.BlockOnOfficialReviewRequests {
			add("branch protection official requests", AuditStatusOutdated, "应阻塞未回应的官方评审请求")
		}
		if !protection.DismissStaleApprovals {
			add("branch protection dismiss stale", AuditStatusOutdated, "推送应作废旧批准")
		}
		if !protection.BlockOnOutdatedBranch {
			add("branch protection outdated branch", AuditStatusOutdated, "应阻塞落后分支")
		}
		if !options.AllowAdminOverride && !protection.BlockAdminMergeOverride {
			add("branch protection admin override", AuditStatusOutdated, "管理员须遵守分支保护规则")
		}
	}

	// 标签体系：与 sync 同口径（名称齐全、scoped 互斥）
	labels, err := c.ListRepositoryLabels(ctx, repository)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]Label, len(labels))
	for _, label := range labels {
		byName[label.Name] = label
	}
	for _, definition := range LabelDefinitions() {
		label, ok := byName[definition.Name]
		if !ok {
			add("label "+definition.Name, AuditStatusMissing, "标签缺失")
			continue
		}
		if definition.Exclusive && !label.Exclusive {
			add("label "+definition.Name, AuditStatusOutdated, "scoped 标签应设为互斥")
		}
	}

	// 协作者权限：reviewer 写、merger 管理员
	for _, want := range []struct{ name, permission string }{
		{options.Reviewer, "write"},
		{options.Merger, "admin"},
	} {
		if want.name == "" {
			continue
		}
		permission, err := c.GetCollaboratorPermission(ctx, repository, want.name)
		if err != nil {
			return nil, err
		}
		switch {
		case permission == "":
			add("collaborator "+want.name, AuditStatusMissing, "不是仓库协作者")
		case permission != want.permission:
			add("collaborator "+want.name, AuditStatusOutdated,
				fmt.Sprintf("权限 %s, want %s", permission, want.permission))
		}
	}

	// merge 令牌 secret：值只写不可读，只能核对存在性
	if options.SkipSecrets {
		add("actions secret MERGE_TOKEN", AuditStatusSkipped, "跳过（未提供管理员令牌）")
		return findings, nil
	}
	names, err := c.ListRepoSecretNames(ctx, repository)
	switch {
	case IsPermissionError(err):
		add("actions secret MERGE_TOKEN", AuditStatusSkipped, "读取被拒（需要仓库管理员）")
	case err != nil:
		return nil, err
	case !slices.Contains(names, "MERGE_TOKEN"):
		add("actions secret MERGE_TOKEN", AuditStatusMissing, "未写入 merge 令牌（assistant actions）")
	}
	return findings, nil
}

// GetCollaboratorPermission 返回用户在该仓库的权限（admin/write/read；空表示
// 不是协作者）。
func (c *Client) GetCollaboratorPermission(ctx context.Context, repository Repository, user string) (string, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/collaborators/%s/permission",
		repository.Owner, repository.Name, user)
	var payload struct {
		Permission string `json:"permission"`
	}
	status, err := c.getJSON(ctx, path, &payload)
	if err != nil {
		if status == http.StatusNotFound {
			return "", nil
		}
		return "", fmt.Errorf("get collaborator %s permission: %w", user, err)
	}
	return payload.Permission, nil
}

// ListRepoSecretNames 列出仓库级 Actions secret 名（值只写不可读）。
func (c *Client) ListRepoSecretNames(ctx context.Context, repository Repository) ([]string, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/actions/secrets", repository.Owner, repository.Name)
	var payload []struct {
		Name string `json:"name"`
	}
	if _, err := c.getJSON(ctx, path, &payload); err != nil {
		return nil, fmt.Errorf("list repository secrets: %w", err)
	}
	names := make([]string, 0, len(payload))
	for _, item := range payload {
		names = append(names, item.Name)
	}
	return names, nil
}

// getJSON 以基础令牌发 GET 并解析 JSON；返回状态码辅助调用方区分 403/404。
// 403 会包装为 PermissionError。
func (c *Client) getJSON(ctx context.Context, path string, out any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.host+path, nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "token "+c.accessToken)
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return response.StatusCode, err
	}
	if response.StatusCode == http.StatusForbidden {
		return response.StatusCode, &PermissionError{Operation: "GET " + path}
	}
	if response.StatusCode/100 != 2 {
		return response.StatusCode, fmt.Errorf("GET %s: HTTP %d: %s", path, response.StatusCode, strings.TrimSpace(string(body)))
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return response.StatusCode, fmt.Errorf("解析 %s: %w", path, err)
		}
	}
	return response.StatusCode, nil
}
