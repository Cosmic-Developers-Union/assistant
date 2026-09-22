package qq

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeAPI 是最小 QQ API 服务端：token（计数）、gateway、消息发送记录与可替换
// 的 WebSocket 网关处理器，全部挂在同一个地址上。
type fakeAPI struct {
	server     *httptest.Server
	sent       []map[string]any
	tokenCalls int32
	// wsHandler 是 GET /ws 的处理器（每个测试自带）
	wsHandler func(t *testing.T, conn *websocket.Conn)

	mu sync.Mutex
	t  *testing.T
}

func newFakeAPI(t *testing.T) *fakeAPI {
	fake := &fakeAPI{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil || request.Form.Get("appId") != "app-1" || request.Form.Get("clientSecret") != "sec-1" {
			http.Error(writer, "bad credentials", http.StatusBadRequest)
			return
		}
		fake.mu.Lock()
		fake.tokenCalls++
		count := fake.tokenCalls
		fake.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "tok-" + strconv.FormatInt(int64(count), 10), "expires_in": "7200"})
	})
	mux.HandleFunc("GET /gateway", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"url": wsURL(fake.server.URL) + "/ws"})
	})
	mux.HandleFunc("/v2/users/", fake.handleSend)
	mux.HandleFunc("/v2/groups/", fake.handleSend)
	mux.HandleFunc("GET /ws", func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		fake.wsHandler(t, conn)
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeAPI) handleSend(writer http.ResponseWriter, request *http.Request) {
	body := make([]byte, 8192)
	n, _ := request.Body.Read(body)
	var payload map[string]any
	_ = json.Unmarshal(body[:n], &payload)
	f.mu.Lock()
	f.sent = append(f.sent, payload)
	f.mu.Unlock()
	if strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ") == "" {
		http.Error(writer, "missing token", http.StatusUnauthorized)
		return
	}
	writer.WriteHeader(http.StatusOK)
}

func (f *fakeAPI) sentSnapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sent...)
}

func (f *fakeAPI) tokenCallCount() int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

func newTestClient(fake *fakeAPI) *Client {
	return NewClient(Config{
		AppID:      "app-1",
		AppSecret:  "sec-1",
		APIBaseURL: fake.server.URL,
		TokenURL:   fake.server.URL + "/token",
		HTTPClient: fake.server.Client(),
	})
}

// wsURL 把 httptest 的 http 地址换成 ws 地址。
func wsURL(httpURL string) string { return "ws" + strings.TrimPrefix(httpURL, "http") }

// writeFrameMarshalled 给测试用的网关帧发送。
func writeFrameMarshalled(t *testing.T, conn *websocket.Conn, f frame) {
	t.Helper()
	data, err := jsonv2.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, data); err != nil {
		t.Errorf("写帧失败（op=%d）：%v", f.Op, err)
	}
}

// readFrameTimeout 带超时读一帧（服务端视角）。
func readFrameTimeout(t *testing.T, conn *websocket.Conn, timeout time.Duration) (frame, bool) {
	t.Helper()
	readCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		return frame{}, false
	}
	var f frame
	if err := jsonv2.Unmarshal(data, &f); err != nil {
		t.Fatalf("测试服务端读帧解析失败：%v", err)
	}
	return f, true
}

// 发消息：带 Bearer token、msg_type/msg_id/msg_seq 正确；token 被缓存复用。
func TestClientSendC2CCachesToken(t *testing.T) {
	fake := newFakeAPI(t)
	client := newTestClient(fake)
	ctx := context.Background()
	if err := client.SendC2CText(ctx, "QQ_u1", "m-1", 1, "你好"); err != nil {
		t.Fatalf("SendC2CText: %v", err)
	}
	if err := client.SendGroupText(ctx, "GG_1", "m-1", 2, "群里好"); err != nil {
		t.Fatalf("SendGroupText: %v", err)
	}
	sent := fake.sentSnapshot()
	if len(sent) != 2 {
		t.Fatalf("sent = %d", len(sent))
	}
	if sent[0]["content"] != "你好" || sent[0]["msg_id"] != "m-1" || sent[0]["msg_seq"] != float64(1) || sent[0]["msg_type"] != float64(0) {
		t.Errorf("C2C body = %+v", sent[0])
	}
	if sent[1]["msg_seq"] != float64(2) {
		t.Errorf("Group body = %+v", sent[1])
	}
	if got := fake.tokenCallCount(); got != 1 {
		t.Errorf("token 应被缓存复用：calls = %d", got)
	}
	if !strings.Contains(client.tokens.token, "tok-") {
		t.Errorf("token 未缓存：%q", client.tokens.token)
	}
}

