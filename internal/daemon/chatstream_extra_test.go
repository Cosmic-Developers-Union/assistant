package daemon

import (
	"strings"
	"testing"
)

// TestChatOutcomeFailureMessageBranches 钉住失败原因的择优顺序：API 层错误（401/
// 超时）优先于 result 的 errors，errors 优先于 subtype——「发消息没反应」时回复里
// 说的原因必须是根因，把 subtype=error_max_turns 摆在认证失败前面会误导排查。
func TestChatOutcomeFailureMessageBranches(t *testing.T) {
	cases := []struct {
		name    string
		outcome chatOutcome
		want    string
	}{
		{"API 错误优先", chatOutcome{APIError: "  HTTP 401 invalid api key  ", Errors: []string{"x"}, Subtype: "error_during_execution"},
			"模型端点报错：HTTP 401 invalid api key"},
		{"多条 errors 用分号连接", chatOutcome{Errors: []string{"限流", "重试耗尽"}, Subtype: "error_during_execution"},
			"限流；重试耗尽"},
		{"只有 subtype 时点名 subtype", chatOutcome{Subtype: "error_max_turns"}, "会话执行失败（error_max_turns）"},
		{"都没有时给兜底文案", chatOutcome{}, "会话执行失败"},
	}
	for _, testCase := range cases {
		if got := testCase.outcome.FailureMessage(); got != testCase.want {
			t.Errorf("%s：FailureMessage() = %q，want %q", testCase.name, got, testCase.want)
		}
	}
}

// TestChatResultAccumulates 钉住 result 帧在 daemon 这层的归集规则：errors 追加而
// 非替换，subtype/is_error/num_turns/cost 全部落到 chatOutcome。
//
// 注意两点与老实现不同（老断言钉的语义已随实现消失，不能照搬）：
//   - result 帧不再把 error.message 兜底进 outcome.APIError：现在只有 api_error
//     事件写 APIError（见 TestChatOutcomeFailureMessageBranches 的择优顺序）；
//   - result 帧给的 session_id 直接覆盖，不再区分「缺席」与「空串」。
func TestChatResultAccumulates(t *testing.T) {
	var outcome chatOutcome
	feedChatStreamLine(&outcome, []byte(`{"type":"system","subtype":"init","session_id":"sess-from-stream"}`), nil)
	feedChatStreamLine(&outcome, []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,`+
		`"result":"失败了","num_turns":7,"total_cost_usd":0.25,`+
		`"errors":["后来的","还有一条"],`+
		`"error":{"message":"result 里的兜底原因"}}`), nil)

	if outcome.SessionID != "sess-from-stream" {
		t.Errorf("result 未给 session_id 时应保留 init 的值：%q", outcome.SessionID)
	}
	if len(outcome.Errors) != 2 {
		t.Errorf("errors 应归集：%v", outcome.Errors)
	}
	if outcome.Subtype != "error_during_execution" || !outcome.IsError || outcome.NumTurns != 7 || outcome.CostUSD != 0.25 {
		t.Errorf("result 字段应全部归集：%+v", outcome)
	}
	if outcome.APIError != "" {
		t.Errorf("APIError 只由 api_error 事件写入，result 不该写它：%q", outcome.APIError)
	}
	// api_error 事件才是 APIError 的唯一来源
	feedChatStreamLine(&outcome, []byte(`{"type":"system","subtype":"api_error","error":{"status":401,"message":"invalid api key"}}`), nil)
	if outcome.APIError != "HTTP 401 invalid api key" {
		t.Errorf("api_error 事件应写入带状态码的 APIError：%q", outcome.APIError)
	}
}

// 真实 CLI 在 API 层失败时的 result 帧（实测 --model 不存在时抓到的原样载荷）：
// is_error=true、subtype 仍是 success、具体原因只在 result 文本里，没有独立
// api_error 事件、也没有 error 字段。用户必须能看到那句具体原因。
func TestRealAPIFailureReachesUser(t *testing.T) {
	var outcome chatOutcome
	line := `{"duration_api_ms":0,"session_id":"5e364317","total_cost_usd":0,"is_error":true,` +
		`"num_turns":1,"subtype":"success","api_error_status":400,` +
		`"result":"API Error: 400 没有匹配的模型路由：nonexistent-model-xyz","type":"result"}`
	if !feedChatStreamLine(&outcome, []byte(line), nil) {
		t.Fatal("应识别为 result")
	}
	if !outcome.IsError {
		t.Error("is_error=true 应判为失败")
	}
	if !strings.Contains(outcome.Result, "没有匹配的模型路由") {
		t.Errorf("具体原因必须保留在 Result：%q", outcome.Result)
	}
	// daemon 的回复优先级：IsError 时先回 result 文本，否则 FailureMessage
	reply := strings.TrimSpace(outcome.Result)
	if reply == "" || !strings.Contains(reply, "没有匹配的模型路由") {
		t.Errorf("用户应看到具体原因，实际回：%q（或泛化的 FailureMessage=%q）", reply, outcome.FailureMessage())
	}
}

// 失败会话里模型给出的说明文本：daemon 在 result.IsError 时优先回给用户
// （chat.go:303）。若归集把它丢掉，用户只能看到泛化的 FailureMessage。
func TestFailedSessionKeepsResultText(t *testing.T) {
	var outcome chatOutcome
	line := `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"已达回合上限，未完成审查","num_turns":50}`
	done := feedChatStreamLine(&outcome, []byte(line), nil)
	if !done {
		t.Fatal("应识别为 result")
	}
	if !outcome.IsError {
		t.Error("应判为失败")
	}
	if outcome.Result != "已达回合上限，未完成审查" {
		t.Errorf("失败会话的说明文本被丢弃：Result = %q（用户会只看到泛化的 FailureMessage）", outcome.Result)
	}
}
