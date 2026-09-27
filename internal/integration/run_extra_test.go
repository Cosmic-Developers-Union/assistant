package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── 通用桥补测 ──────────────────────────────────────────────────────────────
// 这里补的是「坏天气」路径：构造参数缺失、映射失败、对话失败、回复失败、接收
// 失败退避。这些路径恰恰是生产事故最先走到的分支（daemon 停机、配置写错、
// 平台接口挂掉），必须有测试钉住。
//
// 全部只依赖 Conversations 窄接口——这正是抽包的收益：通用层不再需要真的
// daemon.Chat 才能测。

// fakeConversations 是 Conversations 的测试替身：可注入映射/对话/重置行为。
type fakeConversations struct {
	mapping func(transport, user string) (string, error)
	handle  func(conversationID, text string) (string, error)
	reset   func(conversationID string) error
	dir     string

	mu    sync.Mutex
	calls []string
}

func (f *fakeConversations) ConversationFor(transport, user string) (string, error) {
	if f.mapping != nil {
		return f.mapping(transport, user)
	}
	return "", nil
}

func (f *fakeConversations) Handle(_ context.Context, conversationID string, turn Turn) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, conversationID+"|"+turn.Text)
	f.mu.Unlock()
	if f.handle != nil {
		return f.handle(conversationID, turn.Text)
	}
	return "好的", nil
}

func (f *fakeConversations) Reset(conversationID string) error {
	if f.reset != nil {
		return f.reset(conversationID)
	}
	return nil
}

func (f *fakeConversations) WorkspaceDir(string) (string, error) { return f.dir, nil }

func (f *fakeConversations) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeConversations) callsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// stubChannel 是通用桥专用探针集成：Allowed/Receive/SplitLimit 由测试注入，
// 可选实现 Starter / Closer 以覆盖 RunChat 的两条可选钩子分支。
type stubChannel struct {
	name    string
	allowed func(user string) (bool, string)
	receive func(ctx context.Context) (Inbound, error)
	limit   int

	startErr error
	// started / closed 由生产 goroutine（RunChat）自增、测试在另一 goroutine
	// 里读（含轮询等待），必须是原子的——裸 int 会被 -race 判为数据竞争。
	started atomic.Int32
	closed  atomic.Int32
}

func (s *stubChannel) Name() string { return s.name }

func (s *stubChannel) Allowed(user string) (bool, string) {
	if s.allowed == nil {
		return true, ""
	}
	return s.allowed(user)
}

func (s *stubChannel) Receive(ctx context.Context) (Inbound, error) { return s.receive(ctx) }

func (s *stubChannel) SplitLimit() int { return s.limit }

func (s *stubChannel) Start(context.Context) error {
	s.started.Add(1)
	return s.startErr
}

func (s *stubChannel) Close(context.Context) { s.closed.Add(1) }

// stubInbound 是可注入的错误入站消息：Reply 可按序号返回错误，Typing 记账。
type stubInbound struct {
	user     string
	text     string
	typingOn int
	replies  []string
	replyErr func(index int) error
}

func (s *stubInbound) Transport() string { return "stub" }
func (s *stubInbound) User() string      { return s.user }
func (s *stubInbound) Text() string      { return s.text }

func (s *stubInbound) Reply(_ context.Context, text string) error {
	s.replies = append(s.replies, text)
	if s.replyErr != nil {
		return s.replyErr(len(s.replies) - 1)
	}
	return nil
}

func (s *stubInbound) Typing(context.Context, bool) { s.typingOn++ }

