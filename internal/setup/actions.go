package setup

import (
	"context"

	"assistant/internal/instances"
)

// 仓库级 Actions 配置名：仓库内 workflow（automerge/sync）消费。注意 Gitea
// 禁止 secret/variable 名以 `GITEA_`/`GITHUB_` 开头（保留前缀），因此这里用
// 无前缀名，workflow 里再映射到 `GITEA_*` 环境变量。
const (
	// ActionsVariableStateReviewer 是状态评审者账号名（Actions variable）。
	ActionsVariableStateReviewer = "STATE_REVIEWER"
	// ActionsSecretStateToken 是状态评审者令牌（会签/门禁驳回的官方身份）。
	ActionsSecretStateToken = "STATE_TOKEN"
	// ActionsSecretBranchProtectionToken 是分支保护读取令牌（需要 repo admin）。
	ActionsSecretBranchProtectionToken = "BRANCH_PROTECTION_TOKEN"
)

// ConfigureActions 为 instance 的每个仓库幂等写入 Actions variables/secrets：
//
//   - STATE_REVIEWER（variable）= merger 账号名；
//   - STATE_TOKEN（secret）= merger 令牌；
//   - BRANCH_PROTECTION_TOKEN（secret）= 静态 admin_token（仅在实例存有
//     静态令牌时写；OAuth access token 短期有效，不适合放进仓库 secret）。
//
// 名称不含 `GITEA_` 前缀（Gitea 保留前缀，禁止用于 secret/variable 名），
// workflow 里映射到 `GITEA_STATE_REVIEWER` 等环境变量。
//
// Gitea 的 secret 值只写不可读，无法比对差异，因此只能每次覆盖写入——同值
// PUT 是幂等的，唯一代价是审计记录。
func ConfigureActions(
	ctx context.Context,
	admin Admin,
	instance instances.Instance,
	dryRun bool,
	logf func(string, ...any),
) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	for _, repo := range instance.Repos {
		if repo.Name == "" {
			continue
		}
		logf("%s: 写 Actions variable %s=%s", repo.Name, ActionsVariableStateReviewer, instance.Merger.Name)
		logf("%s: 写 Actions secret %s（merger 令牌）", repo.Name, ActionsSecretStateToken)
		writeBranchProtection := instance.AdminToken != ""
		if writeBranchProtection {
			logf("%s: 写 Actions secret %s（分支保护读取）", repo.Name, ActionsSecretBranchProtectionToken)
		} else {
			logf("%s: 无静态 admin_token，跳过 %s（workflow 回退严格门禁）",
				repo.Name, ActionsSecretBranchProtectionToken)
		}
		if dryRun {
			continue
		}
		if err := admin.SetRepoVariable(ctx, repo.Name, ActionsVariableStateReviewer, instance.Merger.Name); err != nil {
			return err
		}
		if err := admin.SetRepoSecret(ctx, repo.Name, ActionsSecretStateToken, instance.Merger.Token); err != nil {
			return err
		}
		if writeBranchProtection {
			if err := admin.SetRepoSecret(ctx, repo.Name, ActionsSecretBranchProtectionToken, instance.AdminToken); err != nil {
				return err
			}
		}
	}
	return nil
}
