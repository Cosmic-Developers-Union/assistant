# Guide

## Login

use `assistant login add`（查看与轮换令牌：`assistant login token list|show|refresh`）

## For Development

需要你是目标仓库的管理员（或 owner）；`init` 系列是 dev 专用，与站点管理员的
`assistant setup`（建号、令牌、MERGE_TOKEN 密钥分发）严格区分。

```shell
assistant init merge # 将 merge 加入协作者, 并赋予 admin 权限, 要求仓库管理员
assistant init labels # 将标签收敛为规范体系: 补齐缺失, scoped 互斥, 删除体系外
assistant install gitea-actions # 安装 Gitea Actions workflow
```

完成后, 开启分支保护

```shell
assistant init branch-protection # 可以多次进行, 结果幂等
```

merge 始终持有一票批准: 它检查 PR 是否满足合并条件, 通过后自动批准并自动合并。
开发者必须在 PR 中 `@merge` 才会触发批准; 自动合并无需人工干预。

此时, 可以通过 `@merge` 要求请求合并, 如果希望通过 `@ai` 要求 review, 则

```shell
assistant init ai # 邀请 ai 加入协作者, 配置可写权限
```

完成后, 更新分支保护

```shell
assistant init branch-protection # required approvals 从 1 升为 2
```

批准规则: 票数不随协作者数量增长。merge 恒有一票——只有 merge 时 required
approvals 为 1; 加入 ai 后为 2——ai 提交内容批准, merge 检查通过后批准并自动
合并。需要更高门槛时用 `--extra-approvals` 显式附加, 例如:

```shell
assistant init branch-protection --extra-approvals 1 # merge+ai 之外再要求 1 票
```

## Action

workflow（`.gitea/workflows/assistant.yml`）包含两个 job:

- `sync` job 运行 `assistant action label-sync`: 收敛标签, 并把 PR 原生评审状态同步为状态标签（标签已由 `init labels`
  收敛为规范体系, label-sync 只做维持）
- `automerge` job 运行 `assistant action automerge`: 对满足门禁的 PR 执行会签与合并；必要检查运行中且分支保护配置了
  必要检查 context（`--status-check-contexts`）时, 先会签再武装 Gitea 原生
  auto-merge, 检查变绿由服务端即时合并——「检查完成」没有任何 Actions 触发
  事件, 不武装就只能等 schedule 兜底
