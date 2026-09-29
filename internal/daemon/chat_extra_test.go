package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/sessionindex"
	"github.com/Cosmic-Developers-Union/assistant/internal/sessionstore"
)

// capturingBlob 是内存版归档目标，记录收到上传的会话键；fail 为真时一律失败。
// 它替真对象存储走归档编排的逻辑测试（MinIO 属真实进程/网络，只进 e2e）。
type capturingBlob struct {
	keys []string
	fail bool
}

func (b *capturingBlob) Put(_ context.Context, key sessionindex.Key, _ []byte) error {
	if b.fail {
		return errors.New("记录库挂了")
	}
	b.keys = append(b.keys, key.String())
	return nil
}

func (b *capturingBlob) Get(_ context.Context, _ sessionindex.Key) ([]byte, bool, error) {
	return nil, false, nil
}

// archiveForTest 造一个归档器：内存 blob + 临时索引 + 给定的采集选项。
func archiveForTest(t *testing.T, sessionDir, stateDir string, blob *capturingBlob, logf func(string, ...any)) *sessionstore.Archiver {
	t.Helper()
	index, err := sessionindex.Open(filepath.Join(t.TempDir(), "index.sqlite3"))
	if err != nil {
		t.Fatalf("打开索引失败：%v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	archive := sessionstore.NewArchiver(sessionstore.CollectOptions{
		Root:    sessionDir,
		ChatDir: stateDir,
	}, blob, index, logf)
	if archive == nil {
		t.Fatal("归档器不该为 nil")
	}
	return archive
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

// TestChatRunInjectedFailureAndBadStream 钉住注入式 Runner 的两条失败路径：
// Runner 返回错误时 Chat.run 必须原样上抛（错误的格式化——「<bin>: <原因>」与
// 截断——已下沉到 internal/claude 的 execRunner，见那里的 runner_test.go），
// 以及整轮输出里没有 result 行时 Chat.run 必须报「会话没有返回结果」——否则上层
// 会把空结果当成「claude 沉默」而不是解析失败。
func TestChatRunInjectedFailureAndBadStream(t *testing.T) {
	chat, err := NewChat(ChatConfig{
		StateDir:   t.TempDir(),
		SessionDir: t.TempDir(),
		Claude:     runnerFailure(errClaudeStub),
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	if _, err := chat.run(t.Context(), "claude-alt", nil, t.TempDir(), nil); !errors.Is(err, errClaudeStub) {
		t.Errorf("Runner 的错误应原样上抛：%v", err)
	}

	chat.config.Claude = runnerOutput("这不是 stream-json\n")
	if _, err := chat.run(t.Context(), "claude", nil, t.TempDir(), nil); err == nil ||
		!strings.Contains(err.Error(), "会话没有返回结果") {
		t.Errorf("无 result 行应报解析失败：%v", err)
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
// 归档启用才提示模型可以用 sessions MCP 回查历史（没启用时说出来只会让模型调用
// 不存在的工具）。
func TestNewChatPromptAnnotationWithArchive(t *testing.T) {
	sessionDir, stateDir := t.TempDir(), t.TempDir()
	chat, err := NewChat(ChatConfig{StateDir: stateDir, SessionDir: sessionDir})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	if prompt := chat.systemPrompt(AgentRuntime{}); strings.Contains(prompt, "session_search") {
		t.Errorf("未启用归档时不应提示 sessions MCP：%q", prompt)
	}
	chat.config.Archive = archiveForTest(t, sessionDir, stateDir, &capturingBlob{}, nil)
	if prompt := chat.systemPrompt(AgentRuntime{}); !strings.Contains(prompt, "session_search") {
		t.Errorf("启用归档时应追加回查提示：%q", prompt)
	}
	// agent 自带提示词整体替换基础提示，但归档提示仍要追加
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

// TestArchiveSessionOnTurnEnd 钉住每轮结束的归档钩子：写完
// projects/<project>/<session>.jsonl 后 archiveSession 必须把该会话归档并记
// 「已归档会话」日志；找不到记录 / 空 sessionID / 未配置归档器都必须静默早退。
//
// 防重由归档器按源文件水位负责（见 sessionstore 的测试），这里只钉住 Chat 这一侧
// 的接线：钩子确实被调用、失败只记日志。
func TestArchiveSessionOnTurnEnd(t *testing.T) {
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

	blob := &capturingBlob{}
	var logs []string
	logf := func(format string, arguments ...any) { logs = append(logs, fmt.Sprintf(format, arguments...)) }
	chat, err := NewChat(ChatConfig{
		StateDir:   stateDir,
		SessionDir: sessionDir,
		Archive:    archiveForTest(t, sessionDir, stateDir, blob, logf),
		Log:        logf,
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	chat.archiveSession(sessionID)
	if len(blob.keys) != 1 {
		t.Fatalf("应归档一次，实际 %d 次", len(blob.keys))
	}
	if !strings.Contains(strings.Join(logs, "\n"), "已归档会话") {
		t.Errorf("应有归档日志：%v", logs)
	}

	// 找不到记录（projects 下没有该会话）时静默返回，不报错也不归档
	chat.archiveSession("sess-不存在")
	if len(blob.keys) != 1 {
		t.Errorf("找不到记录不应归档：%d 次", len(blob.keys))
	}
	// 空 sessionID 与未配置归档器都是早退
	chat.archiveSession("")
	chat.config.Archive = nil
	chat.archiveSession(sessionID)
	if len(blob.keys) != 1 {
		t.Errorf("未配置归档器不应归档：%d 次", len(blob.keys))
	}
}

// TestArchiveSessionLogsFailure 钉住归档失败只记日志、不上抛：对象存储挂了不能
// 影响用户拿到回复。
func TestArchiveSessionLogsFailure(t *testing.T) {
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
	logf2 := func(format string, arguments ...any) { logs = append(logs, fmt.Sprintf(format, arguments...)) }
	chat, err := NewChat(ChatConfig{
		StateDir:   stateDir,
		SessionDir: sessionDir,
		Archive:    archiveForTest(t, sessionDir, stateDir, &capturingBlob{fail: true}, logf2),
		Log:        logf2,
	})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	chat.archiveSession(sessionID)
	if joined := strings.Join(logs, "\n"); !strings.Contains(joined, "归档会话失败") {
		t.Errorf("归档失败应记日志：%v", logs)
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
