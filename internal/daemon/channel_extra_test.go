package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── 通道层（channel.go）补测 ─────────────────────────────────────────────
// 既有 channel_test.go 用 fakeChannel 覆盖了主流程（收信 → 会话 → 切块回复）；
// 这里补的是「坏天气」路径：构造参数缺失、映射失败、对话失败、回复失败、接收
// 失败退避。这些路径恰恰是生产事故最先走到的分支（daemon 停机、配置写错、
// 平台接口挂掉），必须有测试钉住。

// stubChannel 是 channel.go 专用探针通道：Allowed/Receive/SplitLimit 由测试注入，
// 可选实现 starter / closer 以覆盖 RunChannel 的两条可选钩子分支。
type stubChannel struct {
	name    string
	allowed func(user string) (bool, string)
	receive func(ctx context.Context) (Inbound, error)
	limit   int

	startErr error
	started  int
	closed   int
}

func (s *stubChannel) Name() string { return s.name }

func (s *stubChannel) Allowed(user string) (bool, string) {
	if s.allowed == nil {
		return true, ""
	}
	return s.allowed(user)
}

func (s *stubChannel) Receive(ctx context.Context) (Inbound, error) {
	return s.receive(ctx)
}

func (s *stubChannel) SplitLimit() int { return s.limit }

func (s *stubChannel) Start(context.Context) error {
	s.started++
	return s.startErr
}

func (s *stubChannel) Close(context.Context) { s.closed++ }

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

// TestRunChannelRejectsMissingDependencies 钉住 RunChannel 的两条构造前置检查：
// 通道或 Chat 为空时必须立刻报错而不是进入消费循环——否则 nil 解引用会在第一次
// 收信时 panic，而 daemon 早已把「通道已启动」写进日志，故障点被掩盖。
func TestRunChannelRejectsMissingDependencies(t *testing.T) {
	chat, _, _ := startChat(t, ChatConfig{})
	if err := RunChannel(t.Context(), nil, ChannelConfig{Chat: chat}); err == nil ||
		!strings.Contains(err.Error(), "通道未初始化") {
		t.Errorf("空通道应立刻报错：%v", err)
	}
	channel := &stubChannel{name: "stub"}
	if err := RunChannel(t.Context(), channel, ChannelConfig{}); err == nil ||
		!strings.Contains(err.Error(), "对话会话未初始化") {
		t.Errorf("空 Chat 应立刻报错：%v", err)
	}
}

// TestRunChannelStartFailureAndCloser 钉住两条可选钩子的装配：Start 失败只记日志、
// 不阻断消费（微信上线通知失败也要继续收信）；实现了 closer 的通道在 RunChannel
// 退出时必须恰好被 Close 一次（下线通知，漏掉它平台会一直以为机器人还在线）。
// 顺带钉住空 Log 的归一化：nil 日志函数不能 panic。
func TestRunChannelStartFailureAndCloser(t *testing.T) {
	chat, _, _ := startChat(t, ChatConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	var logs []string
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
		done <- RunChannel(ctx, channel, ChannelConfig{
			Chat: chat,
			Log:  func(format string, arguments ...any) { logs = append(logs, format) },
		})
	}()
	// 等 Start 的日志落袋，再取消
	deadline := time.Now().Add(3 * time.Second)
	for channel.started == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunChannel 未退出")
	}
	if channel.started != 1 {
		t.Errorf("Start 应调用一次：%d", channel.started)
	}
	if channel.closed != 1 {
		t.Errorf("Close 应调用一次：%d", channel.closed)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "上线通知失败（忽略）") {
		t.Errorf("Start 失败应记日志但不阻断：%v", logs)
	}
}

