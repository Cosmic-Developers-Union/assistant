package daemon

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

	"assistant/internal/instances"
	"assistant/internal/qq"
	"github.com/coder/websocket"
)

// newFakeQQ 返回最小 QQ API 服务端：token、gateway、发送记录，以及由测试注入
// 的 WS 网关行为（默认 Hello → READY → 心跳应答）。
type fakeQQ struct {
	server *httptest.Server
	sent   []map[string]any
	tokens int32

	mu     sync.Mutex
	ws     func(t *testing.T, conn *websocket.Conn)
	cancel context.CancelFunc
}

func newFakeQQ(t *testing.T) *fakeQQ {
	fake := &fakeQQ{}
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
		fake.mu.Unlock()
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /ws", func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		fake.runWS(t, conn)
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// runWS 执行注入的网关行为；未注入时用默认：Hello → READY → 心跳应答循环。
func (f *fakeQQ) runWS(t *testing.T, conn *websocket.Conn) {
	if handler := f.wsHandler(); handler != nil {
		handler(t, conn)
		return
	}
	writeWSFrame(t, conn, `{"op":10,"d":{"heartbeat_interval":80}}`)
	readWSFrame(t, conn, 2*time.Second) // Identify
	writeWSFrame(t, conn, `{"op":0,"s":1,"t":"READY","d":{"session_id":"sess-qq","user":{"id":"bot"}}}`)
	for {
		next, ok := readWSFrame(t, conn, 2*time.Second)
		if !ok {
			return
		}
		var frame struct {
			Op int `json:"op"`
		}
		_ = json.Unmarshal(next, &frame)
		if frame.Op == 1 {
			writeWSFrame(t, conn, `{"op":11}`)
		}
	}
}

func (f *fakeQQ) wsHandler() func(t *testing.T, conn *websocket.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ws
}

func (f *fakeQQ) setWS(handler func(t *testing.T, conn *websocket.Conn)) {
	f.mu.Lock()
	f.ws = handler
	f.mu.Unlock()
}

func (f *fakeQQ) sentSnapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sent...)
}

func (f *fakeQQ) waitSent(t *testing.T, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sent := f.sentSnapshot(); len(sent) >= want {
			return sent
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待第 %d 条发送超时：已发 %v", want, f.sentSnapshot())
	return nil
}

func writeWSFrame(t *testing.T, conn *websocket.Conn, payload string) {
	t.Helper()
	writeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, []byte(payload)); err != nil {
		t.Errorf("写 WS 帧失败：%v", err)
	}
}

func readWSFrame(t *testing.T, conn *websocket.Conn, timeout time.Duration) ([]byte, bool) {
	readCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		return nil, false
	}
	return data, true
}

// newQQChannelForTest 起一个 QQ 通道 + fake API + RunChannel（返回的 cancel
// 由调用方在断言后触发）。
func newQQChannelForTest(t *testing.T, config instances.QQ) (*fakeQQ, *QQChannel, context.CancelFunc, chan error) {
	t.Helper()
	fake := newFakeQQ(t)
	client := qq.NewClient(qq.Config{
		AppID:      config.AppID,
		AppSecret:  config.AppSecret,
		APIBaseURL: fake.server.URL,
		TokenURL:   fake.server.URL + "/token",
		HTTPClient: fake.server.Client(),
		Log:        t.Logf,
	})
	channel := newQQChannelWithClient(config, client, false, t.Logf)
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
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunChannel(ctx, channel, ChannelConfig{Chat: chat, Log: t.Logf})
	}()
	return fake, channel, cancel, done
}