// logSink 是并发安全的日志收集器。
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) Log(line string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

func (l *logSink) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// 通道或 Conversations 为空时必须立刻报错而不是进入消费循环——否则 nil 解引用
// 会在第一次收信时 panic，而调用方早已把「通道已启动」写进日志，故障点被掩盖。
func TestRunChatRejectsMissingDependencies(t *testing.T) {
	if err := RunChat(t.Context(), nil, Options{Conversations: &fakeConversations{}}); err == nil ||
		!strings.Contains(err.Error(), "通道未初始化") {
		t.Errorf("空通道应立刻报错：%v", err)
	}
	channel := &stubChannel{name: "stub"}
	if err := RunChat(t.Context(), channel, Options{}); err == nil ||
		!strings.Contains(err.Error(), "对话会话未初始化") {
		t.Errorf("空 Conversations 应立刻报错：%v", err)
	}
}

// Start 失败只记日志、不阻断消费（微信上线通知失败也要继续收信）；实现了
// Closer 的集成在 RunChat 退出时必须恰好被 Close 一次（下线通知，漏掉它平台
// 会一直以为机器人还在线）。顺带钉住空 Log 的归一化：nil 日志不能 panic。
func TestRunChatStartFailureAndCloser(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	logs := &logSink{}
	channel := &stubChannel{
		name:     "stub",
		startErr: errors.New("上线通知被拒"),
		receive: func(receiveCtx context.Context) (Inbound, error) {
			<-receiveCtx.Done()
			return nil, receiveCtx.Err()
		},
	}
	done := make(chan error, 1)
	go func() {
		done <- RunChat(ctx, channel, Options{Conversations: &fakeConversations{}, Log: logs.Log})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for channel.started.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunChat 未退出")
	}
	if channel.started.Load() != 1 {
		t.Errorf("Start 应调用一次：%d", channel.started.Load())
	}
	if channel.closed.Load() != 1 {
		t.Errorf("Close 应调用一次：%d", channel.closed.Load())
	}
	if !strings.Contains(logs.joined(), "上线通知失败（忽略）") {
		t.Errorf("Start 失败应记日志但不阻断：%v", logs.joined())
	}
}

// 接收循环的四条 continue：
//   - 普通错误（非致命）退避重试而不是退出通道；
//   - nil 消息跳过（平台空批次解出来的零值）；
//   - 白名单外用户跳过；
//   - 空白文本跳过。
//
// 四条各自漏判的后果都不同：不重试=通道假死，不跳 nil=panic，不跳白名单=越权
// 对话，不跳空白=claude 收到空提问。
func TestRunChatReceiveRetriesAndSkips(t *testing.T) {
	conversations := &fakeConversations{}
	ctx, cancel := context.WithCancel(context.Background())
	logs := &logSink{}
	attempts := 0
	channel := &stubChannel{
		name: "stub",
		allowed: func(user string) (bool, string) {
			if user == "u1" {
				return true, ""
			}
			return false, "白名单外用户"
		},
		receive: func(receiveCtx context.Context) (Inbound, error) {
			attempts++
			switch attempts {
			case 1:
				return nil, errors.New("平台暂时不可用") // 非致命 → 退避重试
			case 2:
				return nil, nil // nil 消息 → 跳过
			case 3:
				return &stubInbound{user: "intruder", text: "你好"}, nil // 白名单外
			case 4:
				return &stubInbound{user: "u1", text: "   "}, nil // 空白文本
			case 5:
				return &stubInbound{user: "u1", text: "问题"}, nil // 合法
			}
			<-receiveCtx.Done()
			return nil, receiveCtx.Err()
		},
	}
	done := make(chan error, 1)
	go func() {
		done <- RunChat(ctx, channel, Options{Conversations: conversations, Log: logs.Log})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for conversations.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunChat 未退出")
	}
	if got := conversations.callsSnapshot(); len(got) != 1 {
		t.Errorf("只有合法消息应进对话：%v", got)
	}
	joined := logs.joined()
	// Log 注入的是「格式串」，不是渲染后的文本：断言必须比对格式串本身
	if !strings.Contains(joined, "通道 %s 接收失败：%v（%s 后重试）") {
		t.Errorf("非致命错误应走退避重试分支：%v", joined)
	}
	if !strings.Contains(joined, "通道 %s 忽略用户 %s 的消息：%s") {
		t.Errorf("白名单外用户应被忽略并记日志：%v", joined)
	}
}

// fatalStubError 模拟通道层自报的致命错误（如 QQ 凭据被平台拒绝）。
type fatalStubError struct{ message string }

func (e fatalStubError) Error() string { return e.message }

// Fatal 实现 RunChat 的停止标记。
func (e fatalStubError) Fatal() bool { return true }

// 有 Starter/Closer 的集成遇到致命错误时仍然走完整收尾：返回错误前必须 Close
// 一次（否则平台侧连接与「机器人在线」状态会残留）。
func TestRunChatFatalErrorStopsAndCloses(t *testing.T) {
	channel := &stubChannel{
		name: "stub",
		receive: func(context.Context) (Inbound, error) {
			return nil, fatalStubError{"凭据被拒绝"}
		},
	}
	err := RunChat(t.Context(), channel, Options{Conversations: &fakeConversations{}, Log: t.Logf})
	if err == nil || err.Error() != "凭据被拒绝" {
		t.Errorf("致命错误应上抛：%v", err)
	}
	if channel.closed.Load() != 1 {
		t.Errorf("致命退出也必须 Close：%d", channel.closed.Load())
	}
}

// 致命错误只调用一次 Receive：重试无意义（凭据不会自愈），且会让操作者以为
// 通道还在跑。
func TestRunChatFatalErrorDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	channel := &stubChannel{
		name: "stub",
		receive: func(context.Context) (Inbound, error) {
			calls.Add(1)
			return nil, fatalStubError{"配置错误"}
		},
	}
	_ = RunChat(t.Context(), channel, Options{Conversations: &fakeConversations{}, Log: t.Logf})
	if calls.Load() != 1 {
		t.Errorf("致命错误不应重试：调用了 %d 次", calls.Load())
	}
}

