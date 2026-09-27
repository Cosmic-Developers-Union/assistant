// Package qqtestsupport 提供 QQ 集成的测试替身：最小 QQ API 服务端（token、
// gateway、发送记录）与 WebSocket 帧读写助手。
//
// 单独成包是因为它同时被 internal/integration/qq（适配器测试）与
// internal/daemon（适配器 + 对话会话的联合测试）使用——放在任一方的 _test.go
// 里另一方就够不着，复制一份又会漂移。
package qqtestsupport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// newFakeQQ 返回最小 QQ API 服务端：token、gateway、发送记录，以及由测试注入
// 的 WS 网关行为（默认 Hello → READY → 心跳应答）。
type Fake struct {
	server *httptest.Server
	sent   []map[string]any
	// paths 与 sent 一一对应，记录每次发送的请求路径——收信方（user_openid /
	// group_openid）编在 URL 路径里，不在请求体中，只比对 body 看不出私聊、群聊
	// 是否打对了接口。
	paths  []string
	tokens int32

	mu     sync.Mutex
	ws     func(t *testing.T, conn *websocket.Conn)
	cancel context.CancelFunc
}

func New(t *testing.T) *Fake {
	fake := &Fake{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(writer http.ResponseWriter, request *http.Request) {
		_ = request.ParseForm()
		fake.mu.Lock()
		fake.tokens++
		count := fake.tokens
		fake.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "tok-" + strconv.FormatInt(int64(count), 10), "expires_in": "7200"})
	})
	mux.HandleFunc("GET /gateway", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"url": "ws" + strings.TrimPrefix(fake.server.URL, "http") + "/ws"})
	})
	mux.HandleFunc("/v2/", func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		fake.mu.Lock()
		fake.sent = append(fake.sent, payload)
		fake.paths = append(fake.paths, request.URL.Path)
		fake.mu.Unlock()
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /ws", func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		fake.RunWS(t, conn)
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// runWS 执行注入的网关行为；未注入时用默认：Hello → READY → 心跳应答循环。
func (f *Fake) RunWS(t *testing.T, conn *websocket.Conn) {
	if handler := f.WSHandler(); handler != nil {
		handler(t, conn)
		return
	}
	WriteFrame(t, conn, `{"op":10,"d":{"heartbeat_interval":80}}`)
	ReadFrame(t, conn, 2*time.Second) // Identify
	WriteFrame(t, conn, `{"op":0,"s":1,"t":"READY","d":{"session_id":"sess-qq","user":{"id":"bot"}}}`)
	for {
		next, ok := ReadFrame(t, conn, 2*time.Second)
		if !ok {
			return
		}
		var frame struct {
			Op int `json:"op"`
		}
		_ = json.Unmarshal(next, &frame)
		if frame.Op == 1 {
			WriteFrame(t, conn, `{"op":11}`)
		}
	}
}

func (f *Fake) WSHandler() func(t *testing.T, conn *websocket.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ws
}

func (f *Fake) SetWS(handler func(t *testing.T, conn *websocket.Conn)) {
	f.mu.Lock()
	f.ws = handler
	f.mu.Unlock()
}

// Server 返回底层 httptest 服务端（少数测试要改 Handler）。
func (f *Fake) Server() *httptest.Server { return f.server }

// URL 返回假服务端的根地址（配置客户端指向它）。
func (f *Fake) URL() string { return f.server.URL }

// Client 返回假服务端的 HTTP 客户端。
func (f *Fake) Client() *http.Client { return f.server.Client() }

func (f *Fake) SentPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func (f *Fake) SentSnapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sent...)
}

func (f *Fake) WaitSent(t *testing.T, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sent := f.SentSnapshot(); len(sent) >= want {
			return sent
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待第 %d 条发送超时：已发 %v", want, f.SentSnapshot())
	return nil
}

func WriteFrame(t *testing.T, conn *websocket.Conn, payload string) {
	t.Helper()
	writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, []byte(payload)); err != nil {
		t.Errorf("写 WS 帧失败：%v", err)
	}
}

func ReadFrame(t *testing.T, conn *websocket.Conn, timeout time.Duration) ([]byte, bool) {
	readCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		return nil, false
	}
	return data, true
}
