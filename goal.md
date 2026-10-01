## instance 子命令

```shell
# 列出已经添加的实例
assistant instance list
# 添加 Gitea, 如果缺省, 进入交互式模式, 注意, 不支持 --password
# 但是允许通过 --password-file 注入
# 其他平台遵循各自的登陆方式, 使用相同的模块
assistant instance add gitea --name instance-name --url https://... --username username
assistant instance add qq ...
assistant instance add weixin ...
assistant instance add telegram ...
# 通过
assistant instance remove instance-name
# 查看
assistant instance show instance-name

```

instance 的配置文件示例

Linux : `~/.config/Cosmic-Developers-Union/assistant/credentials.json`

```json
{
  "instances": {
    "gitea": [],
    "qq": []
  }
}
```

这里选择这样的结构是为了便于严格校验不同类型的配置类型

## mcp 子命令

```shell
assistant mcp gitea
```

自动检测当前项目的 Gitea 实例与当前开发者的访问令牌，再以 stdio 拉起 gitea-mcp
默认 `go run gitea.com/gitea/gitea-mcp@latest`；
检测顺序：
host：--host > GITEA_HOST > 检出内 Gitea remote 探测（多上游时选命中的）>
config.json 唯一 gitea 通道 > 凭据库唯一站点；无上游且登记多平台时
显式报错不猜；
token：--token > GITEA_ACCESS_TOKEN > GITEA_ACCESS_TOKEN_FILE >
assistant login add <host> --user <账号> 的上站点凭据（凭据库）。
凭据库在平台标准配置目录（ASSISTANT_CREDENTIALS 可显式指定位置），与当前
目录无关——从任何项目启动都解析同一份登录状态。

## action

支持的命令

```shell
# 合并门禁全绿且分支未过期的已批准 PR（一次运行至多一个，squash；供 CI schedule 驱动）
assistant action automerge

# 规范 Issue 标签并把 PR 原生评审状态同步为状态标签（单次执行，供 CI 事件驱动）
assistant action label-sync
```

Flags: --dry-run
