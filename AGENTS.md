# AGENTS.md

本文件约束在本仓库（assistant 自身）工作的 AI agent。CLAUDE.md 仅导入本文件。
产品目标以 [goal.md](goal.md) 为准；本次架构是单一 `assistant` 二进制，
不要重新引入 assistantd 或 providers/channels/runtimes 配置层。

## 项目边界

- `cmd/assistant` 是薄入口，信号与退出码统一走 `internal/cli.Execute`。
- `internal/cli` 只翻译命令和装配参数，四个操作面：instance、mcp、action、run。
- `internal/credentials` 是用户级平台实例库；按类型严格校验 credentials.json。
- `internal/runtime` 是运行配置、事件源与 Workspace/Session/AgentRunner 流水线。
- `internal/status` 是标签收敛、评审请求维护与机械合并门禁。
- `internal/claude` 是外部 Claude CLI 的进程、参数与 stream-json 适配器。
- `internal/integration/{gitea,qq,weixin,telegram}` 是平台协议适配；通用消息桥在 integration。
- `internal/mcps` 检测开发者 Gitea 身份并启动官方 stdio MCP。
- `skills/review/SKILL.md` 是内嵌评审协议的唯一提示词来源。
- `content/agents.md` 保存仓库评审约定的内容源。

主仓库是 GitHub Cosmic-Developers-Union/assistant（origin）；Gitea remote 用于
备份与测试。与 Gitea 交互使用官方 Go SDK `gitea.dev/sdk`，复用 internal/status。
不要手写新的 REST 客户端。

## 语义约定

- 内容评审者惯例 ai（别名 reviewer），会签/合并者惯例 merge；账号名是约定，
  命令的授权以令牌权限为准。部署的合并白名单只含 merge，管理员也受分支保护。
- 内容批准与状态会签缺一不可。内容批准必须是内容评审者最新未撤销结论，
  明确指向当前 head；空提交号、旧 head、后续修改意见不能授权合并。
- run 只消费平台 status/review PR 与 status/triage Issue；draft/WIP 不进入评审队列。
  官方请求、按钮复审记录和评论提及的归一化属于 label-sync。
- 完成看平台待办在下一轮消失，不解析模型输出判业务成功，不持久化本地 settled
  标记压住重试。进程退出只进入观测记录。
- 同一待办同时至多一个会话；每轮 barrier 后重新读平台；共享 Git 元数据串行。
- Workspace 管目录准备/清理，Session 管稳定 id/记忆落点/恢复/固化，AgentRunner
  管一次启动。新的工作区或存储实现不能把策略塞回主循环。
- Claude JSONL 是记忆本体，不再复制一份内容数据库；observations 只存执行元数据。
  取消与失败也固化已有记忆，远端恢复失败不得悄悄新开失忆会话。
- PR 以 head 为记忆锚，Issue 以标题为锚。项目评审规则钉住可信基线，PR 自带
  `.claude/` 必须先移除再从基线复制。
- reviewer 只评内容、不改标签；标签由 action label-sync 维护。合并不看标签，
  action automerge 重新读当前 head、分支状态、内容结论与必要检查。
- 一轮至多实际 squash 合并一个 PR。合并请求结果未知时停止本轮，避免第二次请求。
- action 只处理 GITEA_REPOSITORY，必须同时给 GITEA_HOST、GITEA_ACCESS_TOKEN，
  不枚举其他仓库、不借用个人凭据；--dry-run 截断全部写操作。

## 配置与数据

- run 只读 --config，缺省当前目录 config.yaml；不查找 ASSISTANT_CONFIG、不自动
  加载 .env、不读取个人 credentials.json。相对 root 锚定 YAML 所在目录。
- credentials.json 使用平台标准用户配置目录；ASSISTANT_CREDENTIALS 可覆盖。
  Linux 为 ~/.config/Cosmic-Developers-Union/assistant/credentials.json，与 cwd 无关。
