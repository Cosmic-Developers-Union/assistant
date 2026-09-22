# assistant

Gitea 上的例行事务与评审自动化，两块能力：

- **仓库机器人**：`assistant sync` / `automerge` / `check` 在 Gitea Actions 里按事件与定时运行（标签收敛、评审状态同步、机械合并、待办查询）；
- **评审调度引擎**：`assistant run` 常驻宿主机，检测待办 → 为每个待办拉起 headless `claude` 会话 → 验证结论 → 清理，另带只读状态 API 与可选的对话服务端（multi-user / multi-session / multi-agent，通道可插拔：微信、QQ）。

`claude` CLI 是外部运行时依赖（会话在容器或宿主机里跑），其余能力都在本二进制里。

## 约定

这些是系统的硬约定，改代码前先理解：

- **身份固定**：内容评审 `ai`（别名 `reviewer`）、状态评审/合并 `merge`；合并白名单只含 `merge`，管理员也走分支保护。
- **双批准**：内容批准（`ai` 的 APPROVED）+ 状态会签（`merge` 对 head 盖章），缺一不可。
- **完成判定看 Gitea 原生 review**：调度引擎不解析会话输出来判成败——reviewer 名下出现新 review、或 triage 标签被移除，才算完成。
- **标签体系与 PR 流程**：`AGENTS.md`（`assistant install` 写进目标仓库）是唯一权威；reviewer 只评内容、不改标签，标签由 `sync` 收敛。
- **仓库脚手架由 install 托管**：`AGENTS.md`、`CLAUDE.md`、`skills/`、`.mcp.json`、`.claude/settings.json`、`.gitea/workflows/assistant.yml` 的托管段落不要手改；改内容要改本仓库的 `content/`、`skills/` 源文件。
- **评审协议随二进制走**：评审/分诊会话的协议与标签体系由内置 skill 注入（不依赖仓库是否装过脚手架）；项目自有约定放 `.assistant/review.md` 的非托管段落。
- **会话工具面由 assistant 决定**：gitea MCP 由 `assistant mcp gitea` 提供，与仓库里的 `.mcp.json` 无关；仓库自带的其它 MCP server 会被合并保留。
- **配置只有两个文件**：`config.json`（用户手写）与 `credentials.json`（assistant 管理）。没有第三处。

## 数据落点

`<配置目录>` 缺省 `~/.config/Cosmic-Developers-Union/assistant`，`<数据目录>` 缺省 `~/.local/share/Cosmic-Developers-Union/assistant`（`XDG_*`、`--config` / `ASSISTANT_CONFIG` / `ASSISTANT_CREDENTIALS` 可覆盖）。

