package qq

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
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
		// 官方端点只接受 JSON body（appId/clientSecret）；表单会被解析成空
		// appId，平台报 100007 invalid appid——即使凭据正确。
		var payload struct {
			AppID        string `json:"appId"`
			ClientSecret string `json:"clientSecret"`
		}
		body, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(body, &payload)
		if request.Header.Get("Content-Type") != "application/json" || payload.AppID != "app-1" || payload.ClientSecret != "sec-1" {
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
	// REST 鉴权头是 "QQBot <token>"（不是 Bearer）
	if !strings.HasPrefix(request.Header.Get("Authorization"), "QQBot ") {
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

// 发消息：带 QQBot 方案 token、msg_type/msg_id/msg_seq 正确；token 被缓存复用。
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

// token 请求回归测试：必须以 JSON body（{"appId","clientSecret"}）POST，并带
// User-Agent。发成表单会被平台解析成空 appId，返回 100007 invalid appid——
// 即使凭据正确（线上真实故障）。
func TestTokenRequestIsJSON(t *testing.T) {
	var gotContentType, gotUserAgent, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotContentType = request.Header.Get("Content-Type")
		gotUserAgent = request.Header.Get("User-Agent")
		body, _ := io.ReadAll(request.Body)
		gotBody = string(body)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"access_token":"tok-ok","expires_in":"7200"}`))
	}))
	t.Cleanup(server.Close)
	client := NewClient(Config{
		AppID:      "  app-1  ", // 平台侧容忍首尾空白（SDK 同样 trim）
		AppSecret:  "sec-1",
		APIBaseURL: server.URL,
		TokenURL:   server.URL + "/app/getAppAccessToken",
		HTTPClient: server.Client(),
	})
	if err := client.VerifyCredential(t.Context()); err != nil {
		t.Fatalf("VerifyCredential: %v", err)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotUserAgent == "" {
		t.Error("缺少 User-Agent")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("body 不是 JSON：%q（%v）", gotBody, err)
	}
	if payload["appId"] != "app-1" || payload["clientSecret"] != "sec-1" {
		t.Errorf("body = %s（appId 应去空白）", gotBody)
	}
}

// op 9 d=false（会话彻底作废）：丢弃会话、强制刷新 token，重连后走全新
// Identify（token 是重新获取的 tok-2）而不是 Resume。
func TestGatewayReidentifyAfterInvalidSession(t *testing.T) {
	fake := newFakeAPI(t)
	reidentified := make(chan identifyPayload, 1)
	var connections int32
	var mu sync.Mutex
	fake.wsHandler = func(t *testing.T, conn *websocket.Conn) {
		mu.Lock()
		connections++
		which := connections
		mu.Unlock()
		writeFrameMarshalled(t, conn, frame{Op: opHello, D: json.RawMessage(`{"heartbeat_interval":30000}`)})
		first, ok := readFrameTimeout(t, conn, 2*time.Second)
		if !ok {
			return
		}
		var payload identifyPayload
		_ = jsonv2.Unmarshal(first.D, &payload)
		switch which {
		case 1:
			if first.Op != opIdentify {
				t.Errorf("第一条连接应 Identify，got op=%d", first.Op)
				return
			}
			writeFrameMarshalled(t, conn, frame{Op: opDispatch, S: 1, T: EventReady,
				D: json.RawMessage(`{"session_id":"sess-dead0001","user":{"id":"bot"}}`)})
			// 会话彻底作废（d=false），随后断开
			writeFrameMarshalled(t, conn, frame{Op: opInvalidSession, D: json.RawMessage(`false`)})
			time.Sleep(100 * time.Millisecond)
			conn.Close(websocket.StatusNormalClosure, "")
		case 2:
			if first.Op != opIdentify {
				t.Errorf("op 9 d=false 后应重新 Identify，got op=%d", first.Op)
				return
			}
			select {
			case reidentified <- payload:
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
	case payload := <-reidentified:
		if payload.Token != "QQBot tok-2" {
			t.Errorf("重连应强制刷新 token，got %q", payload.Token)
		}
		if gateway.sessionID == "sess-dead0001" {
			t.Error("作废的会话应被丢弃")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未观察到重新 Identify")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未退出")
	}
}

// identifyPayload 是 Identify/Resume 帧的 d 载荷（测试断言用）。
type identifyPayload struct {
	Token     string `json:"token"`
	Intents   int    `json:"intents"`
	Shard     []int  `json:"shard"`
	SessionID string `json:"session_id"`
	Seq       int    `json:"seq"`
}

// 网关 close 4004（token 无效/权限不足）是致命错误：Run 立即透传，不退避重连。
func TestGatewayStopsOnClose4004(t *testing.T) {
	fake := newFakeAPI(t)
	fake.wsHandler = func(t *testing.T, conn *websocket.Conn) {
		writeFrameMarshalled(t, conn, frame{Op: opHello, D: json.RawMessage(`{"heartbeat_interval":30000}`)})
		readFrameTimeout(t, conn, 2*time.Second) // Identify
		conn.Close(gatewayCloseAuthFailed, "invalid token")
	}
	client := newTestClient(fake)
	gateway := NewGateway(client, IntentGroupAndC2CEvent)
	done := make(chan error, 1)
	go func() {
		done <- gateway.Run(context.Background(), func(string, json.RawMessage) {})
	}()
	select {
	case err := <-done:
		if _, fatal := errors.AsType[*FatalError](err); !fatal {
			t.Errorf("close 4004 应按致命错误返回，got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run 未停止（close 4004 不应退避重试）")
	}
}

// 401 的刷新重试：token 过期后平台回 401，客户端必须强制刷新一次并重发，
// 而不是把 401 当终态抛给上层（那会让用户看到「回复失败」而不是自动恢复）。
func TestClientRetriesAfter401(t *testing.T) {
	var attempts int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "tok-fresh", "expires_in": "7200"})
	})
	mux.HandleFunc("/v2/users/", func(writer http.ResponseWriter, request *http.Request) {
		// 第一次（旧 token）拒绝，刷新后放行
		if atomic.AddInt32(&attempts, 1) == 1 {
			http.Error(writer, "expired", http.StatusUnauthorized)
			return
		}
		if got := request.Header.Get("Authorization"); got != "QQBot tok-fresh" {
			t.Errorf("重试时 Authorization = %q, want QQBot tok-fresh", got)
		}
		writer.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := NewClient(Config{
		AppID: "app-1", AppSecret: "sec-1",
		APIBaseURL: server.URL, TokenURL: server.URL + "/token", HTTPClient: server.Client(),
	})

	if err := client.SendC2CText(t.Context(), "QQ_u1", "m-1", 1, "hi"); err != nil {
		t.Fatalf("SendC2CText() error = %v, want 401 后刷新重试成功", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("发送尝试次数 = %d, want 2（一次 401 + 一次重试）", got)
	}
}

// 刷新后仍 401：必须报错并点明「刷新后仍被拒」，否则无从区分凭据错与权限错。
func TestClientReportsPersistent401(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "tok-fresh", "expires_in": "7200"})
	})
	var attempts atomic.Int32
	mux.HandleFunc("/v2/groups/", func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(writer, "forbidden", http.StatusUnauthorized)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := NewClient(Config{
		AppID: "app-1", AppSecret: "sec-1",
		APIBaseURL: server.URL, TokenURL: server.URL + "/token", HTTPClient: server.Client(),
	})

	err := client.SendGroupText(t.Context(), "GG_1", "m-1", 1, "hi")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want 含 401 的发送失败", err)
	}
	// 重试过：token 端点收到 1 次常规 + 1 次强制刷新
	if got := attempts.Load(); got != 2 {
		t.Errorf("发送尝试次数 = %d, want 2", got)
	}
}

// 非 2xx 的错误信息要带状态码与响应体片段（运维据此判断是参数错还是权限错）。
func TestClientReportsSendFailureWithBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "tok-1", "expires_in": "7200"})
	})
	mux.HandleFunc("/v2/users/", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"code":11244,"message":"msg_id 重复"}`, http.StatusBadRequest)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := NewClient(Config{
		AppID: "app-1", AppSecret: "sec-1",
		APIBaseURL: server.URL, TokenURL: server.URL + "/token", HTTPClient: server.Client(),
	})

	err := client.SendC2CText(t.Context(), "QQ_u1", "m-1", 1, "hi")
	if err == nil {
		t.Fatal("error = nil, want 发送失败")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "msg_id 重复") {
		t.Errorf("error = %v, want 含状态码与响应体", err)
	}
}

