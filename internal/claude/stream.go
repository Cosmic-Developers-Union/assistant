package claude

import (
	"encoding/json"
	"strings"
)

// Event 是 stream-json 单行消息的松弛视图：未知字段忽略，缺省字段由消费端回退。
//
// 标量一律用指针：区分「字段未出现」与「零值」。这不只是严谨——result 帧缺
// is_error 时必须判为失败（见 Feed），若用值类型就分不出「没写」与「写了 false」。
type Event struct {
	Type      *string `json:"type"`
	Subtype   *string `json:"subtype"`
	SessionID *string `json:"session_id"`
	Model     *string `json:"model"`
	IsError   *bool   `json:"is_error"`
	NumTurns  *int    `json:"num_turns"`
	// TotalCostUSD 是 CLI 上报的累计花费（估计值，非账单）。
	TotalCostUSD *float64 `json:"total_cost_usd"`
	DurationMS   *int64   `json:"duration_ms"`
	Result       *string  `json:"result"`
	Errors       []string `json:"errors"`
	// PermissionDenials 是权限分类器拒绝的工具调用（只计数量，原文差异太大）。
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	Message           *Message          `json:"message"`
	// EstimatedTokens / EstimatedTokensDelta 是 thinking_tokens 帧的载荷
	// （词元数是 CLI 的估计值，非计费精确值；两字段二选一出现）。
	EstimatedTokens      *int64 `json:"estimated_tokens"`
	EstimatedTokensDelta *int64 `json:"estimated_tokens_delta"`

	// 以下字段来自 init（system/init）事件：会话实况。
	ClaudeCodeVersion *string  `json:"claude_code_version"`
	PermissionMode    *string  `json:"permissionMode"`
	APIKeySource      *string  `json:"apiKeySource"`
	Cwd               *string  `json:"cwd"`
	Tools             []string `json:"tools"`
	Skills            []any    `json:"skills"`
	Agents            []any    `json:"agents"`
	SlashCommands     []string `json:"slash_commands"`
	MCPServers        []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
	Error *struct {
		Message string `json:"message"`
		Status  int    `json:"status"`
	} `json:"error"`
}

// Message 是 assistant / user 事件的消息体。
type Message struct {
	Content []ContentBlock `json:"content"`
}

// ContentBlock 是 message.content 里的一个块：assistant 消息（text / thinking /
// tool_use）与 user 消息里的 tool_result 共用此视图。
type ContentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	ID       string          `json:"id"`
	Input    json.RawMessage `json:"input"`
	// tool_result 块
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   *bool           `json:"is_error"`
}

// IsResult 报告该事件是否终结整轮会话的 result 帧。
func (e Event) IsResult() bool {
	return e.Type != nil && *e.Type == "result"
}

// IsSystem 报告该事件是否为 system 帧（init / api_error / thinking_tokens 等）。
func (e Event) IsSystem(subtype string) bool {
	return e.Type != nil && *e.Type == "system" &&
		e.Subtype != nil && *e.Subtype == subtype
}

// Outcome 是一次会话的归集结果。
//
// 合并了原 dispatcher.SessionOutcome 与 daemon.chatOutcome 的公共面：两个包各自
// 维护一份近亲结构体，CLI 加一个字段就要改两处。字段的**归属**用注释标出，便于
// 消费端知道哪些与自己相关。
type Outcome struct {
	// Subtype 是 success 或 error_max_turns / error_during_execution 等。
	Subtype  string
	IsError  bool
	NumTurns int
	CostUSD  float64
	// DurationMS 是 CLI 上报的会话耗时。
	DurationMS int64
	// SessionID 是本次会话的稳定 ID（--session-id/--resume 用的那个）。
	SessionID string
	// ResolvedArchivedPath 仅供 dispatcher：原始 stream-json 行的归档文件。
	ArchivePath string
	// Resumed 为真表示本次是续接已存在的会话记录。
	Resumed bool
	// Result 是会话的最终文本结论（失败时为空）。
	Result string
	// Errors 是 result 帧上报的错误与本地归类的失败原因。
	Errors []string
	// PermissionDenials 是被权限分类器拒绝的工具调用数量。
	PermissionDenials int
	// SawResult 报告流中是否出现过 result 帧：没出现说明会话被中断（CLI 崩溃、
	// 被 kill），调用方据此判失败而不是把空结论当成功。
	SawResult bool

	// 以下为对话会话（daemon）额外关心：评审侧留零值即可。
	//
	// Model 是 init 事件上报的模型名。
	Model string
	// ThinkingTokens 是本轮模型思考的估算 token 数。
	ThinkingTokens int
	// MCPStatus 是 init 事件里各 MCP server 的连通状态（name → status）。
	MCPStatus map[string]string
	// APIError 是 claude 上报的 API 层错误（如 401 invalid api key）：排查
	// 「发消息没反应」最关键的一行。
	APIError string
}