| 路径 | 内容 | 谁写 | 说明 |
| --- | --- | --- | --- |
| `<配置目录>/config.json` | 平台、仓库、provider、微信桥 | 你（`config new` 生成空骨架，`config init` 补全） | 用户配置，含 provider `api_key` |
| `<配置目录>/credentials.json` | 登录身份 + `review`/`merge`/`admin`/`mcp` 用途令牌 | `login` / `setup` | 0600，不要手改 |
| `<配置目录>/config.schema.json` | 配置的 JSON Schema | `config new` / `config init` | 编辑器补全用 |
| `<配置目录>/claude/` | **会话文本记录 + claude 全局配置** | claude 会话 | `CLAUDE_CONFIG_DIR` 指向这里 |
| `<配置目录>/claude/projects/<项目>/<session-id>.jsonl` | 一次会话的完整事件流（一行一事件） | claude | 保留期 `cleanupPeriodDays=3650` |
| `<配置目录>/chat/sessions.json` | 对话会话 → claude 会话 id 映射 | 对话通道 | 跨重启复用同一会话 |
| `<配置目录>/chat/<chat-8位哈希>/` | 该对话会话的工作目录：`session.json`（元数据）、`settings.json`、`mcp.json`、会话产物 | 对话通道 | 同一会话恒用同一目录 |
| `<配置目录>/chat/conversations.json` | **会话实体映射表**：通道绑定（weixin:用户…、qq:openid…）→ conversation id、该会话下的 claude 会话与 /agent 选择 | 对话通道 | 换通道/并会话只改这张表 |
| `<配置目录>/serve.json` | 记录库服务端端点与令牌 | `serve` 启动时写、退出删 | 0600，同机客户端自举 |
| `<配置目录>/sessions-remote.json` | 远端记录库地址与令牌（服务端在别的机器时手写） | 你 | 0600 |
| `<配置目录>/session-push.json` | 增量推送状态（大小/修改时间） | `session push` | 可删（会重推一次） |
| `<数据目录>/sessions/` | **记录库根**：`manifest.json`（身份：格式/版本/布局/宿主机）+ `<host>/<项目>/<会话>.jsonl` + 同名 `.meta.json` | `serve`（收 push） | 可随会话记录一起备份 |
| `<配置目录>/daemon.json` | 状态 API 端点与令牌 | `run` 启动时写、退出删 | 0600，`mcp daemon` 靠它自举 |
| `<数据目录>/repos/<host>/<owner>/<name>/` | 受管克隆（评审数据源） | `run`（每轮 fetch 并强制对齐 `origin/<基线>`） | 可删除重建 |
| `<数据目录>/state/<host>/<owner>/<name>/logs/` | 每个待办一个会话日志（进度、`[debug]`、结果、验证结论） | 调度器 | 排障第一现场 |
| `<数据目录>/state/.../dispatcher.lock` | 单飞锁 | 调度器 | 运行期 |
| `/tmp/agent-dispatcher/<host>-<owner>-<repo>/worktrees/` | PR/Issue 的 worktree | 调度器 | 会话结束即删 |

要点：**文本记录与对话工作目录都在配置目录里**（随容器挂载、随备份一起走）；受管克隆与待办日志在数据目录里（克隆可重建，日志值得留）。

## 会话模型

| | 评审 / 分诊会话 | 对话会话（微信 / QQ） |
| --- | --- | --- |
| 会话标识 | 由「站点+仓库+待办+锚点」派生 | 通道绑定（如 `weixin:用户`、`qq:openid`）经映射表落到**会话实体** `conversation`（`c-xxxxxxxx`）：工作目录与记录都跟着会话实体走，换通道或把两个通道并到同一会话只改映射表 |
| 工作目录 | `/tmp` 下的 worktree（会话结束删除）；基线 `.claude/` 覆盖进 worktree，PR 自带的那份先删掉 | `<配置目录>/chat/<chat-8位哈希>/`（长期保留） |
| 会话 id | 由「站点+仓库+待办+锚点」确定性派生，同一待办重试 `--resume` 续接 | 首次生成 UUID 存 `sessions.json`；`/new`（或 `/reset`、`重新开始`）显式重开 |
| 记录位置 | `<配置目录>/claude/projects/assistant-<host>-<repo>/<session-id>.jsonl` | `<配置目录>/claude/projects/<由工作目录派生>/<session-id>.jsonl` |
| 项目名 | 注入 `CLAUDE_CODE_PROJECT_DIR_NAME`（worktree 会被删，必须钉住） | 不注入：由稳定工作目录派生，因此 `cd <工作目录> && CLAUDE_CONFIG_DIR=<配置目录>/claude claude --continue` 能接上 |
| 命令面 | `--permission-mode auto --autocompact auto --strict-mcp-config --mcp-config <生成>` + `--setting-sources project` | 同上，另加 `--bare`（探测到支持时）与 `--max-turns 50` |

会话的 `--settings` / `--mcp-config` 由 assistant 生成（env + 权限放行 + provider 覆盖 + MCP），**不写进仓库**；`--setting-sources project` 表示只读项目级设置，不碰操作者的用户级配置。

## 配置

