package project

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	gitea "gitea.dev/sdk"
	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
)

// ProtectionOptions 只操作当前仓库的一条具体分支规则，空分支选择平台默认分支。
// Checks 为 nil 时保留已有检查，空切片表示显式清空。
type ProtectionOptions struct {
	Owner, Name, Branch, Merger string
	Checks                      []string
	DryRun                      bool
}

// Protection 用于显示平台实际规则；Rule 为 nil 表示该分支没有同名保护规则。
type Protection struct {
	Repository string                  `json:"repository"`
	Branch     string                  `json:"branch"`
	Rule       *gitea.BranchProtection `json:"rule"`
}

// GetProtection 先确认仓库存在，再把保护接口的 404 解释为规则缺失。
func (c *Client) GetProtection(ctx context.Context, opt ProtectionOptions) (*Protection, error) {
	if _, _, err := credentials.ParseRepoName(opt.Owner + "/" + opt.Name); err != nil {
		return nil, err
	}
	if opt.Branch != "" && !validProtectionBranch(opt.Branch) {
		return nil, fmt.Errorf("分支名无效；必须指定具体分支，不能使用通配符")
	}
	repo, _, err := c.SDK.Repositories.GetRepo(ctx, opt.Owner, opt.Name)
	if err != nil {
		return nil, fmt.Errorf("读取当前项目: %w", err)
	}
	branch := opt.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	if !validProtectionBranch(branch) {
		return nil, fmt.Errorf("项目没有有效默认分支，请先推送初始提交或指定 --branch")
	}
	result := &Protection{Repository: opt.Owner + "/" + opt.Name, Branch: branch}
	rule, response, err := c.SDK.Repositories.GetBranchProtection(ctx, opt.Owner, opt.Name, branch)
	if response != nil && response.StatusCode == http.StatusNotFound {
		return result, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取分支保护: %w", err)
	}
	result.Rule = rule
	return result, nil
}

// UpdateProtection 创建或更新双批准规范，复用 Configure 的同一实现，保留未指定的检查项。
func (c *Client) UpdateProtection(ctx context.Context, opt ProtectionOptions) (*Protection, error) {
	if !accountName.MatchString(opt.Merger) {
		return nil, fmt.Errorf("合并账号无效")
	}
	for _, check := range opt.Checks {
		if strings.TrimSpace(check) == "" {
			return nil, fmt.Errorf("必要检查名称不能为空；用 --required-checks= 清空检查")
		}
	}
	state, err := c.GetProtection(ctx, opt)
	if err != nil {
		return nil, err
	}
	if err := exactProtection(state); err != nil {
		return nil, err
	}
	if opt.DryRun {
		return state, nil
	}
	checks := opt.Checks
	if checks == nil && state.Rule != nil {
		checks = state.Rule.StatusCheckContexts
	}
	// 空数组必须写入 SDK PATCH，否则旧检查或旧合并团队白名单无法清除。
	if checks == nil {
		checks = []string{}
	}
	if state.Rule == nil {
		state.Rule, _, err = c.SDK.Repositories.CreateBranchProtection(ctx, opt.Owner, opt.Name, gitea.CreateBranchProtectionOption{BranchName: state.Branch, RuleName: state.Branch, EnablePush: false, RequiredApprovals: 2, EnableMergeWhitelist: true, MergeWhitelistUsernames: []string{opt.Merger}, BlockAdminMergeOverride: true, DismissStaleApprovals: true, BlockOnRejectedReviews: true, BlockOnOfficialReviewRequests: true, BlockOnOutdatedBranch: true, EnableStatusCheck: len(checks) > 0, StatusCheckContexts: checks})
	} else {
		state.Rule, _, err = c.SDK.Repositories.EditBranchProtection(ctx, opt.Owner, opt.Name, state.Branch, gitea.EditBranchProtectionOption{EnablePush: new(false), RequiredApprovals: new(int64(2)), EnableMergeWhitelist: new(true), MergeWhitelistUsernames: []string{opt.Merger}, MergeWhitelistTeams: []string{}, BlockAdminMergeOverride: new(true), DismissStaleApprovals: new(true), BlockOnRejectedReviews: new(true), BlockOnOfficialReviewRequests: new(true), BlockOnOutdatedBranch: new(true), EnableStatusCheck: new(len(checks) > 0), StatusCheckContexts: checks})
	}
	if err != nil {
		return nil, fmt.Errorf("配置分支保护: %w", err)
	}
	return state, nil
}

// RemoveProtection 只移除选定分支的同名规则；缺失或重复移除均成功，不动其他规则。
func (c *Client) RemoveProtection(ctx context.Context, opt ProtectionOptions) (*Protection, error) {
	state, err := c.GetProtection(ctx, opt)
	if err != nil {
		return nil, err
	}
	if err := exactProtection(state); err != nil {
		return nil, err
	}
	if state.Rule == nil || opt.DryRun {
		return state, nil
	}
	response, err := c.SDK.Repositories.DeleteBranchProtection(ctx, opt.Owner, opt.Name, state.Branch)
	if err != nil && (response == nil || response.StatusCode != http.StatusNotFound) {
		return nil, fmt.Errorf("移除分支保护: %w", err)
	}
	state.Rule = nil
	return state, nil
}

func exactProtection(state *Protection) error {
	if state.Rule != nil && state.Rule.RuleName != "" && state.Rule.RuleName != state.Branch {
		return fmt.Errorf("平台返回规则 %q，与选定分支 %q 不同，拒绝修改", state.Rule.RuleName, state.Branch)
	}
	return nil
}

func validProtectionBranch(branch string) bool {
	if branch == "" || branch == "@" || branch == "HEAD" || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, " ~^:?*[\\\x00\x7f") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") || strings.HasSuffix(branch, ".") {
		return false
	}
	for _, c := range branch {
		if c < 32 {
			return false
		}
	}
	for part := range strings.SplitSeq(branch, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
