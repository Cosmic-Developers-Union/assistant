package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"assistant/internal/claudecfg"
)

// chatOutcome 是对话会话一轮的归集结果：从 claude 的 stream-json 里累积出来。
// 与评审会话不同，对话会话只需要「最终回复文本 + 出错原因（尤其是 API 报错）」。
type chatOutcome struct {
	Result    string
	SessionID string
	Model     string
	IsError   bool
	Subtype   string
	CostUSD   float64
	NumTurns  int
	Errors    []string
	// ThinkingTokens 是本轮模型思考的估算 token 数（thinking_tokens 事件）
	ThinkingTokens int
	// thinkingReportedAt / thinkingReported 是思考进度的播报节流状态
	thinkingReportedAt time.Time
	thinkingReported   int
	// MCPStatus 是 init 事件里各 MCP server 的连通状态（name → status）
	MCPStatus map[string]string
	// APIError 是 claude 上报的 API 层错误（如 401 invalid api key）：这是排查
	// 「发消息没反应」最关键的一行，必须能透到日志与回复里。
	APIError string
}

// chatStreamEvent 是 stream-json 单条消息的松弛视图（未知字段忽略）。
type chatStreamEvent struct {
	Type                 string   `json:"type"`
	Subtype              string   `json:"subtype"`
	SessionID            string   `json:"session_id"`
	Model                string   `json:"model"`
	IsError              bool     `json:"is_error"`
	Result               string   `json:"result"`
	NumTurns             int      `json:"num_turns"`
	CostUSD              float64  `json:"total_cost_usd"`
	Errors               []string `json:"errors"`
	EstimatedTokens      int      `json:"estimated_tokens"`
	EstimatedTokensDelta int      `json:"estimated_tokens_delta"`
	// init 事件携带的会话实况：版本、认证来源、权限模式、工具面、MCP 状态
	ClaudeCodeVersion string   `json:"claude_code_version"`
	PermissionMode    string   `json:"permissionMode"`
	APIKeySource      string   `json:"apiKeySource"`
	Cwd               string   `json:"cwd"`
	Tools             []string `json:"tools"`
	MCPServers        []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
	Skills        []any    `json:"skills"`
	Agents        []any    `json:"agents"`
	SlashCommands []string `json:"slash_commands"`
	Error         struct {
		Message string `json:"message"`
		Status  int    `json:"status"`
	} `json:"error"`
	Message struct {
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
			Content  json.RawMessage `json:"content"`
			IsError  bool            `json:"is_error"`
		} `json:"content"`
	} `json:"message"`
}

// 事件展示的分行上限：日志一条一行，长结构按缩进 JSON 展开但要封顶。
const (
	toolInputLineLimit  = 12
	toolResultLineLimit = 8
	toolLineWidth       = 200
)

// chatClock / thinkingProgressInterval 控制「思考中」进度的播报节流：thinking_tokens
// 事件每几十毫秒一条，按时间窗播报才既能看到模型在动又不刷屏（测试可替换时钟）。
var (
	chatClock                = time.Now
	thinkingProgressInterval = 2 * time.Second
)

