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
	_, err = c.UpdateProtection(ctx, ProtectionOptions{Owner: opt.Owner, Name: opt.Name, Branch: repo.DefaultBranch, Merger: opt.Merger, Checks: opt.Checks})
	return err
}
