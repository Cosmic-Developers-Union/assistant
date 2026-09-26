package daemon

import (
	"strings"
	"testing"
	"time"
)

// TestFeedThinkingTokensResetAndThrottle 钉住「思考中」进度的两条此前零覆盖的分支：
// 计数回退（新一輪思考从更小的 estimated_tokens 重新计数，必须重置播报节流状态，
// 否则整轮都不会再播报）与时间窗节流（窗口内的事件必须被吞掉，thinking_tokens
// 每几十毫秒一条，不节流会刷屏把日志冲掉）。这里直接替换 chatClock，不依赖真实时间。
func TestFeedThinkingTokensResetAndThrottle(t *testing.T) {
	now := time.Unix(1700000000, 0)
	originalClock, originalInterval := chatClock, thinkingProgressInterval
	chatClock = func() time.Time { return now }
	thinkingProgressInterval = 10 * time.Second
	t.Cleanup(func() {
		chatClock = originalClock
		thinkingProgressInterval = originalInterval
	})

	var lines []string
	collect := func(line string) { lines = append(lines, line) }

	outcome := chatOutcome{}
	// 首次播报带增量
	feedThinkingTokens(&outcome, chatStreamEvent{EstimatedTokens: 100, EstimatedTokensDelta: 40}, collect)
	if len(lines) != 1 || !strings.Contains(lines[0], "约 100 tokens，+40") {
		t.Fatalf("首次应播报带增量：%v", lines)
	}

	// 时间窗内：不播报
	now = now.Add(3 * time.Second)
	feedThinkingTokens(&outcome, chatStreamEvent{EstimatedTokens: 150}, collect)
	if len(lines) != 1 {
		t.Errorf("时间窗内不应再播报：%v", lines)
	}
	if outcome.ThinkingTokens != 150 {
		t.Errorf("计数应更新到 150，实际 %d", outcome.ThinkingTokens)
	}

	// 计数回退 → 重置节流：即便仍在时间窗内也必须重新播报
	now = now.Add(3 * time.Second)
	feedThinkingTokens(&outcome, chatStreamEvent{EstimatedTokens: 20}, collect)
	if len(lines) != 2 || !strings.Contains(lines[1], "约 20 tokens") {
		t.Errorf("计数回退后应重新播报：%v", lines)
	}

	// estimated_tokens 非正数：直接忽略（不能被当成「回退到 0」）
	now = now.Add(30 * time.Second)
	feedThinkingTokens(&outcome, chatStreamEvent{EstimatedTokens: 0, EstimatedTokensDelta: 5}, collect)
	if len(lines) != 2 {
		t.Errorf("非正数不应播报：%v", lines)
	}
}

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

// TestApplyChatResultAccumulates 钉住 result 事件的归集规则：会话 id 只在事件给出时
// 覆盖（空值不能把已解析出的 id 抹掉，续聊靠它）、errors 追加而非替换、以及
// result 里的 error.message 只在 outcome 尚无 APIError 时兜底——api_error 事件带上
// 的 HTTP 状态码比 result 里的裸文案更有诊断价值。
func TestApplyChatResultAccumulates(t *testing.T) {
	outcome := chatOutcome{SessionID: "sess-from-stream", Errors: []string{"先来的"}}
	applyChatResult(&outcome, chatStreamEvent{
		Subtype:   "error_during_execution",
		IsError:   true,
		Result:    "失败了",
		NumTurns:  7,
		CostUSD:   0.25,
		SessionID: "",
		Errors:    []string{"后来的", "还有一条"},
		Error: struct {
			Message string `json:"message"`
			Status  int    `json:"status"`
		}{Message: "result 里的兜底原因"},
	})
	if outcome.SessionID != "sess-from-stream" {
		t.Errorf("空 session_id 不应抹掉已有值：%q", outcome.SessionID)
	}
	if len(outcome.Errors) != 3 {
		t.Errorf("errors 应追加：%v", outcome.Errors)
	}
	if outcome.Subtype != "error_during_execution" || !outcome.IsError || outcome.NumTurns != 7 || outcome.CostUSD != 0.25 {
		t.Errorf("result 字段应全部归集：%+v", outcome)
	}
	if outcome.APIError != "result 里的兜底原因" {
		t.Errorf("无 APIError 时应兜底取 error.message：%q", outcome.APIError)
	}

	// 已有 APIError（api_error 事件更完整）时不能被 result 覆盖
	existing := chatOutcome{APIError: "HTTP 401 invalid api key"}
	applyChatResult(&existing, chatStreamEvent{Error: struct {
		Message string `json:"message"`
		Status  int    `json:"status"`
	}{Message: "后来的兜底"}})
	if existing.APIError != "HTTP 401 invalid api key" {
		t.Errorf("已有 APIError 不应被覆盖：%q", existing.APIError)
	}
}

// TestToolUseLinesAndToolResultTextForms 钉住工具调用/结果的两种输入形态：入参不是
// JSON 时必须退化成单行截断（claude 有时给的是已经是字符串的入参），结果是内容块
// 数组时取各块 text 拼接（空块忽略）。这两条分支此前零覆盖，出错会让日志里出现
// 空行或整段 raw JSON。
func TestToolUseLinesAndToolResultTextForms(t *testing.T) {
	if got := toolUseLines("Bash", nil); got != "🔧 Bash" {
		t.Errorf("空入参只应给名称一行：%q", got)
	}
	broken := toolUseLines("Bash", []byte("不是 JSON 的入参"))
	if !strings.HasPrefix(broken, "🔧 Bash\n  ") || !strings.Contains(broken, "不是 JSON 的入参") {
		t.Errorf("坏 JSON 入参应退化成缩进的单行：%q", broken)
	}
	valid := toolUseLines("Bash", []byte(`{"command":"ls"}`))
	if !strings.Contains(valid, `"command"`) {
		t.Errorf("合法 JSON 入参应展开成缩进 JSON：%q", valid)
	}

	if got := toolResultText(nil); got != "" {
		t.Errorf("空内容应返回空串：%q", got)
	}
	if got := toolResultText([]byte(`"直接是字符串"`)); got != "直接是字符串" {
		t.Errorf("字符串内容应原样返回：%q", got)
	}
	blocks := []byte(`[{"type":"text","text":"第一段"},{"type":"text","text":""},{"type":"text","text":"第二段"}]`)
	if got := toolResultText(blocks); got != "第一段 第二段" {
		t.Errorf("内容块数组应取非空 text 拼接：%q", got)
	}
	if got := toolResultText([]byte(`{"既不是字符串":"也不是数组"}`)); got != `{"既不是字符串":"也不是数组"}` {
		t.Errorf("两种形态都不是时应回退原始文本：%q", got)
	}
}
