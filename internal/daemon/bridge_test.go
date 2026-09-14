package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"assistant/internal/weixin"
)

// newFakeWeixin 返回一个最小 ilink 服务端：第一轮 getUpdates 投递一条文本消息，
// 之后返回空；sendmessage 记录请求体。
func newFakeWeixin(t *testing.T, fromUser string) (*httptest.Server, *[]map[string]any, *int32) {
	t.Helper()
	sent := &[]map[string]any{}
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
			*sent = append(*sent, payload)
			_ = json.NewEncoder(writer).Encode(map[string]any{"ret": 0})
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	return server, sent, &updatesCalls
}

func TestBridgeRepliesToLoginUser(t *testing.T) {
	server, sent, _ := newFakeWeixin(t, "user-1")
	chat, err := NewChat(ChatConfig{
		StateDir: t.TempDir(),
		RunClaude: func(_ context.Context, _ string, _ []string, _ string, _ []string) ([]byte, error) {
			return []byte(`{"subtype":"success","is_error":false,"result":"当前 1 个评审会话"}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunBridge(ctx, BridgeConfig{
			Weixin:      weixin.Config{BaseURL: server.URL, BotToken: "tok", HTTPClient: server.Client()},
			LoginUserID: "user-1",
			Chat:        chat,
			Log:         t.Logf,
		})
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(*sent) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if len(*sent) == 0 {
		t.Fatal("未发送回复")
	}
	message := (*sent)[0]["msg"].(map[string]any)
	if message["to_user_id"] != "user-1" || message["context_token"] != "ctx-1" {
		t.Errorf("sendmessage msg = %+v", message)
	}
	items := message["item_list"].([]any)
	text := items[0].(map[string]any)["text_item"].(map[string]any)["text"]
	if text != "当前 1 个评审会话" {
		t.Errorf("回复文本 = %v", text)
	}
}

func TestBridgeIgnoresOtherUsers(t *testing.T) {
	server, sent, _ := newFakeWeixin(t, "intruder")
	chat, err := NewChat(ChatConfig{
		StateDir:  t.TempDir(),
		RunClaude: func(context.Context, string, []string, string, []string) ([]byte, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunBridge(ctx, BridgeConfig{
			Weixin:      weixin.Config{BaseURL: server.URL, BotToken: "tok", HTTPClient: server.Client()},
			LoginUserID: "user-1",
			Chat:        chat,
			Log:         t.Logf,
		})
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	if len(*sent) != 0 {
		t.Errorf("白名单外用户不应回复：%+v", *sent)
	}
}
