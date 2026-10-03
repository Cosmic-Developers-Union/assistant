# 按 goal.md 重构的架构

产品只有一个二进制和四个操作面。CLI 负责输入和装配，业务规则分别留在
runtime 与 status；平台 SDK、消息协议、Claude 子进程通过窄接口接入。

| 操作面 | 输入 | 职责 | 可写数据 |
|---|---|---|---|
| instance | 交互/平台登录参数 | 验证接入、保存个人连接 | 用户级 credentials.json |
| mcp gitea | 显式参数、环境、Git remote、个人连接 | 解析站点与访问身份、启动工具 | 平台工具授权的操作 |
| action | 三项 GITEA 环境变量 | 单仓库标签收敛与实时合并门禁 | 显式仓库 |
| run | config.yaml 与环境 | 发现待办、运行 bot、收尾 | 平台操作及运行 root |
| mcp sessions | config.yaml | 查询执行元数据 | 无 |

删除原 dispatcher/daemon 双调度、providers/channels/runtimes 间接配置、独立
assistantd、内容数据库和脚手架安装命令。保留已有 Gitea 状态机、消息协议与
Claude 流解析实现，避免把成熟的协议适配重新写一遍。

## 生命周期

runtime.Config 严格解析 YAML 并验证连接、bot 与 MCP 引用。Start 装配 Source
和 Worker；Engine 只按接口调用，不解析平台凭据，不决定存储类型。

每个事件依次经历：稳定 ID → 恢复本地记忆 → 准备工作区 → agent 执行 →
固化副本 → 登记元数据 → 清理工作区。Session.Dir 与 Sync 在 goal.md 的示意
接口上增加 context 和错误返回，让取消、权限不足和损坏副本可以显式失败。

调度只保存运行中的 pending；没有 settled、缓存批准或本地完成标记。
下一轮平台仍有待办就重试，同一 ID 自动续接；PR head 改变则新开会话。
轮询采用每轮等待任务收尾的 barrier，行为容易观察和证明；代价是长任务延迟
下一轮发现。消息接收独立运行，同用户串行，不同用户并行。

共享克隆的 Git 操作串行，任务 worktree 独立。PR ref 必须仍指向发现时的 head，
否则本轮失败并等待重读；基线 .claude/.assistant 替换 PR 的规则。工作区退出后清理，聊天
用户目录保留。整个 root 使用系统文件锁，禁止两个运行进程竞争同一份记忆。

LocalSession 直接使用 Claude JSONL。S3Session 只上传 projects 子树，包括子代理；
恢复在取用前完成，上传发生在失败或取消之后。远端请求失败不能退回新记忆。
恢复在暂存目录校验路径、类型、大小与 gzip 校验和后替换；桶由操作者预建。

## 动作与观测

action 不做仓库发现、不借用个人登录。dry-run 使用同一个判定流程，API 装饰器
截断所有写方法。automerge 只接受最新、未撤销、显式绑定当前 head 的内容批准，
会签后 squash；合并响应未知时停止本轮，读取单个候选失败则继续并聚合错误。

observations 只记录事件、ID、时间、结局和开销，不存用户消息或模型回复。
状态 API 使用临时 Bearer token，sessions MCP 只读这些元数据；两者均不影响入队。
配置和外部错误中的已知密钥不进入日志，遮蔽后的错误仍保留原始错误链。

## 验证与边界

契约测试覆盖恢复/执行/上传/观测/清理顺序，失败与取消、并发去重、重试、会话锚点、
配置隔离、Git worktree 基线、stdio MCP、只读 API，以及动作的写拦截和当前 head 门禁。
MinIO e2e 使用独立临时桶验证换本地根后的主会话/子代理恢复。
Lean 证明调度守卫与平台待办语义；TLC 检查状态机和当前 head 批准的合并不变量。

新旧配置与会话落点有意不自动转换，迁移步骤见 [config.md](config.md#旧版迁移)。
真实 Claude 模型调用和生产 Gitea 写操作仍须在操作者准备的测试站点验收；本地验证
不触碰生产账号、仓库或服务。系统故障与单测边界见 [coverage-gaps.md](coverage-gaps.md)。