// TestRunChannelReceiveRetriesAndSkips 钉住接收循环的四条 continue：
//   - 普通错误（非致命）退避重试而不是退出通道；
//   - nil 消息跳过（平台空批次解出来的零值）；
//   - 白名单外用户跳过；
//   - 空白文本跳过。
//
// 这四条各自的漏判后果都不同：不重试=通道假死，不跳 nil=panic，
// 不跳白名单=越权对话，不跳空白=claude 收到空提问。
func TestRunChannelReceiveRetriesAndSkips(t *testing.T) {
	chat, _, calls := startChat(t, ChatConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	var logs []string
	attempts := 0
	channel := &stubChannel{
		name: "stub",
		// 只放行 u1，且 Allowed 区分大小写
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
		done <- RunChannel(ctx, channel, ChannelConfig{
			Chat: chat,
			Log:  func(format string, arguments ...any) { logs = append(logs, format) },
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(*calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunChannel 未退出")
	}
	if got := *calls; len(got) != 1 {
		t.Errorf("只有合法消息应进 Chat：%v", got)
	}
	joined := strings.Join(logs, "\n")
	// Log 注入的是「格式串」，不是渲染后的文本：断言必须比对格式串本身
	if !strings.Contains(joined, "通道 %s 接收失败：%v（%s 后重试）") {
		t.Errorf("非致命错误应走退避重试分支：%v", logs)
	}
	if !strings.Contains(joined, "通道 %s 忽略用户 %s 的消息：%s") {
		t.Errorf("白名单外用户应被忽略并记日志：%v", logs)
	}
}

// TestRunChannelFatalErrorStopsWithStarterError 钉住「有 starter/closer 的通道」
// 遇到致命错误时仍然走完整收尾：返回错误前必须 Close 一次（否则平台侧连接与
// 「机器人在线」状态会残留）。这条把 fatal 分支与 defer Close 的组合钉住。
func TestRunChannelFatalErrorStopsWithStarterError(t *testing.T) {
	chat, _, _ := startChat(t, ChatConfig{})
	channel := &stubChannel{
		name: "stub",
		receive: func(context.Context) (Inbound, error) {
			return nil, fatalStubError{"凭据被拒绝"}
		},
	}
	err := RunChannel(t.Context(), channel, ChannelConfig{Chat: chat, Log: t.Logf})
	if err == nil || err.Error() != "凭据被拒绝" {
		t.Errorf("致命错误应上抛：%v", err)
	}
	if channel.closed != 1 {
		t.Errorf("致命退出也必须 Close：%d", channel.closed)
	}
}

// TestHandleInboundFailurePaths 钉住 handleInbound 的三条失败路径：
//   - 会话映射失败：退回「通道名+用户 id」当会话 id，记日志但继续对话（映射表
//     坏了不该让用户彻底没法说话）；
//   - 对话失败：把错误包装成用户可读的「处理失败：…」发回去，而不是静默；
//   - 回复为空：补「（无回复）」，否则平台侧收到空消息，用户以为机器人哑了。
func TestHandleInboundFailurePaths(t *testing.T) {
	// 映射失败：让 ConversationFor 落到错误分支（conversations.json 的父级是
	// 普通文件 → 映射表写不进去 → MkdirAll 报 ENOTDIR）。工作目录用另一个
	// 未受污染的状态目录，从而把「映射失败」与「对话失败」彻底分开。
	t.Run("映射失败退回通道用户", func(t *testing.T) {
		stateDir := t.TempDir()
		blocker := filepath.Join(stateDir, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		chat, _, _ := startChat(t, ChatConfig{})
		var logs []string
		config := ChannelConfig{Chat: chat, Log: func(format string, arguments ...any) {
			logs = append(logs, format)
		}}
		message := &stubInbound{user: "u1", text: "你好"}
		chat.config.RunClaude = func(context.Context, string, []string, string, []string) ([]byte, error) {
			return []byte(`{"subtype":"success","is_error":false,"result":"OK"}`), nil
		}
		// 破坏映射表落点之后再把工作目录指向干净目录
		chat.config.StateDir = filepath.Join(blocker, "chat")
		workspaceRoot := t.TempDir()
		chat.conversationWorkspace = func(string) string { return workspaceRoot }
		handleInbound(context.Background(), &stubChannel{name: "stub"}, config, message, "你好")
		if len(message.replies) != 1 || message.replies[0] != "OK" {
			t.Errorf("映射失败仍应完成对话：%v", message.replies)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "会话映射失败（%s），退回通道用户 id：%v") {
			t.Errorf("映射失败应记日志：%v", logs)
		}
	})

	// 对话失败：错误文本发回给用户
	t.Run("对话失败回发错误", func(t *testing.T) {
		chat, _, _ := startChat(t, ChatConfig{RunClaude: func(context.Context, string, []string, string, []string) ([]byte, error) {
			return nil, errors.New("claude 崩了")
		}})
		message := &stubInbound{user: "u1", text: "你好"}
		handleInbound(context.Background(), &stubChannel{name: "stub"},
			ChannelConfig{Chat: chat, Log: t.Logf}, message, "你好")
		if len(message.replies) != 1 || !strings.Contains(message.replies[0], "处理失败：") ||
			!strings.Contains(message.replies[0], "claude 崩了") {
			t.Errorf("对话失败应回发错误：%v", message.replies)
		}
	})

	// 空回复：补「（无回复）」
	t.Run("空回复占位", func(t *testing.T) {
		chat, _, _ := startChat(t, ChatConfig{RunClaude: func(context.Context, string, []string, string, []string) ([]byte, error) {
			return []byte(`{"subtype":"success","is_error":false,"result":"   "}`), nil
		}})
		message := &stubInbound{user: "u1", text: "你好"}
		handleInbound(context.Background(), &stubChannel{name: "stub"},
			ChannelConfig{Chat: chat, Log: t.Logf}, message, "你好")
		if len(message.replies) != 1 || message.replies[0] != "（无回复）" {
			t.Errorf("空回复应占位：%v", message.replies)
		}
	})
}

// stubLogCollector 收集 config.Log 的格式串（调用方取实际渲染后的日志时可用
// 参数，这里只比对格式串里的关键词）。
func stubLogCollector() *[]string {
	logs := &[]string{}
	return logs
}

// TestHandleInboundReplyFailureAbortsChunks 钉住发送失败的短路：第一块发送失败
// 后必须停止后续切块（不 return 的话会连打一串注定失败的请求，把平台限流配额
// 全烧光），并且失败必须进日志（否则「用户收不到回复」在运维侧完全不可见）。
func TestHandleInboundReplyFailureAbortsChunks(t *testing.T) {
	chat, channel, _ := startChat(t, ChatConfig{RunClaude: func(context.Context, string, []string, string, []string) ([]byte, error) {
		return []byte(`{"subtype":"success","is_error":false,"result":"第一行\n第二行内容很长"}`), nil
	}})
	logs := stubLogCollector()
	message := &stubInbound{
		user: "u1", text: "你好",
		replyErr: func(int) error { return errors.New("平台限流") },
	}
	handleInbound(context.Background(), splitLimitChannel{channel}, ChannelConfig{
		Chat: chat,
		Log:  func(format string, arguments ...any) { *logs = append(*logs, format) },
	}, message, "你好")
	if len(message.replies) != 1 {
		t.Errorf("首块失败后不应继续发送：%v", message.replies)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "发送回复失败") {
		t.Errorf("发送失败应记日志：%v", *logs)
	}
	if channel.replies() != nil && len(channel.replies()) != 0 {
		t.Errorf("fake 通道不应收到回复：%v", channel.replies())
	}
}

// TestHandleChatCommandResetAndTyping 钉住控制命令的两条路径：
//   - 未知命令返回空串（由上层补「（无回复）」）；
//   - reset 必须真的丢掉会话映射但保留工作目录（用户的文件与历史都还在），
//     并且回复里带上工作目录路径，让用户知道去哪儿找文件。
//     顺带钉住 handleInbound 的 Typing 开关成对出现（true → 处理 → false），
//     否则平台侧「正在输入」会永远转下去。
func TestHandleChatCommandResetAndTyping(t *testing.T) {
	chat, _, _ := startChat(t, ChatConfig{})
	config := ChannelConfig{Chat: chat, Log: t.Logf}
	if got := handleChatCommand(config, "c1", command{}); got != "" {
		t.Errorf("未知命令应返回空串：%q", got)
	}

	// 建立会话映射后重置
	if _, err := chat.Handle(t.Context(), "c1", Turn{Transport: "stub", Text: "你好"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	before, err := chat.ConversationFor("stub", "u1")
	if err != nil {
		t.Fatalf("ConversationFor: %v", err)
	}
	if before == "" {
		t.Fatal("应已建立映射")
	}
	reply := handleChatCommand(config, "c1", command{name: "reset"})
	if !strings.Contains(reply, "已开始新会话") || !strings.Contains(reply, "工作目录保留") {
		t.Errorf("reset 回复文案 = %q", reply)
	}
	// ConversationFor 是「必要时创建」的映射：再查一次会重新登记绑定，返回的仍是
	// 同一个会话实体（映射本身没被丢掉），因此这里钉的是「绑定的会话实体不变」+
	// 「claude 会话映射确实被 Reset 清空」。
	mapAfter, err := chat.ConversationFor("stub", "u1")
	if err != nil {
		t.Fatalf("ConversationFor: %v", err)
	}
	if mapAfter != before {
		t.Errorf("会话实体映射应保留：%q，want %q", mapAfter, before)
	}
	if sessionID, _ := chat.session("c1"); sessionID != "" {
		t.Errorf("reset 应丢弃 claude 会话映射：%q", sessionID)
	}
	workspace, err := chat.WorkspaceDir("c1")
	if err != nil || workspace == "" {
		t.Errorf("重置后工作目录应保留：%q %v", workspace, err)
	}

	// Typing 成对：handleInbound 处理一条普通消息
	message := &stubInbound{user: "u1", text: "你好"}
	chat.config.RunClaude = func(context.Context, string, []string, string, []string) ([]byte, error) {
		return []byte(`{"subtype":"success","is_error":false,"result":"好的"}`), nil
	}
	handleInbound(context.Background(), &stubChannel{name: "stub"}, config, message, "你好")
	if message.typingOn != 2 {
		t.Errorf("Typing 应成对调用（开/关）：%d", message.typingOn)
	}
}

// ── NewChat 构造参数校验 ────────────────────────────────────────────────
// TestNewChatRequiresDirs 钉住 NewChat 的两条构造前置检查：StateDir/SessionDir
// 为空时必须能解析出缺省落点（前者 = 当前目录/chat，后者 = $CLAUDE_CONFIG_DIR
// 或 当前目录/claude），而不是给一个会在第一次落盘时 panic 的 Chat。
// 落到缺省化之后的 MkdirAll / load 上，用「父级是普通文件」把落点暴露出来：
// t.Chdir 到普通文件会直接失败（不是目录），因此改成把 cwd 指到只读目录之外的
// 手段——这里用「cwd 是一个普通文件所在目录」的方式不可行，改为验证 SessionDir
// 缺省解析（CLAUDE_CONFIG_DIR）与显式路径校验。
func TestNewChatRequiresDirs(t *testing.T) {
	stateDir := t.TempDir()
	blocker := filepath.Join(stateDir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// SessionDir 缺省分支：CLAUDE_CONFIG_DIR 指到可写落点时 NewChat 直接用该路径
	claudeDir := filepath.Join(t.TempDir(), "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	chat, err := NewChat(ChatConfig{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("缺省 SessionDir 不应让 NewChat 失败：%v", err)
	}
	if chat.SessionDir() != claudeDir {
		t.Errorf("SessionDir 应取 CLAUDE_CONFIG_DIR：%q，want %q", chat.SessionDir(), claudeDir)
	}
	// 显式落点不可写（父级是普通文件）：NewChat 必须报错，而不是给一个第一条
	// 消息才炸的 Chat
	if _, err := NewChat(ChatConfig{
		StateDir:   filepath.Join(blocker, "chat"),
		SessionDir: t.TempDir(),
	}); err == nil {
		t.Errorf("StateDir 落点不可写应报错")
	}
	if _, err := NewChat(ChatConfig{
		StateDir:   t.TempDir(),
		SessionDir: filepath.Join(blocker, "claude"),
	}); err == nil {
		t.Errorf("SessionDir 落点不可写应报错")
	}
}
