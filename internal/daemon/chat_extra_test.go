package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/sessionstore"
)

// sessionstoreRemoteForTest 造一份指向测试服务端的记录库连接配置。
func sessionstoreRemoteForTest(url string) sessionstore.RemoteConfig {
	return sessionstore.RemoteConfig{URL: url, Token: "tok"}
}

// TestAgentRuntimeFallbacks 钉住 agent 运行时的两条回退链：claude 可执行文件
// （agent 覆盖 → 全局配置 → "claude"）与单轮超时（agent 覆盖 → 全局 → 3 分钟）。
// 回退错会让会话用错二进制（跑不起来）或拿到 0 超时（立刻被取消）。
func TestAgentRuntimeFallbacks(t *testing.T) {
	if got := (AgentRuntime{}).claudeBinOrDefault(""); got != "claude" {
		t.Errorf("两级都空应回退 claude：%q", got)
	}
	if got := (AgentRuntime{}).claudeBinOrDefault("/usr/local/bin/claude-alt"); got != "/usr/local/bin/claude-alt" {
		t.Errorf("应使用全局配置：%q", got)
	}
	if got := (AgentRuntime{ClaudeBin: " /opt/claude "}).claudeBinOrDefault("ignored"); got != "/opt/claude" {
		t.Errorf("agent 覆盖应优先且去空白：%q", got)
	}

	if got := (AgentRuntime{}).timeoutOrDefault(0); got != 3*time.Minute {
		t.Errorf("两级都非正应回退 3 分钟：%v", got)
	}
	if got := (AgentRuntime{}).timeoutOrDefault(30 * time.Second); got != 30*time.Second {
		t.Errorf("应使用全局超时：%v", got)
	}
	if got := (AgentRuntime{Timeout: time.Minute}).timeoutOrDefault(30 * time.Second); got != time.Minute {
		t.Errorf("agent 覆盖应优先：%v", got)
	}
}

