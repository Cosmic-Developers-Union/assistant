package claude

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// collect 返回一个把进度收进切片的回调。
func collect(lines *[]string) func(string) {
	return func(line string) { *lines = append(*lines, line) }
}

// ParseLine 的边界：合法 JSON 解析成功；空行、非 JSON、非对象都返回 ok=false。
// CLI 会往 stdout 混入杂音（警告、进度条），不能因此中断整轮会话。
func TestParseLineBoundaries(t *testing.T) {
	for _, test := range []struct {
		name string
		line string
		want bool
	}{
		{name: "空行", line: "", want: false},
		{name: "纯空白", line: "   \t ", want: false},
		{name: "非 JSON 文本", line: "starting claude…", want: false},
		{name: "截断的 JSON", line: `{"type":"assist`, want: false},
		{name: "JSON 数组不是对象", line: `[1,2,3]`, want: false},
		{name: "JSON 字符串不是对象", line: `"just a string"`, want: false},
		{name: "JSON 数字不是对象", line: `42`, want: false},
		{name: "合法对象", line: `{"type":"result"}`, want: true},
		{name: "前后有空白", line: "  {\"type\":\"result\"}  ", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, ok := ParseLine([]byte(test.line))
			if ok != test.want {
				t.Errorf("ParseLine(%q) ok = %v, want %v", test.line, ok, test.want)
			}
		})
	}
}

// 指针字段区分「字段未出现」与「零值」：这是把 is_error 缺省判为失败的前提。
func TestParseLineDistinguishesAbsentFromZero(t *testing.T) {
	absent, ok := ParseLine([]byte(`{"type":"result"}`))
	if !ok {
		t.Fatal("解析失败")
	}
	if absent.IsError != nil {
		t.Error("未出现的字段应为 nil（不能与 false 混同）")
	}
	if absent.NumTurns != nil || absent.TotalCostUSD != nil || absent.Result != nil {
		t.Error("未出现的数值/文本字段应为 nil")
	}

	explicit, ok := ParseLine([]byte(`{"type":"result","is_error":false,"num_turns":0,"total_cost_usd":0}`))
	if !ok {
		t.Fatal("解析失败")
	}
	if explicit.IsError == nil || *explicit.IsError {
		t.Error("显式 false 应解析为指向 false 的指针")
	}
	if explicit.NumTurns == nil || *explicit.NumTurns != 0 {
		t.Error("显式 0 应解析为指向 0 的指针")
	}
}

// init 事件：会话 ID 与模型归集、MCP 状态建表。会话实况是排查「工具用不了」的
// 第一手线索。
func TestFeedInit(t *testing.T) {
	var progress []string
	outcome := NewOutcome()
	line := `{"type":"system","subtype":"init","session_id":"sess-42","model":"claude-sonnet-5",
		"claude_code_version":"2.1.0","permissionMode":"auto","apiKeySource":"none","cwd":"/work",
		"tools":["Read","Write"],"skills":[{}],"agents":[{}],
		"mcp_servers":[{"name":"gitea","status":"connected"},{"name":"broken","status":"failed"}]}`

	if Feed(&outcome, []byte(line), ProgressTerse, collect(&progress)) {
		t.Error("init 不是 result 帧")
	}
	if outcome.SessionID != "sess-42" {
		t.Errorf("SessionID = %q", outcome.SessionID)
	}
	if outcome.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q", outcome.Model)
	}
	if got := outcome.MCPStatus["gitea"]; got != "connected" {
		t.Errorf("MCPStatus[gitea] = %q", got)
	}
	// 未就绪的 server 必须能报出来（工具用不了的最常见原因）
	if got := BrokenMCPServers(&outcome); got != "broken=failed" {
		t.Errorf("BrokenMCPServers = %q, want broken=failed", got)
	}
	// 紧凑形态：一行 session=…，外加一行未就绪 MCP 告警
	if len(progress) != 2 {
		t.Fatalf("紧凑形态应报 session 一行 + MCP 告警一行：%v", progress)
	}
	if !strings.Contains(progress[0], "session=sess-42") {
		t.Errorf("首行应含 session=…：%v", progress[0])
	}
	if !strings.Contains(progress[1], "MCP 服务未就绪") || !strings.Contains(progress[1], "broken=failed") {
		t.Errorf("次行应报未就绪的 MCP：%v", progress[1])
	}
}