- `config.json`：`instances[]`（host / reviewer / merger / repos）、`providers`（AI 供应商：`api_key` 简写 + `env`/`settings`/`mcp` 覆盖）、`default_provider`、`optimizations`、对话服务端（`weixin` / `qq` 通道 + `agents` 命名 agent 池 + `default_agent`）。示例见 `config.example.json`，供应商预设与各家的坑见 `providers.md`。
- `credentials.json`：`login` 派生该账号的 `mcp`（管理员另有 `admin`），`setup` 为 `ai`/`merge` 建机器人账号并派生 `review`/`merge`。令牌名 `assistant-<purpose>-<host>-<账号>`，重复登录复用，`--rotate` 轮换。
- 校验：`assistant validate`（只读、不联网；会指出"定义了却没被引用的 provider"这类失误）与 `assistant doctor`（本地脚手架 + 服务端分支保护/标签/协作者/secret）。
- 微信桥要显式开启：`assistant weixin login` 会把 `weixin.enabled=true` 与凭据写进 `config.json`（`assistant qq status` 可显示 QQ 通道状态并实测凭据）。

## 对话服务端（multi-user / multi-session / multi-agent）

`assistant run` 就是聊天服务端：并行承载多个消息通道，每个（通道, 用户）映射到独立会话，会话之间并发、同会话串行，互不串扰。

- **通道可插拔**：内置 `weixin`（openclaw ilink 长轮询）与 `qq`（QQ 开放平台官方 Bot API v2：WebSocket 网关收事件 + REST 被动回复，出站连接无需公网 IP）。`weixin.enabled` / `qq.enabled` 或 `run --weixin` / `--qq` 开启。
- **命名 agent 池（multi-agent）**：`agents` 节定义多个 agent（各自 `provider`/`model`/`system_prompt`/`claude_bin`/`session_timeout_ms`），`default_agent` 兜底；每个通道可用 `weixin.agent` / `qq.agent` 指定默认。用户在聊天里：
  - `/agent` 列出可用 agent 与当前生效；`/agent <名>` 切换（按会话记忆，下一条消息生效，可随时切回）；
  - `/new`（或 `/reset`、`重新开始`）重开 claude 会话（工作目录保留）；`/help` 看命令。
- **准入（multi-user）**：每通道 `admin_users` 白名单；含 `"*"` 放开所有人（公网平台慎用）；weixin 空白名单时只允许扫码登录者，qq 空白名单时全拒。群聊事件平台只在 @ 机器人时派发。

QQ 通道接入步骤：

