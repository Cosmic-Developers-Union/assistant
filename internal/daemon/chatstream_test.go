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
