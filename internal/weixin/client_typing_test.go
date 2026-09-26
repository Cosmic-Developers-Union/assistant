package weixin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient 指向假服务并记录每个请求，供各方法的契约断言复用。
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient(Config{
		BaseURL:        server.URL,
		BotToken:       "bot-token",
		ChannelVersion: "1.2.3",
		BotAgent:       "assistant-test",
		HTTPClient:     server.Client(),
	})
	return client, server
}

// GetTypingTicket：路径与请求体契约（ilink_user_id / base_info），
// context_token 只在非空时带上。
func TestGetTypingTicket(t *testing.T) {
	t.Run("带 context_token", func(t *testing.T) {
		var gotPath string
		var gotBody map[string]any
		client, _ := newTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
			gotPath = request.URL.Path
			raw, _ := io.ReadAll(request.Body)
			_ = json.Unmarshal(raw, &gotBody)
			_ = json.NewEncoder(writer).Encode(map[string]any{"ret": 0, "typing_ticket": "ticket-1"})
		})

		ticket, err := client.GetTypingTicket(t.Context(), "u1", "ctx-1")
		if err != nil {
			t.Fatalf("GetTypingTicket() error = %v", err)
		}
		if ticket != "ticket-1" {
			t.Errorf("ticket = %q, want ticket-1", ticket)
		}
		if gotPath != "/ilink/bot/getconfig" {
			t.Errorf("path = %q, want /ilink/bot/getconfig", gotPath)
		}
		if gotBody["ilink_user_id"] != "u1" {
			t.Errorf("ilink_user_id = %v, want u1", gotBody["ilink_user_id"])
		}
		if gotBody["context_token"] != "ctx-1" {
			t.Errorf("context_token = %v, want ctx-1", gotBody["context_token"])
		}
		base, _ := gotBody["base_info"].(map[string]any)
		if base["channel_version"] != "1.2.3" || base["bot_agent"] != "assistant-test" {
			t.Errorf("base_info = %v, want config 里的版本与 agent", base)
		}
	})

	t.Run("空 context_token 不带该字段", func(t *testing.T) {
		var gotBody map[string]any
		client, _ := newTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
			raw, _ := io.ReadAll(request.Body)
			_ = json.Unmarshal(raw, &gotBody)
			_ = json.NewEncoder(writer).Encode(map[string]any{"ret": 0, "typing_ticket": "t"})
		})

		if _, err := client.GetTypingTicket(t.Context(), "u1", ""); err != nil {
			t.Fatalf("GetTypingTicket() error = %v", err)
		}
		if _, present := gotBody["context_token"]; present {
			t.Errorf("空 context_token 不应出现在请求体：%v", gotBody)
		}
	})

	t.Run("服务端错误透传", func(t *testing.T) {
		client, _ := newTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`boom`))
		})
		if _, err := client.GetTypingTicket(t.Context(), "u1", "ctx"); err == nil {
			t.Error("GetTypingTicket() error = nil, want 失败")
		}
	})
}

// SendTyping：状态值原样送达（TypingOn/TypingOff 是协议枚举，不能改写）。
func TestSendTyping(t *testing.T) {
	for _, status := range []int{TypingOn, TypingOff} {
		var gotBody map[string]any
		client, _ := newTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/ilink/bot/sendtyping" {
				t.Errorf("path = %q, want /ilink/bot/sendtyping", request.URL.Path)
			}
			raw, _ := io.ReadAll(request.Body)
			_ = json.Unmarshal(raw, &gotBody)
			_ = json.NewEncoder(writer).Encode(map[string]any{"ret": 0})
		})

		if err := client.SendTyping(t.Context(), "u1", "ticket-1", status); err != nil {
			t.Fatalf("SendTyping(%d) error = %v", status, err)
		}
		if got, _ := gotBody["status"].(float64); int(got) != status {
			t.Errorf("status = %v, want %d", gotBody["status"], status)
		}
		if gotBody["typing_ticket"] != "ticket-1" {
			t.Errorf("typing_ticket = %v", gotBody["typing_ticket"])
		}
	}
}

// Notify：上线与下线走不同路径（notifystart / notifystop），ret 非 0 要报错。
func TestNotify(t *testing.T) {
	for _, test := range []struct {
		name     string
		start    bool
		wantPath string
	}{
		{name: "上线", start: true, wantPath: "/ilink/bot/msg/notifystart"},
		{name: "下线", start: false, wantPath: "/ilink/bot/msg/notifystop"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gotPath string
			client, _ := newTestClient(t, func(writer http.ResponseWriter, request *http.Request) {
				gotPath = request.URL.Path
				_ = json.NewEncoder(writer).Encode(map[string]any{"ret": 0})
			})

			if err := client.Notify(t.Context(), test.start); err != nil {
				t.Fatalf("Notify() error = %v", err)
			}
			if gotPath != test.wantPath {
				t.Errorf("path = %q, want %q", gotPath, test.wantPath)
			}
		})
	}

	t.Run("ret 非 0 报错并带 errmsg", func(t *testing.T) {
		client, _ := newTestClient(t, func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"ret": 5, "errmsg": "not allowed"})
		})
		err := client.Notify(t.Context(), true)
		if err == nil {
			t.Fatal("Notify() error = nil, want ret 非 0 的错误")
		}
		if !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("error = %v, want 含服务端 errmsg", err)
		}
	})
}

// normalizeBaseURL：缺 scheme 时补 https；已有 scheme 原样保留；去首尾空白。
func TestNormalizeBaseURL(t *testing.T) {
	for _, test := range []struct {
		host string
		want string
	}{
		{host: "api.example.com", want: "https://api.example.com"},
		{host: "  api.example.com  ", want: "https://api.example.com"},
		{host: "https://api.example.com", want: "https://api.example.com"},
		{host: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{host: "", want: "https://"},
	} {
		if got := normalizeBaseURL(test.host); got != test.want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", test.host, got, test.want)
		}
	}
}

// firstNonEmpty：取第一个非空白值。
func TestFirstNonEmpty(t *testing.T) {
	for _, test := range []struct {
		name   string
		values []string
		want   string
	}{
		{name: "无参数", values: nil, want: ""},
		{name: "全空白", values: []string{"", "  "}, want: ""},
		{name: "取第一个", values: []string{"", "a", "b"}, want: "a"},
		{name: "去空白", values: []string{"  a  "}, want: "a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := firstNonEmpty(test.values...); got != test.want {
				t.Errorf("firstNonEmpty(%q) = %q, want %q", test.values, got, test.want)
			}
		})
	}
}