// 全部 MCP 就绪时不产告警行（避免误导读者去找一个不存在的问题）。
func TestFeedInitNoMCPWarningWhenHealthy(t *testing.T) {
	var progress []string
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"system","subtype":"init","session_id":"s","mcp_servers":[
		{"name":"gitea","status":"connected"}]}`), ProgressTerse, collect(&progress))
	for _, line := range progress {
		if strings.Contains(line, "MCP 服务未就绪") {
			t.Errorf("全部就绪时不该告警：%v", progress)
		}
	}
}

// 展开形态的 init：逐字段多行（版本/认证/权限/工作目录/工具面/MCP）。
func TestFeedInitVerbose(t *testing.T) {
	var progress []string
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"system","subtype":"init","model":"m","claude_code_version":"2.1.0",
		"apiKeySource":"none","permissionMode":"auto","cwd":"/work","tools":["Read"],
		"mcp_servers":[{"name":"gitea","status":"connected"}]}`), ProgressVerbose, collect(&progress))

	if len(progress) != 1 {
		t.Fatalf("展开形态的一条进度 = 1 行组：%v", progress)
	}
	text := progress[0]
	for _, want := range []string{"会话已启动", "model = m", "claude = 2.1.0", "工具 = 1 个", "MCP = gitea=connected"} {
		if !strings.Contains(text, want) {
			t.Errorf("init 实况缺少 %q：\n%s", want, text)
		}
	}
}

// 全部 MCP 都就绪时不报「未就绪」。
func TestBrokenMCPServersAllConnected(t *testing.T) {
	outcome := NewOutcome()
	outcome.MCPStatus = map[string]string{"a": "connected", "b": "connected"}
	if got := BrokenMCPServers(&outcome); got != "" {
		t.Errorf("全部就绪时应返回空：%q", got)
	}
	// 空状态（无 server 声明）同样返回空
	empty := NewOutcome()
	if got := BrokenMCPServers(&empty); got != "" {
		t.Errorf("无声明时应返回空：%q", got)
	}
}

// assistant 事件：文本进进度、工具调用带摘要、turns 递增。
func TestFeedAssistant(t *testing.T) {
	var progress []string
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"assistant","message":{"content":[
		{"type":"text","text":"正在审查这个 PR"},
		{"type":"tool_use","name":"mcp__gitea__issue_read","input":{"number":42,"owner":"acme"}}
	]}}`), ProgressTerse, collect(&progress))

	if outcome.NumTurns != 1 {
		t.Errorf("NumTurns = %d, want 1", outcome.NumTurns)
	}
	if len(progress) != 2 {
		t.Fatalf("进度 = %v, want 文本 + 工具两行", progress)
	}
	if progress[0] != "正在审查这个 PR" {
		t.Errorf("文本进度 = %q", progress[0])
	}
	// 工具调用带摘要：读者能判断「当前步在干什么」而不用打开原始记录
	if !strings.HasPrefix(progress[1], "🔧 mcp__gitea__issue_read") {
		t.Errorf("工具进度 = %q", progress[1])
	}
	if !strings.Contains(progress[1], "42") {
		t.Errorf("工具进度应含入参摘要：%q", progress[1])
	}
}

// 工具调用无可摘要入参时只报工具名（不产出空摘要的破折号尾巴）。
func TestFeedAssistantToolUseWithoutDetail(t *testing.T) {
	var progress []string
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Task"}]}}`),
		ProgressTerse, collect(&progress))
	if len(progress) != 1 || progress[0] != "🔧 Task" {
		t.Errorf("进度 = %v, want [🔧 Task]", progress)
	}
}

