package setup

import (
	"context"
	"fmt"

	"assistant/internal/instances"
)

// 仓库级 Actions 配置名：仓库内 workflow（assistant.yml 的 sync/automerge
// job）消费。注意 Gitea 禁止 secret 名以 `GITEA_`/`GITHUB_` 开头（保留前缀），
// 因此这里用无前缀名，workflow 里再映射到 `GITEA_*` 环境变量。身份是约定
// （内容评审 ai、状态评审/合并 merge），不需要任何 variable。
const (
	// ActionsSecretStateToken 是 merge 账号的仓库专属令牌（会签/合并）。
	ActionsSecretStateToken = "STATE_TOKEN"
	// ActionsSecretBranchProtectionToken 是分支保护读取令牌（需要 repo admin）。
	ActionsSecretBranchProtectionToken = "BRANCH_PROTECTION_TOKEN"
)

// ConfigureActions 为 instance 的每个仓库幂等写入 Actions secrets：
//
//   - STATE_TOKEN（secret）= 该仓库专属的 merger 令牌（唯一必需的配置）；
//   - BRANCH_PROTECTION_TOKEN（secret）= 静态 admin_token（仅在实例存有
//     静态令牌时写；OAuth access token 短期有效，不适合放进仓库 secret）。
//
// sync job 使用 Actions 内置令牌，评审者/合并者身份是约定 ai / merge，因此
// 无需 variable。名称不含 `GITEA_` 前缀（Gitea 保留前缀），workflow 里映射到
// `GITEA_STATE_TOKEN` / `GITEA_BRANCH_PROTECTION_TOKEN` 环境变量。
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
		if repo.MergerToken == "" && !dryRun {
			return fmt.Errorf("%s: 缺少仓库专属 merger 令牌（先运行 setup）", repo.Name)
		}
		logf("%s: 写 Actions secret %s（该项目专属 merge 令牌）", repo.Name, ActionsSecretStateToken)
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
		if err := admin.SetRepoSecret(ctx, repo.Name, ActionsSecretStateToken, repo.MergerToken); err != nil {
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