// feedChatStreamLine 解析一行 stream-json，累积进 outcome，并通过 onProgress 输出
// **可读**的实时进度（会话实况、思考、assistant 文本、工具调用与结果、API 错误）。
// 不打印原始 JSON：未知事件只报 type/subtype，需要原文时看文本记录。
func feedChatStreamLine(outcome *chatOutcome, line []byte, onProgress func(string)) bool {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var event chatStreamEvent
	if err := json.Unmarshal(trimmed, &event); err != nil {
		// 单行坏 JSON 直接忽略，不影响整轮会话
		return false
	}
	if event.SessionID != "" && outcome.SessionID == "" {
		outcome.SessionID = event.SessionID
	}
	switch event.Type {
	case "system":
		switch event.Subtype {
		case "init":
			applyChatInit(outcome, event)
			progress(onProgress, describeChatInit(event))
			if broken := brokenMCPServers(outcome); broken != "" {
				progress(onProgress, "⚠ MCP 服务未就绪："+broken+"（检查该 server 的命令是否可用，或在该 provider 的 mcp 里给 null 关闭）")
			}
		case "api_error":
			message := strings.TrimSpace(event.Error.Message)
			if message == "" {
				message = "（未提供原因）"
			}
			if event.Error.Status > 0 {
				message = fmt.Sprintf("HTTP %d %s", event.Error.Status, message)
			}
			outcome.APIError = message
			progress(onProgress, "API 错误："+message)
		case "thinking_tokens":
			feedThinkingTokens(outcome, event, onProgress)
		default:
			if event.Subtype != "" {
				progress(onProgress, "事件 system/"+event.Subtype)
			}
		}
	case "assistant":
		for _, block := range event.Message.Content {
			switch block.Type {
			case "text":
				if text := strings.TrimSpace(block.Text); text != "" {
					progress(onProgress, "claude: "+singleLine(text))
				}
			case "thinking":
				if thinking := strings.TrimSpace(block.Thinking); thinking != "" {
					progress(onProgress, "思考: "+truncate(singleLine(thinking), 200))
				}
			case "tool_use":
				progress(onProgress, toolUseLines(block.Name, block.Input))
			}
		}
	case "user":
		for _, block := range event.Message.Content {
			if block.Type != "tool_result" {
				continue
			}
			body := toolResultText(block.Content)
			mark := ""
			if block.IsError {
				mark = "（错误）"
			}
			lines := []string{fmt.Sprintf("↩ 工具结果%s（%d 字）", mark, len([]rune(strings.TrimSpace(body))))}
			for _, line := range claudecfg.TextLines(body, toolResultLineLimit, toolLineWidth) {
				lines = append(lines, "  "+line)
			}
			progress(onProgress, strings.Join(lines, "\n"))
		}
	case "result":
		applyChatResult(outcome, event)
		return true
	default:
		// 兼容 --output-format json 的单对象输出（测试注入与旧格式）
		if event.Type == "" && (event.Subtype != "" || event.Result != "") {
			applyChatResult(outcome, event)
			return true
		}
		if event.Type != "" {
			line := "事件 " + event.Type
			if event.Subtype != "" {
				line += "/" + event.Subtype
			}
			progress(onProgress, line)
		}
	}
	return false
}

// applyChatInit 归集 init 事件：MCP server 状态用于「声明的 vs 实际连上的」对照。
func applyChatInit(outcome *chatOutcome, event chatStreamEvent) {
	if event.Model != "" {
		outcome.Model = event.Model
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

// describeChatInit 把 init 事件整理成**逐行**会话实况：每行一个字段，日志里不出现
// 长行（版本/认证/权限/工作目录/工具面/MCP 连通状态）。
func describeChatInit(event chatStreamEvent) string {
	lines := []string{"会话已启动"}
	add := func(label, value string) {
		if strings.TrimSpace(value) != "" {
			lines = append(lines, "  "+label+" = "+value)
		}
	}
	add("model", event.Model)
	add("claude", event.ClaudeCodeVersion)
	add("认证", event.APIKeySource)
	add("权限", event.PermissionMode)
	add("cwd", event.Cwd)
	if len(event.Tools) > 0 {
		mcpTools := 0
		for _, tool := range event.Tools {
			if strings.HasPrefix(tool, "mcp__") {
				mcpTools++
			}
		}
		add("工具", fmt.Sprintf("%d 个（内建 %d，MCP %d）", len(event.Tools), len(event.Tools)-mcpTools, mcpTools))
	}
	if len(event.Skills) > 0 {
		add("skills", fmt.Sprintf("%d 个", len(event.Skills)))
	}
	for _, server := range event.MCPServers {
		add("MCP."+server.Name, server.Status)
	}
	return strings.Join(lines, "\n")
}

// brokenMCPServers 汇总未连上的 MCP server（init 事件里的实际状态）。
func brokenMCPServers(outcome *chatOutcome) string {
	broken := make([]string, 0, len(outcome.MCPStatus))
	for name, status := range outcome.MCPStatus {
		if status != "" && status != "connected" {
			broken = append(broken, name+"="+status)
		}
	}
	if len(broken) == 0 {
		return ""
	}
	sort.Strings(broken)
	return strings.Join(broken, " ")
}

// toolUseLines 渲染一次工具调用：名称一行 + 入参的**缩进 JSON 多行**（密钥字段
// 打码、行数受限）。JSON 原样分行展示，比压成一条长行可读得多。
func toolUseLines(name string, raw json.RawMessage) string {
	lines := []string{"🔧 " + name}
	if len(bytes.TrimSpace(raw)) == 0 {
		return lines[0]
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		lines = append(lines, "  "+truncate(singleLine(string(raw)), toolLineWidth))
		return strings.Join(lines, "\n")
	}
	for _, line := range claudecfg.JSONLines(value, toolInputLineLimit) {
		lines = append(lines, "  "+line)
	}
	return strings.Join(lines, "\n")
}

// toolResultText 从 tool_result 的 content 里取文本（可能是字符串或内容块数组）。
func toolResultText(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, " ")
	}
	return string(raw)
}

