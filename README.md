# assistant

一个 Go 二进制，提供平台实例管理、AI 会话工具、仓库自动化和 bot 常驻运行。
设计规范见 [goal.md](goal.md)，实现分为四个操作面：

```sh
assistant instance add gitea --name work --url https://gitea.example --username developer
assistant instance list
assistant mcp gitea
assistant action label-sync --dry-run
assistant run --config config.yaml
```

`instance` 管开发者登录态，`run` 管一次服务运行。两者互不读取配置：
用户级 `credentials.json` 保存平台实例，当前目录 `config.yaml` 声明
connects、mcp、bots 与 session。未知字段、无效引用和跨平台字段立即报错。

复制 [config.example.yaml](config.example.yaml)，填站点与仓库，设置服务令牌及
Claude 所需的模型环境变量，再运行：

```sh
cp config.example.yaml config.yaml
export GITEA_REVIEW_TOKEN='<ai 账号令牌>'
export ANTHROPIC_API_KEY='<模型密钥>'
assistant run --dry-run       # 单次只读检查待办，不创建工作区或会话
assistant run                # 常驻运行，Ctrl+C 收尾退出
```

令牌必须写成 `"{{VAR}}"` 环境引用；未定义或为空时报错，展开值不写回配置。
`.env` 不会自动加载；容器使用 `env_file`，systemd 使用 `EnvironmentFile`，
前台运行由 shell 注入。配置说明见 [docs/config.md](docs/config.md)。

`run` 消费平台上的 `status/review` PR 与 `status/triage` Issue。
工作区使用受管克隆与独立 worktree；评审钉住事件 head，并以基线 `.claude/` 和 `.assistant/`
替换 PR 自带规则。同一待办同时至多一个会话；每轮重新读平台待办，进程退出
或模型声称完成均不算业务完成。评审协议由二进制内嵌的
[review skill](skills/review/SKILL.md) 注入。

运行核心的三个接口位于 `internal/runtime`：Workspace 管准备与清理目录，
Session 管稳定 id、记忆恢复与固化，AgentRunner 管一次 agent 执行。
Claude 直接在运行根的 sessions 目录写 JSONL；PR head 变化新建记忆，Issue
同标题重试续接。缺省本地存储，可选择 S3，换机后在首次取用前恢复完整记录
及子代理记录。恢复失败停止该待办；执行失败或取消也会尝试固化已有记忆。

消息平台也使用相同流水线：`kind: chat`，连接支持 QQ、微信、Telegram，
工作区使用 `type: directory`。同用户消息串行，不同用户独立；`/new` 重开
记忆并保留用户文件。

仓库自动化只处理环境指定的一个仓库：

```sh
export GITEA_HOST=https://gitea.example.com
export GITEA_ACCESS_TOKEN='<本轮令牌>'
export GITEA_REPOSITORY=acme/repo
assistant action label-sync --verbose
assistant action automerge --verbose
```

label-sync 收敛标签和原生评审状态；automerge 实时校验当前 head 的内容批准、
分支新鲜度和必要检查，以合并令牌会签后 squash，一轮至多合并一个 PR。
必要检查仍在运行且配置了分支保护 context 时武装原生 auto-merge，门禁失效
撤销排定。`--dry-run` 运行同一判定，只报告写操作。
分支保护需由仓库管理员预先配置（双批准、过期批准作废、阻止落后分支、
必要检查与 merge 合并白名单），详见 [使用指南](docs/guide.md)。

观测数据位于运行根的 observations 目录，不保存消息本体，不影响调度。
`assistant mcp sessions --config config.yaml` 提供 `session_list`、`session_read`
查询执行元数据；可选状态 API 提供鉴权的 `GET /state`，临时访问令牌写入运行根
`api.json`（0600），退出时删除。

本次架构替换移除了 `assistantd`、旧 login/setup/init/install/doctor/config
命令与 providers/channels/runtimes JSON 配置。迁移步骤见
[docs/config.md](docs/config.md#旧版迁移)；旧文件不会自动覆盖或转换。

```sh
make build-local
make test
make cover
lean formal/Dispatcher.lean
make compose-up
```

Go 版本以 go.mod 为准。评审状态与调度语义分别由 `spec/ReviewStateMachine.tla`
与 `formal/Dispatcher.lean` 约束。开发约定见 [AGENTS.md](AGENTS.md)。
