# AGENTS.md

本文件约束在**本仓库**（assistant 自身）里工作的 AI agent。`CLAUDE.md` 只是
`@AGENTS.md` 导入，不要单独维护。

注意区分两类同名文件：本仓库根部的 `AGENTS.md` 是维护者手写的开发约定（本文件）；
`assistant install` 写进**目标仓库**的 AGENTS.md 段落，内容源是仓库里的
`content/`、`skills/`、`internal/repoinstall/templates/`——改生成物之前先改源文件。

## 项目概览

assistant 是一个 Go 单二进制，两块能力：

- **仓库机器人**：`assistant sync` / `automerge` / `check` 在 Gitea Actions 里按
  事件与定时运行（标签收敛、评审状态同步、机械合并、待办查询）；
- **评审调度引擎 + 对话服务端**：`assistant run` 常驻宿主机，检测待办 → 为每个
  待办拉起 headless `claude` 会话 → 验证结论 → 清理；同一进程并行承载多个消息通道
  （微信 / QQ / Telegram / Gitea），会话之间并发、同会话串行。

`claude` CLI 是外部运行时依赖（会话在容器或宿主机里跑），其余能力都在本二进制内。

托管与发布：

- **主仓库在 GitHub**（`Cosmic-Developers-Union/assistant`，remote `origin`）；
  GitHub Actions 负责 Release 与镜像发布（ghcr.io）。
- **Gitea 侧（remote `gitea`）用于备份与测试**：仓库级 Actions、`make push` 的
  generic package 发布演练都在这里跑。
- 与 Gitea 交互统一用官方 Go SDK（源仓库 <https://gitea.com/gitea/go-sdk>，
  import 路径 `gitea.dev/sdk`），不要手搓 REST 调用；优先复用
  `internal/status`、`internal/setup` 等已有封装。

## 常用命令

```bash
make build-local   # 本地平台二进制 → ./assistant（开发用）
make build         # Linux/amd64 静态二进制（generic package 发布用）
make test          # go test ./...
make test-e2e      # 起临时 Gitea + runner 跑端到端（-tags e2e）
make gitea-up      # 只起临时 Gitea，保留现场手动调试（gitea-down 清理）
make compose-up    # 重建容器部署（daemon + 状态 API + 对话通道，挂载宿主二进制）
make install       # 构建并安装到 /usr/local/bin（PREFIX 可改）
make review-image  # 评审会话镜像（images/review/Dockerfile，target review）
```

- 提交前**只跑与本次变更相关的** fmt / lint / test / typecheck；全仓检查只在发布
  流程或用户明确要求时跑。
- 本机若 `~/.cache/go-build` 只读：`export GOCACHE=/tmp/assistant-gocache GOTMPDIR=/tmp`。
- e2e 测试没有测试环境变量时自动跳过；需要真实 Gitea 时用 `make test-e2e`。

## 目录结构

- `cmd/assistant/`：CLI 与子命令（cobra），一个主题一个文件。
- `internal/`：核心实现——`dispatcher`（调度引擎）、`daemon`（对话服务端）、
  `status`（评审状态机）、`instances`/`config`（配置装载）、`credentials`
  （凭据库）、`provider`（各家模型接入）、`claudecfg`（claude 会话配置）、
  `repoinstall`（目标仓库脚手架）、`setup`、`weixin`/`qq`/`telegram`。
- `schema/config.schema.json`：配置 JSON Schema，随二进制分发。
- `content/` + `skills/`：install 写进目标仓库的托管内容源。
- `images/review/Dockerfile`：评审 / daemon 镜像，多 target。
- `test/e2e`（`//go:build e2e`）+ `test/gitea`（临时 Gitea 环境）。
- `formal/Dispatcher.lean` + `spec/ReviewStateMachine.tla`：调度语义与评审状态机的
  形式化规格。
- `docs/`、`providers.md`、`README.md`：面向使用者的文档与供应商踩坑记录。

## 硬约定（改代码前先理解）

这些是系统的语义约定，不是可调参数：

- **身份固定**：内容评审 `ai`（别名 `reviewer`）、状态评审/合并 `merge`；合并白名单
  只含 `merge`，管理员也走分支保护。
- **双批准**：内容批准（`ai` 的 APPROVED）+ 状态会签（`merge` 对 head 盖章），缺一
  不可。
- **完成判定看 Gitea 原生 review**：调度引擎不解析会话输出来判成败——reviewer 名下
  出现新 review、或 triage 标签被移除，才算完成。
- **标签体系与 PR 流程**：写进目标仓库的 `AGENTS.md` / `content/agents.md` 是唯一
  权威；reviewer 只评内容、不改标签，标签由 `sync` 收敛。
- **评审协议随二进制走**：评审 / 分诊会话的协议与标签体系由内置 skill 注入（提示词
  事实源是 `skills/review/SKILL.md`），不依赖目标仓库是否装过脚手架；项目自有约定放
  `.assistant/review.md` 的非托管段落。
- **会话工具面由 assistant 决定**：gitea MCP 由 `assistant mcp gitea` 提供，与仓库里
  的 `.mcp.json` 无关；目标仓库自带的其它 MCP server 会被合并保留。