// splitLimitChannel 是带上限的探针（切块行为要能被驱动）。
type splitLimitChannel struct{ *stubChannel }

func (splitLimitChannel) SplitLimit() int { return 5 }

// handleInbound 的三条失败路径：
//   - 会话映射失败：退回「通道名+用户 id」当会话 id，记日志但继续对话（映射表
//     坏了不该让用户彻底没法说话）；
//   - 对话失败：把错误包装成用户可读的「处理失败：…」发回去，而不是静默；
//   - 回复为空：补「（无回复）」，否则平台侧收到空消息，用户以为机器人哑了。
func TestHandleInboundFailurePaths(t *testing.T) {
	logs := &logSink{}
	// 映射失败
	mappingFailed := &fakeConversations{
		mapping: func(string, string) (string, error) { return "", errors.New("映射表不可写") },
	}
	message := &stubInbound{user: "u1", text: "你好"}
	HandleInbound(context.Background(),
		splitLimitChannel{&stubChannel{name: "stub"}},
		Options{Conversations: mappingFailed, Log: logs.Log}, message, "你好")
	if len(message.replies) != 1 || message.replies[0] != "好的" {
		t.Errorf("映射失败应退回用户 id 继续对话：%v", message.replies)
	}
	if !strings.Contains(logs.joined(), "会话映射失败") {
		t.Errorf("映射失败应记日志：%v", logs.joined())
	}

	// 对话失败
	logs = &logSink{}
	failing := &fakeConversations{
		handle: func(string, string) (string, error) { return "", errors.New("claude 挂了") },
	}
	message = &stubInbound{user: "u1", text: "你好"}
	HandleInbound(context.Background(),
		splitLimitChannel{&stubChannel{name: "stub"}},
		Options{Conversations: failing, Log: logs.Log}, message, "你好")
	// 切块上限是 5，长回复会被切成多块，拼起来看
	if joined := strings.Join(message.replies, ""); !strings.Contains(joined, "处理失败：") {
		t.Errorf("对话失败应回可读错误而不是静默：%v", message.replies)
	}

	// 回复为空 → 补「（无回复）」
	empty := &fakeConversations{
		handle: func(string, string) (string, error) { return "   ", nil },
	}
	message = &stubInbound{user: "u1", text: "你好"}
	HandleInbound(context.Background(),
		splitLimitChannel{&stubChannel{name: "stub"}},
		Options{Conversations: empty, Log: logs.Log}, message, "你好")
	if len(message.replies) != 1 || message.replies[0] != "（无回复）" {
		t.Errorf("空回复应补占位：%v", message.replies)
	}
}

// 首块回复失败后必须停止后续切块：平台限流时继续硬发只会加重限流，且用户收到
// 的是残缺消息。失败必须记日志（否则「用户收不到回复」在运维侧完全不可见）。
func TestHandleInboundReplyFailureAbortsChunks(t *testing.T) {
	logs := &logSink{}
	conversations := &fakeConversations{
		handle: func(string, string) (string, error) { return "第一行\n第二行内容很长", nil },
	}
	message := &stubInbound{
		user: "u1", text: "你好",
		replyErr: func(int) error { return errors.New("平台限流") },
	}
	HandleInbound(context.Background(),
		splitLimitChannel{&stubChannel{name: "stub"}},
		Options{Conversations: conversations, Log: logs.Log}, message, "你好")
	if len(message.replies) != 1 {
		t.Errorf("首块失败后不应继续发送：%v", message.replies)
	}
	if !strings.Contains(logs.joined(), "发送回复失败") {
		t.Errorf("发送失败应记日志：%v", logs.joined())
	}
}

