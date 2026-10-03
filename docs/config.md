# 配置与持久化

服务运行只读取 `--config` 指定的文件，缺省当前目录 `config.yaml`。
不查找用户配置、不读取 `ASSISTANT_CONFIG`、不读取 credentials.json、
不自动加载 `.env`。配置相对路径锚定 YAML 所在目录。

## 运行配置

完整起步示例：[config.example.yaml](../config.example.yaml)。

| 块 | 职责 |
|---|---|
| connects | 平台连接：type、url、token；Gitea 以令牌账号为召唤身份 |
| mcp | 命名 stdio server：cmd、args、env |
| bots | kind、use、with、workspace、agent 的组合 |
| session | local 或 s3 记忆副本 |
| runtime | root、interval、timeout、concurrency、api-listen |

`run` 没有仓库白名单。Gitea 连接扫描可见仓库的标签待办，同时通过全站
`mentioned_by` 搜索发现 @ 当前账号的 PR；公开仓库不必先安装 workflow 或加入协作者。
正式回应后的历史提及不重复触发，最新回应后的新 @ 评论可再次召唤。
CI 的 `action` 始终要求显式指定一个仓库，不做发现。

bot 内置 kind 是 `gitea-review`、`triage`、`chat`。`use` 必须绑定一个匹配平台
的连接。重复消费同一连接的同一种事件会报错；评审与分诊可以共享连接。
`with.identity` 若指定，启动时必须与 Gitea 连接令牌实际账号一致；它不会切换
令牌身份。`with.prompt` 是附加提示。

worktree 的 repo/ref 支持 `{{event.repo}}`、`{{event.pr.ref}}`、
`{{event.pr.head}}`、`{{event.base}}`。PR ref 缺省用平台 refs/pull/N/head，
Issue ref 缺省用受管仓库的默认基线。Git 操作受取消控制，共享元数据受锁保护。

agent 支持 system、mcp（server 名列表）、model、bin（缺省 claude）。
模型凭据来自启动进程的 `ANTHROPIC_*` 等环境变量。Gitea bot 选择的
`args: [mcp, gitea]` server 自动得到该连接的 GITEA_HOST/GITEA_ACCESS_TOKEN，
覆盖同名 server 环境；其他 server 的 env 按声明注入。PR 工作目录仍保留项目
自有规则，MCP 只使用 bot 明确选择的 server。

token/app-secret/S3 access-key/secret-key 与 MCP 密钥环境字段必须是单个
`{{VAR}}` 引用，不能直接填写明文，也不能拼接后缀。其他 MCP env 可用普通字符串
或环境引用。工作区模板与环境引用分别在取用事件时、启动时展开。

运行参数缺省：root=data、interval=30s、timeout=30m、concurrency=8。
api-listen 缺省关闭；配置如 `127.0.0.1:8770` 开启，`off` 显式关闭。
`--verbose` 展示步骤及 agent 进度，`--debug` 当前包含同样的执行信息。

## 消息 bot

```yaml
connects:
  support: {type: telegram, token: "{{TELEGRAM_TOKEN}}", admin-users: ["123456789"]}
bots:
  support:
    kind: chat
    use: {telegram: support}
    workspace: {type: directory}
    agent: {system: "用中文回答", model: ""}
```

QQ 使用 app-id、app-secret；微信使用 token、url、user-id；Telegram 使用 token。
消息平台可用 admin-users 配准入，含 `"*"` 放开所有人；QQ/Telegram 缺省拒绝所有人，
微信可回退为 user-id 指定的扫码用户。多个用户的记忆和文件分别隔离，`/new`
重开记忆、文件保留。个人 `instance add` 不会替服务填写连接凭据。

## 会话

```yaml
session:
  store: s3
  endpoint: https://minio.example.com
  bucket: assistant-sessions
  prefix: sessions
  access-key: "{{AWS_ACCESS_KEY_ID}}"
  secret-key: "{{AWS_SECRET_ACCESS_KEY}}"
```

S3 桶需预先创建。每个稳定会话 id 对应一个压缩对象，内含 Claude projects
子树（含子代理 JSONL），不包含 MCP 令牌配置或 Claude 认证设置。上传覆盖同一个
对象；已有本地记录优先，不会被远端旧副本覆盖。远端不存在时才开新记忆，
请求失败、权限不足、损坏归档均报错。恢复使用临时目录，拒绝越界、符号链接和
过大对象；检查完整后再落入会话目录。

数据根内：repos 为受管克隆，workspaces 为短期评审工作区，chat 为用户文件，
sessions 为 Claude 配置根及本地记忆，observations 为每次执行元数据。
local 方案需备份 sessions；s3 方案备份对象桶。观测元数据不用于判断完成。

## 开发者实例

`assistant instance add gitea --name work --url https://site --username developer`。
缺字段在 TTY 逐项询问；密码不回显，非交互用 `--password-file`。
Gitea 登录后保存 MCP 令牌与使用内置密钥加密的密码。instance token 只读输出已有令牌；
instance create-token 复用密码创建令牌，管理员可通过 --user 为其他账号发令牌。
目标凭据独立保存，详见 [bootstrap.md](bootstrap.md)。
QQ 用 `--app-id`/`--app-secret-file`，Telegram 用 `--token-file`，微信扫码登录。
添加前实测平台身份，成功后保存唯一用户级 credentials.json，0600。

Linux 默认 `~/.config/Cosmic-Developers-Union/assistant/credentials.json`，
其他系统用 os.UserConfigDir，`ASSISTANT_CREDENTIALS` 可覆盖。按实例名全局唯一，
按 gitea/qq/weixin/telegram 分组，未知字段直接报错。
show/list 不输出令牌；remove 只删除本地实例，不撤销平台侧令牌或账号。

MCP host：显式 --host → GITEA_HOST → Git remote 探测 → 凭据库唯一站点。
MCP token：--token → GITEA_ACCESS_TOKEN → GITEA_ACCESS_TOKEN_FILE → 对应站点实例。
多站点或同站点多账号不会猜；使用显式 host/token 或环境变量选择。

## 旧版迁移

1. 先备份旧 config.json、credentials.json 和运行数据。新旧凭据格式不兼容；
   新版发现旧格式会报错，绝不自动覆盖。
2. 另建 config.yaml，用 connects/mcp/bots 替换 providers/channels/runtimes；
   将服务密钥放入进程环境，模型选择放进 agent.model。
3. 开发者需要新凭据路径时设置 ASSISTANT_CREDENTIALS，再用 instance add 重新登录。
   服务本身无需个人登录，评审和合并令牌由部署环境管理。
4. 原 assistantd 服务入口改为 assistant run --config /path/config.yaml。
5. 旧版 Claude JSONL 不会自动导入新会话 id/落点；需要历史记录时保留旧数据副本。
6. 旧 workflow 的 assistant sync/automerge 改成 assistant action label-sync/automerge，
   注入 GITEA_HOST、GITEA_ACCESS_TOKEN、GITEA_REPOSITORY，先用 --dry-run 核对。

运行根的 run.lock 使用操作系统锁；正常退出或进程崩溃后锁自动释放。
锁文件保留，不要通过删除文件启动第二个进程；并行部署须使用不同 root。
--debug 会在 --verbose 信息上增加装配与续接原因，所有已知连接密钥均从日志遮蔽。

MCP 安装默认无需配置或凭据，只写 assistant mcp gitea 启动命令；连接在启动时解析。
同站点多账号通过已有的令牌参数或环境变量选择，MCP 不增加实例参数。
站点接入与项目安装见 [bootstrap.md](bootstrap.md)，不会改写 run 的服务连接。
