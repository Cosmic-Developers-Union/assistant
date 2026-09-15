package daemon

import (
	"strings"
	"testing"
)

// stream-json 逐行解析：assistant 文本、工具调用、API 错误都实时产出进度行，最终
// 归集出回复文本；API 报错要能透出来（这是「发消息没反应」最关键的线索）。
func TestFeedChatStreamLineProgressAndResult(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s-1","model":"MiniMax-M3"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"我来查一下\n队列状态"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__daemon__daemon_status"}]}}`,
		`{"type":"system","subtype":"api_error","error":{"status":401,"message":"invalid api key"}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"有 3 个待办","num_turns":4,"total_cost_usd":0.01,"session_id":"s-1"}`,
	}
	var progress []string
	var outcome chatOutcome
	found := false
	for _, line := range lines {
		if feedChatStreamLine(&outcome, []byte(line), func(text string) { progress = append(progress, text) }) {
			found = true
		}
	}
	if !found {
		t.Fatal("没有识别出 result 事件")
	}
	joined := strings.Join(progress, "\n")
	for _, want := range []string{
		"claude 会话已启动（model=MiniMax-M3 id=s-1）",
		"claude: 我来查一下 队列状态",
		"🔧 mcp__daemon__daemon_status",
		"API 错误：HTTP 401 invalid api key",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("进度缺少 %q：\n%s", want, joined)
		}
	}
	if outcome.Result != "有 3 个待办" || outcome.SessionID != "s-1" || outcome.NumTurns != 4 {
		t.Errorf("outcome = %+v", outcome)
	}
	if outcome.APIError != "HTTP 401 invalid api key" {
		t.Errorf("APIError = %q", outcome.APIError)
	}
}

// 失败的会话：结果里没有文本时，回复要说清是 API 报错而不是含糊的 subtype。
func TestChatOutcomeFailureMessagePrefersAPIError(t *testing.T) {
	outcome := chatOutcome{Subtype: "error_during_execution", IsError: true}
	outcome.APIError = "HTTP 401 invalid api key"
	if got := outcome.FailureMessage(); !strings.Contains(got, "401") {
		t.Errorf("FailureMessage = %q", got)
	}
	empty := chatOutcome{Subtype: "error_during_execution"}
	if got := empty.FailureMessage(); !strings.Contains(got, "error_during_execution") {
		t.Errorf("FailureMessage = %q", got)
	}
}

// 老格式（--output-format json 的单对象）仍能被折叠出来。
func TestParseChatStreamSingleObject(t *testing.T) {
	outcome, err := parseChatStream([]byte(`{"subtype":"success","is_error":false,"result":"好的"}`), nil)
	if err != nil {
		t.Fatalf("parseChatStream: %v", err)
	}
	if outcome.Result != "好的" {
		t.Errorf("outcome = %+v", outcome)
	}
	if _, err := parseChatStream([]byte("not json"), nil); err == nil {
		t.Error("没有 result 时应报错")
	}
}

// thinking_tokens：字段名是 estimated_tokens/estimated_tokens_delta（session_id 为
// 会话 id），按步长播报且首条即报；计数重置时重新开始播报；坏行（例如缺冒号的
// JSON）静默忽略，不影响整轮会话。
func TestFeedChatStreamThinkingTokens(t *testing.T) {
	var progress []string
	var outcome chatOutcome
	report := func(text string) { progress = append(progress, text) }
	lines := []string{
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":109,"estimated_tokens_delta":27,"session_id":"7931f2be-66b6-4070-a86e-01e60124e3cc"}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":400,"estimated_tokens_delta":291}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":1200,"estimated_tokens_delta":800}`,
		`{"type":"system","subtype":"thinking_tokens","session:"7931f2be-"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"好了"}`,
	}
	for _, line := range lines {
		feedChatStreamLine(&outcome, []byte(line), report)
	}
	joined := strings.Join(progress, "\n")
	if !strings.Contains(joined, "思考中…（约 109 tokens，+27）") {
		t.Errorf("首条 thinking_tokens 应播报：%s", joined)
	}
	if strings.Contains(joined, "约 400 tokens") {
		t.Errorf("未到步长不该重复播报：%s", joined)
	}
	if !strings.Contains(joined, "思考中…（约 1200 tokens，+800）") {
		t.Errorf("跨过步长应播报：%s", joined)
	}
	if outcome.ThinkingTokens != 1200 {
		t.Errorf("ThinkingTokens = %d", outcome.ThinkingTokens)
	}
	if outcome.SessionID != "7931f2be-66b6-4070-a86e-01e60124e3cc" {
		t.Errorf("应从 thinking 事件取到 session id：%q", outcome.SessionID)
	}
	if outcome.Result != "好了" {
		t.Errorf("坏行不该影响后续解析：%+v", outcome)
	}
}
