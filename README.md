# assistant

一个 Go 静态二进制，两种角色：

1. **仓库辅助机器人**（Gitea Actions 事件 / schedule 驱动）：机械、启发式地处理 Issue、PR 的例行事务（标签体系维护、Issue 标签规范、PR 评审状态同步、已批准 PR 自动合并）。
2. **评审会话调度引擎**（宿主机 systemd 常驻）：检测 Gitea 待办并自动拉起 headless 评审会话，直到 review 完成。

两种角色共享 `GITEA_HOST` / `GITEA_ACCESS_TOKEN` / `GITEA_REPOSITORY` 配置约定与 Gitea API 客户端。`claude` CLI 是调度引擎的外部运行时依赖（不在本仓库内），需在宿主机安装并登录；其余能力全部由二进制自带。

## 命令

| 命令 | 角色 | 说明 |
| --- | --- | --- |
| `assistant check` | 机器人 | 按标签检索待 triage 的 Issue 和待 review 的 PR（只读） |
| `assistant sync` | 机器人 | 规范 Issue 标签并把 PR 原生评审状态同步为状态标签（单次执行） |
| `assistant automerge` | 机器人 | 合并门禁全绿的已批准 PR（一次至多一个，squash） |
| `assistant run` | 调度引擎 | 长驻主循环：检测待办 → 每待办一个会话 → 验证 → 清理 |
| `assistant list` | 调度引擎 | 只读列出当前待办（验证 host/仓库/令牌/标签链路） |
| `assistant review <n>` | 调度引擎 | 立即评审单个 PR（跳过检测，端到端调试用） |
| `assistant triage <n>` | 调度引擎 | 立即分诊单个 Issue（跳过检测） |

`assistant --help` 查看全部选项。

## 仓库辅助机器人

- `sync`：补齐标签体系、规范 open Issue 标签（含 duplicate/wontfix 自动关闭）、把 PR 原生评审状态同步为状态标签。单次执行、幂等，由 Gitea Actions 事件驱动运行。
- `check`：按标签检索待处理项并输出报告。默认立即返回；`--wait` 持续轮询直到出现待办，`--timeout` 轮询到超时为止（两者互斥，时长支持 `d` / `w` 单位，如 `1d1m1s`）；`--interval` 控制间隔（默认 15s），Ctrl+C 可随时中断。
- `automerge`：把带 `status/approved` + `awaiting/merge` 标签且门禁全绿（分支未落后 main、无冲突、必要检查通过）的 PR squash 合并；一次运行至多合并一个 PR。由 schedule 工作流每 5 分钟驱动。

### 退出码

| 退出码 | 含义                                                             |
| ------ | ---------------------------------------------------------------- |
| `0`    | 正常结束                                                         |
| `1`    | 瞬时错误（网络不通、Gitea 暂不可用）或其他运行失败               |
| `78`   | 配置类致命错误：认证失败（HTTP 401/403）或 `GITEA_HOST` 配置错误 |

三个子命令均为单次执行，不做进程内重试；CI 由事件和 schedule 兜底重跑。

### 环境变量

- `GITEA_HOST`: Gitea 服务器地址（必需，完整的 HTTP/HTTPS URL）
- `GITEA_ACCESS_TOKEN`: Gitea 访问令牌（必需，需要读写仓库权限）
- `GITEA_BRANCH_PROTECTION_TOKEN`: 分支保护读取专用令牌（可选，仓库管理员 PAT）。分支保护端点要求 repo
  admin 权限；未配置时读取被拒（HTTP 403）会自动回退为「任何失败 context 即阻塞」的严格模式
- `GITEA_STATE_REVIEWER`: 状态评审者账号名（可选，如 `merge`）。配置后 sync 按作者角色区分内容/状态两条
  review 通道，automerge 在合并前以该角色对 head 会签；未配置时保持单通道行为
- `GITEA_STATE_TOKEN`: 状态评审者令牌（可选，与 `GITEA_STATE_REVIEWER` 配套）
- `GITEA_REPOSITORY`: 指定仓库（可选，格式: owner/name，用于 CI 环境锁定仓库）

优先级: `--repo` 命令行参数 > `GITEA_REPOSITORY` 环境变量。

## 评审会话调度引擎