- **配置只有两个文件**：`config.json`（用户手写）与 `credentials.json`（assistant
  管理），没有第三处。
- **上游变更优先 rebase**：作者 rebase 到 `origin/main` 后重新走评审 / 合并门禁。

## 配置与数据落点

- 配置定位按 `--config` → `ASSISTANT_CONFIG` → 当前目录 `./config.json`；用
  `.env` 时只加载 config.json 同目录的那份，不向上搜索。
- 凭据是**用户级**状态，落平台标准配置目录（XDG Base Directory Specification /
  Windows Known Folders / macOS Library Directory 约定，Linux 为
  `~/.config/Cosmic-Developers-Union/assistant/`），从任何目录启动都解析同一份登录
  态，绝不锚定 cwd。
- 运行产物跟随 runtime 的 `$root`（repos / state / review / chat / claude）；
  `data/` 与 `config.json` 不入库（见 `.gitignore`）。
- 密钥类字段（`*_TOKEN` / `*_KEY` / `*_SECRET` …）支持 `$VAR`、`${VAR}`、
  `${VAR:-default}` 引用且不落盘；日志只显示前 6 位与后 4 位。
- 完整说明见 `docs/config.md`，示例见 `config.example.json` 与 `.env.example`。

## 日志与可观测性

日志必须让操作者能够观察系统状态、关键事件和故障原因；`--verbose` 增加正常运行
细节，`--debug` 进一步暴露内部诊断信息。更具体地说：

- `默认`：只输出操作者真正需要知道的状态、结果、警告和错误；
- `--verbose`：展示“系统正在做什么”，例如步骤、目标、进度、外部调用和状态变化；
- `--debug`：展示“系统为什么这样做”，例如内部决策、参数、分支、重试、底层错误和
  诊断上下文。

```text
default   = What happened?
verbose   = What is happening?
debug     = Why is it happening?
```

## 代码风格（Go）

- Go 版本以 `go.mod` 为准（当前 1.27）；写 / 改 Go 代码前先读
  `.agents/skills/use-modern-go/SKILL.md` 并按它运行 Modern Go Guidelines CLI
  （先 `list`，必要时对相关 ID 跑 `explain`），以版本化建议为准。
- 注释、日志、错误信息、CLI 帮助一律中文；导出标识符要有说明“为什么”的 Godoc
  注释（新增 package 的包注释写清职责与边界）。
- 错误用 `fmt.Errorf("语境: %w", err)` 包装；识别错误链用 `errors.As` / `errors.Is`，
  不要用类型断言或字符串匹配（见 commit 479a1c7）。
- 单元测试与被测代码同目录，命名 `*_test.go`；端到端测试放 `test/e2e`，用
  `//go:build e2e` 标记。
- 不引入新依赖除非确有必要；能复用标准库与既有封装就不另起一套。
- 移除死代码、过时引用与失效测试；有特殊保留原因要写注释说明。

## 形式化规格

改变调度语义或评审状态机时，必须同步更新 `formal/Dispatcher.lean` /
`spec/ReviewStateMachine.tla`，并让证明与模型检查重新通过
（`lean formal/Dispatcher.lean`）。

## 托管文件不要手改

本仓库自身也被 `assistant install` 托管：`.gitea/workflows/assistant.yml`、
`.claude/settings.json` 的白名单键、`.claude/` 与 `.agents/skills/` 下 install 安装的
技能（如 `review`）都来自脚手架，带 `managed-by: assistant` 标记。要改内容就改源
文件（`internal/repoinstall/templates/`、`content/`、`skills/`），再重新 `install`；
目标仓库里的托管段落同理。`.assistant/review.md` 只有托管段落受管，自有内容写在
段落之外。

## 提交与 PR

- 一个主题一个提交，提交信息 `type(scope): 中文描述`（type 取
  `feat` / `fix` / `docs` / `refactor` / `test` / `chore`），正文写清动机与取舍。
- 提交或更新 PR 前先 rebase 到 `origin/main`；解决冲突后通读完整 diff，确认没有残留
  冲突标记（`git add -A` 会把标记一起暂存，dprint 还会把它们重排进正文，事后极难
  发现）。
- PR 中允许包含目标代码与相关脚手架（`AGENTS.md`、`CLAUDE.md`、`skills/` 等）的
  同步变更，评审不应要求拆成单独 PR。
- 相关的功能请求与 bug 修复尽量在同一个 PR 完成；PR 创建时标记 `WIP` / draft，
  避免意外合并（draft 不进评审队列，也不参与 automerge）。
- 提交前自查清单：变更是否单一主题；死代码是否已删；文档（`README.md`、
  `docs/`、`providers.md`）是否同步、过时内容与不存在的引用是否清除；失效测试是否
  已删；新行为是否有测试覆盖。
- 评审与合并走仓库自身流程：内容批准由 `ai` 提交、状态会签与合并由 `merge` 执行，
  合并只能由 `merge` 账号完成（分支保护对管理员同样生效）。