// NewOutcome 返回一份已初始化集合的结果（避免 nil map/slice 在 JSON 与日志里
// 呈现成 null）。
func NewOutcome() Outcome {
	return Outcome{Errors: []string{}, MCPStatus: map[string]string{}}
}

// ParseLine 解析一行 stream-json。非 JSON、非对象、空行都返回 ok=false——
// CLI 会往 stdout 混入杂音（警告、进度条），不能因此中断整轮会话。
func ParseLine(line []byte) (Event, bool) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return Event{}, false
	}
	var event Event
	if err := json.Unmarshal([]byte(trimmed), &event); err != nil {
		return Event{}, false
	}
	return event, true
}

// Progress 是进度展示风格：两个调用方对「会话进度」的诉求不同，合并时保留两种
// 呈现而不是静默选一个。
type Progress int

const (
	// ProgressTerse 是评审会话（dispatcher）用的紧凑形态：一行一个动作
	// （`🔧 工具名: 摘要`、`  ↳ 工具回执`），落进待办日志便于事后通读。
	ProgressTerse Progress = iota
	// ProgressVerbose 是对话会话（daemon）用的展开形态：init 实况逐字段、
	// 工具入参按缩进 JSON 展开并封顶，供交互式排查。
	ProgressVerbose
)

// Feed 把一行折进 outcome，并把可读进度交给 onProgress；返回该行是否为 result 帧。
//
// style 决定进度的呈现形态（见 Progress）。onProgress 为 nil 时只归集、不播报。
func Feed(outcome *Outcome, line []byte, style Progress, onProgress func(string)) bool {
	event, ok := ParseLine(line)
	if !ok {
		return false
	}
	return FeedEvent(outcome, event, style, onProgress)
}

// FeedEvent 把已解析的事件折进 outcome（供已自行解析的调用方复用，避免重复解析）。
func FeedEvent(outcome *Outcome, event Event, style Progress, onProgress func(string)) bool {
	report := func(line string) {
		if onProgress != nil {
			onProgress(line)
		}
	}

	switch {
	case event.IsSystem("init"):
		applyInit(outcome, event)
		if style == ProgressVerbose {
			report(describeInit(event))
		} else if event.SessionID != nil {
			report("session=" + *event.SessionID + " model=" + pointerOr(event.Model, "?"))
		}
		// 未就绪的 MCP server 单独告警一行：init 里声明了却没连上，是「工具用不了」
		// 的最常见原因。两种形态都报——评审会话也常因此卡住，且这一行是排查起点，
		// 不该只在展开形态里出现。
		if broken := BrokenMCPServers(outcome); broken != "" {
			report("⚠ MCP 服务未就绪：" + broken +
				"（检查该 server 的命令是否可用，或在所属 provider 的 mcp 配置里给 null 关闭）")
		}
	case event.IsSystem("thinking_tokens"):
		feedThinkingTokens(outcome, event, onProgress)
	case event.IsSystem("api_error"):
		message := "（未提供原因）"
		status := 0
		if event.Error != nil {
			if trimmed := strings.TrimSpace(event.Error.Message); trimmed != "" {
				message = trimmed
			}
			status = event.Error.Status
		}
		if status > 0 {
			message = "HTTP " + itoa(status) + " " + message
		}
		outcome.APIError = message
		report("API 错误：" + message)
	case event.Type != nil && *event.Type == "assistant":
		outcome.NumTurns++
		if event.Message != nil {
			feedAssistant(outcome, *event.Message, style, report)
		}
	case event.Type != nil && *event.Type == "user":
		// user 事件承载 tool_result：把每个工具的回执折成可读行（错误显式标出）
		if event.Message != nil {
			feedToolResults(*event.Message, style, report)
		}
	case event.IsResult():
		applyResult(outcome, event)
		outcome.SawResult = true
		return true
	default:
		// CLI 在 --output-format json 下（或测试注入时）可能给单个结果对象：
		// 没有 type 但有 subtype/result 时按 result 处理。
		if (event.Type == nil || *event.Type == "") && (event.Subtype != nil || event.Result != nil) {
			applyResult(outcome, event)
			outcome.SawResult = true
			return true
		}
		if event.Type != nil && *event.Type != "" {
			line := "事件 " + *event.Type
			if event.Subtype != nil && *event.Subtype != "" {
				line += "/" + *event.Subtype
			}
			report(line)
		}
	}
	return false
}

