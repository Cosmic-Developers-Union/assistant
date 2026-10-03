# 独立 agent-runtime 工作区

本目录交给独立 agent 实现；当前只有接口契约，没有引擎实现。它是独立 Go module，
主线 assistant 不导入它，不将新依赖、配置或权限模型接入生产运行。

用户要求保留 TUI 用于调试。建议目录：

- contract/：唯一宿主接口，已经定义；修改先说明兼容性影响。
- engine/：模型循环、流式响应、工具调用与用量。
- provider/：Anthropic / OpenAI compatible 适配，不向其他层泄漏供应商事件。
- permission/：显式能力与单次确认；headless 无确认默认拒绝。
- environment/：文件边界与容器执行；不能把 cwd 当沙箱。
- session/：版本化 JSONL，恢复、回放、有限上下文；不另建内容数据库。
- tui/ 与 cmd/agent/：调试界面与入口，复用 contract.Runner，不另起一个引擎。
- adapter/：将来连接 assistant 的适配层，本轮不要实现或修改主线 runtime。

## 上游调研与取舍

参考 https://github.com/tunsuy/claude-code-go ，本次检查提交
`db88b726bec47ddacad895ac0ef1353d0fac5ef0`。它的 bootstrap 同时装配 engine、
permissions、MCP、hooks、协调器与状态等；核心已有独立 agtkeel 模块。直接导入
原 CLI 会带入 Bubble Tea、渲染器以及大量 CC 兼容行为。独立 agent 应进一步评估
提取/复用 agtkeel 的核心或在此裁剪，不要求重写全部 CC，也不要只包装另一个 CLI。
保留 TUI 是调试需求，可以复用上游 TUI，但 TUI 依赖必须留在本 module。

复用上游代码须保留 MIT 许可证与来源，固定 commit/version；不要跟随 latest。
允许 TUI、轻量库、headless 三种消费方式共用引擎。

## 接口语义与宿主适配

contract.Runner.Run 接受 Request、事件 Sink，返回 Result 和可识别的错误。
宿主完成工作区准备与记忆恢复；引擎只执行一轮；宿主负责上传副本、观测和 cleanup。
平台待办/批准/完成判定属于 assistant，运行时不得依赖 Gitea、QQ 或其他平台。

未来 adapter 将 assistant AgentSpec/Event 转成 Request，将流事件转成进度，将
Result 转成 Outcome。为新的运行时命名空间派生会话 ID，不能混读 Claude JSONL。
S3 只是宿主的 Session 实现，运行时不持有对象存储或平台密钥。
会话拒绝版本或后端不匹配时应清楚失败，不悄悄新建记忆。

TUI 接收有序 Event，支持流式文本、工具参数、许可决策、用量、停止/取消、失败原因、
会话回放。界面只发交互授权，不直接执行工具、不解析供应商流。
Sink 失败或取消后终止本轮；事件流可写时合法请求必须产生且仅产生一次 terminal，
不能再发工具事件。Sink 已失败时直接返回 Result/error，由宿主或 TUI 根据返回值收尾，
不向损坏的 Sink 强行发送 terminal。

## 权限与执行环境

chat 缺省无工具；support 只开放显式知识目录的读工具；work 的读/写/执行能力
分别授权。精确 allow、deny 优先、无权限默认拒绝，不从模型文本或工具输出读取授权。
读取/写入须使用抗 symlink 越界的文件系统根，限制文件大小，保护运行时记忆与密钥文件。

命令工具只能在显式 container 环境运行，不默认开放宿主 Bash。不挂宿主 HOME、
Docker socket 或模型/平台密钥；设定用户、CPU/内存/PID/输出/时间上限，默认无网络。
容器隔离与工具能力是两个独立约束。TUI 的批准只覆盖当前 CallID；headless 无
Approver 时不能自动批准。取消须停止并回收进程及容器，不能只停止等待。

## 交付和验收

独立 agent 首先提交复用范围与依赖评估，再实现最小聊天循环、TUI，再逐项加入
客服/work 能力。测试必须覆盖：

1. 同一请求在 TUI/headless 的工具决策与模型行为一致。
2. 缺省无工具；拒绝优先；越界、symlink、权限升级与无确认器都拒绝。
3. 模型工具轮、多工具结果、用量、异常帧、断流、超时与限额。
4. 稳定记忆、重启续接、版本不匹配、部分写入、敏感数据不进入日志/回放。
5. 事件顺序、只一次 terminal、Sink 失败/取消后没有工具副作用。
6. 容器清理、无网络/密钥/宿主挂载；TUI 取消与非 TTY 错误行为。

命令在此目录单独运行：`go test ./...`、`go test -race ./...`、`go vet ./...`。
core ≥90% 的有价值覆盖，TUI 至少契约测试与可复现演示。当前主线只检查契约编译，
不把尚未实现的引擎声称成可运行功能。
