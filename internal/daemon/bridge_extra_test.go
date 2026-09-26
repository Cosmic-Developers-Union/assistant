package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/weixin"
)

// fakeIlink 返回一个最小 ilink 服务端：把每个请求的路径记进 paths（按顺序），
// 由 handler 决定响应体。微信通道的三条网络路径（notifystart/notifystop、
// getupdates、sendmessage）都打在这里。
func fakeIlink(t *testing.T, handler func(path string, body map[string]any) (int, string)) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		status, payload := handler(request.URL.Path, body)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)
	return server, &paths
}

// weixinChannelForTest 造一个指向测试服务端的微信通道。
func weixinChannelForTest(server *httptest.Server, config WeixinChannelConfig, log func(string, ...any)) *WeixinChannel {
	config.Weixin = weixin.Config{BaseURL: server.URL, HTTPClient: server.Client(), BotToken: "bot-tok"}
	return NewWeixinChannel(config, log)
}

// TestWeixinAllowedLoginFallback 钉住 Allowed 的后半条链：白名单为空时只能靠
// login_user_id 兜底——本人放行、他人拒绝（文案必须是「非登录用户」，否则操作者
// 以为是自己没配白名单）；两者都空时彻底拒绝并点名两个配置键。这条兜底是扫码
// 登录模式下唯一能用的准入规则。
func TestWeixinAllowedLoginFallback(t *testing.T) {
	channel := &WeixinChannel{loginUserID: "owner"}
	if allowed, reason := channel.Allowed("owner"); !allowed || reason != "" {
		t.Errorf("登录者本人应放行：%v %q", allowed, reason)
	}
	if allowed, reason := channel.Allowed("别人"); allowed || reason != "非登录用户" {
		t.Errorf("非登录者应拒绝：%v %q", allowed, reason)
	}

	empty := &WeixinChannel{}
	allowed, reason := empty.Allowed("任何")
	if allowed || reason != "未配置 weixin.admin_users 且无 login_user_id" {
		t.Errorf("两者都空应拒绝并点名配置：%v %q", allowed, reason)
	}
	// 空白用户标识在进白名单判断之前就被拒（"*" 通配也不例外）
	wildcard := &WeixinChannel{adminUsers: []string{"*"}}
	if allowed, reason := wildcard.Allowed("   "); allowed || reason != "用户标识为空" {
		t.Errorf("空白标识应拒绝：%v %q", allowed, reason)
	}
}

// TestWeixinCloseLogsNotifyFailure 钉住 Close 的失败处理：下线通知失败只记日志、
// 不上抛也不 panic——daemon 停机时通道关闭必须走完，通知失败不能挡住退出。
func TestWeixinCloseLogsNotifyFailure(t *testing.T) {
	server, paths := fakeIlink(t, func(string, map[string]any) (int, string) {
		return http.StatusInternalServerError, `{}`
	})
	var logs []string
	channel := weixinChannelForTest(server, WeixinChannelConfig{}, func(format string, arguments ...any) {
		logs = append(logs, format)
	})
	channel.Close(t.Context())
	if !slices.Contains(*paths, "/ilink/bot/msg/notifystop") {
		t.Errorf("Close 应打下线通知：%v", *paths)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "通知下线失败（忽略）") {
		t.Errorf("通知失败应记日志：%v", logs)
	}

	// 成功路径：不应有任何日志
	ok, _ := fakeIlink(t, func(string, map[string]any) (int, string) {
		return http.StatusOK, `{"ret":0}`
	})
	logs = nil
	weixinChannelForTest(ok, WeixinChannelConfig{}, func(format string, arguments ...any) {
		logs = append(logs, format)
	}).Close(t.Context())
	if len(logs) != 0 {
		t.Errorf("成功下线不应记日志：%v", logs)
	}
}

