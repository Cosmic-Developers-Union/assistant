package setup

import (
	"context"
	"fmt"
	"slices"
)

// 仓库级 Actions 配置名：仓库内 workflow（assistant.yml 的 automerge job）
// 消费。Gitea 禁止 secret 名以 `GITEA_`/`GITHUB_` 开头（保留前缀），因此这里
// 用无前缀名，workflow 里映射到 `GITEA_ACCESS_TOKEN`。
const (
	// ActionsSecretMergeToken 是 merge 账号令牌（(host, merge) 唯一；
	// 评审请求维护/分支保护读取/会签/合并）。同一站点的仓库写同一个值。
	ActionsSecretMergeToken = "MERGE_TOKEN"
)

// SyncMergeSecrets 扫描实例上全部仓库，对 merge 账号是管理员协作者的仓库幂等
// 写入 MERGE_TOKEN secret——Actions 密钥分发完全由 setup（站点管理员）负责，
// init 不写服务端密钥。Gitea 的 secret 值只写不可读，无法比对差异，因此每次
// 覆盖写入（同值 PUT 幂等，唯一代价是审计记录）。dry-run 只输出计划。
func SyncMergeSecrets(
	ctx context.Context,
	admin Admin,
	mergerName, mergeToken string,
	dryRun bool,
	logf func(string, ...any),
) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	repos, err := admin.ListAllRepos(ctx)
	if err != nil {
		return err
	}
	for _, fullName := range repos {
		collaborators, err := admin.ListCollaborators(ctx, fullName)
		if err != nil {
			// 无权读取协作者的仓库（如私有仓库）跳过，不阻塞其他仓库
			logf("%s: 读取协作者失败（%v），跳过", fullName, err)
			continue
		}
		index := slices.IndexFunc(collaborators, func(collaborator Collaborator) bool {
			return collaborator.Name == mergerName
		})
		if index < 0 || collaborators[index].Permission != "admin" {
			continue
		}
		if mergeToken == "" && !dryRun {
			return fmt.Errorf("%s: 缺少 merge 令牌（先运行 setup）", fullName)
		}
		logf("%s: 写 Actions secret %s（merge 账号 %s 是管理员协作者）",
			fullName, ActionsSecretMergeToken, mergerName)
		if dryRun {
			continue
		}
		if err := admin.SetRepoSecret(ctx, fullName, ActionsSecretMergeToken, mergeToken); err != nil {
			return err
		}
	}
	return nil
}