// token 管理：有效期内复用，force 强制刷新。
func TestTokenManagerRefresh(t *testing.T) {
	fake := newFakeAPI(t)
	client := newTestClient(fake)
	ctx := context.Background()
	first, err := client.tokens.get(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	again, err := client.tokens.get(ctx, false)
	if err != nil || again != first {
		t.Fatalf("缓存失效：%q vs %q (%v)", again, first, err)
	}
	if _, err := client.tokens.get(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := fake.tokenCallCount(); got != 2 {
		t.Errorf("token calls = %d, want 2", got)
	}
}

// 网关：Hello → Identify（token + intents + shard）→ READY/派发事件顺序回调，
// 心跳带最新 seq 且得到 ACK 后连接保持。
func TestGatewayIdentifyAndDispatch(t *testing.T) {
	fake := newFakeAPI(t)
	events := make(chan frame, 8)
	fake.wsHandler = func(t *testing.T, conn *websocket.Conn) {
		defer conn.Close(websocket.StatusNormalClosure, "")
		writeFrameMarshalled(t, conn, frame{Op: opHello, D: json.RawMessage(`{"heartbeat_interval":80}`)})
		identify, ok := readFrameTimeout(t, conn, 2*time.Second)
		if !ok {
			return
		}
		if identify.Op != opIdentify {
			t.Errorf("首帧应为 Identify，got op=%d", identify.Op)
			return
		}
		var payload struct {
			Token   string `json:"token"`
			Intents int    `json:"intents"`
			Shard   []int  `json:"shard"`
		}
		_ = jsonv2.Unmarshal(identify.D, &payload)
		if payload.Token != "QQBot tok-1" || payload.Intents != IntentGroupAndC2CEvent || len(payload.Shard) != 2 {
			t.Errorf("Identify payload = %+v", payload)
		}
		writeFrameMarshalled(t, conn, frame{Op: opDispatch, S: 1, T: EventReady,
			D: json.RawMessage(`{"session_id":"sess-abcdef123456","user":{"id":"bot"}}`)})
		writeFrameMarshalled(t, conn, frame{Op: opDispatch, S: 2, T: EventGroupAtMessage,
			D: json.RawMessage(`{"id":"m-1","content":" hi ","author":{"member_openid":"u1"}}`)})
		// 心跳应答循环：验证心跳带最新 seq，连接保持到测试结束
		for {
			next, ok := readFrameTimeout(t, conn, 2*time.Second)
			if !ok {
				return
			}
			if next.Op == opHeartbeat {
				var seq int
				_ = jsonv2.Unmarshal(next.D, &seq)
				if seq != 2 {
					t.Errorf("心跳 seq = %d, want 2", seq)
				}
				writeFrameMarshalled(t, conn, frame{Op: opHeartbeatACK})
				select {
				case events <- frame{T: "HB"}:
				default:
				}
			}
		}
	}
	client := newTestClient(fake)
	gateway := NewGateway(client, IntentGroupAndC2CEvent)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- gateway.Run(ctx, func(eventType string, payload json.RawMessage) {
			events <- frame{T: eventType, D: payload}
		})
	}()

	waitEvent := func(wantType string) frame {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case event := <-events:
				if event.T == wantType {
					return event
				}
			case <-deadline:
				t.Fatalf("等待事件 %s 超时", wantType)
			}
		}
	}
	// READY 是网关的会话管理事件（不回调 onEvent），用网关状态断言
	dispatched := waitEvent(EventGroupAtMessage)
	var message struct {
		ID      string `json:"id"`
		Content string `json:"content"`
		Author  struct {
			MemberOpenID string `json:"member_openid"`
		} `json:"author"`
	}
	if err := jsonv2.Unmarshal(dispatched.D, &message); err != nil {
		t.Fatal(err)
	}
	if message.ID != "m-1" || message.Author.MemberOpenID != "u1" {
		t.Errorf("派发事件 payload = %+v", message)
	}
	// 心跳往返至少一次（连接保持）
	waitEvent("HB")
	if gateway.sessionID != "sess-abcdef123456" || gateway.lastSeq.Load() != 2 {
		t.Errorf("会话状态未记录：session=%q seq=%d", gateway.sessionID, gateway.lastSeq.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未退出")
	}
}