// TestWeixinStartNotifyFailurePropagates 钉住 Start 把上线通知的错误交出去：
// 由通用桥决定记日志还是重试，通道不能自己吞掉——否则「微信没上线」这种故障
// 在日志里完全看不见。
func TestWeixinStartNotifyFailurePropagates(t *testing.T) {
	server, paths := fakeIlink(t, func(path string, _ map[string]any) (int, string) {
		return http.StatusOK, `{"ret":100,"errmsg":"未登录"}`
	})
	err := weixinChannelForTest(server, WeixinChannelConfig{}, nil).Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "ret=100") {
		t.Errorf("上线通知失败应上抛并带 ret：%v", err)
	}
	if !slices.Contains(*paths, "/ilink/bot/msg/notifystart") {
		t.Errorf("Start 应打上线通知：%v", *paths)
	}

	ok, _ := fakeIlink(t, func(string, map[string]any) (int, string) {
		return http.StatusOK, `{"ret":0}`
	})
	if err := weixinChannelForTest(ok, WeixinChannelConfig{}, nil).Start(t.Context()); err != nil {
		t.Errorf("上线通知成功不应报错：%v", err)
	}
}

// TestWeixinReceivePendingDrainAndCursor 钉住 Receive 的两条此前零覆盖路径：
// pending 里上一批的余量必须逐条先交付（并保持顺序），取空后建立游标；以及
// Bot 自己的消息（message_type=2）不得进 pending——否则 "*" 放开时会自问自答
// 打环。cursor 必须随平台返回值前进，否则每轮都重取同一批消息。
func TestWeixinReceivePendingDrainAndCursor(t *testing.T) {
	// 手工塞入 pending：不经网络，直接覆盖 drain 分支
	channel := &WeixinChannel{name: "weixin", typing: &typingCache{tickets: map[string]string{}}, log: func(string, ...any) {}}
	channel.pending = []weixin.Message{
		{FromUserID: "u1", MessageType: messageTypeUser, ItemList: []weixin.MessageItem{{Type: 1, TextItem: &struct {
			Text string `json:"text"`
		}{Text: "第一条"}}}},
		{FromUserID: "u2", MessageType: messageTypeUser, ContextToken: "ctx-2", ItemList: []weixin.MessageItem{{Type: 1, TextItem: &struct {
			Text string `json:"text"`
		}{Text: "第二条"}}}},
	}
	want := []struct{ user, text string }{{"u1", "第一条"}, {"u2", "第二条"}}
	for index, expect := range want {
		inbound, err := channel.Receive(t.Context())
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		if inbound.Text() != expect.text || inbound.User() != expect.user {
			t.Errorf("第 %d 条应按序交付：%q %q", index, inbound.User(), inbound.Text())
		}
		if inbound.Transport() != "weixin" {
			t.Errorf("Transport = %q", inbound.Transport())
		}
	}
	if len(channel.pending) != 0 {
		t.Errorf("pending 应已排空：%+v", channel.pending)
	}

	// 批内过滤：用户消息进队列、Bot 自己的消息丢弃；cursor 前进
	user := `{"message_type":1,"from_user_id":"me","item_list":[{"type":1,"text_item":{"text":"你好"}}]}`
	bot := `{"message_type":2,"from_user_id":"bot","item_list":[{"type":1,"text_item":{"text":"我是机器人"}}]}`
	server, _ := fakeIlink(t, func(path string, _ map[string]any) (int, string) {
		if path != "/ilink/bot/getupdates" {
			t.Errorf("意外请求：%s", path)
		}
		return http.StatusOK, `{"ret":0,"get_updates_buf":"cursor-2","msgs":[` + user + `,` + bot + `]}`
	})
	live := weixinChannelForTest(server, WeixinChannelConfig{}, t.Logf)
	inbound, err := live.Receive(t.Context())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if inbound.Text() != "你好" {
		t.Errorf("应只交付用户消息：%q", inbound.Text())
	}
	if live.cursor != "cursor-2" {
		t.Errorf("游标应随平台返回值前进：%q", live.cursor)
	}
}

