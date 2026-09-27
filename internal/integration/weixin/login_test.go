package weixin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginFlow(t *testing.T) {
	old := PollInterval
	PollInterval = 5 * time.Millisecond
	defer func() { PollInterval = old }()

	step := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"qrcode": "qr-1", "qrcode_img_content": "https://example.com/qr.png",
			})
		case "/ilink/bot/get_qrcode_status":
			step++
			switch {
			case request.URL.Query().Get("verify_code") == "123456":
				_ = json.NewEncoder(writer).Encode(map[string]any{
					"status": "confirmed", "bot_token": "tok", "ilink_bot_id": "bot-1",
					"baseurl": "https://ilink.example.com", "ilink_user_id": "user-1",
				})
			case step == 1:
				_ = json.NewEncoder(writer).Encode(map[string]any{"status": "wait"})
			default:
				_ = json.NewEncoder(writer).Encode(map[string]any{"status": "need_verifycode"})
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	var qr QRCode
	credentials, err := Login(context.Background(), server.URL, func(code QRCode) { qr = code },
		func(int) (string, error) { return "123456", nil }, server.Client())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if qr.Value != "qr-1" || qr.Content == "" {
		t.Errorf("QRCode = %+v", qr)
	}
	if credentials.BotToken != "tok" || credentials.BotID != "bot-1" ||
		credentials.BaseURL != "https://ilink.example.com" || credentials.UserID != "user-1" {
		t.Errorf("credentials = %+v", credentials)
	}
}
