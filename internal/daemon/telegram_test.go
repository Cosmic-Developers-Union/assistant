package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

)

// newFakeTelegram 返回最小 Bot API 服务端：第一批 getUpdates 投递一条私聊文本，
// 之后空轮询；sendMessage 记录请求体。
func newFakeTelegram(t *testing.T, fromUserID int64, text string) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var sent []map[string]any
	var updatesCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /bottok-1/getUpdates", func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		updatesCalls++
		first := updatesCalls == 1
		mu.Unlock()
		if first {
			_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true, "result": []any{map[string]any{
				"update_id": 1,
				"message": map[string]any{
					"message_id": 1, "text": text,
					"from": map[string]any{"id": fromUserID, "username": "alice"},
					"chat": map[string]any{"id": fromUserID, "type": "private"},
				},
			}}})
			return
		}
		time.Sleep(30 * time.Millisecond)
		_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true, "result": []any{}})
	})
	mux.HandleFunc("POST /bottok-1/sendMessage", func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		sent = append(sent, payload)
		mu.Unlock()
		_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /bottok-1/sendChatAction", func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	snapshot := func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), sent...)
	}
	return server, snapshot
}

func newTelegramChannelForTest(t *testing.T, server *httptest.Server, adminUsers []string) (*TelegramChannel, *Chat) {
	t.Helper()
	channel := NewTelegramChannel(TelegramChannelConfig{
		BotToken:   "tok-1",
		APIBaseURL: server.URL,
		AdminUsers: adminUsers,
		HTTPClient: server.Client(),
	}, t.Logf)
	chat, err := NewChat(ChatConfig{
		StateDir:   t.TempDir(),
		SessionDir: t.TempDir(),
		RunClaude: func(_ context.Context, _ string, _ []string, _ string, _ []string) ([]byte, error) {
			return []byte(`{"subtype":"success","is_error":false,"result":"好的"}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return channel, chat
}

func runTelegram(t *testing.T, channel *TelegramChannel, chat *Chat) (context.CancelFunc, chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunChannel(ctx, channel, ChannelConfig{Chat: chat, Log: t.Logf})
	}()
	return cancel, done
}

// 私聊：白名单用户 → claude 回复经 sendMessage 发回原会话；会话键为通道实例键。
func TestTelegramChannelReplies(t *testing.T) {
	server, sent := newFakeTelegram(t, 7, "状态")
	channel, chat := newTelegramChannelForTest(t, server, []string{"7"})
	cancel, done := runTelegram(t, channel, chat)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(sent()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	messages := sent()
	if len(messages) == 0 {
		t.Fatal("未发送回复")
	}
	if messages[0]["chat_id"] != "7" || messages[0]["text"] != "好的" {
		t.Errorf("sendMessage = %+v", messages[0])
	}
	if channel.Name() != "telegram" {
		t.Errorf("Name = %q", channel.Name())
	}
	if _, err := chat.ConversationFor("telegram", "7"); err != nil {
		t.Errorf("ConversationFor: %v", err)
	}
}

// 白名单外用户：不回复。
func TestTelegramChannelDenies(t *testing.T) {
	server, sent := newFakeTelegram(t, 999, "你好")
	channel, chat := newTelegramChannelForTest(t, server, []string{"7"})
	cancel, done := runTelegram(t, channel, chat)
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	if messages := sent(); len(messages) != 0 {
		t.Errorf("白名单外用户不应回复：%v", messages)
	}
}

// 命名实例的键：NewTelegramChannel 空 name 回退 telegram，显式 key 原样保留。
func TestTelegramChannelKey(t *testing.T) {
	channel := NewTelegramChannel(TelegramChannelConfig{BotToken: "t"}, nil)
	if channel.Name() != "telegram" {
		t.Errorf("缺省键 = %q", channel.Name())
	}
	channel = NewTelegramChannel(TelegramChannelConfig{Name: "telegram/alt", BotToken: "t"}, nil)
	if channel.Name() != "telegram/alt" {
		t.Errorf("命名键 = %q", channel.Name())
	}
	if channel.SplitLimit() != DefaultTelegramSplitLimit {
		t.Errorf("SplitLimit = %d", channel.SplitLimit())
	}
	if allowed, reason := channel.Allowed(""); allowed || !strings.Contains(reason, "用户标识") {
		t.Errorf("空用户应拒绝：%v %q", allowed, reason)
	}
}