// 空白文本不产生进度行（CLI 会发空 text 块）。
func TestFeedAssistantIgnoresBlankText(t *testing.T) {
	var progress []string
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"   "}]}}`),
		ProgressTerse, collect(&progress))
	if len(progress) != 0 {
		t.Errorf("空白文本不应产出进度：%v", progress)
	}
}

// 思考块在两种形态下都报（展开形态报，紧凑形态也报一行摘要）。
func TestFeedAssistantThinking(t *testing.T) {
	var progress []string
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"先看改动\n再判断"}]}}`),
		ProgressTerse, collect(&progress))
	if len(progress) != 1 || !strings.HasPrefix(progress[0], "思考: ") {
		t.Fatalf("思考进度 = %v", progress)
	}
	// 多行思考折成单行（日志一条一行）
	if strings.Contains(progress[0], "\n") {
		t.Errorf("思考进度不应含换行：%q", progress[0])
	}
}

// user 事件承载 tool_result：紧凑形态折成一行回执、错误标 ✗；展开形态给字数与缩进正文。
func TestFeedToolResults(t *testing.T) {
	t.Run("紧凑形态", func(t *testing.T) {
		var progress []string
		outcome := NewOutcome()
		Feed(&outcome, []byte(`{"type":"user","message":{"content":[
			{"type":"tool_result","content":"读取成功"},
			{"type":"tool_result","is_error":true,"content":"拒绝访问"}
		]}}`), ProgressTerse, collect(&progress))

		if len(progress) != 2 {
			t.Fatalf("进度 = %v", progress)
		}
		if !strings.HasPrefix(progress[0], "  ↳ （返回") {
			t.Errorf("成功回执 = %q", progress[0])
		}
		if !strings.HasPrefix(progress[1], "  ↳ ✗ ") {
			t.Errorf("错误回执应标 ✗：%q", progress[1])
		}
	})

	t.Run("展开形态", func(t *testing.T) {
		var progress []string
		outcome := NewOutcome()
		Feed(&outcome, []byte(`{"type":"user","message":{"content":[
			{"type":"tool_result","is_error":true,"content":"报告有误"}
		]}}`), ProgressVerbose, collect(&progress))
		if len(progress) != 1 || !strings.Contains(progress[0], "↩ 工具结果（错误）") {
			t.Errorf("展开形态回执 = %v", progress)
		}
	})
}

// 非 tool_result 的 user 块不产出进度（user 事件也承载注入的提示词文本）。
func TestFeedToolResultsIgnoresOtherBlocks(t *testing.T) {
	var progress []string
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"user","message":{"content":[{"type":"text","text":"注入的文本"}]}}`),
		ProgressTerse, collect(&progress))
	if len(progress) != 0 {
		t.Errorf("非工具回执不应产出进度：%v", progress)
	}
}

// result 帧：结论归集、返回 true、错误列表累积、权限拒绝计数。
func TestFeedResult(t *testing.T) {
	var progress []string
	outcome := NewOutcome()
	done := Feed(&outcome, []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":7,"total_cost_usd":0.1234,"duration_ms":45000,"session_id":"sess-9","result":"已提交评审","errors":[],"permission_denials":[{},{}]}`), ProgressTerse, collect(&progress))

	if !done {
		t.Fatal("result 帧必须返回 true（调用方据此判会话结束）")
	}
	if !outcome.SawResult {
		t.Error("SawResult 应为真")
	}
	if outcome.Subtype != "success" || outcome.IsError {
		t.Errorf("subtype/isError = %q/%v", outcome.Subtype, outcome.IsError)
	}
	if outcome.NumTurns != 7 || outcome.CostUSD != 0.1234 || outcome.DurationMS != 45000 {
		t.Errorf("归集数值 = turns %d cost %v duration %d", outcome.NumTurns, outcome.CostUSD, outcome.DurationMS)
	}
	if outcome.SessionID != "sess-9" {
		t.Errorf("SessionID = %q", outcome.SessionID)
	}
	if outcome.Result != "已提交评审" {
		t.Errorf("Result = %q", outcome.Result)
	}
	if outcome.PermissionDenials != 2 {
		t.Errorf("PermissionDenials = %d, want 2", outcome.PermissionDenials)
	}
}