// get 的失败分支与空响应体：空体不解析（不报「解析失败」），非 2xx 带路径。
func TestClientGetBranches(t *testing.T) {
	t.Run("非 2xx 带路径与响应体", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /token", func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "tok-1", "expires_in": "7200"})
		})
		mux.HandleFunc("GET /gateway", func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := NewClient(Config{
			AppID: "app-1", AppSecret: "sec-1",
			APIBaseURL: server.URL, TokenURL: server.URL + "/token", HTTPClient: server.Client(),
		})
		if _, err := client.GetGateway(t.Context()); err == nil ||
			!strings.Contains(err.Error(), "/gateway") || !strings.Contains(err.Error(), "503") {
			t.Errorf("GetGateway() error = %v, want 含路径与 503", err)
		}
	})

	t.Run("空响应体不触发解析错误", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /token", func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "tok-1", "expires_in": "7200"})
		})
		mux.HandleFunc("GET /empty", func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusOK)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := NewClient(Config{
			AppID: "app-1", AppSecret: "sec-1",
			APIBaseURL: server.URL, TokenURL: server.URL + "/token", HTTPClient: server.Client(),
		})
		var out struct {
			URL string `json:"url"`
		}
		if err := client.get(t.Context(), "/empty", &out); err != nil {
			t.Errorf("get() error = %v, want nil（空体跳过解析）", err)
		}
	})

	t.Run("响应体不是 JSON 时报解析错误", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /token", func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": "tok-1", "expires_in": "7200"})
		})
		mux.HandleFunc("GET /garbage", func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte("not json"))
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client := NewClient(Config{
			AppID: "app-1", AppSecret: "sec-1",
			APIBaseURL: server.URL, TokenURL: server.URL + "/token", HTTPClient: server.Client(),
		})
		var out struct {
			URL string `json:"url"`
		}
		if err := client.get(t.Context(), "/garbage", &out); err == nil {
			t.Error("get() error = nil, want 解析失败")
		}
	})
}

