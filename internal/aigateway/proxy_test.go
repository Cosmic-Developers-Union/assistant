package aigateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeUpstream 记录收到的请求（含头/体），按脚本返回状态与响应。
type fakeUpstream struct {
	mu       sync.Mutex
	requests []recordedRequest
	status   int
	body     string
	stream   bool
}

type recordedRequest struct {
	Path   string
	Header http.Header
	Body   map[string]any
}

func (f *fakeUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		document := map[string]any{}
		_ = json.Unmarshal(raw, &document)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{Path: r.URL.Path, Header: r.Header.Clone(), Body: document})
		status := f.status
		body := f.body
		stream := f.stream
		f.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		if stream {
			w.Header().Set("content-type", "text/event-stream")
			w.WriteHeader(status)
			flusher, _ := w.(http.Flusher)
			for _, chunk := range []string{"event: message_start\ndata: {}\n\n", "event: message_stop\ndata: {}\n\n"} {
				_, _ = io.WriteString(w, chunk)
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		if body == "" {
			body = `{"type":"message","content":[{"type":"text","text":"ok"}]}`
		}
		_, _ = io.WriteString(w, body)
	}
}

func (f *fakeUpstream) last() recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func testConfig(upstreams ...Upstream) *Config {
	config := &Config{
		Listen:    "127.0.0.1:0",
		Session:   SessionConfig{Secret: "test-secret"},
		Keys:      []APIKey{{Name: "assistant", Token: "gw-token"}},
		Upstreams: upstreams,
		Routes:    map[string][]string{"claude-sonnet-5": {"primary", "secondary"}, "*": {"secondary"}},
	}
	config.Normalize()
	return config
}

func doMessages(t *testing.T, handler http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// 鉴权与错误结构。
func TestGatewayAuth(t *testing.T) {
	upstream := &fakeUpstream{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()
	gateway := New(testConfig(Upstream{Name: "primary", BaseURL: server.URL, Token: "up-token"}), t.Logf)

	response := doMessages(t, gateway.Handler(), "", `{"model":"claude-sonnet-5","messages":[]}`)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
	var document map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document["type"] != "error" {
		t.Errorf("错误结构 = %+v", document)
	}
}

// 模型映射、会话注入（头 + prompt_cache_key + metadata.user_id）、客户端凭据
// 不泄露给上游。
func TestGatewaySessionAndModelMapping(t *testing.T) {
	upstream := &fakeUpstream{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()
	config := testConfig(Upstream{
		Name:    "primary",
		BaseURL: server.URL,
		Token:   "up-token",
		Models:  map[string]string{"claude-sonnet-5": "glm-5.3-flash[1m]"},
		Session: SessionSpec{
			Headers:        []string{"x-opencode-session", "x-session-affinity"},
			BodyField:      "prompt_cache_key",
			MetadataUserID: true,
		},
	})
	gateway := New(config, t.Logf)

	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"你好"}],
		"metadata":{"user_id":"{\"session_id\":\"client-session\",\"device_id\":\"d1\"}"}}`
	response := doMessages(t, gateway.Handler(), "gw-token", body)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	recorded := upstream.last()
	if recorded.Body["model"] != "glm-5.3-flash[1m]" {
		t.Errorf("上游模型 = %v", recorded.Body["model"])
	}
	if recorded.Header.Get("x-opencode-session") != "client-session" ||
		recorded.Header.Get("x-session-affinity") != "client-session" {
		t.Errorf("会话头未注入：%+v", recorded.Header)
	}
	if recorded.Body["prompt_cache_key"] != "client-session" {
		t.Errorf("prompt_cache_key = %v", recorded.Body["prompt_cache_key"])
	}
	metadata, _ := recorded.Body["metadata"].(map[string]any)
	var userID map[string]any
	if err := json.Unmarshal([]byte(metadata["user_id"].(string)), &userID); err != nil {
		t.Fatalf("metadata.user_id = %v", metadata["user_id"])
	}
	if userID["session_id"] != "client-session" || userID["device_id"] != "d1" {
		t.Errorf("metadata.user_id = %+v", userID)
	}
	if token := recorded.Header.Get("Authorization"); token != "Bearer up-token" {
		t.Errorf("上游凭据 = %q（客户端令牌不应透传）", token)
	}
	if recorded.Header.Get("x-api-key") != "" {
		t.Errorf("客户端 x-api-key 不应透传：%v", recorded.Header.Get("x-api-key"))
	}
}

// 无显式会话时用内容派生的稳定键，并在重试/多轮间保持一致。
func TestGatewayDerivedSessionStable(t *testing.T) {
	upstream := &fakeUpstream{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()
	gateway := New(testConfig(Upstream{
		Name:    "primary",
		BaseURL: server.URL,
		Token:   "t",
		Session: SessionSpec{Headers: []string{"x-session-affinity"}},
	}), t.Logf)

	first := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"第一轮"}]}`
	second := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"第一轮"},{"role":"assistant","content":"答"},{"role":"user","content":"第二轮"}]}`
	if response := doMessages(t, gateway.Handler(), "gw-token", first); response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	one := upstream.last().Header.Get("x-session-affinity")
	if response := doMessages(t, gateway.Handler(), "gw-token", second); response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	two := upstream.last().Header.Get("x-session-affinity")
	if one == "" || one != two {
		t.Errorf("派生会话应跨轮稳定：%q vs %q", one, two)
	}
}

// 故障转移：主上游 500 → 次上游成功；客户端拿到的仍是正常响应。
func TestGatewayFailover(t *testing.T) {
	primary := &fakeUpstream{status: http.StatusInternalServerError, body: `{"error":"boom"}`}
	secondary := &fakeUpstream{}
	primaryServer := httptest.NewServer(primary.handler())
	defer primaryServer.Close()
	secondaryServer := httptest.NewServer(secondary.handler())
	defer secondaryServer.Close()
	gateway := New(testConfig(
		Upstream{Name: "primary", BaseURL: primaryServer.URL, Token: "t"},
		Upstream{Name: "secondary", BaseURL: secondaryServer.URL, Token: "t"},
	), t.Logf)

	response := doMessages(t, gateway.Handler(), "gw-token", `{"model":"claude-sonnet-5","messages":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if primary.count() != 1 || secondary.count() != 1 {
		t.Errorf("尝试次数 primary=%d secondary=%d", primary.count(), secondary.count())
	}

	// 4xx 不回退：让调用方看到真实错误
	primary4xx := &fakeUpstream{status: http.StatusBadRequest, body: `{"error":"bad"}`}
	server4xx := httptest.NewServer(primary4xx.handler())
	defer server4xx.Close()
	directConfig := &Config{
		Keys:      []APIKey{{Name: "k", Token: "t"}},
		Upstreams: []Upstream{{Name: "only", BaseURL: server4xx.URL, Token: "t"}},
		Routes:    map[string][]string{"*": {"only"}},
	}
	directConfig.Normalize()
	direct := New(directConfig, t.Logf)
	if response := doMessages(t, direct.Handler(), "t", `{"model":"m","messages":[]}`); response.Code != http.StatusBadRequest {
		t.Errorf("4xx 应原样回传：%d", response.Code)
	}
}

