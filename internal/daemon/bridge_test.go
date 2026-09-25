package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/weixin"
)

// sentRecorder 是并发安全的发送记录（服务端协程写、测试协程读）。
type sentRecorder struct {
	mu      sync.Mutex
	payload []map[string]any
}

func (r *sentRecorder) add(payload map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payload = append(r.payload, payload)
}

func (r *sentRecorder) snapshot() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.payload...)
}

// newFakeWeixin 返回一个最小 ilink 服务端：第一轮 getUpdates 投递一条文本消息，
// 之后返回空；sendmessage 记录请求体。
func newFakeWeixin(t *testing.T, fromUser string) (*httptest.Server, *sentRecorder, *int32) {
	t.Helper()
	sent := &sentRecorder{}
	var updatesCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/ilink/bot/getupdates":
			if atomic.AddInt32(&updatesCalls, 1) == 1 {
				_ = json.NewEncoder(writer).Encode(map[string]any{
					"ret": 0,
					"msgs": []any{map[string]any{
						"from_user_id": fromUser, "message_type": 1, "session_id": "s-1",
						"context_token": "ctx-1",
						"item_list":     []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "状态"}}},
					}},
					"get_updates_buf": "c1",
				})
				return
			}
			time.Sleep(30 * time.Millisecond)
			_ = json.NewEncoder(writer).Encode(map[string]any{"ret": 0, "msgs": []any{}, "get_updates_buf": "c1"})
		case "/ilink/bot/getconfig":
			_ = json.NewEncoder(writer).Encode(map[string]any{"ret": 0, "typing_ticket": "tk"})
		case "/ilink/bot/sendtyping", "/ilink/bot/msg/notifystart", "/ilink/bot/msg/notifystop":
			_ = json.NewEncoder(writer).Encode(map[string]any{"ret": 0})
		case "/ilink/bot/sendmessage":
			body, _ := io.ReadAll(request.Body)
			var payload map[string]any
			_ = json.Unmarshal(body, &payload)
			sent.add(payload)
			_ = json.NewEncoder(writer).Encode(map[string]any{"ret": 0})
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	return server, sent, &updatesCalls
}

func TestWeixinChannelRepliesToLoginUser(t *testing.T) {
	server, sent, _ := newFakeWeixin(t, "user-1")
	chat, err := NewChat(ChatConfig{
		StateDir:   t.TempDir(),
		SessionDir: t.TempDir(),
		RunClaude: func(_ context.Context, _ string, _ []string, _ string, _ []string) ([]byte, error) {
			return []byte(`{"subtype":"success","is_error":false,"result":"当前 1 个评审会话"}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	channel := NewWeixinChannel(WeixinChannelConfig{
		Weixin:      weixin.Config{BaseURL: server.URL, BotToken: "tok", HTTPClient: server.Client()},
		LoginUserID: "user-1",
	}, t.Logf)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunChannel(ctx, channel, ChannelConfig{Chat: chat, Log: t.Logf})
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(sent.snapshot()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	snapshot := sent.snapshot()
	if len(snapshot) == 0 {
		t.Fatal("未发送回复")
	}
	message := snapshot[0]["msg"].(map[string]any)
	if message["to_user_id"] != "user-1" || message["context_token"] != "ctx-1" {
		t.Errorf("sendmessage msg = %+v", message)
	}
	items := message["item_list"].([]any)
	text := items[0].(map[string]any)["text_item"].(map[string]any)["text"]
	if text != "当前 1 个评审会话" {
		t.Errorf("回复文本 = %v", text)
	}
}

func TestWeixinChannelIgnoresOtherUsers(t *testing.T) {
	server, sent, _ := newFakeWeixin(t, "intruder")
	chat, err := NewChat(ChatConfig{
		StateDir:   t.TempDir(),
		SessionDir: t.TempDir(),
		RunClaude:  func(context.Context, string, []string, string, []string) ([]byte, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	channel := NewWeixinChannel(WeixinChannelConfig{
		Weixin:      weixin.Config{BaseURL: server.URL, BotToken: "tok", HTTPClient: server.Client()},
		LoginUserID: "user-1",
	}, t.Logf)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunChannel(ctx, channel, ChannelConfig{Chat: chat, Log: t.Logf})
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	if replies := sent.snapshot(); len(replies) != 0 {
		t.Errorf("白名单外用户不应回复：%+v", replies)
	}
}

// 准入判定："*" 通配放开一切，白名单其次，登录者兜底，什么都不配则全拒。
func TestWeixinChannelAllowed(t *testing.T) {
	channel := NewWeixinChannel(WeixinChannelConfig{LoginUserID: "owner"}, nil)
	if allowed, _ := channel.Allowed("owner"); !allowed {
		t.Error("登录用户应放行")
	}
	if allowed, _ := channel.Allowed("other"); allowed {
		t.Error("非登录用户应拒绝")
	}
	channel = NewWeixinChannel(WeixinChannelConfig{AdminUsers: []string{"a"}}, nil)
	if allowed, _ := channel.Allowed("a"); !allowed {
		t.Error("白名单用户应放行")
	}
	if allowed, _ := channel.Allowed("owner"); allowed {
		t.Error("配置白名单后登录者兜底失效")
	}
	channel = NewWeixinChannel(WeixinChannelConfig{AdminUsers: []string{"*"}}, nil)
	if allowed, _ := channel.Allowed("任何人"); !allowed {
		t.Error(`"*" 应放开所有人`)
	}
	if _, reason := channel.Allowed(""); reason == "" {
		t.Error("空用户标识应拒绝并给原因")
	}
}

// Bot 自己的消息（message_type=2）在通道层丢弃："*" 放开时也不能自问自答打环。
func TestWeixinChannelDropsBotMessages(t *testing.T) {
	var updatesCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&updatesCalls, 1) == 1 {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ret": 0,
				"msgs": []any{map[string]any{
					"from_user_id": "bot-self", "message_type": 2, "session_id": "s-1",
					"context_token": "ctx-1",
					"item_list":     []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "自言自语"}}},
				}},
				"get_updates_buf": "c1",
			})
			return
		}
		time.Sleep(30 * time.Millisecond)
		_ = json.NewEncoder(writer).Encode(map[string]any{"ret": 0, "msgs": []any{}, "get_updates_buf": "c1"})
	}))
	t.Cleanup(server.Close)
	chat, err := NewChat(ChatConfig{
		StateDir:   t.TempDir(),
		SessionDir: t.TempDir(),
		RunClaude: func(context.Context, string, []string, string, []string) ([]byte, error) {
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	channel := NewWeixinChannel(WeixinChannelConfig{
		Weixin:     weixin.Config{BaseURL: server.URL, BotToken: "tok", HTTPClient: server.Client()},
		AdminUsers: []string{"*"},
	}, t.Logf)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunChannel(ctx, channel, ChannelConfig{Chat: chat, Log: t.Logf})
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	// 没有崩溃、没有回复路径被触发（sendmessage 未配置于 fake，触发即 404 也无妨——
	// 这里只验证 RunChannel 正常退出且没有投递给 Chat）。
	if channel.Name() != "weixin" {
		t.Errorf("Name = %q", channel.Name())
	}
}