```
检测（只读标签）                    每待办一个会话                      完成判定（简单规则）
─────────────────                 ─────────────────────              ─────────────────────
awaiting/reviewer PR ──────────▶  review pr #N（起始提示词） ──────▶  起点之后 reviewer 的新 review
status/triage     Issue ───────▶  triage issue #N                  ▶  status/triage 标签已移除
```

- **PR 评审**在 `refs/pull/<N>/head` 的独立 worktree 里进行（默认 `<系统临时目录>/agent-dispatcher/worktrees/pr-<N>`，`--worktree-root` 可改），会话结束后移除；Issue 分诊会话的 cwd 是仓库主检出。
- **评审标准锚定基线，PR 不可自改**：worktree 建成后立即以宿主检出的 `.claude/` 整体覆盖（skills、settings）——worktree 按 PR head 检出，随带的 `.claude` 是 PR 自己的版本，照单全收等于允许 PR 改弱自己被审的规则。宿主检出缺 `.claude/` 时拒绝起会话（fail-closed）。
- **镜像同步（`--sync-mirror` / `DISPATCH_SYNC_MIRROR=1`，部署形态开启）**：每轮检测前把宿主检出强制对齐 `origin/<基线分支>`（`--base-branch` / `DISPATCH_BASE_BRANCH`，缺省 `main`）——`git fetch --prune origin` → `git checkout -f -B <基线> origin/<基线>` → `git clean -fd`，本地任何分叉一律丢弃；`.gitignore` 豁免的本地产物不受影响。同步失败跳过本轮等待重试。共享开发检出勿开启。
- **会话驱动**：`claude -p` 子进程，命令面与手工运维一致（`--permission-mode auto`、`--autocompact auto`、stream-json 输出）。无人值守必需项：`--strict-mcp-config --mcp-config` 从宿主仓库 `.mcp.json` 显式注入 gitea MCP，外加 `--max-turns` / abort 超时兜底 runaway 会话。
- **进度两路落点**：控制台实时显示会话 init 与编号的工具调用；完整明细实时写待办日志——stdout 按 stream-json 逐行解析，assistant 文本原样、工具调用记 `🔧 名称`。
- **验证失败重试一次，head 漂移即作废**：每个 PR 会话开工时钉定当时的 head；完成后若 reviewer 提交了新 review 但 head 已被作者推进，本轮评审视为作废——直接放行等下一轮以新 head 重开。head 未变仅缺 review 才重试；两个会话后仍无完成动作则记录放行。
- **有界并发（`--concurrency` / `DISPATCH_CONCURRENCY`，缺省 1）**：一轮待办至多同时跑 N 个会话；轮与轮之间是天然 barrier——同一 PR/Issue 同一时刻至多一个会话。
- **单飞**：`dispatcher.lock` 记 PID（原子创建），同机第二实例拒绝启动，死 PID 残留自动接管（`review`/`triage` 一次性命令共用此锁）。
- **优雅退出**：首个 SIGINT/SIGTERM 等当前待办处理完；二次信号强杀。

### 命令行

```
assistant run             长驻主循环（部署形态；--interval/--timeout 可调）
assistant run --dry-run   只读演练：按同一检测口径列出将执行的待办，逐步说明
                          将发生的动作（日志、worktree、会话、超时、完成判定、
                          重试、清理），零副作用
assistant list            只读列出当前待办（快速验证 host/仓库/令牌/标签链路）
assistant review <n>      立即评审单个 PR（跳过检测，端到端调试用）
assistant triage <n>      立即分诊单个 Issue
```

配置优先级：**命令行参数 > 环境变量 > git remote 自动检测 / 默认值**。

- **host 与仓库缺省从 origin remote 推导**：http(s) remote（如 `http://gitea.example.com:3000/owner/repo.git`）可完整推出 API 根地址与 owner/repo；ssh/scp remote 只可靠推出仓库，host 以 `http://<主机名>` 尽力猜测，此时用 `--host` / `GITEA_HOST` 显式指定。
- 时长参数（`--interval` / `--timeout` 及对应 `DISPATCH_*_MS` 环境变量）接受 `30s` / `10m` / `1h` / `2d` 或毫秒裸数字。
- 路径默认值锚定宿主检出根（`git rev-parse --show-toplevel`），与启动 cwd 无关：日志 `<检出根>/logs`、锁 `<检出根>/dispatcher.lock`；worktree 在系统临时目录。
- 其余环境变量：`DISPATCH_LOG_DIR`、`DISPATCH_WORKTREE_ROOT`、`DISPATCH_LOCK_FILE`、`DISPATCH_MODEL`、`DISPATCH_REVIEWER`、`DISPATCH_CLAUDE_BIN`。