// 控制命令的两条路径：未知命令返回空串（由上层补「（无回复）」）；reset 必须
// 真的丢掉会话映射但保留工作目录，并把路径写进回复让用户知道去哪儿找文件。
// 顺带钉住 Typing 开关成对出现（true → 处理 → false），否则平台侧「正在输入」
// 会永远转下去。
func TestHandleCommandResetAndTyping(t *testing.T) {
	if got := HandleCommand(Options{Conversations: &fakeConversations{}}, "c1", Command{}); got != "" {
		t.Errorf("未知命令应返回空串：%q", got)
	}

	var reset []string
	conversations := &fakeConversations{
		reset: func(conversationID string) error { reset = append(reset, conversationID); return nil },
		dir:   "/workspace/c1",
	}
	reply := HandleCommand(Options{Conversations: conversations, Log: t.Logf}, "c1", Command{Name: "reset"})
	if len(reset) != 1 || reset[0] != "c1" {
		t.Errorf("reset 应丢弃该会话映射：%v", reset)
	}
	if !strings.Contains(reply, "/workspace/c1") {
		t.Errorf("reset 回复应带上工作目录：%q", reply)
	}
	if !strings.Contains(reply, "工作目录保留") {
		t.Errorf("reset 回复应说明工作目录保留：%q", reply)
	}

	// help 走另一条分支
	if got := HandleCommand(Options{Conversations: conversations}, "c1", Command{Name: "help"}); !strings.Contains(got, "/new") {
		t.Errorf("help 应列可用命令：%q", got)
	}

	// Typing 成对：进入时 on、退出时 off
	message := &stubInbound{user: "u1", text: "你好"}
	HandleInbound(context.Background(),
		splitLimitChannel{&stubChannel{name: "stub"}},
		Options{Conversations: conversations, Log: t.Logf}, message, "你好")
	if message.typingOn != 2 {
		t.Errorf("Typing 应开关成对调用（true+false）：%d", message.typingOn)
	}
}

// reset 失败只记日志、不阻断回复：会话映射写不进去不该让用户连「已重置」都收不到。
func TestHandleCommandResetFailureStillReplies(t *testing.T) {
	logs := &logSink{}
	conversations := &fakeConversations{
		reset: func(string) error { return errors.New("磁盘只读") },
	}
	reply := HandleCommand(Options{Conversations: conversations, Log: logs.Log}, "c1", Command{Name: "reset"})
	if reply == "" {
		t.Error("reset 失败也应回复用户")
	}
	if !strings.Contains(logs.joined(), "重置会话失败") {
		t.Errorf("reset 失败应记日志：%v", logs.joined())
	}
}

// FirstNonEmpty 返回第一个非空白值（QQ 取用户标识用：群聊成员 openid 优先，
// 回落作者 id）。
func TestFirstNonEmpty(t *testing.T) {
	if got := FirstNonEmpty("", "  ", "b", "c"); got != "b" {
		t.Errorf("FirstNonEmpty = %q, want b", got)
	}
	if got := FirstNonEmpty("", " "); got != "" {
		t.Errorf("全空白应返回空串：%q", got)
	}
}

// Truncate / SingleLine 是日志格式化的公共件。
func TestTruncateAndSingleLine(t *testing.T) {
	if got := Truncate("abcdef", 3); got != "abc…" {
		t.Errorf("Truncate = %q", got)
	}
	if got := Truncate("ab", 5); got != "ab" {
		t.Errorf("未超上限不该动：%q", got)
	}
	if got := SingleLine("a\nb\tc  d"); got != "a b c d" {
		t.Errorf("SingleLine = %q", got)
	}
}

// HelpText 列出的命令必须与 ParseCommand 认得的命令一致：帮助里写了却不认，
// 用户照着敲没反应；认了却不写，用户不知道有这条。
func TestHelpTextMatchesParseCommand(t *testing.T) {
	help := HelpText()
	for _, name := range []string{"/new", "/help"} {
		if !strings.Contains(help, name) {
			t.Errorf("帮助应列出 %s：%q", name, help)
		}
	}
	if ParseCommand("/new").Name != "reset" || ParseCommand("/help").Name != "help" {
		t.Error("帮助里列的命令必须被 ParseCommand 认得")
	}
}
