package daemon

import (
	"strings"
	"testing"
	"time"
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
		"会话已启动（model=MiniMax-M3）",
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

// thinking_tokens：字段名是 estimated_tokens / estimated_tokens_delta（会话 id 键是
// session_id），按时间窗播报（默认 2s）避免刷屏；坏行静默忽略，不影响整轮会话。
func TestFeedChatStreamThinkingTokens(t *testing.T) {
	now := time.Unix(0, 0)
	chatClock = func() time.Time { return now }
	thinkingProgressInterval = 2 * time.Second
	t.Cleanup(func() { chatClock = time.Now })

	var progress []string
	var outcome chatOutcome
	report := func(text string) { progress = append(progress, text) }
	lines := []string{
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":89,"estimated_tokens_delta":2,"session_id":"7931f2be-66b6-4070-a86e-01e60124e3cc"}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":110,"estimated_tokens_delta":21}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":180,"estimated_tokens_delta":70}`,
	}
	for _, line := range lines {
		feedChatStreamLine(&outcome, []byte(line), report)
	}
	now = now.Add(2100 * time.Millisecond)
	feedChatStreamLine(&outcome, []byte(`{"type":"system","subtype":"thinking_tokens","estimated_tokens":420,"estimated_tokens_delta":240}`), report)
	// 坏 JSON（缺冒号）与未知事件：忽略/只报 type，不 panic
	feedChatStreamLine(&outcome, []byte(`{"type":"system","subtype":"thinking_tokens","session:"7931f2be-"}`), report)
	feedChatStreamLine(&outcome, []byte(`{"type":"system","subtype":"compact_boundary","session_id":"s"}`), report)
	feedChatStreamLine(&outcome, []byte(`{"type":"result","subtype":"success","is_error":false,"result":"好了"}`), report)

	joined := strings.Join(progress, "\n")
	if !strings.Contains(joined, "思考中…（约 89 tokens，+2）") {
		t.Errorf("首条 thinking_tokens 应播报：%s", joined)
	}
	if strings.Contains(joined, "约 110 tokens") || strings.Contains(joined, "约 180 tokens") {
		t.Errorf("时间窗内不该重复播报：%s", joined)
	}
	if !strings.Contains(joined, "思考中…（约 420 tokens，+240）") {
		t.Errorf("过了时间窗应再报：%s", joined)
	}
	if !strings.Contains(joined, "事件 system/compact_boundary") {
		t.Errorf("未知事件只报 type/subtype：%s", joined)
	}
	if strings.Contains(joined, `"type"`) {
		t.Errorf("日志里不该出现原始 JSON：%s", joined)
	}
	if outcome.ThinkingTokens != 420 {
		t.Errorf("ThinkingTokens = %d", outcome.ThinkingTokens)
	}
	if outcome.SessionID != "7931f2be-66b6-4070-a86e-01e60124e3cc" {
		t.Errorf("应从 thinking 事件取到 session id：%q", outcome.SessionID)
	}
	if outcome.Result != "好了" {
		t.Errorf("坏行不该影响后续解析：%+v", outcome)
	}
}

// init 事件要报出会话实况（版本/认证/权限/工具面/MCP 状态），MCP 未连上要单独告警
// ——「声明的 MCP」与「实际连上的 MCP」是两件事。
func TestFeedChatStreamInitReportsConfigAndBrokenMCP(t *testing.T) {
	var progress []string
	var outcome chatOutcome
	initLine := `{"type":"system","subtype":"init","session_id":"s-1","model":"MiniMax-M3[1m]",
		"claude_code_version":"2.1.270","apiKeySource":"ANTHROPIC_AUTH_TOKEN","permissionMode":"auto",
		"tools":["Bash","Edit","mcp__daemon__daemon_status","mcp__MiniMax__search"],
		"mcp_servers":[{"name":"daemon","status":"connected"},{"name":"MiniMax","status":"failed"}]}`
	feedChatStreamLine(&outcome, []byte(initLine), func(text string) { progress = append(progress, text) })
	joined := strings.Join(progress, "\n")
	for _, want := range []string{
		"claude=2.1.270",
		"认证=ANTHROPIC_AUTH_TOKEN",
		"权限=auto",
		"工具 4 个（内建 2，MCP 2）",
		"MCP 服务 daemon=connected MiniMax=failed",
		"⚠ MCP 服务未就绪：MiniMax=failed",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("init 行缺少 %q：\n%s", want, joined)
		}
	}
	if outcome.MCPStatus["MiniMax"] != "failed" || outcome.MCPStatus["daemon"] != "connected" {
		t.Errorf("MCPStatus = %v", outcome.MCPStatus)
	}
}

// 工具调用与结果：入参摘要里的密钥字段打码，结果按长度截断。
func TestFeedChatStreamToolUseMasksSecrets(t *testing.T) {
	var progress []string
	var outcome chatOutcome
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"先看一下队列"},
			{"type":"tool_use","name":"mcp__gitea__create_issue","input":{"title":"x","api_key":"sk-cp-abcdefghijklmn"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"已创建 #12"}]}]}}`,
	}
	for _, line := range lines {
		feedChatStreamLine(&outcome, []byte(line), func(text string) { progress = append(progress, text) })
	}
	joined := strings.Join(progress, "\n")
	if !strings.Contains(joined, "思考: 先看一下队列") {
		t.Errorf("thinking 块应展示：%s", joined)
	}
	if !strings.Contains(joined, "🔧 mcp__gitea__create_issue") || !strings.Contains(joined, `"title":"x"`) {
		t.Errorf("工具调用应带入参摘要：%s", joined)
	}
	if strings.Contains(joined, "sk-cp-abcdefghijklmn") {
		t.Errorf("入参里的密钥未打码：%s", joined)
	}
	if !strings.Contains(joined, "sk-cp-…klmn") {
		t.Errorf("密钥应显示首尾便于核对：%s", joined)
	}
	if !strings.Contains(joined, "↩ 工具结果（7 字）：已创建 #12") {
		t.Errorf("工具结果应展示：%s", joined)
	}
}