### 访问令牌的作用

`--token` / `GITEA_ACCESS_TOKEN` 一处配置、两处消费，且**决定评审身份**：

1. **dispatcher 自身**：按标签检测待办、完成判定验证（读 review 与标签），只需读权限。
2. **评审会话**：gitea MCP 子进程继承同一组 `GITEA_HOST` / `GITEA_ACCESS_TOKEN`，会话提交的 Pull Request Review 以该令牌的账号身份落库——「reviewer 名下出现新 review」正是完成判定的依据。

因此令牌必须是 reviewer 账号（默认 `ai`）的令牌：换成其他账号，检测与验证照常工作，但提交的 review 不再匹配 `--reviewer`，PR 会被反复重开会话。令牌无法自动检测，必须显式提供（命令行传参可见于进程列表，推荐环境变量）。

### 部署（专用开发机）

依赖：claude CLI 已安装并登录（PATH 可见；否则用 `--claude-bin` 指绝对路径）。二进制本身无其他运行时依赖。

```bash
make build                        # 或 make push 发布到 generic package registry
export GITEA_ACCESS_TOKEN=<ai 账号令牌>
export DISPATCH_SYNC_MIRROR=1     # 每轮把宿主检出强制对齐 origin/main（评审标准的数据源）
./assistant run                   # 在仓库检出内任意目录启动（路径默认值锚定检出根）
```

systemd 示例（`WorkingDirectory` 建议仓库检出根；systemd 的最小 PATH 通常不含 claude 安装目录，`--claude-bin` 用绝对路径）：

```ini
[Unit]
Description=review agent dispatcher
After=network-online.target

[Service]
WorkingDirectory=/srv/repo
Environment=DISPATCH_CLAUDE_BIN=/usr/local/bin/claude
Environment=DISPATCH_SYNC_MIRROR=1
EnvironmentFile=/etc/assistant.env
ExecStart=/srv/repo/assistant run
Restart=on-failure
KillSignal=SIGTERM
TimeoutStopSec=1800

[Install]
WantedBy=multi-user.target
```

`/etc/assistant.env` 至少包含 `GITEA_ACCESS_TOKEN=<ai 账号令牌>`。

### 日志

每个待办一个归档：`logs/<kind>-<number>-<时间戳>.log`——起始提示词、worktree/head、会话 init、实时进度（assistant 文本与 `🔧` 工具调用）、结果 JSON（turns、cost、permission denials）、验证结论。

## 形式化规格

- `formal/Dispatcher.lean`：调度语义的 Lean 4 形式化规格（时间线建模、完成判定谓词）。机器检验的性质包括：作者 push 不同代码时完成判定不可能通过（A1）、更新 PR 说明对完成判定无影响（A2）、head 漂移本轮作废、同一待办至多两个会话、入队受 pending 与并发上限双重守卫。

  ```bash
  lean formal/Dispatcher.lean   # 退出码 0 即全部证明通过（纯 core Lean，无需 mathlib）
  ```

- `spec/ReviewStateMachine.tla`：PR 评审状态机的 TLA+ 规格（标签体系与状态流转）。

规格与实现语言无关；改变这些语义的实现改动必须同步修改模型并让证明重新通过。

## 开发

```bash
make test          # go test -v ./...
make build-local   # 构建本地平台二进制（用于开发测试）
make build         # 构建 Linux/amd64 静态二进制（与 CI 发布产物相同）
make push          # 手动发布 latest 到 generic package registry（引导/紧急修复；需要 GITEA_HOST / GITEA_ACCESS_TOKEN）
make clean         # 清理构建产物
```

发布的是统一二进制 `assistant`。仓库内的 Gitea Actions 工作流（发布、`sync`、`automerge`）需相应把下载/执行目标从旧的 `gitea-assistant` 改为 `assistant`；发布步骤仍使用带 `write:package` scope 的 PAT secret。

## 许可

MIT