// is_error 缺省即失败：把一次未明确成功的会话当成功，会让「完成判定」在错误
// 结论上放行——这是最危险的一种静默错误。
func TestFeedResultDefaultsToErrorWhenIsErrorAbsent(t *testing.T) {
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"result","subtype":"error_max_turns","result":"不该被采纳"}`),
		ProgressTerse, nil)

	if !outcome.IsError {
		t.Error("is_error 缺席时必须判为失败")
	}
	if outcome.Result != "" {
		t.Errorf("失败时不应采纳 result 文本：%q", outcome.Result)
	}
}

// 失败的 result 不采纳 result 文本（避免把错误说明当成会话结论）。
func TestFeedResultExplicitError(t *testing.T) {
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"result","is_error":true,"result":"出错了","errors":["boom","boom2"]}`),
		ProgressTerse, nil)
	if !outcome.IsError {
		t.Error("应判为失败")
	}
	if outcome.Result != "" {
		t.Errorf("失败时 Result 应为空：%q", outcome.Result)
	}
	if !slices.Equal(outcome.Errors, []string{"boom", "boom2"}) {
		t.Errorf("Errors = %v", outcome.Errors)
	}
}

// 兼容 --output-format json 的单对象输出：没有 type 但有 subtype/result 时按结果处理。
func TestFeedSingleObjectResult(t *testing.T) {
	outcome := NewOutcome()
	done := Feed(&outcome, []byte(`{"subtype":"success","is_error":false,"result":"完成"}`),
		ProgressTerse, nil)
	if !done {
		t.Fatal("单对象结果应返回 true")
	}
	if outcome.Subtype != "success" || outcome.Result != "完成" {
		t.Errorf("归集 = %q/%q", outcome.Subtype, outcome.Result)
	}
}

// 未知事件只报 type/subtype，且不误判为 result。
func TestFeedUnknownEvent(t *testing.T) {
	var progress []string
	outcome := NewOutcome()
	done := Feed(&outcome, []byte(`{"type":"system","subtype":"something_new"}`),
		ProgressTerse, collect(&progress))
	if done {
		t.Error("未知事件不是 result")
	}
	if len(progress) != 1 || !strings.Contains(progress[0], "something_new") {
		t.Errorf("未知事件应报 type/subtype：%v", progress)
	}
}

// api_error 是最关键的一行（排查「发消息没反应」）：必须归集到 APIError 并播报，
// 且带 HTTP 状态。
func TestFeedAPIError(t *testing.T) {
	var progress []string
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"system","subtype":"api_error","error":{"message":"invalid api key","status":401}}`),
		ProgressTerse, collect(&progress))

	if !strings.Contains(outcome.APIError, "401") || !strings.Contains(outcome.APIError, "invalid api key") {
		t.Errorf("APIError = %q, want 含状态与原因", outcome.APIError)
	}
	if len(progress) != 1 || !strings.Contains(progress[0], "API 错误") {
		t.Errorf("应播报 API 错误：%v", progress)
	}
}

// api_error 缺原因时给占位文案（不能是空的「API 错误：」）。
func TestFeedAPIErrorWithoutMessage(t *testing.T) {
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"system","subtype":"api_error"}`), ProgressTerse, nil)
	if outcome.APIError == "" {
		t.Error("APIError 不应为空")
	}
}