// token 错误分类：凭据/参数被拒（2xx + 平台业务码、4xx）是致命错误；限流、
// 5xx 与未知响应形态维持可重试的普通错误。
func TestTokenErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantFatal bool
	}{
		{"平台业务码拒绝", http.StatusOK, `{"code":100007,"message":"appid invalid"}`, true},
		{"HTTP 401 被拒", http.StatusUnauthorized, `{"message":"unauthorized"}`, true},
		{"HTTP 429 限流", http.StatusTooManyRequests, `{"code":11253,"message":"too many requests"}`, false},
		{"HTTP 500", http.StatusInternalServerError, `{}`, false},
		{"未知响应形态", http.StatusOK, `{}`, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(testCase.status)
				_, _ = writer.Write([]byte(testCase.body))
			}))
			t.Cleanup(server.Close)
			client := NewClient(Config{
				AppID:      "app-1",
				AppSecret:  "sec-1",
				APIBaseURL: server.URL,
				TokenURL:   server.URL + "/token",
				HTTPClient: server.Client(),
			})
			err := client.VerifyCredential(context.Background())
			if err == nil {
				t.Fatal("应报错")
			}
			if _, fatal := errors.AsType[*FatalError](err); fatal != testCase.wantFatal {
				t.Errorf("致命判定 = %v（want %v）：err = %v", fatal, testCase.wantFatal, err)
			}
		})
	}
}

// 凭据被平台拒绝时网关不退避：Run 立即透传致命错误返回，token 端点只请求一次。
func TestGatewayStopsOnFatalCredentialError(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"code":100007,"message":"appid invalid"}`))
	}))
	t.Cleanup(server.Close)
	client := NewClient(Config{
		AppID:      "app-1",
		AppSecret:  "sec-1",
		APIBaseURL: server.URL,
		TokenURL:   server.URL + "/token",
		HTTPClient: server.Client(),
	})
	gateway := NewGateway(client, IntentGroupAndC2CEvent)
	done := make(chan error, 1)
	go func() {
		done <- gateway.Run(context.Background(), func(string, json.RawMessage) {})
	}()
	select {
	case err := <-done:
		if _, fatal := errors.AsType[*FatalError](err); !fatal {
			t.Errorf("Run 应透传致命错误，got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未停止（致命错误不应退避重试）")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("token 端点应只请求一次（不重试），got %d", got)
	}
}

// 网关断线自动重连：第二条连接应走 Resume（带 session_id 与最新 seq）。
func TestGatewayResumeAfterDrop(t *testing.T) {
	fake := newFakeAPI(t)
	resumed := make(chan struct{}, 1)
	var connections int32
	var mu sync.Mutex
	fake.wsHandler = func(t *testing.T, conn *websocket.Conn) {
		mu.Lock()
		connections++
		which := connections
		mu.Unlock()
		defer conn.Close(websocket.StatusNormalClosure, "")
		writeFrameMarshalled(t, conn, frame{Op: opHello, D: json.RawMessage(`{"heartbeat_interval":30000}`)})
		first, ok := readFrameTimeout(t, conn, 2*time.Second)
		if !ok {
			return
		}
		switch which {
		case 1:
			if first.Op != opIdentify {
				t.Errorf("第一条连接应 Identify，got op=%d", first.Op)
				return
			}
			writeFrameMarshalled(t, conn, frame{Op: opDispatch, S: 1, T: EventReady,
				D: json.RawMessage(`{"session_id":"sess-resume01","user":{"id":"bot"}}`)})
			// 直接断开：客户端应自动重连并 Resume
		case 2:
			if first.Op != opResume {
				t.Errorf("第二条连接应 Resume，got op=%d", first.Op)
				return
			}
			var payload struct {
				Token     string `json:"token"`
				SessionID string `json:"session_id"`
				Seq       int    `json:"seq"`
			}
			_ = jsonv2.Unmarshal(first.D, &payload)
			if payload.SessionID != "sess-resume01" || payload.Seq != 1 {
				t.Errorf("Resume payload = %+v", payload)
			}
			writeFrameMarshalled(t, conn, frame{Op: opDispatch, S: 0, T: EventResumed, D: json.RawMessage(`null`)})
			select {
			case resumed <- struct{}{}:
			default:
			}
			// 保持连接到测试结束
			for {
				if _, ok := readFrameTimeout(t, conn, 500*time.Millisecond); !ok {
					return
				}
			}
		}
	}
	client := newTestClient(fake)
	gateway := NewGateway(client, IntentGroupAndC2CEvent)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- gateway.Run(ctx, func(string, json.RawMessage) {})
	}()
	select {
	case <-resumed:
	case <-time.After(5 * time.Second):
		t.Fatal("未观察到 Resume")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未退出")
	}
}
