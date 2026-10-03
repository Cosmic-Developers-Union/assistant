# 部署与仓库流程

安装一个 assistant 二进制，同时安装 Git 和外部 claude CLI；服务入口为
`assistant run --config /path/config.yaml`。构建安装：`make install`；
宿主 systemd 可用 `install.sh`，容器可用 `make compose-up`。
配置与数据落点见 [config.md](config.md)。

在 Gitea 中预先准备内容评审账号 ai、会签/合并账号 merge，并授予必要仓库权限。
服务 connects.token 用 ai 的仓库/Issue 读写令牌，label-sync 用 Actions 内置令牌，
automerge 用能合并并指定 reviewer 的令牌。账号名称是惯例；令牌权限须由平台校验。

管理员应配置分支保护：禁止直接推送，required approvals=2，批准随新提交过期，
阻止落后分支与未回应评审请求，配置必要检查；合并白名单只含 merge，管理员也
受保护。机器人不会在内容评审中改标签；标签由 label-sync 收敛，合并由 automerge
根据当前 head 实时判定。规范标签与分诊协议见 [goal.md](../goal.md)。

Actions 示例：

```yaml
steps:
  - run: assistant action label-sync --verbose
    env:
      GITEA_HOST: ${{ github.server_url }}
      GITEA_ACCESS_TOKEN: ${{ secrets.GITHUB_TOKEN }}
      GITEA_REPOSITORY: ${{ github.repository }}
```

automerge 同样注入三项环境，用 merge secret 替换令牌，按事件及 schedule 调用。
先执行 `--dry-run` 验证门禁与标签动作。任何 merge 请求结果未知时本轮停止，避免
网络失败已经合并后又尝试另一条；读取候选失败则聚合错误并继续其余候选。

服务退出时取消会话进程、限时固化已有记忆并清理 worktree，用户工作文件保留。
状态 API 是可选只读观测面，`GET /state` 需要运行根 api.json 中的 Bearer token。
`assistant mcp sessions` 从 observations 查询 id、事件、结局、耗时与开销。