// 流式（SSE）原样透传。
func TestGatewayStreamingPassthrough(t *testing.T) {
	upstream := &fakeUpstream{stream: true}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()
	gateway := New(testConfig(Upstream{Name: "primary", BaseURL: server.URL, Token: "t"}), t.Logf)

	response := doMessages(t, gateway.Handler(), "gw-token", `{"model":"claude-sonnet-5","messages":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if contentType := response.Header().Get("content-type"); contentType != "text/event-stream" {
		t.Errorf("content-type = %q", contentType)
	}
	if !strings.Contains(response.Body.String(), "event: message_stop") {
		t.Errorf("流未透传：%q", response.Body.String())
	}
}

// 模型白名单与 /v1/models 列表；count_tokens 走同一路由。
func TestGatewayModelAllowlistAndDiscovery(t *testing.T) {
	upstream := &fakeUpstream{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()
	config := &Config{
		Keys:      []APIKey{{Name: "k", Token: "t", Models: []string{"claude-sonnet-5"}}},
		Upstreams: []Upstream{{Name: "primary", BaseURL: server.URL, Token: "t"}},
		Routes: map[string][]string{
			"claude-sonnet-5": {"primary"},
			"glm-5.3":         {"primary"},
			"*":               {"primary"},
		},
	}
	config.Normalize()
	gateway := New(config, t.Logf)

	if response := doMessages(t, gateway.Handler(), "t", `{"model":"glm-5.3","messages":[]}`); response.Code != http.StatusForbidden {
		t.Errorf("白名单外应 403：%d", response.Code)
	}
	if response := doMessages(t, gateway.Handler(), "t", `{"model":"claude-sonnet-5","messages":[]}`); response.Code != http.StatusOK {
		t.Errorf("status = %d", response.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	request.Header.Set("Authorization", "Bearer t")
	recorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Errorf("count_tokens status = %d", recorder.Code)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	listRequest.Header.Set("Authorization", "Bearer t")
	listRecorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(listRecorder, listRequest)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("models status = %d", listRecorder.Code)
	}
	var list map[string]any
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	data, _ := list["data"].([]any)
	if len(data) != 2 {
		t.Errorf("models = %+v", list)
	}
}

// 配置校验：未知上游、非法 base_url、重复名字。
func TestConfigValidate(t *testing.T) {
	base := func() *Config {
		config := &Config{
			Keys:      []APIKey{{Name: "k", Token: "t"}},
			Upstreams: []Upstream{{Name: "a", BaseURL: "https://up.example.com", Token: "t"}},
			Routes:    map[string][]string{"*": {"a"}},
		}
		config.Normalize()
		return config
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("基准配置应通过：%v", err)
	}
	broken := base()
	broken.Routes = map[string][]string{"*": {"missing"}}
	if err := broken.Validate(); err == nil {
		t.Error("未知上游应报错")
	}
	broken = base()
	broken.Upstreams[0].BaseURL = "not-a-url"
	if err := broken.Validate(); err == nil {
		t.Error("非法 base_url 应报错")
	}
	broken = base()
	broken.Upstreams = append(broken.Upstreams, broken.Upstreams[0])
	if err := broken.Validate(); err == nil {
		t.Error("重复上游名应报错")
	}
	broken = base()
	broken.Keys = nil
	if err := broken.Validate(); err == nil {
		t.Error("空 keys 应报错")
	}
	if config := base(); config.Upstreams[0].TimeoutMS != DefaultUpstreamTimeoutMS {
		t.Errorf("默认超时 = %d", config.Upstreams[0].TimeoutMS)
	}
	if fmt.Sprint(base().UpstreamsFor("anything")[0].Name) != "a" {
		t.Error("未命中应走 * 兜底")
	}
}