- connects/mcp/bots/session/runtime 分别声明连接、工具、工人、存储与运行参数。
  未知字段、悬空引用、跨平台字段、重复事件消费者均在启动前报错。
- 密钥用 {{VAR}} 引用，不回写、不进日志；密码只在 instance 登录时使用，不落盘。
- 所有产物在运行 root 内：repos/workspaces/chat/sessions/observations/api.json。
  config.yaml、.env 和 data 不入库；用户真实配置不改不删。
- 旧版配置有意不自动迁移，格式错误应清楚报告。迁移说明在 docs/config.md。

## 日志

默认输出操作者需要的状态、结果、警告与错误（What happened）。
--verbose 展示正在执行的步骤、目标、外部调用与进度（What is happening）。
--debug 展示内部决策、参数与诊断上下文（Why is it happening）。
stdout 是主体/协议输出，stderr 是日志/错误；stdio MCP 不能混入日志。
日志不打印完整令牌；密钥展示仅保留前 6 位与后 4 位。

## Go 与测试

- 版本以 go.mod 为准；写 Go 前读 `.agents/skills/use-modern-go/SKILL.md`，
  运行 Modern Go Guidelines CLI list，按需 explain。
- 注释、日志、错误与 CLI 帮助用中文；导出标识符写清职责与原因的 Godoc。
- 错误用 fmt.Errorf("语境: %w", err) 包装，用 errors.Is/As 识别链，不匹配错误字符串。
- 单测与代码同目录，端到端在 test/e2e，使用 //go:build e2e。
- 不引入无必要依赖；删死代码、旧引用和失效测试，保留有独立价值的契约测试。
- 核心 runtime/project/status/credentials/cli/claude/integration ≥90%；平台/MCP/skills ≥80%。
  覆盖率逐包算；公共 API、边界、失败/恢复与协议必须有测试，数字不是充分条件。
- 真实 Claude、真实 Gitea、真实 S3 调用用 e2e；逻辑通过窄接口注入替身。
  不可触达的系统故障如实登记 docs/coverage-gaps.md，不堆无意义测试。
- 只跑变更相关检查；整体架构变更覆盖全部受影响包。make cover 是逐包门禁。

改变调度或评审状态语义必须同步 formal/Dispatcher.lean、spec/ReviewStateMachine.tla，
并运行 Lean 证明与 TLC 模型检查。规格应对应实际实现，不用删除性质绕过失败。

## 命令与提交

make build-local 构建开发二进制；make build 构建 Linux/amd64 静态二进制；
make test 跑单测；make test-e2e 起临时环境并跑端到端；make cover/cover-report 检查覆盖率；
make compose-up 启动容器服务；make install 安装单二进制；make review-image 构建评审环境。
只读 Go 缓存时使用 GOCACHE=/tmp/assistant-gocache GOTMPDIR=/tmp。

一个主题一个提交，type(scope): 中文描述，正文解释动机与取舍。
提交/更新 PR 前 rebase origin/main，通读完整 diff、检查冲突标记。
PR 先 draft/WIP；相关实现、文档与技能可在同一 PR。评审与合并遵循仓库自身
ai 内容批准、merge 会签/合并流程；不擅自合并或部署。

`.agents/skills/` 与 `.claude/skills/` 的既有托管文件不要直接修改；修改评审协议
用 skills/review/SKILL.md 源。新项目工具只管理 project install/uninstall 指定的 Actions/MCP；旧托管技能保留给
开发工具使用。项目自有约定仍放 `.assistant/review.md` 的托管段落之外。

`run` 的 Gitea 消费范围是账号可见仓库与全站 @ 召唤，不加仓库白名单。
`project` 只规范当前项目；禁止把站点接入、项目安装和常驻调度混成一个命令。
`agent-runtime/` 是独立 module，TUI 与轻量引擎由独立任务实现，本轮不集成主线。