// applyInit 归集 init 事件：会话 ID、模型与 MCP server 连通状态。
func applyInit(outcome *Outcome, event Event) {
	if outcome.SessionID == "" && event.SessionID != nil {
		outcome.SessionID = *event.SessionID
	}
	if event.Model != nil && *event.Model != "" {
		outcome.Model = *event.Model
	}
	if len(event.MCPServers) == 0 {
		return
	}
	if outcome.MCPStatus == nil {
		outcome.MCPStatus = make(map[string]string, len(event.MCPServers))
	}
	for _, server := range event.MCPServers {
		outcome.MCPStatus[server.Name] = server.Status
	}
}

// applyResult 归集 result 帧：会话结论与失败原因都在这里。
func applyResult(outcome *Outcome, event Event) {
	if event.Subtype != nil {
		outcome.Subtype = *event.Subtype
	}
	// is_error 缺省即失败：result 帧没写 is_error 时按失败处理更安全——把一次
	// 未明确成功的会话当成功，会让「完成判定」在错误结论上放行。
	if event.IsError != nil {
		outcome.IsError = *event.IsError
	} else {
		outcome.IsError = true
	}
	if event.NumTurns != nil {
		outcome.NumTurns = *event.NumTurns
	}
	outcome.CostUSD = 0
	if event.TotalCostUSD != nil {
		outcome.CostUSD = *event.TotalCostUSD
	}
	outcome.DurationMS = 0
	if event.DurationMS != nil {
		outcome.DurationMS = *event.DurationMS
	}
	if event.SessionID != nil {
		outcome.SessionID = *event.SessionID
	}
	outcome.Errors = append(outcome.Errors, event.Errors...)
	outcome.PermissionDenials = len(event.PermissionDenials)
	if !outcome.IsError && event.Result != nil {
		outcome.Result = *event.Result
	}
}

// feedAssistant 播报 assistant 消息里的文本与工具调用。
func feedAssistant(outcome *Outcome, message Message, style Progress, report func(string)) {
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			text := strings.TrimSpace(block.Text)
			if text == "" {
				continue
			}
			if style == ProgressVerbose {
				report("claude: " + singleLine(text))
			} else {
				report(text)
			}
		case "thinking":
			thinking := strings.TrimSpace(block.Thinking)
			if thinking == "" {
				continue
			}
			report("思考: " + truncateRunes(singleLine(thinking), 200))
		case "tool_use":
			if block.Name == "" {
				continue
			}
			if style == ProgressVerbose {
				report(ToolUseLines(block.Name, block.Input))
				continue
			}
			if detail := describeToolInput(block.Name, block.Input); detail != "" {
				report("🔧 " + block.Name + ": " + detail)
			} else {
				report("🔧 " + block.Name)
			}
		}
	}
}

// feedToolResults 播报 user 消息里的工具回执。
func feedToolResults(message Message, style Progress, report func(string)) {
	for _, block := range message.Content {
		if block.Type != "tool_result" {
			continue
		}
		if style == ProgressVerbose {
			body := toolResultText(block.Content)
			mark := ""
			if block.IsError != nil && *block.IsError {
				mark = "（错误）"
			}
			lines := []string{"↩ 工具结果" + mark + "（" + itoa(len([]rune(strings.TrimSpace(body)))) + " 字）"}
			for _, line := range textLines(body, toolResultLineLimit, toolLineWidth) {
				lines = append(lines, "  "+line)
			}
			report(strings.Join(lines, "\n"))
			continue
		}
		report("  ↳ " + DescribeToolResult(&block))
	}
}

// feedThinkingTokens 播报思考进度：thinking_tokens 每几十毫秒一条，按时间窗
// 节流，既能看到模型在动又不刷屏。
func feedThinkingTokens(outcome *Outcome, event Event, onProgress func(string)) {
	total := 0
	switch {
	case event.EstimatedTokens != nil:
		total = int(*event.EstimatedTokens)
		outcome.ThinkingTokens = total
	case event.EstimatedTokensDelta != nil:
		outcome.ThinkingTokens += int(*event.EstimatedTokensDelta)
		total = outcome.ThinkingTokens
	}
	if onProgress == nil {
		return
	}
	now := Now()
	state := thinkingState.get()
	if now.Sub(state.reportedAt) < ThinkingProgressInterval && total-state.reported < thinkingBurstDelta {
		return
	}
	thinkingState.set(thinkingReport{reportedAt: now, reported: total})
	onProgress("思考中…（约 " + itoa(total) + " 词元）")
}

// pointerOr 取指针值，缺失时用回退值。
func pointerOr(value *string, fallback string) string {
	if value != nil && *value != "" {
		return *value
	}
	return fallback
}

// singleLine 把多行文本折成单行（日志一条一行，换行会打乱时间线）。
func singleLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
