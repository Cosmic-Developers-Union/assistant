package setup

import (
	"context"
	"fmt"

	"assistant/internal/instances"
)

// 仓库级 Actions 配置名：仓库内 workflow（assistant.yml 的 sync/automerge
// job）消费。注意 Gitea 禁止 secret 名以 `GITEA_`/`GITHUB_` 开头（保留前缀），
// 因此这里用无前缀名，workflow 里再映射到 `GITEA_*` 环境变量。
const (
	// ActionsSecretStateToken 是 merge 账号的仓库专属令牌（会签/合并/评审请求维护）。
	ActionsSecretStateToken = "STATE_TOKEN"
)

// ConfigureActions 为 instance 的每个仓库幂等写入 Actions secret：
//
//   - STATE_TOKEN（secret）= 该仓库专属的 merge 令牌，唯一需要配置的项。
//
// sync 用 Actions 内置令牌（gitea-actions 权限模型），身份是约定 ai / merge，
// 无需 variable；分支保护读取、会签、合并都以 merge 令牌完成（merge 是仓库
// 管理员协作者）。名称不含 `GITEA_` 前缀（Gitea 保留前缀），workflow 里映射到
// `GITEA_STATE_TOKEN` 环境变量。
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
		if dryRun {
			continue
		}
		if err := admin.SetRepoSecret(ctx, repo.Name, ActionsSecretStateToken, repo.MergerToken); err != nil {
			return err
		}
	}
	return nil
}
