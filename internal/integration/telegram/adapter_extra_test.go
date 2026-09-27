package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTelegramAllowedWhitelistAndFallback 钉住 Allowed 的两条此前零覆盖分支：
// 白名单命中（必须放行，否则配好的管理员永远被拒）与完全没有白名单时的文案
// （文案必须点出 qq/telegram 配置键名，操作者才知道去哪加人）；顺带钉住
// 数字 id 白名单是精确匹配、不是前缀匹配。
func TestTelegramAllowedWhitelistAndFallback(t *testing.T) {
	whitelisted := NewAdapter(AdapterConfig{AdminUsers: []string{"1001", "1002"}}, nil)
	if allowed, reason := whitelisted.Allowed("1002"); !allowed || reason != "" {
		t.Errorf("白名单命中应放行：%v %q", allowed, reason)
	}
	if allowed, reason := whitelisted.Allowed("100"); allowed || reason != "白名单外用户" {
		t.Errorf("前缀不算命中：%v %q", allowed, reason)
	}

	// 未配置白名单：必须拒绝并提示配置方法（含 admin_users）
	empty := NewAdapter(AdapterConfig{}, nil)
	allowed, reason := empty.Allowed("1001")
	if allowed || !strings.Contains(reason, "admin_users") {
		t.Errorf("未配置白名单应拒绝并提示配置：%v %q", allowed, reason)
	}
	if allowed, _ := empty.Allowed("  "); allowed {
		t.Errorf("空白用户标识应拒绝")
	}
	if reason != `未配置 admin_users（配置 ["*"] 可放开所有人）` {
		t.Errorf("未配置白名单文案变了：%q", reason)
	}
}

// TestTelegramReceiveDrainsPendingFirst 钉住 pending 优先交付：上一批更新里多余
// 的消息必须先逐条交付完（保持平台顺序），再去长轮询新更新——pending 里的消息
// 被跳过就等于丢消息。Receive 的 pending 分支不经任何网络，直接手工塞入。
func TestTelegramReceiveDrainsPendingFirst(t *testing.T) {
	channel := NewAdapter(AdapterConfig{BotToken: "tok-1", APIBaseURL: "http://127.0.0.1:1"}, t.Logf)
	channel.pending = []Message{
		{MessageID: 1, From: &User{ID: 11}, Chat: Chat{ID: 11}, Text: "第一条"},
		{MessageID: 2, From: &User{ID: 22}, Chat: Chat{ID: 22}, Text: "第二条"},
	}
	for _, want := range []string{"第一条", "第二条"} {
		message, err := channel.Receive(t.Context())
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		if message.Text() != want {
			t.Errorf("应按顺序交付 pending：%q，want %q", message.Text(), want)
		}
		if message.Transport() != "telegram" {
			t.Errorf("Transport = %q", message.Transport())
		}
	}
	// pending 已排空，且此时再无网络可用：错误必须上抛而不是假装成功
	if _, err := channel.Receive(t.Context()); err == nil {
		t.Errorf("pending 排空且服务不可达时应报错")
	}
}

// TestTelegramReceiveSkipsNonTextUpdates 钉住更新过滤的三条 continue：没有
// message 的更新（回调按钮/频道帖）、没有 from 的消息（匿名/频道）、以及空文本
// （图片/贴纸）。不过滤的话前者会让通道层 panic（nil 解引用）或把空消息当成
// 用户提问丢给 claude。用一个脚本化的 getUpdates 服务端喂入这批更新。
func TestTelegramReceiveSkipsNonTextUpdates(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/getUpdates") {
			http.NotFound(writer, request)
			return
		}
		calls++
		writer.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			// 第一条可用，后面三条都必须被丢弃
			_, _ = writer.Write([]byte(`{"ok":true,"result":[` +
				`{"update_id":1,"message":{"message_id":1,"from":{"id":7},"chat":{"id":7},"text":"要回复的"}},` +
				`{"update_id":2,"callback_query":{"id":"x"}},` +
				`{"update_id":3,"message":{"message_id":3,"chat":{"id":9},"text":"没有 from"}},` +
				`{"update_id":4,"message":{"message_id":4,"from":{"id":8},"chat":{"id":8},"text":"   "}}` +
				`]}`))
			return
		}
		// 后续轮询：空批次，让 Receive 的 continue 分支跑一次
		time.Sleep(20 * time.Millisecond)
		_, _ = writer.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer server.Close()

	channel := NewAdapter(AdapterConfig{
		BotToken: "tok", APIBaseURL: server.URL, HTTPClient: server.Client(),
	}, t.Logf)
	message, err := channel.Receive(t.Context())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if message.User() != "7" || message.Text() != "要回复的" {
		t.Errorf("应交付唯一可用的私聊文本：%q %q", message.User(), message.Text())
	}
	if len(channel.pending) != 0 {
		t.Errorf("其余三条更新都应被丢弃，pending = %+v", channel.pending)
	}

	// 上下文取消后 Receive 必须返回 ctx.Err()（长轮询不能在 daemon 停机时挂死）
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := channel.Receive(ctx); err == nil {
		t.Errorf("上下文已取消应返回错误")
	}
}

// TestTelegramInboundTypingAndReply 钉住 adapterInbound 的两条零覆盖交互：
// Typing(false) 必须是空操作（Telegram 无关闭语义，发个假动作只会浪费配额），
// Typing(true) 必须真的打 sendChatAction；Reply 必须打到该消息的 chat id
// （群聊里回到群里，而不是发给发送者本人）。
func TestTelegramInboundTypingAndReply(t *testing.T) {
	var paths []string
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		buffer := make([]byte, 1024)
		read, _ := request.Body.Read(buffer)
		bodies = append(bodies, string(buffer[:read]))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()

	channel := NewAdapter(AdapterConfig{
		BotToken: "tok", APIBaseURL: server.URL, HTTPClient: server.Client(),
	}, t.Logf)
	// 群聊消息：chat id 与 from id 不同，Reply 必须回到 chat
	inbound := channel.inbound(Message{
		From: &User{ID: 42}, Chat: Chat{ID: -100500}, Text: "群里的问题",
	})
	inbound.Typing(t.Context(), false)
	if len(paths) != 0 {
		t.Errorf("Typing(false) 应无任何请求：%v", paths)
	}
	inbound.Typing(t.Context(), true)
	if err := inbound.Reply(t.Context(), "回复内容"); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	joined := strings.Join(paths, " ")
	if !strings.Contains(joined, "sendChatAction") || !strings.Contains(joined, "sendMessage") {
		t.Errorf("应依次打 sendChatAction 与 sendMessage：%v", paths)
	}
	if !strings.Contains(strings.Join(bodies, " "), "回复内容") {
		t.Errorf("回复正文应带到请求里：%v", bodies)
	}
	// 空白文本消息：Text 原样返回（过滤在 Receive 里做，不在 Inbound 上）
	if got := channel.inbound(Message{From: &User{ID: 1}, Text: "  "}).Text(); got != "  " {
		t.Errorf("Inbound.Text 应透传原文：%q", got)
	}
}