// HTTPClient 的缺省：未注入时回退 http.DefaultClient（网关拨号用）。
func TestHTTPClientFallsBackToDefault(t *testing.T) {
	client := NewClient(Config{AppID: "a", AppSecret: "s"})
	if client.HTTPClient() != http.DefaultClient {
		t.Error("未注入 client 时应回退 http.DefaultClient")
	}
	注入 := &http.Client{}
	withInjected := NewClient(Config{AppID: "a", AppSecret: "s", HTTPClient: 注入})
	if withInjected.HTTPClient() != 注入 {
		t.Error("注入的 client 应被原样返回（可能带自定义 Transport/超时）")
	}
}

// NewClient 的缺省填充：空 URL 落到官方端点，Log 为 nil 时给个空实现以免调用点
// 到处判空。
func TestNewClientDefaults(t *testing.T) {
	client := NewClient(Config{AppID: "a", AppSecret: "s"})
	if client.config.APIBaseURL != DefaultAPIBaseURL {
		t.Errorf("APIBaseURL = %q, want %q", client.config.APIBaseURL, DefaultAPIBaseURL)
	}
	if client.config.TokenURL != DefaultTokenURL {
		t.Errorf("TokenURL = %q, want %q", client.config.TokenURL, DefaultTokenURL)
	}
	if client.log == nil {
		t.Fatal("log 不应为 nil")
	}
	client.log("空实现不应 panic：%d", 1)

	custom := NewClient(Config{APIBaseURL: "https://example.com", TokenURL: "https://example.com/t"})
	if custom.config.APIBaseURL != "https://example.com" || custom.config.TokenURL != "https://example.com/t" {
		t.Errorf("显式配置被覆盖：%+v", custom.config)
	}
}

