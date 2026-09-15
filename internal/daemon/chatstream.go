package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
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
	// thinkingReported 是上次播报的思考 token 步进（按 thinkingProgressStep 报）
	thinkingReported int
	// APIError 是 claude 上报的 API 层错误（如 401 invalid api key）：这是排查
	// 「发消息没反应」最关键的一行，必须能透到日志与回复里。
	APIError string
}

// chatStreamEvent 是 stream-json 单条消息的松弛视图（未知字段忽略）。
type chatStreamEvent struct {
	Type      string   `json:"type"`
	Subtype   string   `json:"subtype"`
	SessionID string   `json:"session_id"`
	Model     string   `json:"model"`
	IsError   bool     `json:"is_error"`
	Result    string   `json:"result"`
	NumTurns  int      `json:"num_turns"`
	CostUSD   float64  `json:"total_cost_usd"`
	Errors    []string `json:"errors"`
	// thinking_tokens 事件：模型思考的估算 token 数（高频，按步长播报）
	EstimatedTokens      int `json:"estimated_tokens"`
	EstimatedTokensDelta int `json:"estimated_tokens_delta"`
	Error                struct {
		Message string `json:"message"`
		Status  int    `json:"status"`
	} `json:"error"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
}

// feedChatStreamLine 解析一行 stream-json，累积进 outcome，并通过 onProgress 输出
// 可读的实时进度（assistant 文本、工具调用、API 错误）。返回是否为 result 行。
func feedChatStreamLine(outcome *chatOutcome, line []byte, onProgress func(string)) bool {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var event chatStreamEvent
	if err := json.Unmarshal(trimmed, &event); err != nil {
		// 单行坏 JSON 直接忽略（--debug 下原始行照旧落日志），不影响整轮会话
		return false
	}
	if event.SessionID != "" && outcome.SessionID == "" {
		outcome.SessionID = event.SessionID
	}
	switch event.Type {
	case "system":
		switch event.Subtype {
		case "init":
			if event.SessionID != "" {
				outcome.SessionID = event.SessionID
			}
			if event.Model != "" {
				outcome.Model = event.Model
			}
			progress(onProgress, fmt.Sprintf("claude 会话已启动（model=%s id=%s）", event.Model, event.SessionID))
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
			// 其余 system 事件只进 --debug 的原始事件行
		}
	case "assistant":
		for _, block := range event.Message.Content {
			switch block.Type {
			case "text":
				if text := strings.TrimSpace(block.Text); text != "" {
					progress(onProgress, "claude: "+singleLine(text))
				}
			case "tool_use":
				progress(onProgress, "🔧 "+block.Name)
			}
		}
	case "user":
		// 工具结果回灌：只在调试时才有价值，交给调用方按行处理
	case "result":
		applyChatResult(outcome, event)
		return true
	default:
		// 兼容 --output-format json 的单对象输出（测试注入与旧格式）
		if event.Type == "" && (event.Subtype != "" || event.Result != "") {
			applyChatResult(outcome, event)
			return true
		}
	}
	return false
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

// thinkingProgressStep 是「思考中」进度的播报步长：thinking_tokens 事件每几百毫秒
// 就来一条，按 1000 token 报一次（外加第一次），既能看到模型在动，又不刷屏。
const thinkingProgressStep = 1000

// feedThinkingTokens 解析 thinking_tokens 事件（estimated_tokens/…_delta，键名是
// session_id）：累积估算值并按步长播报。坏行/缺字段时静默忽略。
func feedThinkingTokens(outcome *chatOutcome, event chatStreamEvent, onProgress func(string)) {
	if event.EstimatedTokens <= 0 {
		return
	}
	if event.EstimatedTokens < outcome.ThinkingTokens {
		// 新一轮思考（计数重置）：重新开始播报
		outcome.thinkingReported = 0
	}
	outcome.ThinkingTokens = event.EstimatedTokens
	switch {
	case outcome.thinkingReported == 0:
		progress(onProgress, thinkingLine(event))
		outcome.thinkingReported = event.EstimatedTokens
	case event.EstimatedTokens-outcome.thinkingReported >= thinkingProgressStep:
		progress(onProgress, thinkingLine(event))
		outcome.thinkingReported = event.EstimatedTokens
	}
}

func thinkingLine(event chatStreamEvent) string {
	if event.EstimatedTokensDelta > 0 {
		return fmt.Sprintf("思考中…（约 %d tokens，+%d）", event.EstimatedTokens, event.EstimatedTokensDelta)
	}
	return fmt.Sprintf("思考中…（约 %d tokens）", event.EstimatedTokens)
}