// feedThinkingTokens 解析 thinking_tokens 事件（estimated_tokens/…_delta，键名是
// session_id）：按时间窗播报「思考中」，既不漏掉进度也不刷屏。
func feedThinkingTokens(outcome *chatOutcome, event chatStreamEvent, onProgress func(string)) {
	if event.EstimatedTokens <= 0 {
		return
	}
	if event.EstimatedTokens < outcome.ThinkingTokens {
		// 新一轮思考（计数重置）：重新开始播报
		outcome.thinkingReported = 0
		outcome.thinkingReportedAt = time.Time{}
	}
	outcome.ThinkingTokens = event.EstimatedTokens
	now := chatClock()
	if !outcome.thinkingReportedAt.IsZero() && now.Sub(outcome.thinkingReportedAt) < thinkingProgressInterval {
		return
	}
	outcome.thinkingReportedAt = now
	outcome.thinkingReported = event.EstimatedTokens
	if event.EstimatedTokensDelta > 0 {
		progress(onProgress, fmt.Sprintf("思考中…（约 %d tokens，+%d）", event.EstimatedTokens, event.EstimatedTokensDelta))
		return
	}
	progress(onProgress, fmt.Sprintf("思考中…（约 %d tokens）", event.EstimatedTokens))
}

// applyChatResult 把 result 事件归集进 outcome。
func applyChatResult(outcome *chatOutcome, event chatStreamEvent) {
	outcome.Subtype = event.Subtype
	outcome.IsError = event.IsError
	outcome.Result = event.Result
	outcome.NumTurns = event.NumTurns
	outcome.CostUSD = event.CostUSD
	if event.SessionID != "" {
		outcome.SessionID = event.SessionID
	}
	if len(event.Errors) > 0 {
		outcome.Errors = append(outcome.Errors, event.Errors...)
	}
	if event.Error.Message != "" && outcome.APIError == "" {
		outcome.APIError = event.Error.Message
	}
}

// parseChatStream 折叠整段 stream-json 输出（供一次性拿到 stdout 的调用方/测试用）。
func parseChatStream(output []byte, onProgress func(string)) (chatOutcome, error) {
	var outcome chatOutcome
	found := false
	for _, line := range bytes.Split(output, []byte("\n")) {
		if feedChatStreamLine(&outcome, line, onProgress) {
			found = true
		}
	}
	if !found {
		return outcome, fmt.Errorf("会话输出里没有 result 事件（前 200 字节：%s）", truncate(string(output), 200))
	}
	return outcome, nil
}

// FailureMessage 汇总失败原因：优先 API 层错误（401/超时等），其次是 result 的
// errors/subtype。这样「没反应」的回复能直接说清是认证失败还是模型拒绝。
func (o chatOutcome) FailureMessage() string {
	if message := strings.TrimSpace(o.APIError); message != "" {
		return "模型端点报错：" + message
	}
	if len(o.Errors) > 0 {
		return strings.Join(o.Errors, "；")
	}
	if o.Subtype != "" {
		return "会话执行失败（" + o.Subtype + "）"
	}
	return "会话执行失败"
}

func progress(onProgress func(string), line string) {
	if onProgress != nil && line != "" {
		onProgress(line)
	}
}

// singleLine 把多行文本压成一行（日志一行一条，便于 grep）。
func singleLine(text string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", " ")), " ")
}
