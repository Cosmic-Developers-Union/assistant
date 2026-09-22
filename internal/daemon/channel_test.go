package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeChannel 是内存通道：测试用 push 逐条投递消息，Reply 收集回复。
// Receive 在队列空时阻塞，投递下一条前等上一条的回复落袋，保证顺序确定。
type fakeChannel struct {
	name    string
	allow   func(user string) bool
	replies func() []string

	mu      sync.Mutex
	queue   []Inbound
	sent    []string
	notify  chan struct{}
	runDone chan error
}

func newFakeChannel(name string) *fakeChannel {
	fake := &fakeChannel{
		name:    name,
		notify:  make(chan struct{}, 1),
		runDone: make(chan error, 1),
	}
	fake.replies = func() []string {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return append([]string(nil), fake.sent...)
	}
	return fake
}

func (f *fakeChannel) Name() string { return f.name }

func (f *fakeChannel) SplitLimit() int { return 0 }

func (f *fakeChannel) Allowed(user string) (bool, string) {
	if f.allow == nil || f.allow(user) {
		return true, ""
	}
	return false, "白名单外用户"
}

func (f *fakeChannel) Receive(ctx context.Context) (Inbound, error) {
	for {
		f.mu.Lock()
		if len(f.queue) > 0 {
			next := f.queue[0]
			f.queue = f.queue[1:]
			f.mu.Unlock()
			return next, nil
		}
		f.mu.Unlock()
		select {
		case <-f.notify:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// push 投递一条消息（测试与被测代码不同协程，全部走锁）。
func (f *fakeChannel) push(user, text string) {
	f.mu.Lock()
	f.queue = append(f.queue, &fakeInbound{channel: f, user: user, text: text})
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

// waitReplies 等到回复数量达到 want，超时失败。
func (f *fakeChannel) waitReplies(t *testing.T, want int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if replies := f.replies(); len(replies) >= want {
			return replies
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待第 %d 条回复超时：已收到 %v", want, f.replies())
	return nil
}

// stop 取消并等待 RunChannel 退出（waitGroup 收割所有处理协程）。
func (f *fakeChannel) stop(t *testing.T, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	select {
	case <-f.runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("RunChannel 未退出")
	}
}

type fakeInbound struct {
	channel *fakeChannel
	user    string
	text    string
}

func (m *fakeInbound) Transport() string { return m.channel.name }

func (m *fakeInbound) User() string { return m.user }

func (m *fakeInbound) Text() string { return m.text }

func (m *fakeInbound) Reply(_ context.Context, text string) error {
	m.channel.mu.Lock()
	defer m.channel.mu.Unlock()
	m.channel.sent = append(m.channel.sent, text)
	return nil
}

func (m *fakeInbound) Typing(context.Context, bool) {}

// startChat 起一个带 RunClaude 注入的 Chat 与 fake 通道。
func startChat(t *testing.T, config ChatConfig) (*Chat, *fakeChannel, *[][]string) {
	t.Helper()
	var calls [][]string
	if config.RunClaude == nil {
		config.RunClaude = func(_ context.Context, _ string, args []string, _ string, _ []string) ([]byte, error) {
			calls = append(calls, args2copy(args))
			return []byte(`{"subtype":"success","is_error":false,"result":"好的"}`), nil
		}
	}
	config.StateDir = t.TempDir()
	config.SessionDir = t.TempDir()
	chat, err := NewChat(config)
	if err != nil {
		t.Fatal(err)
	}
	return chat, newFakeChannel("fake"), &calls
}

// 通用桥端到端：普通对话进 Chat（由主 agent 接待）、控制命令不进 Chat、
// 白名单外用户被忽略。
func TestRunChannelEndToEnd(t *testing.T) {
	chat, channel, calls := startChat(t, ChatConfig{
		MainAgent: AgentRuntime{Name: "main", Model: "m-main"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		channel.runDone <- RunChannel(ctx, channel, ChannelConfig{Chat: chat, Log: t.Logf})
	}()

	// 普通对话：主 agent 生效（模型进 claude 参数）
	channel.push("user-1", "你好")
	channel.waitReplies(t, 1)
	channel.stop(t, cancel)
	if len(*calls) != 1 {
		t.Fatalf("普通对话应进 Chat：calls=%d", len(*calls))
	}
	if !contains((*calls)[0], "--model") || argumentAfter((*calls)[0], "--model") != "m-main" {
		t.Errorf("主 agent 模型应生效：%v", (*calls)[0])
	}
}

// chatConversationIDForTest 反查 fake 通道用户映射到的会话 id。
func chatConversationIDForTest(t *testing.T, chat *Chat, channel *fakeChannel, user string) string {
	t.Helper()
	id, err := chat.ConversationFor(channel.Name(), user)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// 白名单外用户：不回复、不建会话。
func TestRunChannelDeniesUser(t *testing.T) {
	chat, channel, calls := startChat(t, ChatConfig{})
	channel.allow = func(user string) bool { return user == "user-1" }
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		channel.runDone <- RunChannel(ctx, channel, ChannelConfig{Chat: chat, Log: t.Logf})
	}()
	channel.push("intruder", "你好")
	time.Sleep(150 * time.Millisecond)
	channel.stop(t, cancel)
	if len(channel.replies()) != 0 || len(*calls) != 0 {
		t.Errorf("白名单外用户不应触发对话：replies=%v calls=%d", channel.replies(), len(*calls))
	}
}

// /help 回复命令一览；命令不进 Chat；普通聊天不误触命令。
func TestRunChannelCommands(t *testing.T) {
	chat, channel, calls := startChat(t, ChatConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		channel.runDone <- RunChannel(ctx, channel, ChannelConfig{Chat: chat, Log: t.Logf})
	}()
	channel.push("user-1", "/help")
	replies := channel.waitReplies(t, 1)
	if !strings.Contains(replies[0], "/new") || !strings.Contains(replies[0], "/help") {
		t.Errorf("/help 应列出命令：%q", replies[0])
	}
	channel.push("user-1", "/new")
	channel.waitReplies(t, 2)
	// 「重新开始吧，但先回答我」是普通聊天，不是命令
	channel.push("user-1", "重新开始吧，但先回答我")
	channel.waitReplies(t, 3)
	channel.stop(t, cancel)
	if len(*calls) != 1 {
		t.Errorf("命令不应进 Chat，普通聊天应进：%v", calls)
	}
}

// 子代理注入：配置 Subagents 后 claude 参数带 --agents JSON（含 description/
// prompt/model），子代理 MCP 并入会话 mcp.json。
func TestRunChannelInjectsSubagents(t *testing.T) {
	chat, channel, calls := startChat(t, ChatConfig{
		MainAgent: AgentRuntime{Name: "main"},
		Subagents: []SubagentDefinition{
			{Name: "ops", Description: "查状态", Prompt: "你是运维。", Model: "m-ops",
				MCP: map[string]any{"ops-tools": map[string]any{"command": "ops-mcp"}}},
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		channel.runDone <- RunChannel(ctx, channel, ChannelConfig{Chat: chat, Log: t.Logf})
	}()
	channel.push("user-1", "你好")
	channel.waitReplies(t, 1)
	channel.stop(t, cancel)
	if len(*calls) != 1 {
		t.Fatalf("calls = %d", len(*calls))
	}
	if !contains((*calls)[0], "--agents") {
		t.Fatalf("应注入 --agents：%v", (*calls)[0])
	}
	var agents map[string]struct {
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
		Model       string `json:"model"`
	}
	if err := json.Unmarshal([]byte(argumentAfter((*calls)[0], "--agents")), &agents); err != nil {
		t.Fatalf("--agents JSON 解析失败：%v", err)
	}
	ops, ok := agents["ops"]
	if !ok || ops.Description != "查状态" || ops.Prompt != "你是运维。" || ops.Model != "m-ops" {
		t.Errorf("ops 子代理定义 = %+v", ops)
	}
	// 子代理 MCP 并入会话 mcp.json（会话工作目录按 conversation id 哈希命名：
	// 直接扫目录找 mcp.json）
	entries, _ := os.ReadDir(chat.StateDir())
	found := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(chat.StateDir(), entry.Name(), "mcp.json"))
		if readErr != nil {
			continue
		}
		found = true
		var document struct {
			Servers map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatalf("mcp.json 解析失败：%v", err)
		}
		if _, ok := document.Servers["ops-tools"]; !ok {
			t.Errorf("子代理 MCP 应并入会话 mcp.json：%v", document.Servers)
		}
	}
	if !found {
		t.Fatalf("会话工作目录没有 mcp.json：%v", entries)
	}
}

// 切块：超限回复按 rune 在换行处切开，逐块回复。
func TestRunChannelSplitsLongReply(t *testing.T) {
	chat, channel, _ := startChat(t, ChatConfig{})
	config := ChannelConfig{Chat: chat, Log: t.Logf}
	message := &fakeInbound{channel: channel, user: "user-1"}
	text := "第一行\n第二行内容很长"
	chat.config.RunClaude = func(context.Context, string, []string, string, []string) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"subtype":"success","is_error":false,"result":%q}`, text)), nil
	}
	handleInbound(context.Background(), splitLimitChannel{channel}, config, message, "你好")
	replies := channel.replies()
	if len(replies) < 2 || replies[0] != "第一行" {
		t.Errorf("超限回复应切块：%v", replies)
	}
}

// splitLimitChannel 给 fake 通道一个切块上限。
type splitLimitChannel struct{ *fakeChannel }

func (splitLimitChannel) SplitLimit() int { return 5 }

// 控制命令解析：斜杠与直白说法都认，普通聊天不受影响。
func TestParseCommand(t *testing.T) {
	for text, want := range map[string]command{
		"/new":     {name: "reset"},
		" /reset ": {name: "reset"},
		"重新开始":     {name: "reset"},
		"新会话":      {name: "reset"},
		"/clear":   {name: "reset"},
		"/help":    {name: "help"},
		"帮助":       {name: "help"},
	} {
		if got := parseCommand(text); got != want {
			t.Errorf("parseCommand(%q) = %+v, want %+v", text, got, want)
		}
	}
	for _, text := range []string{"继续", "new", "重新开始吧，但先回答我", "/news", "/agents", "/agentx ops", ""} {
		if got := parseCommand(text); got.name != "" {
			t.Errorf("parseCommand(%q) 不该识别为命令：+%v", text, got)
		}
	}
}

// 切块逻辑：按 rune 切、尽量在换行处切、短文本不切。
func TestSplitText(t *testing.T) {
	chunks := splitText("第一行\n第二行内容很长", 5)
	if len(chunks) < 2 || chunks[0] != "第一行" {
		t.Errorf("splitText = %q", chunks)
	}
	if chunks := splitText("短", 10); len(chunks) != 1 || chunks[0] != "短" {
		t.Errorf("短文本不该切：%q", chunks)
	}
	if chunks := splitText("无换行的长文本内容", 5); len(chunks) != 2 {
		t.Errorf("无换行也要能切：%q", chunks)
	}
	if chunks := splitText("任意", 0); len(chunks) != 1 {
		t.Errorf("上限 <=0 表示不切：%q", chunks)
	}
}