// thinking_tokens：归集累计词元数；播报按时间窗节流（用可替换时钟验证，不 sleep）。
func TestFeedThinkingTokensThrottling(t *testing.T) {
	originalNow := Now
	originalInterval := ThinkingProgressInterval
	t.Cleanup(func() { Now = originalNow; ThinkingProgressInterval = originalInterval })
	thinkingState.set(thinkingReport{})

	base := nowStub(t)
	var progress []string
	outcome := NewOutcome()

	// 第一帧：水位为零值 ⇒ 立即播报
	Feed(&outcome, []byte(`{"type":"system","subtype":"thinking_tokens","estimated_tokens":500}`),
		ProgressTerse, collect(&progress))
	if len(progress) != 1 {
		t.Fatalf("首帧应播报：%v", progress)
	}
	if outcome.ThinkingTokens != 500 {
		t.Errorf("ThinkingTokens = %d, want 500", outcome.ThinkingTokens)
	}

	// 时间窗内的小幅跳动：不播报
	base.advance(100 * 1e6) // 100ms
	Feed(&outcome, []byte(`{"type":"system","subtype":"thinking_tokens","estimated_tokens":600}`),
		ProgressTerse, collect(&progress))
	if len(progress) != 1 {
		t.Errorf("时间窗内不应重复播报：%v", progress)
	}

	// 越过时间窗：播报
	base.advance(3_000 * 1e6) // 3s
	Feed(&outcome, []byte(`{"type":"system","subtype":"thinking_tokens","estimated_tokens":700}`),
		ProgressTerse, collect(&progress))
	if len(progress) != 2 {
		t.Errorf("越过时间窗应播报：%v", progress)
	}

	// 增量形态（delta）累加到总数
	Feed(&outcome, []byte(`{"type":"system","subtype":"thinking_tokens","estimated_tokens_delta":50}`),
		ProgressTerse, nil)
	if outcome.ThinkingTokens != 750 {
		t.Errorf("delta 应累加：%d, want 750", outcome.ThinkingTokens)
	}
}

// onProgress 为 nil 时只归集、不 panic（评审会话在某些路径下不订阅进度）。
func TestFeedNilProgressIsTolerated(t *testing.T) {
	outcome := NewOutcome()
	for _, line := range []string{
		`{"type":"system","subtype":"init","model":"m"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"x"},{"type":"tool_use","name":"T"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"c"}]}}`,
		`{"type":"system","subtype":"api_error","error":{"message":"e"}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"r"}`,
	} {
		Feed(&outcome, []byte(line), ProgressTerse, nil)
	}
	if outcome.Result != "r" {
		t.Errorf("归集应在无进度订阅时照常工作：%+v", outcome)
	}
}

// 坏行静默忽略但不影响后续行：CLI 混入杂音时整轮会话仍要跑完。
func TestFeedIgnoresNoiseAndContinues(t *testing.T) {
	outcome := NewOutcome()
	if Feed(&outcome, []byte("npm warn: deprecated"), ProgressTerse, nil) {
		t.Error("杂音不应判为 result")
	}
	if !Feed(&outcome, []byte(`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`),
		ProgressTerse, nil) {
		t.Error("杂音之后的 result 仍须识别")
	}
	if outcome.Result != "ok" {
		t.Errorf("Result = %q", outcome.Result)
	}
}

// NewOutcome 的集合非 nil：nil 集合在 JSON 与日志里会呈现成 null，客户端按数组
// 解析会失败。
func TestNewOutcomeInitializesCollections(t *testing.T) {
	outcome := NewOutcome()
	if outcome.Errors == nil {
		t.Error("Errors 不应为 nil")
	}
	if outcome.MCPStatus == nil {
		t.Error("MCPStatus 不应为 nil")
	}
	encoded, err := json.Marshal(outcome)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "null") {
		t.Errorf("序列化不应出现 null：%s", encoded)
	}
}

