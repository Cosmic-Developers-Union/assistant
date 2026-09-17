# run.yaml —— daemon 运行配置

`assistant run`（daemon 部署形态）的运行配置。run.yaml 存在时，run **完全按它
运行**：站点（monitor）、数据落点（root/repos-dir）、评审工作区（review-root
与命名模板）都来自它；config.json 退为账号身份层（providers/optimizations/
weixin），其 `instances` 不再参与 run 的目标解析。

## 落点

按顺序定位（先到先用）：

1. `--run <path>` 显式指定；
2. `ASSISTANT_RUN` 环境变量；
3. config.json 同目录的 run.yaml（随配置目录一起挂载/备份，容器部署推荐）；
4. 平台标准配置目录下的 run.yaml（XDG / Known Folders / Library）。

都不存在时保持原语义：按 config.json 的 instances × repos 运行。

## 字段

```yaml
monitor:
  http://gitea.example:1234:   # 键是 Gitea 根地址（可配多个站点）
    token: ...                 # 站点访问令牌；缺省回退凭据库 purpose=review
    repos:                     # 监控的仓库清单（owner/name）
      - user/repo1
      - user/repo2
provider:
  type: minimax                # 目前只支持 minimax（国内站点用 minimax-cn）
  token: $MINIMAX_TOKEN        # API key；支持 $VAR / ${VAR} 环境变量引用
root: ${XDG_DATA_HOME:-$HOME/.local/share}/Cosmic-Developers-Union/assistant
repos-dir: $root/repos         # $root/repos/{instance-name}/{username-or-org}/{name}
review-root: /tmp              # 评审 worktree 根
review-name-template: "${instance-name}-{username-or-org}--{name}-{pr|issue}-{index}"
# Use jsonl 记录 claude 的 session 记录
sessions-dir: $root/sessions   # 会话原始 stream-json 归档根（可选；不配不归档）
sessions-name-template: "${instance-name}-{username-or-org}--{name}/{session-id}.jsonl"
# Use sqlite 作为状态记录和同步的模式, use sqlite3 WAL
state-dir: ${XDG_STATE_HOME:-$HOME/.local/state}/Cosmic-Developers-Union/assistant
state-file: $state-dir/state.sqlite3
```

- **monitor.<host>.token**：daemon 检测待办、克隆仓库与会话 MCP 都以它身份
  运行（评审以该账号落库）。缺省回退凭据库 purpose=review 的令牌（本地开发
  的 run.yaml 不必含敏感值）。
- **provider**（可选）：本次运行的 AI 供应商简写。`type` 选内置预设（目前只
  支持 minimax；国内站点用 minimax-cn），`token` 是该供应商的 API key——支持
  `$VAR` / `${VAR}` 环境变量引用，未定义展开为空、由预设与进程环境兜底。给出
  provider 节时覆盖 config.json 的 default_provider；端点、模型映射与官方 MCP
  仍由内置预设提供。省略时回退 config.json 的 default_provider。
- **sessions-dir / sessions-name-template**（可选）：claude 会话的原始
  stream-json 记录归档（每行原样落盘，daemon 重启不丢）。模板占位符
  `{instance-name}` `{username-or-org}` `{name}` `{session-id}`；未配置
  sessions-dir 时不归档。
- **state-dir / state-file**（可选）：SQLite WAL 共享状态库。daemon 把目标、
  队列、会话生命周期与结果持久化于此，任何工具（sqlite3 CLI、DBeaver）可直接
  只读内省；同一待办的**跨进程互斥**也由它保证——同一时刻一个 PR/Issue 至多
  一个 claude 会话，崩溃残留超时自动接管。`$state-dir` 可在 state-file 中自
  引用。两者都未配置时状态库不启用（互斥退化为进程内守卫）。

### 处理模式

- **倒序扫描**：待办按编号**降序**处理——最新请求的条目最先起会话。
- **单会话互斥**：同一 PR/Issue 同一时刻只允许一个 claude（sqlite 事务保证
  跨进程原子），重复触发只记日志不重开会话。
