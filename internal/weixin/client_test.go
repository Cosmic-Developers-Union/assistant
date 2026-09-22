package weixin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientGetUpdatesAndSendText(t *testing.T) {
	var updatesAuth, updatesUIN, updatesAppID string
	var sendBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		switch request.URL.Path {
		case "/ilink/bot/getupdates":
			updatesAuth = request.Header.Get("Authorization")
			updatesUIN = request.Header.Get("X-WECHAT-UIN")
			updatesAppID = request.Header.Get("iLink-App-Id")
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ret": 0,
				"msgs": []any{map[string]any{
					"seq": 1, "from_user_id": "u1", "message_type": 1, "session_id": "s1",
					"context_token": "ctx-1",
					"item_list": []any{
						map[string]any{"type": 1, "text_item": map[string]any{"text": "你好"}},
						map[string]any{"type": 3, "voice_item": map[string]any{"text": "语音转写"}},
					},
				}},
				"get_updates_buf":        "cursor-2",
				"longpolling_timeout_ms": 35000,
			})
		case "/ilink/bot/sendmessage":
			_ = json.Unmarshal(body, &sendBody)
			_ = json.NewEncoder(writer).Encode(map[string]any{"ret": 0})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, BotToken: "bot-token", HTTPClient: server.Client()})
	updates, err := client.GetUpdates(context.Background(), "")
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(updates.Messages) != 1 || updates.Cursor != "cursor-2" {
		t.Fatalf("updates = %+v", updates)
	}
	if text := updates.Messages[0].Text(); text != "你好\n语音转写" {
		t.Errorf("Text() = %q", text)
	}
	if updatesAuth != "Bearer bot-token" || updatesUIN == "" || updatesAppID != "bot" {
		t.Errorf("头部 = auth:%q uin:%q app:%q", updatesAuth, updatesUIN, updatesAppID)
	}

	if err := client.SendText(context.Background(), "u1", "ctx-1", "回复内容"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	message := sendBody["msg"].(map[string]any)
	if message["to_user_id"] != "u1" || message["context_token"] != "ctx-1" || message["message_type"] != float64(2) {
		t.Errorf("sendMessage msg = %+v", message)
	}
	items := message["item_list"].([]any)
	first := items[0].(map[string]any)["text_item"].(map[string]any)
	if first["text"] != "回复内容" {
		t.Errorf("发送文本 = %+v", first)
	}
	if _, ok := sendBody["base_info"].(map[string]any); !ok {
		t.Errorf("缺少 base_info: %+v", sendBody)
	}
}

func TestSendTextChecksRet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"ret": 40001, "errmsg": "bad"})
	}))
	defer server.Close()
	client := NewClient(Config{BaseURL: server.URL, BotToken: "t", HTTPClient: server.Client()})
	err := client.SendText(context.Background(), "u", "", "x")
	if err == nil || !strings.Contains(err.Error(), "40001") {
		t.Fatalf("err = %v, want ret 40001", err)
	}
}

func TestEncodeClientVersion(t *testing.T) {
	if got := EncodeClientVersion("1.2.3"); got != "66051" {
		t.Errorf("EncodeClientVersion = %q, want 66051", got)
	}
	if got := EncodeClientVersion("0.0.1"); got != "1" {
		t.Errorf("EncodeClientVersion = %q, want 1", got)
	}
}

// 终端二维码渲染：非空内容输出半块字符，空内容报错（CLI 回退打印链接）。
func TestRenderQR(t *testing.T) {
	var output strings.Builder
	if err := RenderQR(&output, "https://example.com/qr?x=1"); err != nil {
		t.Fatalf("RenderQR: %v", err)
	}
	rendered := output.String()
	if len(rendered) < 200 {
		t.Errorf("渲染结果太短：%d 字节", len(rendered))
	}
	if !strings.ContainsAny(rendered, "▄▀█") {
		t.Errorf("缺少二维码块字符：%q", rendered[:min(len(rendered), 80)])
	}
	if err := RenderQR(&output, "   "); err == nil {
		t.Error("空内容应报错")
	}
}
