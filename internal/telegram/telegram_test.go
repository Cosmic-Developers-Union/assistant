package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI 是最小 Bot API 服务端：所有方法挂在 /bot<token>/ 下，记录请求体。
type fakeAPI struct {
	server    *httptest.Server
	bodies    map[string][]map[string]any
	sent      []map[string]any
	getMeOK   bool
	mu        sync.Mutex
	t         *testing.T
	updatesFn func(call int) []map[string]any
}

func newFakeAPI(t *testing.T, token string) *fakeAPI {
	fake := &fakeAPI{t: t, bodies: map[string][]map[string]any{}, getMeOK: true}
	mux := http.NewServeMux()
	prefix := "/bot" + token + "/"
	handle := func(method string, handler func(writer http.ResponseWriter, request *http.Request)) {
		mux.HandleFunc("POST "+prefix+method, handler)
	}
	record := func(method string, request *http.Request) map[string]any {
		body := make([]byte, 16<<10)
		n, _ := request.Body.Read(body)
		var payload map[string]any
		_ = json.Unmarshal(body[:n], &payload)
		fake.mu.Lock()
		fake.bodies[method] = append(fake.bodies[method], payload)
		fake.mu.Unlock()
		return payload
	}
	handle("getMe", func(writer http.ResponseWriter, request *http.Request) {
		record("getMe", request)
		if !fake.getMeOK {
			_ = json.NewEncoder(writer).Encode(map[string]any{"ok": false, "description": "Unauthorized"})
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true, "result": map[string]any{"id": 42, "username": "assistant_bot"}})
	})
	calls := 0
	handle("getUpdates", func(writer http.ResponseWriter, request *http.Request) {
		payload := record("getUpdates", request)
		_ = payload
		fake.mu.Lock()
		calls++
		batch := fake.updatesFn(calls)
		fake.mu.Unlock()
		if batch == nil {
			time.Sleep(20 * time.Millisecond)
			batch = []map[string]any{}
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true, "result": batch})
	})
	handle("sendMessage", func(writer http.ResponseWriter, request *http.Request) {
		payload := record("sendMessage", request)
		fake.mu.Lock()
		fake.sent = append(fake.sent, payload)
		fake.mu.Unlock()
		_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 1}})
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeAPI) callsOf(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies[method])
}

func (f *fakeAPI) bodyOf(method string, call int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[method][call-1]
}

func (f *fakeAPI) sentSnapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sent...)
}

func newTestClient(fake *fakeAPI) *Client {
	return NewClient(Config{
		BotToken:   "tok-1",
		APIBaseURL: fake.server.URL,
		HTTPClient: fake.server.Client(),
	})
}

// 长轮询：首批取到消息后 offset 推进；下一轮带 offset=last+1 确认消费；
// sendMessage 原样传 chat_id/text。
func TestGetUpdatesOffsetAndSend(t *testing.T) {
	fake := newFakeAPI(t, "tok-1")
	fake.updatesFn = func(call int) []map[string]any {
		if call != 1 {
			return nil
		}
		return []map[string]any{
			{"update_id": 100, "message": map[string]any{
				"message_id": 1, "text": "你好",
				"from": map[string]any{"id": 7, "username": "alice", "first_name": "Alice"},
				"chat": map[string]any{"id": 7, "type": "private"},
			}},
			{"update_id": 101, "message": map[string]any{
				"message_id": 2, "text": "群里问",
				"from": map[string]any{"id": 8},
				"chat": map[string]any{"id": -100200, "type": "supergroup"},
			}},
		}
	}
	client := newTestClient(fake)
	ctx := context.Background()

	updates, err := client.GetUpdates(ctx)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(updates) != 2 || updates[0].Message.Text != "你好" || updates[1].Message.Chat.ID != -100200 {
		t.Fatalf("updates = %+v", updates)
	}
	if client.offset != 101 {
		t.Errorf("offset = %d, want 101", client.offset)
	}
	if _, err := client.GetUpdates(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fake.bodyOf("getUpdates", 2)["offset"]; got != float64(102) {
		t.Errorf("第二轮 offset = %v, want 102（last+1 确认消费）", got)
	}

	if err := client.SendMessage(ctx, FormatChatID(-100200), "群里好"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	sent := fake.sentSnapshot()
	if len(sent) != 1 || sent[0]["chat_id"] != "-100200" || sent[0]["text"] != "群里好" {
		t.Errorf("sendMessage body = %+v", sent)
	}
}

// getMe 自检与错误传播。
func TestGetMeAndErrors(t *testing.T) {
	fake := newFakeAPI(t, "tok-1")
	client := newTestClient(fake)
	me, err := client.GetMe(context.Background())
	if err != nil || me.Username != "assistant_bot" || me.ID != 42 {
		t.Fatalf("GetMe = %+v err=%v", me, err)
	}
	fake.getMeOK = false
	if _, err := client.GetMe(context.Background()); err == nil {
		t.Fatal("ok=false 应报错")
	} else if !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("错误应带平台描述：%v", err)
	}
}

