# assistant 设计规范

单一二进制 `assistant`,不引入第二个常驻程序。操作面:

- `instance` — 平台接入管理 (gitea / qq / weixin / telegram)
- `mcp` — AI 会话的工具面 (stdio)
- `action` — 仓库自动化 (在 CI 里按事件与定时运行)
- `assistant run` — 常驻进程:评审调度 + 消息通道

## 1. instance 子命令

实例 = 一个已接入的平台连接:名字、地址、账号、令牌。

```shell
# 列出已添加的实例
assistant instance list

# 添加 Gitea;参数缺省时进入交互式模式
# 不支持 --password(避免进 shell 历史),非交互场景用 --password-file 注入
assistant instance add gitea --name instance-name --url https://... --username username

# 其他平台遵循各自的登录方式,使用相同的模块
assistant instance add qq ...
assistant instance add weixin ...
assistant instance add telegram ...

# 移除与查看
assistant instance remove instance-name
assistant instance show instance-name
```

- `add` 对缺失的参数逐项交互询问;密码只在登录时使用,不落盘、不回显。
- `remove` 只删本地实例条目,不动平台侧的账号。

### 实例的存放:credentials.json

用户级文件,与当前目录无关。Linux 落
`~/.config/Cosmic-Developers-Union/assistant/credentials.json`
(`ASSISTANT_CREDENTIALS` 可显式指定位置):

```json
{
  "instances": {
    "gitea": [],
    "qq": []
  }
}
```

按类型分键:每种类型有自己的字段集,校验按类型严格进行 (未知字段直接报错);
新增平台只需定义该类型的字段与登录方式,不牵动其他类型。

## 2. mcp 子命令

```shell
assistant mcp gitea
```

自动检测当前项目的 Gitea 实例与当前开发者的访问令牌,再以 stdio 拉起
gitea-mcp (默认 `go run gitea.com/gitea/gitea-mcp@latest`)。

host 检测顺序:

1. `--host`
2. `GITEA_HOST`
3. 当前检出内的 Gitea remote 探测 (多个上游时选命中的)
4. 实例配置中唯一的 gitea 实例
5. 凭据库中唯一的站点

无上游且登记了多个平台时显式报错,不猜。

token 检测顺序:

1. `--token`
2. `GITEA_ACCESS_TOKEN`
3. `GITEA_ACCESS_TOKEN_FILE`
4. `assistant instance add <host> --username <账号>` 登记的该站点凭据

凭据库与当前目录无关——从任何项目启动都解析同一份登录状态。

## 3. action 子命令

```shell
# 合并门禁全绿且分支未过期的已批准 PR (一次运行至多一个,squash;供 CI schedule 驱动)
assistant action automerge

# 规范 Issue 标签,并把 PR 原生评审状态同步为状态标签 (单次执行,供 CI 事件驱动)
assistant action label-sync
```

Flags: `--dry-run`

## 4. assistant run (常驻)

评审调度与消息通道 (微信 / QQ / Telegram / Gitea) 在同一进程承载:

- 检测待办 → 每个待办拉起一个 headless `claude` 会话 → 验证结论 → 清理;
- 会话之间并发,同一会话串行;
- 消息通道并行接入,互不阻塞。