// TestChatAccessorsAndHelpers 钉住四个此前零覆盖的小工具：SessionDir 访问器
// （daemon 的会话配置根，外部诊断用）、shortSession 的三条分支（长 id 截断、
// 空 id 显示 "-"、短 id 原样）、truncate 的边界（正好等于上限不截断，超限才加
// 省略号；必须按 rune 截断，否则中文会被截成半个字符）与 firstNonEmptyString。
func TestChatAccessorsAndHelpers(t *testing.T) {
	// SessionDir 必须回显配置里的值（不是用户的 ~/.claude）
	chat, err := NewChat(ChatConfig{StateDir: t.TempDir(), SessionDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	if chat.SessionDir() != chat.config.SessionDir || chat.SessionDir() == "" {
		t.Errorf("SessionDir = %q，config = %q", chat.SessionDir(), chat.config.SessionDir)
	}
	if chat.StateDir() != chat.config.StateDir {
		t.Errorf("StateDir = %q", chat.StateDir())
	}

	if got := shortSession("0123456789abcdef"); got != "01234567" {
		t.Errorf("长 id 应截前 8 位：%q", got)
	}
	if got := shortSession(""); got != "-" {
		t.Errorf("空 id 应显示占位：%q", got)
	}
	if got := shortSession("abc"); got != "abc" {
		t.Errorf("短 id 应原样：%q", got)
	}

	// truncate 在 limit 处补省略号，即结果总长 limit+1 个 rune
	if got := truncate("中文五字超限了", 5); got != "中文五字超…" {
		t.Errorf("超限应按 rune 截断并加省略号：%q", got)
	}
	if got := truncate("正好五个字", 5); got != "正好五个字" {
		t.Errorf("正好等于上限不应截断：%q", got)
	}
	if got := truncate("中文五字", 5); got != "中文五字" {
		t.Errorf("正好五个 rune 不应截断：%q", got)
	}
	if got := truncate("", 3); got != "" {
		t.Errorf("空串应返回空串：%q", got)
	}

	if got := firstNonEmptyString("", "  ", " 取我 ", "不要我"); got != "取我" {
		t.Errorf("应返回第一个非空串（去空白）：%q", got)
	}
	if got := firstNonEmptyString("", " "); got != "" {
		t.Errorf("全空应返回空串：%q", got)
	}
}

// TestTailBufferKeepsTail 钉住 stderr 尾巴缓冲：失败时报的是**最后** limit 字节
// （编译/认证错误总在末尾，报开头等于没报），limit<=0 表示不截断（保留全部）。
func TestTailBufferKeepsTail(t *testing.T) {
	buffer := &tailBuffer{limit: 8}
	if written, err := buffer.Write([]byte("0123456789abcdef")); written != 16 || err != nil {
		t.Fatalf("Write 应报写入长度：%d %v", written, err)
	}
	if got := buffer.String(); got != "89abcdef" {
		t.Errorf("应只保留最后 8 字节：%q", got)
	}
	_, _ = buffer.Write([]byte("XY"))
	if got := buffer.String(); got != "abcdefXY" {
		t.Errorf("续写后仍应只保留最后 8 字节：%q", got)
	}

	full := &tailBuffer{limit: 0}
	_, _ = full.Write([]byte("全部保留"))
	if got := full.String(); got != "全部保留" {
		t.Errorf("limit<=0 应保留全部：%q", got)
	}
}

// TestProgressLoggerSplitsLinesAndTruncates 钉住进度日志的两个契约：多行事件必须
// 按行各自带前缀（否则工具入参的缩进 JSON 会在日志里变成一坨），以及单行超过 300
// rune 时截断（工具结果动辄上万字，不截会把日志冲爆）。末尾换行会被先裁掉，所以
// 末尾空行不产生任何日志。
func TestProgressLoggerSplitsLinesAndTruncates(t *testing.T) {
	var lines []string
	chat, err := NewChat(ChatConfig{
		StateDir:   t.TempDir(),
		SessionDir: t.TempDir(),
		Log:        func(format string, arguments ...any) { lines = append(lines, fmt.Sprintf(format, arguments...)) },
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	progress := chat.progressLogger("0123456789abcdef")
	progress("第一行\n第二行\n \n")
	progress(strings.Repeat("长", 400))
	if len(lines) != 4 {
		t.Fatalf("应输出 4 条（裁掉末尾空行后剩 3 行 + 长行 1 条）：%v", lines)
	}
	if !strings.HasPrefix(lines[0], "对话[01234567] 第一行") || !strings.HasPrefix(lines[1], "对话[01234567] 第二行") {
		t.Errorf("每行都要带会话前缀：%v", lines)
	}
	if want := "对话[01234567] " + strings.Repeat("长", 300) + "…"; lines[3] != want {
		t.Errorf("超长行应截断到 300 rune：%q", lines[3])
	}
}

// TestChatRunInjectedFailureAndBadStream 钉住注入式 RunClaude 的两条失败路径：
// 进程返回错误时错误信息必须点名二进制并带上原因（截断 400），以及整段输出里
// 没有 result 事件时 parseChatStream 必须报「没有 result 事件」——否则上层会把
// 空结果当成「claude 沉默」而不是解析失败。
func TestChatRunInjectedFailureAndBadStream(t *testing.T) {
	chat, err := NewChat(ChatConfig{
		StateDir:   t.TempDir(),
		SessionDir: t.TempDir(),
		RunClaude: func(context.Context, string, []string, string, []string) ([]byte, error) {
			return nil, fmt.Errorf("%s", strings.Repeat("认证失败", 200))
		},
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	if _, err := chat.run(t.Context(), "claude-alt", nil, t.TempDir(), nil); err == nil ||
		!strings.Contains(err.Error(), "claude-alt: ") || !strings.Contains(err.Error(), "…") {
		t.Errorf("应报「<bin>: <截断原因>」：%v", err)
	}

	chat.config.RunClaude = func(context.Context, string, []string, string, []string) ([]byte, error) {
		return []byte("这不是 stream-json\n"), nil
	}
	if _, err := chat.run(t.Context(), "claude", nil, t.TempDir(), nil); err == nil ||
		!strings.Contains(err.Error(), "没有 result 事件") {
		t.Errorf("无 result 事件应报解析失败：%v", err)
	}
}

// writeFakeClaude 写一个假 claude 可执行脚本（#!/bin/sh），把输出固定成 want、
// stderr 固定成 stderr、退出码固定成 exitCode——用来覆盖 runStreaming 的真实进程
// 路径而不需要装 claude。
func writeFakeClaude(t *testing.T, want, stderr string, exitCode int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-fake")
	script := "#!/bin/sh\ncat > /dev/null\n" +
		"printf '%s' '" + want + "'\n" +
		"printf '%s' '" + stderr + "' >&2\n" +
		fmt.Sprintf("exit %d\n", exitCode)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestChatRunStreamingRealProcess 钉住 runStreaming 的真实进程路径（用假 claude
// 脚本走 exec）：正常 stream 要归集出 result 并把 assistant 文本透进进度日志；
// 非 JSON 噪音行必须被忽略而不是让整轮失败（stdio 上混日志是常态）。
func TestChatRunStreamingRealProcess(t *testing.T) {
	var lines []string
	chat, err := NewChat(ChatConfig{
		StateDir:   t.TempDir(),
		SessionDir: t.TempDir(),
		Log:        func(format string, arguments ...any) { lines = append(lines, fmt.Sprintf(format, arguments...)) },
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	output := strings.Join([]string{
		`日志噪音：这不是 JSON`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"正在处理"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"好了","session_id":"sess-1","num_turns":2}`,
	}, "\n") + "\n"

	outcome, err := chat.runStreaming(t.Context(), writeFakeClaude(t, output, "", 0), nil, t.TempDir(), chat.progressLogger("sess-1"))
	if err != nil {
		t.Fatalf("runStreaming: %v", err)
	}
	if outcome.Result != "好了" || outcome.SessionID != "sess-1" || outcome.NumTurns != 2 || outcome.IsError {
		t.Errorf("应归集出 result：%+v", outcome)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "claude: 正在处理") {
		t.Errorf("assistant 文本应进进度日志：%v", lines)
	}
}

// TestChatRunStreamingFailurePaths 钉住 runStreaming 的三条失败路径：非零退出且
// stderr 有内容时错误必须带 stderr 尾巴（编译/认证错误在那里）；stderr 为空时退化成
// 退出状态文案；进程正常退出但没有任何 result 事件时报「没有返回结果」并把 stderr
// 附上（诊断「claude 什么都没说」）。
func TestChatRunStreamingFailurePaths(t *testing.T) {
	chat, err := NewChat(ChatConfig{StateDir: t.TempDir(), SessionDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	workspace := t.TempDir()

	_, err = chat.runStreaming(t.Context(), writeFakeClaude(t, "", "致命错误：配置无效", 3), nil, workspace, nil)
	if err == nil || !strings.Contains(err.Error(), "claude-fake: ") || !strings.Contains(err.Error(), "配置无效") {
		t.Errorf("非零退出应带 stderr 尾巴：%v", err)
	}

	_, err = chat.runStreaming(t.Context(), writeFakeClaude(t, "", "", 3), nil, workspace, nil)
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("stderr 为空时应退化报退出状态：%v", err)
	}

	_, err = chat.runStreaming(t.Context(), writeFakeClaude(t, "", "只打印了日志", 0), nil, workspace, nil)
	if err == nil || !strings.Contains(err.Error(), "会话没有返回结果") || !strings.Contains(err.Error(), "只打印了日志") {
		t.Errorf("无 result 事件应报「会话没有返回结果」并附 stderr：%v", err)
	}
}

// TestChatRunStreamingStartFailure 钉住启动失败分支：二进制不可执行时必须报
// 「启动 <bin>: <原因>」——daemon 启动脚本靠这句话区分「没装 claude」与
// 「claude 跑了但失败」。用空 PATH 让 exec 必然找不到可执行文件。
func TestChatRunStreamingStartFailure(t *testing.T) {
	t.Setenv("PATH", "/nonexistent-assistant-test")
	chat, err := NewChat(ChatConfig{StateDir: t.TempDir(), SessionDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	_, err = chat.runStreaming(t.Context(), "claude-不存在", nil, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "启动 claude-不存在") {
		t.Errorf("找不到二进制应报「启动 <bin>」：%v", err)
	}
}

// TestChatSessionEnvExplicitConfig 钉住会话进程环境：始终注入托管的
// CLAUDE_CONFIG_DIR；配置了 config.json 路径时额外注入 ASSISTANT_CONFIG，让会话内的
// assistant MCP 解析到同一份配置与 daemon 端点，而不是靠 cwd 猜。
func TestChatSessionEnvExplicitConfig(t *testing.T) {
	chat, err := NewChat(ChatConfig{StateDir: t.TempDir(), SessionDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	env := chat.sessionEnv()
	if len(env) != 1 || !strings.HasPrefix(env[0], "CLAUDE_CONFIG_DIR=") {
		t.Fatalf("未配置路径时应只有 CLAUDE_CONFIG_DIR：%v", env)
	}
	chat.config.AssistantConfig = "/etc/assistant/config.json"
	env = chat.sessionEnv()
	if len(env) != 2 || env[1] != "ASSISTANT_CONFIG=/etc/assistant/config.json" {
		t.Errorf("应注入显式 ASSISTANT_CONFIG：%v", env)
	}
}

// TestNewChatPromptAnnotationWithRemote 钉住记录库可用时的系统提示词追加：只有
// Remote.URL 非空才提示模型可以用 sessions MCP 回查历史（没配记录库时说出来只会
// 让模型调用不存在的工具）。
func TestNewChatPromptAnnotationWithRemote(t *testing.T) {
	chat, err := NewChat(ChatConfig{StateDir: t.TempDir(), SessionDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	if prompt := chat.systemPrompt(AgentRuntime{}); strings.Contains(prompt, "session_search") {
		t.Errorf("无记录库时不应提示 sessions MCP：%q", prompt)
	}
	chat.config.Remote.URL = "https://serve.example.com"
	if prompt := chat.systemPrompt(AgentRuntime{}); !strings.Contains(prompt, "session_search") {
		t.Errorf("有记录库时应追加回查提示：%q", prompt)
	}
	// agent 自带提示词整体替换基础提示，但记录库提示仍要追加
	prompt := chat.systemPrompt(AgentRuntime{SystemPrompt: "你是自定义助手"})
	if !strings.HasPrefix(prompt, "你是自定义助手") || !strings.Contains(prompt, "session_search") {
		t.Errorf("agent 提示词应替换基础并保留回查提示：%q", prompt)
	}
}

// TestNewChatRejectsUnwritableSessionDir 钉住配置根不可写时 NewChat 必须立刻报错：
// 会话配置根由 assistant 托管，启动时就建好，别把失败留到第一条微信消息。
func TestNewChatRejectsUnwritableSessionDir(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewChat(ChatConfig{StateDir: t.TempDir(), SessionDir: filepath.Join(blocker, "claude")})
	if err == nil || !strings.Contains(err.Error(), "创建会话配置根") {
		t.Errorf("配置根不可建时应报错：%v", err)
	}
}

// TestNewChatDropsLegacySharedConfig 钉住一次性迁移：早期版本把所有会话共用的
// settings.json / mcp.json 放在状态目录，现在每个会话写进自己的工作目录，遗留文件
// 必须被删掉（留着会与新配置混淆），且迁移要在 NewChat 里完成。
func TestNewChatDropsLegacySharedConfig(t *testing.T) {
	stateDir := t.TempDir()
	legacy := filepath.Join(stateDir, "settings.json")
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewChat(ChatConfig{StateDir: stateDir, SessionDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("遗留的共享 settings.json 应被清理：%v", err)
	}
}

// TestPushSessionArchivesToRemote 钉住归档会话的成功路径与三条早退：写完
// projects/<project>/<session>.jsonl 后 pushSession 必须把该会话推给记录库并记
// 「已归档会话」日志；找不到记录 / 空 sessionID / 未配置记录库都必须静默早退。
//
// 注意 Chat.pushSession 里那条「大小未变化就跳过」的早退在**本函数路径上不可达**：
// 它比对 session.Meta.Bytes，而 sessionstore.SessionFor 不填 Bytes（只有批量采集
// 的 CollectSession 会填），所以每次都是 0、永远判不出「无变化」。这里如实钉住
// 这一现状（重复调用会再次推送），不去假装它已被去重——真要去重要么给 SessionFor
// 补 Bytes，要么改比 session.Meta.UpdatedAt。
func TestPushSessionArchivesToRemote(t *testing.T) {
	var bodies []map[string]any
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/sessions" {
			http.NotFound(writer, request)
			return
		}
		auth = request.Header.Get("Authorization")
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		bodies = append(bodies, body)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"stored": 1})
	}))
	defer server.Close()

	sessionDir, stateDir := t.TempDir(), t.TempDir()
	sessionID := "sess-push-1"
	projectDir := filepath.Join(sessionDir, "projects", "-tmp-workspace")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"type":"user","message":{"role":"user","content":"你好"}}` + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, sessionID+".jsonl"), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}

	var logs []string
	chat, err := NewChat(ChatConfig{
		StateDir:   stateDir,
		SessionDir: sessionDir,
		Remote:     sessionstoreRemoteForTest(server.URL),
		Log:        func(format string, arguments ...any) { logs = append(logs, fmt.Sprintf(format, arguments...)) },
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	chat.pushSession(sessionID)
	if len(bodies) != 1 {
		t.Fatalf("应推送一次，实际 %d 次", len(bodies))
	}
	if !strings.Contains(strings.Join(logs, "\n"), "已归档会话") {
		t.Errorf("应有归档日志：%v", logs)
	}

	// 找不到记录（projects 下没有该会话）时静默返回，不报错也不推送
	chat.pushSession("sess-不存在")
	if len(bodies) != 1 {
		t.Errorf("找不到记录不应推送：%d 次", len(bodies))
	}
	// 空 sessionID 与未配置 Remote 都是早退
	chat.pushSession("")
	chat.config.Remote.URL = ""
	chat.pushSession(sessionID)
	if len(bodies) != 1 {
		t.Errorf("未配置记录库不应推送：%d 次", len(bodies))
	}
	// 令牌必须随请求带上：记录库靠它鉴权，漏掉会让归档全部 401
	if auth != "Bearer tok" {
		t.Errorf("归档请求应带记录库令牌，实际 %q", auth)
	}
}

// TestPushSessionLogsRemoteFailure 钉住推送失败只记日志、不上抛：记录库挂了不能
// 影响用户拿到回复。用一个必然失败的地址最能验证这条。
func TestPushSessionLogsRemoteFailure(t *testing.T) {
	sessionDir, stateDir := t.TempDir(), t.TempDir()
	sessionID := "sess-push-fail"
	projectDir := filepath.Join(sessionDir, "projects", "-tmp-workspace")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, sessionID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs []string
	chat, err := NewChat(ChatConfig{
		StateDir:   stateDir,
		SessionDir: sessionDir,
		Remote:     sessionstoreRemoteForTest("http://127.0.0.1:1"),
		Log:        func(format string, arguments ...any) { logs = append(logs, fmt.Sprintf(format, arguments...)) },
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	chat.pushSession(sessionID)
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "归档会话失败") {
		t.Errorf("推送失败应记日志：%v", logs)
	}
	if chat.pushed[sessionID] != 0 {
		t.Errorf("失败不应记录已推送大小：%d", chat.pushed[sessionID])
	}
}

// TestSaveSessionsLockedFailurePropagates 钉住会话映射落盘失败的传播：新建一个名字
// 已被目录占住的文件（os.CreateTemp 的 joinAsFile + onDisk 让 Create 必失败）作为
// sessions.json 的路径，NewChat 仍能起来（读取时按不存在处理），但 remember 落盘时
// 必须把错误上抛——静默失败会让重启后丢掉全部会话映射。
func TestSaveSessionsLockedFailurePropagates(t *testing.T) {
	stateDir := t.TempDir()
	// 让 sessions.json 的位置先被目录占住，且 load 按「不存在」跳过
	blocker := filepath.Join(stateDir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	chat, err := NewChat(ChatConfig{StateDir: stateDir, SessionDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	// 换成写不进去的路径：父级是普通文件 → MkdirAll 报 ENOTDIR
	chat.config.StateDir = filepath.Join(blocker, "chat")
	if err := chat.remember("c-1", "s-1"); err == nil {
		t.Errorf("会话映射写不进去时应报错")
	}
}
