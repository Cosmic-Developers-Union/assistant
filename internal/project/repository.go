package project

import (
	"context"
	"fmt"

	gitea "gitea.dev/sdk"
	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// RepositoryOptions 只约束当前项目，不给常驻账号添加仓库白名单。
type RepositoryOptions struct {
	Owner, Name, Reviewer, Merger string
	Checks                        []string
	DryRun                        bool
	Log                           func(string, ...any)
}

// Configure 规范标签、协作者与默认分支保护；保留未指定的已有检查上下文。
func (c *Client) Configure(ctx context.Context, opt RepositoryOptions) error {
	if opt.Owner == "" || opt.Name == "" || !accountName.MatchString(opt.Reviewer) || !accountName.MatchString(opt.Merger) || opt.Reviewer == opt.Merger {
		return fmt.Errorf("项目、内容评审者或合并者无效")
	}
	if _, _, err := credentials.ParseRepoName(opt.Owner + "/" + opt.Name); err != nil {
		return err
	}
	repo, _, err := c.SDK.Repositories.GetRepo(ctx, opt.Owner, opt.Name)
	if err != nil {
		return fmt.Errorf("读取当前项目: %w", err)
	}
	if repo.DefaultBranch == "" {
		return fmt.Errorf("项目没有默认分支，请先推送初始提交")
	}
	log := opt.Log
	if log == nil {
		log = func(string, ...any) {}
	}
	log("规范 %s/%s：标签、%s=write、%s=admin、%s 双批准保护", opt.Owner, opt.Name, opt.Reviewer, opt.Merger, repo.DefaultBranch)
	api, err := status.NewClient(c.Entry.URL, c.Entry.Token)
	if err != nil {
		return err
	}
	var labelAPI status.API = api
	if opt.DryRun {
		labelAPI = status.NewDryRunAPI(api, log)
	}
	if err := status.NewManager(labelAPI).ReconcileLabels(ctx, status.Repository{Owner: opt.Owner, Name: opt.Name}); err != nil {
		return err
	}
	if opt.DryRun {
		return nil
	}
	for _, role := range []struct {
		name string
		mode gitea.AccessMode
	}{{opt.Reviewer, gitea.AccessModeWrite}, {opt.Merger, gitea.AccessModeAdmin}} {
		if _, err := c.SDK.Repositories.AddCollaborator(ctx, opt.Owner, opt.Name, role.name, gitea.AddCollaboratorOption{Permission: new(role.mode)}); err != nil {
			return fmt.Errorf("配置协作者 @%s: %w", role.name, err)
		}
	}
	old, response, err := c.SDK.Repositories.GetBranchProtection(ctx, opt.Owner, opt.Name, repo.DefaultBranch)
	if err != nil && (response == nil || response.StatusCode != 404) {
		return fmt.Errorf("读取分支保护: %w", err)
	}
	checks := opt.Checks
	if checks == nil && old != nil {
		checks = old.StatusCheckContexts
	}
	if response != nil && response.StatusCode == 404 {
		_, _, err = c.SDK.Repositories.CreateBranchProtection(ctx, opt.Owner, opt.Name, gitea.CreateBranchProtectionOption{BranchName: repo.DefaultBranch, RuleName: repo.DefaultBranch, EnablePush: false, RequiredApprovals: 2, EnableMergeWhitelist: true, MergeWhitelistUsernames: []string{opt.Merger}, BlockAdminMergeOverride: true, DismissStaleApprovals: true, BlockOnRejectedReviews: true, BlockOnOfficialReviewRequests: true, BlockOnOutdatedBranch: true, EnableStatusCheck: len(checks) > 0, StatusCheckContexts: checks})
	} else {
		_, _, err = c.SDK.Repositories.EditBranchProtection(ctx, opt.Owner, opt.Name, repo.DefaultBranch, gitea.EditBranchProtectionOption{EnablePush: new(false), RequiredApprovals: new(int64(2)), EnableMergeWhitelist: new(true), MergeWhitelistUsernames: []string{opt.Merger}, BlockAdminMergeOverride: new(true), DismissStaleApprovals: new(true), BlockOnRejectedReviews: new(true), BlockOnOfficialReviewRequests: new(true), BlockOnOutdatedBranch: new(true), EnableStatusCheck: new(len(checks) > 0), StatusCheckContexts: checks})
	}
	if err != nil {
		return fmt.Errorf("配置分支保护: %w", err)
	}
	return nil
}

// InstallActionsSettings 开启项目 Actions 并写入会签账号 secret，令牌不进入 workflow。
func (c *Client) InstallActionsSettings(ctx context.Context, owner, repo, token string, dry bool) error {
	if token == "" {
		return fmt.Errorf("安装 Actions 缺少合并账号令牌")
	}
	if dry {
		return nil
	}
	if _, _, err := c.SDK.Repositories.EditRepo(ctx, owner, repo, gitea.EditRepoOption{HasActions: new(true)}); err != nil {
		return fmt.Errorf("启用项目 Actions: %w", err)
	}
	if _, err := c.SDK.Actions.CreateRepoSecret(ctx, owner, repo, "MERGE_TOKEN", gitea.CreateOrUpdateSecretOption{Data: token}); err != nil {
		return fmt.Errorf("设置 MERGE_TOKEN: %w", err)
	}
	return nil
}