1. 在 [q.qq.com](https://q.qq.com) QQ 开放平台创建机器人（群/私聊场景），拿到 AppID/AppSecret，按平台要求配置 IP 白名单；
2. 写入 `config.json`：`"qq": {"enabled": true, "app_id": "...", "app_secret": "...", "admin_users": ["<用户openid>"]}`（openid 在机器人收到第一条消息的日志里可见）；
3. `assistant qq status` 自检凭据，`assistant run` 启动后 QQ 里 @ 机器人即可对话（回复是被动消息：机器人只能回复收到的消息，15 分钟窗口内）。

> 限制：QQ 平台不允许主动发消息，对话必须由用户先开口；机器人回复为纯文本（富媒体留待后续）。

## 运行与观测

```bash
make compose-up                                            # 构建宿主二进制 + 重建容器（daemon + 状态 API + 对话通道）
docker compose exec -it assistant assistant weixin login    # 首次扫码（-it 必需）
docker compose logs -f                                      # compose 默认带 --debug
```

- 容器跑的是**宿主 `make build` 的二进制**（只读挂载），镜像只提供评审环境；升级 = `make compose-up`。
- **不挂载 `~/.claude`**：会话配置根是 `<配置目录>/claude`，凭据来自 `config.json` 的 `providers`/`optimizations`（或进程环境的 `ANTHROPIC_*`）。
- 日志一行一个事实：会话实况（版本/认证/权限/工作目录/工具面/**MCP 连通状态**）、`思考: …`、`claude: …`、`🔧 工具名` + 缩进 JSON 入参、`↩ 工具结果`、`API 错误：…`、回复摘要。`--debug` 追加生效配置（env 逐行、settings/MCP 路径、声明的 MCP）与 `[debug] 事件 type/subtype` 时间线。
- 密钥类字段（`*_TOKEN`/`*_KEY`/`*_SECRET`…）只显示前 6 与后 4 位（`sk-cp-…klmn`）；端点与模型名原样展示。原始 stream-json 不写日志，需要时读文本记录。
- 启动自检：`claude` 版本、会话配置根、每个 provider 的凭据来源，并用一个 `max_tokens=1` 请求**实测端点**——401 立刻告警（典型：国内 MiniMax 账号配了国际端点）。

## 主机部署（systemd）

不跑容器时的宿主机部署，一键安装：

```bash
curl -fsSL https://raw.githubusercontent.com/Cosmic-Developers-Union/assistant/main/install.sh | bash
```

按平台自动选择形态（从 GitHub Release 下载对应 OS/ARCH 的二进制）：

- **macOS（测试形态）**：装二进制 + 生成空配置（当前用户，配置落 `~/Library/Application Support/Cosmic-Developers-Union/assistant`），不建服务用户，直接前台跑 `assistant run`。token 已备好时零配置起步：

  ```bash
  export GITEA_HOST=https://<gitea地址> GITEA_ACCESS_TOKEN=<token>
  assistant run          # config.json 的 instances 为空时自动按环境变量单实例运行
  ```

- **Linux（服务形态）**：创建独立系统服务用户 + XDG 目录 + systemd 单元 + 空配置，`assistant run` 由 systemd 常驻运行（详见下）。

检出内运行则优先用现成/本地构建的二进制：`sudo ./install.sh --enable`（Linux；`make install-service` 等价）。

Linux 服务形态细节：

- **独立服务用户**（缺省 `assistant`，`--user`/`--home` 可改）：系统账号、nologin、无密码——仅供 systemd 运行服务，无需登陆；
- **XDG 目录**建在服务用户家目录下（缺省 `/var/lib/assistant`）：配置 `~/.config/Cosmic-Developers-Union/assistant`、数据 `~/.local/share/…`、状态 `~/.local/state/…`，与二进制缺省解析一致；
- 安装二进制到 `/usr/local/bin/assistant`、写 `assistant.service`（`ProtectSystem=strict`，仅 XDG 目录与私有 /tmp 可写），并以服务用户身份执行 `assistant config new` 生成空配置骨架；
- 幂等：重复执行只更新二进制与单元文件，已有用户/目录/配置不动。

之后以服务用户身份补全配置并启动：

```bash
sudo -u assistant -H assistant login https://gitea.example --user <管理员账号>   # 管理员登录
sudo -u assistant -H assistant setup                                            # 建 ai/merge 机器人账号并派生令牌
sudo -u assistant -H assistant validate && sudo systemctl enable --now assistant
journalctl -u assistant -f                                                      # 观测
```

评审会话还需要 `claude` CLI 在服务用户的 PATH 上（建议装到 `/usr/local/bin`）。

## 会话记录服务（serve / session push / sessions MCP）

内置三件套：

| 命令 | 角色 |
| --- | --- |
| `assistant serve` | 记录库服务端：收 `session push` 的记录，按 **(host, project, session)** 存成原样 jsonl + `.meta.json`；提供 list / read / search / conversations 接口 |
| `assistant session push` | 客户端：扫 `<配置目录>/claude/projects/*/*.jsonl`，按大小+修改时间增量推送；聊天会话附带会话实体与通道 |
| `assistant mcp sessions` | 给 agent 的 MCP：`session_search` / `session_list` / `session_read` / `conversation_list`——**上下文被压缩后回查完整历史** |

```bash
assistant serve &                      # 本机记录库：写 <配置目录>/serve.json（0600，退出即删）
assistant session push                 # 同机自动发现端点，增量推送
assistant session push --dry-run       # 先看会推什么
# 远端：服务端机器跑 serve，客户端写 <配置目录>/sessions-remote.json {"url":"…","token":"…"}
# 也可用 ASSISTANT_SESSIONS_URL / ASSISTANT_SESSIONS_TOKEN
```

- `host` 是宿主机标签（缺省主机名）：区分不同机器上的同名项目；记录库缺省在 `<数据目录>/Cosmic-Developers-Union/assistant/sessions`（`serve --root` 可改）。
- **目录名不参与身份判断**：库根必须有 `manifest.json`（`{"format":"assistant.sessions","version":1,…}`）。`serve` 打开一个非空、却没有（或格式不符）manifest 的目录会直接拒绝——所以 `/srv/sessions` 这种名字被别的程序占用时不会互相写坏；确认是空目录或你的库才初始化，要接管已有目录用 `--force`。远端示例：`--root /srv/cosmic-developers-union/assistant/sessions`（短名 `/srv/cdu/assistant/sessions` 也行，安全由 manifest 保证）。`GET /healthz` 会回 format/version/host/root，便于确认连的是哪个库。
- 微信对话桥在配置了记录库时**每轮结束自动归档**该会话（日志 `已归档会话 …`），所以同一轮里 agent 就能查到自己被压缩掉的历史；聊天会话已自动注入 sessions MCP（`mcp__sessions__*` 已放行）。
- 评审会话用 `assistant session push` 定时归档即可（增量，重复执行无副作用）；接口细节看 `--help`。

**不想跑 serve** 时的等价做法（记录就是文件）：

| 数据 | 路径 | 含密钥 |
| --- | --- | --- |
| 会话文本记录 | `<配置目录>/claude/` | 否 |
| 微信会话工作目录 | `<配置目录>/chat/` | **是**（`settings.json`/`mcp.json`） |
| 待办会话日志 | `<数据目录>/state/*/*/*/logs/` | 否 |

1. 把 `<配置目录>/claude` 做成 git 仓库并定时 commit+push（记录文件名是 session id，跨机不冲突）；
2. `CLAUDE_CONFIG_DIR` 指向已同步目录（rclone/NFS/网盘），assistant 只透传；
3. 定时 `tar`/`rsync` 到对象存储，或 `jq` 抽取 `projects/**/*.jsonl`（一行一事件）落检索系统。

注意：`chat/*/settings.json`、`chat/*/mcp.json`、`config.json`、`credentials.json` 都含密钥，归档/同步前排除或加密。

## 开发

```bash
make build-local   # 本地平台二进制
make test          # go test ./...
make install       # 构建并安装到 /usr/local/bin
make test-e2e      # 起临时 Gitea + runner 跑端到端
```

- 本机若 `~/.cache/go-build` 只读，用 `export GOCACHE=/tmp/assistant-gocache GOTMPDIR=/tmp`。
- 目录：`cmd/assistant`（CLI）、`internal/{dispatcher,daemon,status,instances,credentials,provider,claudecfg,repoinstall,setup,weixin}`、`schema/`（配置 schema，随二进制分发）、`content/` + `skills/`（install 的托管内容源）、`images/review/Dockerfile`（评审/daemon 镜像，多 target）、`test/e2e` + `test/gitea`（端到端）、`formal/` + `spec/`（调度语义与评审状态机的形式化规格）。
- 改变调度语义或评审状态机时必须同步改 `formal/Dispatcher.lean` / `spec/ReviewStateMachine.tla`，并让证明与模型检查重新通过（`lean formal/Dispatcher.lean`）。
- 提交前只跑本次变更相关的 fmt/lint/test；一个主题一个提交；移除死代码与过时引用。
