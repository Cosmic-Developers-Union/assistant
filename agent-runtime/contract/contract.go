// Package contract 定义独立聊天运行时与宿主/TUI 之间的边界。
// 不依赖 assistant 内部包，不包含引擎、HTTP、TUI 或平台实现。
package contract

import (
	"context"
	json "encoding/json/jsontext"
	"time"
)

// Version 是协议版本，宿主与运行时须在启动前核验，不做隐式降级。
const Version = 1

// Runner 执行一轮 agent；TUI 与未来 assistant 适配器使用同一接口。
// Run 正常返回只代表本轮结束，不代表平台任务完成。
type Runner interface {
	Run(context.Context, Request, Sink) (Result, error)
}

// Request 不含平台连接；宿主负责准备工作区、恢复会话和注入选定工具。
type Request struct {
	Version     int         `json:"version"`
	ID          string      `json:"id"`
	Input       string      `json:"input"`
	System      string      `json:"system,omitempty"`
	Workspace   string      `json:"workspace"`
	Session     Session     `json:"session"`
	Model       Model       `json:"model"`
	Policy      Policy      `json:"policy"`
	Environment Environment `json:"environment"`
	Limits      Limits      `json:"limits"`
	Tools       []ToolSpec  `json:"tools,omitempty"`
	Approver    Approver    `json:"-"`
}

// Session 的目录由宿主隔离，运行时只写自己的版本化 JSONL。
// 宿主在 Run 前恢复、Run 后固化；运行时不访问 S3、不控制平台调度。
type Session struct {
	ID, Dir string
	Resume  bool
}

// Model 显式模型接入，不探测用户登录或 Claude 私有配置。
// APIKey 只在内存中出现，不写入 JSONL、日志、事件或 TUI 回放。
type Model struct {
	Protocol, Endpoint, Name string
	APIKey                   string `json:"-"`
}

// Limits 所有限额必须有边界；0 表示采用实现公开的默认值，不表示无限。
type Limits struct {
	Turns, OutputTokens, ContextTokens, ToolOutputBytes int
	Timeout                                             time.Duration
}

// ToolSpec 定义可见工具，不代表已获授权；每次执行仍须检查 Policy。
// Execute 可由 MCP 桥提供，协议层不依赖任何 MCP 客户端。
type ToolSpec struct {
	Name, Description string
	Schema            json.Value
	Execute           func(context.Context, json.Value) (ToolResult, error) `json:"-"`
}

// ToolResult 将工具结果与错误标记交回模型，不允许以输出内容扩权。
type ToolResult struct {
	Text    string
	IsError bool
}

// Policy 使用显式能力，无通配默认放行，deny 优先于 allow。
// 工作目录本身不是沙箱；执行环境需要 Environment 单独约束。
type Policy struct {
	Profile               string   // chat / support / work
	Allow                 []string // 精确工具名
	Deny                  []string
	ReadRoots, WriteRoots []string
	NetworkHosts          []string
	Interactive           bool
}

// Environment 区分进程内文件操作与容器执行；不得默认为宿主 shell。
// container 只挂载宿主准备的工作区，不传平台/模型密钥、Docker socket 或用户 HOME。
type Environment struct {
	Mode        string // files-only / container
	Image       string
	User        string
	Network     bool
	MemoryBytes int64
	Processes   int
}

// Approver 由 TUI 注入交互确认；无确认器的 headless 请求遇到 ask 必须拒绝。
// 授权只针对该次调用，不自动持久化或升级整轮权限。
type Approver interface {
	Decide(context.Context, PermissionRequest) (Decision, error)
}

// PermissionRequest 保留结构化工具参数与政策原因，TUI 负责遮蔽敏感内容。
type PermissionRequest struct {
	CallID, Tool, Reason string
	Arguments            json.Value
}

// Decision 是本次工具调用的结论。
type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
)

// Sink 按序接收事件；返回错误即取消本轮，不继续产生工具副作用。
type Sink func(Event) error

// Event 面向可回放的 TUI 与宿主日志；可写事件流只有一个 terminal，位于最后。
// Sink 已失败时宿主以 Run 的返回值收尾，不承诺向损坏的 Sink 再投递事件。
// Seq 单调递增，ToolStart 必须先有同 CallID 的许可决策。
type Event struct {
	Version      int
	RunID        string
	Seq          uint64
	Kind         EventKind
	Text         string
	CallID, Tool string
	Arguments    json.Value
	Decision     Decision
	Usage        Usage
	Result       *Result
}

// EventKind 保持界面与引擎解耦，TUI 不解析供应商私有流格式。
type EventKind string

const (
	Started      EventKind = "started"
	TextDelta    EventKind = "text_delta"
	Permission   EventKind = "permission"
	ToolStart    EventKind = "tool_start"
	ToolEnd      EventKind = "tool_end"
	Warning      EventKind = "warning"
	UsageUpdated EventKind = "usage"
	Terminal     EventKind = "terminal"
)

// Result 包含用户可见结局，不携带平台完成标记。
type Result struct {
	RunID, SessionID, Text string
	Reason                 StopReason
	Resumed                bool
	Usage                  Usage
}

// StopReason 区分自然结束与限额/拒绝/取消；工具错误不伪装成成功。
type StopReason string

const (
	Completed StopReason = "completed"
	Limited   StopReason = "limited"
	Rejected  StopReason = "rejected"
	Canceled  StopReason = "canceled"
	Failed    StopReason = "failed"
)

// Usage 记录本轮用量；未知金额不得填成看似精确的成本。
type Usage struct {
	InputTokens, OutputTokens int64
	CostUSD                   *float64
}