- **双通道守卫**：请求与 mention 是两类行为，去重守卫随之分离。请求类通道
  以 **settled** 守卫吸收同一请求的重复信号，请求出清后自动解除；mention 通道
  （@ai）以**水位线**记录最后回应时刻——mention 条目会持续留在 `mentioned_by`
  清单里，只有水位线之后出现他人新评论才重新触发，且以追问轮（同一会话续聊）
  回应，不重跑全量协议。条目关闭（离开 mention 清单）后水位线清除，重新 @ai
  视为全新请求。
  触发口径（新规范）：PR 评审走 `@ai` mention 或**原生 review 请求**
  （requested_reviewers，优先接口——评论中的 @提及 / /review 由 sync 以
  merge 身份转正为官方请求）；reviewer 在当前 head 上正式回应后请求出清，
  作者推进 head 后旧回应失效、自动重新排队。标签不再驱动任何工作：
  status/review、status/approved 等由 sync 继续维护，仅作观测产物；
  status/triage（Issue）保留人工打标触发分诊的兼容通道。automerge 的合并
  门禁 = 内容评审者对当前 head 的官方批准（approved、未 dismiss、
  CommitID==head）+ 未落后 + 可合并 + 必要检查全绿。
- **追问轮**（follow-up）：会话完成判定通过后（或 mention 通道检测到水位线
  之后的新消息时，由检测循环直接派发追问），dispatcher 检查条目上是否有
  assistant 账号之外的新评论；有则以**同一会话 --resume 续聊**，提示词要求
  ai 回复前先完整阅读全部新消息（至多 3 轮，防死循环），并在条目上留一条
  「正在读取后续消息」的系统评论。
- **stream 解析**：待办日志实时展示会话时间线——assistant 文本、工具调用带
  入参摘要（`🔧 Bash: git diff --stat` 形态）、工具回执（错误显式标出）、
  最终 result，读者无需打开原始记录即可判断当前步与状态。
- **root**：本次运行的数据根。状态目录（日志、单飞锁）落 `$root/state/`。
  支持 `${VAR}` / `${VAR:-default}` 环境变量展开与 `$root` 自引用；缺省为平台
  数据目录下的 `Cosmic-Developers-Union/assistant`，与受管克隆的既有落点一致。
- **repos-dir**：受管克隆根，缺省 `$root/repos`；布局
  `{instance-name}/{username-or-org}/{name}`（instance-name 是站点 slug）。
- **review-root**：评审工作区根，缺省系统临时目录。每个仓库在其下按
  **review-name-template**（去掉逐待办叶子后）建立目录，worktree 在该目录的
  `worktrees/` 下（`pr-N` / `issue-N`）。
- **review-name-template**：命名模板，占位符 `{instance-name}`
  （站点 slug）、`{username-or-org}`、`{name}`、`{pr|issue}`（pr 或 issue）、
  `{index}`（待办编号）。`{...}` 与 `${...}` 两种写法等价。模板保证站点与
  仓库隔离，同名仓库跨站点不会相撞。

示例仓库的完整样例见仓库根的 `run.example.yaml`。

## 目录布局（run.yaml 模式）

```
$root/
├── repos/{instance-name}/{owner}/{name}   # 受管克隆（基线检出）
└── state/{instance-name}/{owner}/{name}/  # 日志 logs/ 与单飞锁 dispatcher.lock
<review-root>/
└── <模板展开的仓库目录>/worktrees/         # pr-N / issue-N（会话工作区，用完即删）
```

## 会话身份

评审会话的 Gitea 身份由 dispatcher 显式注入（`GITEA_HOST` /
`GITEA_ACCESS_TOKEN` = monitor.token），不依赖会话环境；AI 凭据来自 provider
节（token，支持环境变量引用）、config.json 的 providers/optimizations 或环境
变量兜底。

## 迁移

从 config.json instances 迁移：把每个 `instances[].host` 挪到 `monitor` 键、
`repos[].name` 挪到 `monitor.repos` 即可。root 缺省值与受管克隆的既有落点
（`<数据目录>/Cosmic-Developers-Union/assistant`）相同，已有克隆、日志与状态
零搬迁。reviewer/merger 语义不变（完成判定仍匹配 reviewer 提交的新 review）。