// TestWeixinReceiveContextCanceled 钉住 ctx 取消时的收尾：长轮询里 ctx 一旦取消，
// Receive 必须交出 ctx.Err() 而不是继续循环——daemon 停机时桥不能被微信轮询挂住。
func TestWeixinReceiveContextCanceled(t *testing.T) {
	server, _ := fakeIlink(t, func(string, map[string]any) (int, string) {
		// 总是空批次：让 Receive 的 continue 分支成立，从而最终落到 ctxDone 判断
		return http.StatusOK, `{"ret":0,"msgs":[]}`
	})
	channel := weixinChannelForTest(server, WeixinChannelConfig{}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := channel.Receive(ctx); err == nil {
		t.Errorf("ctx 取消后 Receive 应报错")
	}
}

// TestWeixinInboundReplyAndTyping 钉住 weixinInbound 的两条网络路径：Reply 必须
// 带 context_token 回到原会话（平台靠它定位会话，漏掉就发不出去），Typing 走
// typing 票据缓存并区分开关状态；非用户消息（Bot 自己）直接早退、不发 typing。
func TestWeixinInboundReplyAndTyping(t *testing.T) {
	var bodies []map[string]any
	server, _ := fakeIlink(t, func(path string, body map[string]any) (int, string) {
		bodies = append(bodies, body)
		switch path {
		case "/ilink/bot/getconfig":
			return http.StatusOK, `{"ret":0,"typing_ticket":"ticket-1"}`
		default:
			return http.StatusOK, `{"ret":0}`
		}
	})
	channel := weixinChannelForTest(server, WeixinChannelConfig{}, nil)
	message := weixin.Message{FromUserID: "u1", ContextToken: "ctx-1", MessageType: messageTypeUser}
	inbound := channel.inbound(message)
	if err := inbound.Reply(t.Context(), "回复内容"); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	inbound.Typing(t.Context(), true)
	inbound.Typing(t.Context(), false)

	sent := 0
	tickets := 0
	for _, body := range bodies {
		if raw, ok := body["msg"].(map[string]any); ok {
			sent++
			if raw["context_token"] != "ctx-1" {
				t.Errorf("回复应带 context_token：%+v", raw)
			}
		}
		if body["typing_ticket"] != nil {
			tickets++
		}
	}
	if sent != 1 || tickets != 2 {
		t.Errorf("应各发一次回复与两次 typing（票据缓存复用）：sent=%d tickets=%d", sent, tickets)
	}

	// 非用户消息：Typing 直接早退，不打任何请求
	before := len(bodies)
	botInbound := channel.inbound(weixin.Message{FromUserID: "bot", MessageType: 2})
	botInbound.Typing(t.Context(), true)
	if len(bodies) != before {
		t.Errorf("Bot 自己的消息不应触发 typing：%d → %d", before, len(bodies))
	}
}

// TestWeixinTypingTicketCacheAndFailure 钉住 typing 票据缓存的两条路径：命中缓存
// 时不再打 getconfig（它是重接口），取票据失败时上抛错误让上层静默——票据为空
// 也不缓存，避免把空票据钉死。
func TestWeixinTypingTicketCacheAndFailure(t *testing.T) {
	calls := 0
	server, _ := fakeIlink(t, func(string, map[string]any) (int, string) {
		calls++
		return http.StatusOK, `{"ret":0,"typing_ticket":"ticket-1"}`
	})
	client := weixin.NewClient(weixin.Config{BaseURL: server.URL, HTTPClient: server.Client(), BotToken: "tok"})
	cache := &typingCache{tickets: map[string]string{}}
	for range 2 {
		ticket, err := cache.ticket(t.Context(), client, "u1", "ctx")
		if err != nil || ticket != "ticket-1" {
			t.Fatalf("ticket: %v %q", err, ticket)
		}
	}
	if calls != 1 {
		t.Errorf("第二次应命中缓存，不再打 getconfig：%d 次", calls)
	}

	failing, _ := fakeIlink(t, func(string, map[string]any) (int, string) {
		return http.StatusInternalServerError, `{}`
	})
	failClient := weixin.NewClient(weixin.Config{BaseURL: failing.URL, HTTPClient: failing.Client()})
	if _, err := (&typingCache{tickets: map[string]string{}}).ticket(t.Context(), failClient, "u2", "ctx"); err == nil {
		t.Errorf("取票据失败应上抛错误")
	}
}