// nowStub 是可推进的假时钟（返回指针以便测试显式推进，避免 sleep 赌时序）。
type fakeClock struct {
	millis int64
}

func (c *fakeClock) advance(nanos int64) { c.millis += nanos }

func nowStub(t *testing.T) *fakeClock {
	t.Helper()
	clock := &fakeClock{}
	Now = func() time.Time { return time.Unix(0, clock.millis) }
	return clock
}

// 工具名缺失的 tool_use 块被跳过：产出「🔧 」这种空名字行只会误导读者。
func TestFeedAssistantSkipsNamelessToolUse(t *testing.T) {
	var progress []string
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","input":{"a":1}}]}}`),
		ProgressTerse, collect(&progress))
	if len(progress) != 0 {
		t.Errorf("无名工具不应产出进度：%v", progress)
	}
	// 展开形态同样跳过
	progress = nil
	Feed(&outcome, []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","input":{"a":1}}]}}`),
		ProgressVerbose, collect(&progress))
	if len(progress) != 0 {
		t.Errorf("展开形态无名工具也不应产出进度：%v", progress)
	}
}

// assistant 事件的展开形态：文本加 claude: 前缀并折成单行。
func TestFeedAssistantVerbose(t *testing.T) {
	var progress []string
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"第一行\n第二行"}]}}`),
		ProgressVerbose, collect(&progress))
	if len(progress) != 1 || !strings.HasPrefix(progress[0], "claude: ") {
		t.Fatalf("展开形态文本 = %v", progress)
	}
	if strings.Contains(progress[0], "\n") {
		t.Errorf("展开形态文本也应折成单行：%q", progress[0])
	}
}

// 空思考块不产出进度（CLI 会发空的 thinking 块）。
func TestFeedAssistantIgnoresBlankThinking(t *testing.T) {
	var progress []string
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"  "}]}}`),
		ProgressTerse, collect(&progress))
	if len(progress) != 0 {
		t.Errorf("空思考不应产出进度：%v", progress)
	}
}

// MCPStatus 为 nil 的 outcome（不是经 NewOutcome 构造的）遇到 init 事件时要能建表：
// dispatcher/daemon 转换来的 Outcome 可能没走构造函数。
func TestFeedInitBuildsNilMCPStatusMap(t *testing.T) {
	outcome := Outcome{}
	Feed(&outcome, []byte(`{"type":"system","subtype":"init","mcp_servers":[{"name":"gitea","status":"connected"}]}`),
		ProgressTerse, nil)
	if outcome.MCPStatus == nil {
		t.Fatal("应就地建表而不是 panic 或丢弃")
	}
	if outcome.MCPStatus["gitea"] != "connected" {
		t.Errorf("MCPStatus = %v", outcome.MCPStatus)
	}
}

// 首个 init 已给出 session_id 时，后续 init（重连）不覆盖既有 ID：会话身份必须
// 稳定，否则续接与记录归档会对错文件。
func TestFeedInitKeepsExistingSessionID(t *testing.T) {
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"system","subtype":"init","session_id":"first"}`), ProgressTerse, nil)
	Feed(&outcome, []byte(`{"type":"system","subtype":"init","session_id":"second"}`), ProgressTerse, nil)
	if outcome.SessionID != "first" {
		t.Errorf("SessionID = %q, want first（不覆盖既有身份）", outcome.SessionID)
	}
}

// init 不带 model 时保留原有模型名（后续帧不应把它清空）。
func TestFeedInitKeepsModelWhenAbsent(t *testing.T) {
	outcome := NewOutcome()
	Feed(&outcome, []byte(`{"type":"system","subtype":"init","model":"sonnet"}`), ProgressTerse, nil)
	Feed(&outcome, []byte(`{"type":"system","subtype":"init"}`), ProgressTerse, nil)
	if outcome.Model != "sonnet" {
		t.Errorf("Model = %q, want sonnet", outcome.Model)
	}
}