// truncateBody 的边界：200 字以内原样、超限截断并带省略号（错误信息不能把
// 整个响应体灌进日志）。
func TestTruncateBodyBoundaries(t *testing.T) {
	exact := strings.Repeat("x", 200)
	if got := truncateBody([]byte(exact)); got != exact {
		t.Errorf("恰好 200 字不应截断：%d 字 → %q", len(got), got[:min(20, len(got))])
	}
	over := strings.Repeat("x", 201)
	got := truncateBody([]byte(over))
	// 按字节截到 200，再拼省略号：总共 203 字节（多字节字符可能切在中途，
	// 这是刻意取舍——错误信息只是诊断片段，不值得为对齐 rune 边界放大逻辑）
	if len(got) != 200+len("…") || !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, over[:200]) {
		t.Errorf("超限应截断到前 200 字节 + 省略号：实际 %d 字节", len(got))
	}
	// 中文不会因按字节截断而 panic，且仍是合法 UTF-8 前缀的形态
	chinese := strings.Repeat("错", 100) // 300 字节
	if got := truncateBody([]byte(chinese)); len(got) != 200+len("…") {
		t.Errorf("中文截断长度 = %d 字节, want 203", len(got))
	}
	if got := truncateBody(nil); got != "" {
		t.Errorf("truncateBody(nil) = %q, want 空", got)
	}
}

// shortSessionID 只展示前 8 位：会话 ID 长且无信息量，完整打印会把日志行撑爆，
// 但 8 位以内必须原样（否则短 ID 反而不可读）。
func TestShortSessionIDBoundaries(t *testing.T) {
	for _, test := range []struct{ id, want string }{
		{id: "", want: ""},
		{id: "abc", want: "abc"},
		{id: "12345678", want: "12345678"},
		{id: "123456789", want: "12345678…"},
		{id: "abcdefghijklmnop", want: "abcdefgh…"},
	} {
		if got := shortSessionID(test.id); got != test.want {
			t.Errorf("shortSessionID(%q) = %q, want %q", test.id, got, test.want)
		}
	}
}

// FatalError.Fatal 是给 daemon 通道层看的标记：凭据类错误必须报告「不可重试」，
// 否则 daemon 会对着一个永远失败的凭据无限退避重试。
func TestFatalErrorMarksNonRetryable(t *testing.T) {
	fatal := &FatalError{Stage: "换取 access token", Code: "100007", Message: "appid invalid"}
	if !fatal.Fatal() {
		t.Error("FatalError.Fatal() = false, want true（凭据错不可重试）")
	}
	if !errors.Is(fatal, error(fatal)) {
		t.Error("FatalError 应可用 errors.Is 识别（错误链契约）")
	}
	// 错误文案必须点名排查方向：AppID/AppSecret 与 $VAR 引用的 .env
	message := fatal.Error()
	for _, want := range []string{"换取 access token", "100007", "AppID/AppSecret", ".env"} {
		if !strings.Contains(message, want) {
			t.Errorf("error = %q, want 含 %q", message, want)
		}
	}
	// 无 Body 时不留悬空的冒号
	if got := (&FatalError{Stage: "换取 access token"}).Error(); strings.Contains(got, "：。") {
		t.Errorf("无 Body 的错误文案 = %q, want 不带空正文段", got)
	}
}