// 私聊：白名单用户的消息 → claude 回复以被动消息发回（带 msg_id、msg_seq=1）。
func TestQQChannelRepliesWithMsgID(t *testing.T) {
	fake, _, cancel, done := newQQChannelForTest(t, instances.QQ{
		AppID: "app-1", AppSecret: "sec-1", AdminUsers: []string{"u1"},
	})
	fake.setWS(func(t *testing.T, conn *websocket.Conn) {
		writeWSFrame(t, conn, `{"op":10,"d":{"heartbeat_interval":80}}`)
		readWSFrame(t, conn, 2*time.Second)
		writeWSFrame(t, conn, `{"op":0,"s":1,"t":"READY","d":{"session_id":"s","user":{"id":"bot"}}}`)
		writeWSFrame(t, conn, `{"op":0,"s":2,"t":"C2C_MESSAGE_CREATE","d":{"id":"m-1","content":"你好","author":{"user_openid":"u1"}}}`)
		for {
			if next, ok := readWSFrame(t, conn, 2*time.Second); !ok {
				return
			} else if strings.Contains(string(next), `"op":1`) {
				writeWSFrame(t, conn, `{"op":11}`)
			}
		}
	})
	sent := fake.waitSent(t, 1)
	cancel()
	<-done
	if sent[0]["content"] != "好的" || sent[0]["msg_id"] != "m-1" || sent[0]["msg_seq"] != float64(1) {
		t.Errorf("回复 payload = %+v", sent[0])
	}
}

// 群聊：@ 消息（残留提及前缀被清理）→ 回到对应群，白名单按成员 openid 判定。
func TestQQChannelGroupReply(t *testing.T) {
	fake, _, cancel, done := newQQChannelForTest(t, instances.QQ{
		AppID: "app-1", AppSecret: "sec-1", AdminUsers: []string{"*"},
	})
	fake.setWS(func(t *testing.T, conn *websocket.Conn) {
		writeWSFrame(t, conn, `{"op":10,"d":{"heartbeat_interval":80}}`)
		readWSFrame(t, conn, 2*time.Second)
		writeWSFrame(t, conn, `{"op":0,"s":1,"t":"READY","d":{"session_id":"s","user":{"id":"bot"}}}`)
		writeWSFrame(t, conn, `{"op":0,"s":2,"t":"GROUP_AT_MESSAGE_CREATE","d":{"id":"g-1","content":"<@!PKIXXBOT> 查下状态","group_openid":"GG1","author":{"member_openid":"m1"}}}`)
		for {
			if next, ok := readWSFrame(t, conn, 2*time.Second); !ok {
				return
			} else if strings.Contains(string(next), `"op":1`) {
				writeWSFrame(t, conn, `{"op":11}`)
			}
		}
	})
	sent := fake.waitSent(t, 1)
	cancel()
	<-done
	if sent[0]["msg_id"] != "g-1" {
		t.Errorf("回复 payload = %+v", sent[0])
	}
	// claude 收到的应是清理后的文本：用回复内容无法验证入参，这里验证切块路径
	// 走通即可；文本清理逻辑单测见 TestCleanQQContent。
}

// 准入：未配置白名单时全部拒绝（不回复也不建会话）。
func TestQQChannelDeniesWithoutWhitelist(t *testing.T) {
	fake, channel, cancel, done := newQQChannelForTest(t, instances.QQ{
		AppID: "app-1", AppSecret: "sec-1",
	})
	if allowed, reason := channel.Allowed("anyone"); allowed || reason == "" {
		t.Errorf("未配置白名单应全拒：allowed=%v reason=%q", allowed, reason)
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	if sent := fake.sentSnapshot(); len(sent) != 0 {
		t.Errorf("不应有发送：%v", sent)
	}
}

// 文本清理：残留的 @ 提及前缀（<@!bot>、<@bot>、@机器人）被剥掉，正文保留。
func TestCleanQQContent(t *testing.T) {
	cases := map[string]string{
		"<@!BOT123> 查状态":   "查状态",
		"<@BOT123>你好":       "你好",
		"@机器人 帮我看看":         "帮我看看",
		"  纯文本 ":             "纯文本",
		"两段<@A> <@B> mention": "两段<@A> <@B> mention",
	}
	for input, want := range cases {
		if got := cleanQQContent(input); got != want {
			t.Errorf("cleanQQContent(%q) = %q, want %q", input, got, want)
		}
	}
}
