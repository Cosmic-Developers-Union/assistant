package aigateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeUpstream 记录收到的请求（含头/原始体），按脚本返回状态与响应。
type fakeUpstream struct {
	mu       sync.Mutex
	requests []recordedRequest
	status   int
	body     string
	stream   bool
}

type recordedRequest struct {
	Path    string
	Header  http.Header
	Body    map[string]any
	RawBody []byte
}

func (f *fakeUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		document := map[string]any{}
		_ = json.Unmarshal(raw, &document)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{Path: r.URL.Path, Header: r.Header.Clone(), Body: document, RawBody: raw})
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

// newConfig 组装并校验测试配置（Listen 环回 + 一个接入密钥）。
func newConfig(t *testing.T, models ...Model) *Config {
	t.Helper()
	config := &Config{
		Listen:  "127.0.0.1:8780",
		Session: SessionConfig{Secret: "test-secret"},
		Keys:    []APIKey{{Name: "assistant", Token: "gw-token"}},
		Models:  models,
	}
	config.Normalize()
	if err := config.Validate(); err != nil {
		t.Fatalf("测试配置无效：%v", err)
	}
	return config
}

func newTestGateway(t *testing.T, config *Config) *Gateway {
	t.Helper()
	gateway, err := New(config, t.Logf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return gateway
}

// standardModel 是一个标准后端条目（纯透传，端点由 base_url 给定）。
func standardModel(id, protocol, baseURL, token string) Model {
	return Model{ID: id, Protocol: protocol, Backend: "standard", BaseURL: baseURL, APIKey: token}
}

func doPost(t *testing.T, handler http.Handler, path, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
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
	gateway := newTestGateway(t, newConfig(t, standardModel("m", ProtocolAnthropic, server.URL, "up-token")))

	response := doPost(t, gateway.Handler(), "/v1/messages", "", `{"model":"m","messages":[]}`, nil)
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

	// 未配置 keys 时（环回）不鉴权
	open := newConfig(t, standardModel("m", ProtocolAnthropic, server.URL, "t"))
	open.Keys = nil
	openGateway := newTestGateway(t, open)
	if response := doPost(t, openGateway.Handler(), "/v1/messages", "", `{"model":"m","messages":[]}`, nil); response.Code != http.StatusOK {
		t.Errorf("无 keys 时不应要求鉴权：%d", response.Code)
	}
}

// opencode 后端：模型映射 + 会话头/prompt_cache_key/metadata 自动注入、
// 客户端凭据不泄露。
func TestGatewayOpencodeSessionAndModelMapping(t *testing.T) {
	upstream := &fakeUpstream{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()
	gateway := newTestGateway(t, newConfig(t, Model{
		ID: "claude-sonnet-5", Protocol: ProtocolAnthropic,
		Backend: "opencode-go", BaseURL: server.URL, APIKey: "up-token",
		Model: "glm-5.3-flash[1m]",
	}))

	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"你好"}],
		"metadata":{"user_id":"{\"session_id\":\"client-session\",\"device_id\":\"d1\"}"}}`
	response := doPost(t, gateway.Handler(), "/v1/messages", "gw-token", body, nil)
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
}

// 标准后端纯透传：请求体逐字节原样（键序/空白/转义都不动），无任何注入。
func TestGatewayStandardRawPassthrough(t *testing.T) {
	upstream := &fakeUpstream{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()
	gateway := newTestGateway(t, newConfig(t, standardModel("m", ProtocolAnthropic, server.URL, "t")))

	body := "{\n  \"model\": \"m\",\n  \"messages\": [ {\"role\":\"user\", \"content\": \"你好 \\u4e16界\"} ]\n}"
	response := doPost(t, gateway.Handler(), "/v1/messages", "gw-token", body, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if string(upstream.last().RawBody) != body {
		t.Errorf("请求体应逐字节透传：\n%s\n---\n%s", body, upstream.last().RawBody)
	}
	if upstream.last().Header.Get("x-opencode-session") != "" {
		t.Error("标准后端不应注入会话头")
	}
}

// 无显式会话时用内容派生的稳定键（opencode 后端自动注入），多轮保持一致。
func TestGatewayDerivedSessionStable(t *testing.T) {
	upstream := &fakeUpstream{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()
	gateway := newTestGateway(t, newConfig(t, Model{
		ID: "m", Protocol: ProtocolOpenAIChat, Backend: "opencode-go",
		BaseURL: server.URL, APIKey: "t",
	}))

	first := `{"model":"m","messages":[{"role":"user","content":"第一轮"}]}`
	second := `{"model":"m","messages":[{"role":"user","content":"第一轮"},{"role":"assistant","content":"答"},{"role":"user","content":"第二轮"}]}`
	if response := doPost(t, gateway.Handler(), "/v1/chat/completions", "gw-token", first, nil); response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	one := upstream.last().Header.Get("x-opencode-session")
	if response := doPost(t, gateway.Handler(), "/v1/chat/completions", "gw-token", second, nil); response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	two := upstream.last().Header.Get("x-opencode-session")
	if one == "" || one != two {
		t.Errorf("派生会话应跨轮稳定：%q vs %q", one, two)
	}
	if upstream.last().Body["prompt_cache_key"] != two {
		t.Errorf("prompt_cache_key = %v", upstream.last().Body["prompt_cache_key"])
	}
}

// 同一 id 多条 = 故障转移链：主上游 500 → 次上游成功；4xx 原样回传。
func TestGatewayFailover(t *testing.T) {
	primary := &fakeUpstream{status: http.StatusInternalServerError, body: `{"error":"boom"}`}
	secondary := &fakeUpstream{}
	primaryServer := httptest.NewServer(primary.handler())
	defer primaryServer.Close()
	secondaryServer := httptest.NewServer(secondary.handler())
	defer secondaryServer.Close()
	gateway := newTestGateway(t, newConfig(t,
		standardModel("m", ProtocolAnthropic, primaryServer.URL, "t"),
		standardModel("m", ProtocolAnthropic, secondaryServer.URL, "t"),
	))

	response := doPost(t, gateway.Handler(), "/v1/messages", "gw-token", `{"model":"m","messages":[]}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if primary.count() != 1 || secondary.count() != 1 {
		t.Errorf("尝试次数 primary=%d secondary=%d", primary.count(), secondary.count())
	}

	// 4xx 不回退：让调用方看到真实错误
	bad := &fakeUpstream{status: http.StatusBadRequest, body: `{"error":"bad"}`}
	badServer := httptest.NewServer(bad.handler())
	defer badServer.Close()
	direct := newTestGateway(t, newConfig(t, standardModel("m", ProtocolAnthropic, badServer.URL, "t")))
	if response := doPost(t, direct.Handler(), "/v1/messages", "gw-token", `{"model":"m","messages":[]}`, nil); response.Code != http.StatusBadRequest {
		t.Errorf("4xx 应原样回传：%d", response.Code)
	}
}

// 协议面：同一模型可声明不同协议；后端不支持的协议不参与路由。
func TestGatewayProtocols(t *testing.T) {
	upstream := &fakeUpstream{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()
	// opencode-go 支持三协议
	gateway := newTestGateway(t, newConfig(t, Model{
		ID: "m", Protocol: ProtocolOpenAIResponses, Backend: "opencode-go",
		BaseURL: server.URL, APIKey: "t",
	}))
	response := doPost(t, gateway.Handler(), "/v1/responses", "gw-token", `{"model":"m","input":"hi"}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("responses status = %d body=%s", response.Code, response.Body.String())
	}
	if got := upstream.last().Path; got != "/v1/responses" {
		t.Errorf("上游路径 = %q", got)
	}

	// zhipu 只支持 anthropic-messages：openai-compatible 无候选 → 400（不触上游）
	zhipuConfig := newConfig(t, Model{ID: "glm", Protocol: ProtocolAnthropic, Backend: "zhipu", APIKey: "t"})
	if len(zhipuConfig.Candidates("glm", ProtocolOpenAIChat)) != 0 {
		t.Error("zhipu 不应支持 openai-compatible")
	}
	zhipuGateway := newTestGateway(t, zhipuConfig)
	before := upstream.count()
	response = doPost(t, zhipuGateway.Handler(), "/v1/chat/completions", "gw-token", `{"model":"glm","messages":[]}`, nil)
	if response.Code != http.StatusBadRequest {
		t.Errorf("协议不匹配应 400：%d", response.Code)
	}
	if upstream.count() != before {
		t.Error("协议不匹配不应触上游")
	}
}

// 流式（SSE）原样透传。
func TestGatewayStreamingPassthrough(t *testing.T) {
	upstream := &fakeUpstream{stream: true}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()
	gateway := newTestGateway(t, newConfig(t, standardModel("m", ProtocolAnthropic, server.URL, "t")))

	response := doPost(t, gateway.Handler(), "/v1/messages", "gw-token", `{"model":"m","messages":[]}`, nil)
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

// 模型白名单与 /v1/models 列表（同 id 多条只列一次）。
func TestGatewayModelAllowlistAndDiscovery(t *testing.T) {
	upstream := &fakeUpstream{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()
	config := newConfig(t,
		standardModel("claude-sonnet-5", ProtocolAnthropic, server.URL, "t"),
		standardModel("glm-5.3", ProtocolAnthropic, server.URL, "t"),
		standardModel("claude-sonnet-5", ProtocolAnthropic, server.URL, "t"), // 备用链
	)
	config.Keys[0].Models = []string{"claude-sonnet-5"}
	gateway := newTestGateway(t, config)

	if response := doPost(t, gateway.Handler(), "/v1/messages", "gw-token", `{"model":"glm-5.3","messages":[]}`, nil); response.Code != http.StatusForbidden {
		t.Errorf("白名单外应 403：%d", response.Code)
	}
	if response := doPost(t, gateway.Handler(), "/v1/messages", "gw-token", `{"model":"claude-sonnet-5","messages":[]}`, nil); response.Code != http.StatusOK {
		t.Errorf("status = %d", response.Code)
	}
	if len(config.Candidates("claude-sonnet-5", ProtocolAnthropic)) != 2 {
		t.Error("同 id 应形成故障转移链")
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer gw-token")
	recorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("models status = %d", recorder.Code)
	}
	var list map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if data, _ := list["data"].([]any); len(data) != 2 {
		t.Errorf("models = %+v", list)
	}
}

// 工具画像：claude/codex/dsh 识别 + UA 规范化（通用 SDK UA 换专属署名）。
func TestGatewayToolDetectionAndUserAgent(t *testing.T) {
	upstream := &fakeUpstream{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()
	gateway := newTestGateway(t, newConfig(t, standardModel("m", ProtocolAnthropic, server.URL, "t")))

	response := doPost(t, gateway.Handler(), "/v1/messages", "gw-token", `{"model":"m","messages":[]}`,
		map[string]string{"User-Agent": "claude-cli/2.1.270 (external, cli)"})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	ua := upstream.last().Header.Get("User-Agent")
	if !strings.HasPrefix(ua, "claude-cli/2.1.270") || !strings.Contains(ua, DefaultUserAgent) {
		t.Errorf("专属 UA 应保留并附网关标识：%q", ua)
	}

	response = doPost(t, gateway.Handler(), "/v1/messages", "gw-token",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"User-Agent": "OpenAI/Python 1.0", "originator": "codex_cli_rs"})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	ua = upstream.last().Header.Get("User-Agent")
	if !strings.HasPrefix(ua, "codex_cli_rs/assistant-gateway") {
		t.Errorf("通用 UA 应改写为工具署名：%q", ua)
	}

	if tool := DetectTool(http.Header{"User-Agent": []string{"dsh/0.4.0"}}); tool != "dsh" {
		t.Errorf("dsh 识别 = %q", tool)
	}
	if tool := DetectTool(http.Header{"X-Dsh-Session": []string{"s1"}}); tool != "dsh" {
		t.Errorf("dsh 头识别 = %q", tool)
	}
}

// 数据驻留：完整保留客户端请求体、改写后的上游请求体与流式响应，元信息脱敏。
func TestGatewayResidency(t *testing.T) {
	upstream := &fakeUpstream{stream: true}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()
	archive := t.TempDir()
	config := newConfig(t, Model{
		ID: "claude-sonnet-5", Protocol: ProtocolAnthropic, Backend: "opencode-go",
		BaseURL: server.URL, APIKey: "t", Model: "glm-5.3-flash[1m]",
	})
	config.Residency = Residency{Enabled: true, Dir: archive}
	gateway := newTestGateway(t, config)

	body := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"你好"}]}`
	response := doPost(t, gateway.Handler(), "/v1/messages", "gw-token", body,
		map[string]string{"User-Agent": "claude-cli/2.1.270"})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}

	recordDir := findRecordDir(t, archive)
	clientBody, err := os.ReadFile(filepath.Join(recordDir, "client-request.body"))
	if err != nil || string(clientBody) != body {
		t.Errorf("客户端请求体未保留：%v %q", err, clientBody)
	}
	upstreamBody, err := os.ReadFile(filepath.Join(recordDir, "upstream-request.body"))
	if err != nil || !strings.Contains(string(upstreamBody), "glm-5.3-flash[1m]") {
		t.Errorf("上游改写体未保留：%v %q", err, upstreamBody)
	}
	responseBody, err := os.ReadFile(filepath.Join(recordDir, "upstream-response.body"))
	if err != nil || !strings.Contains(string(responseBody), "event: message_stop") {
		t.Errorf("流式响应未完整保留：%v %q", err, responseBody)
	}
	var meta RequestMeta
	metaRaw, err := os.ReadFile(filepath.Join(recordDir, "request.meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Tool != "claude" || meta.Session == "" || meta.SessionSource == "" {
		t.Errorf("元信息不完整：%+v", meta)
	}
	if got := meta.Headers["Authorization"]; len(got) != 1 || got[0] != "<redacted>" {
		t.Errorf("凭据未脱敏：%+v", meta.Headers)
	}
	var responseMeta ResponseMeta
	responseMetaRaw, err := os.ReadFile(filepath.Join(recordDir, "response.meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(responseMetaRaw, &responseMeta); err != nil {
		t.Fatal(err)
	}
	if responseMeta.Status != http.StatusOK || responseMeta.Bytes == 0 {
		t.Errorf("响应元信息 = %+v", responseMeta)
	}
}

func findRecordDir(t *testing.T, archive string) string {
	t.Helper()
	var recordDir string
	entries, err := os.ReadDir(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, dateEntry := range entries {
		records, _ := os.ReadDir(filepath.Join(archive, dateEntry.Name()))
		if len(records) == 1 {
			recordDir = filepath.Join(archive, dateEntry.Name(), records[0].Name())
		}
	}
	if recordDir == "" {
		t.Fatal("未找到归档目录")
	}
	return recordDir
}

// 配置校验：未知后端、标准后端缺 base_url、协议不匹配、缺 api-key、$ENV 展开、
// 对外监听必须配 keys。
func TestConfigValidate(t *testing.T) {
	base := func() *Config {
		config := &Config{
			Listen: "127.0.0.1:8780",
			Keys:   []APIKey{{Name: "k", Token: "t"}},
			Models: []Model{{ID: "m", Protocol: ProtocolAnthropic, Backend: "zhipu", APIKey: "t"}},
		}
		config.Normalize()
		return config
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("基准配置应通过：%v", err)
	}

	broken := base()
	broken.Models[0].Backend = "nope"
	if err := broken.Validate(); err == nil {
		t.Error("未知后端应报错")
	}
	broken = base()
	broken.Models[0] = Model{ID: "m", Protocol: ProtocolAnthropic, Backend: "standard", APIKey: "t"}
	broken.Normalize()
	if err := broken.Validate(); err == nil {
		t.Error("标准后端缺 base_url 应报错")
	}
	broken = base()
	broken.Models[0] = Model{ID: "m", Protocol: ProtocolOpenAIChat, Backend: "zhipu", APIKey: "t"}
	broken.Normalize()
	if err := broken.Validate(); err == nil {
		t.Error("zhipu 不支持 openai-compatible，应报错")
	}
	broken = base()
	broken.Models[0].APIKey = ""
	if err := broken.Validate(); err == nil {
		t.Error("缺 api-key 应报错")
	}
	broken = base()
	broken.Models[0].Protocol = "nope"
	broken.Normalize()
	if err := broken.Validate(); err == nil {
		t.Error("未知协议应报错")
	}

	// $ENV 展开与未设置报错
	t.Setenv("GW_TEST_KEY", "secret-key")
	expanded := base()
	expanded.Models[0].APIKey = "$GW_TEST_KEY"
	expanded.Normalize()
	if err := expanded.Validate(); err != nil {
		t.Fatalf("环境变量应展开：%v", err)
	}
	if expanded.Candidates("m", ProtocolAnthropic)[0].Token != "secret-key" {
		t.Error("环境变量未展开到 token")
	}
	missing := base()
	missing.Models[0].APIKey = "${GW_TEST_MISSING}"
	missing.Normalize()
	if err := missing.Validate(); err == nil {
		t.Error("未设置的变量应报错")
	}

	// 对外监听 + 空 keys 拒绝；环回 + 空 keys 允许
	exposed := base()
	exposed.Listen = "0.0.0.0:8780"
	exposed.Keys = nil
	if err := exposed.Validate(); err == nil {
		t.Error("对外监听必须配置 keys")
	}
	open := base()
	open.Keys = nil
	if err := open.Validate(); err != nil {
		t.Errorf("环回 + 空 keys 应允许：%v", err)
	}

	// 命名后端：type 指向内置 + 覆盖 base_url
	named := &Config{
		Listen: "127.0.0.1:8780",
		Backends: map[string]BackendConfig{
			"my-go": {Type: "opencode-go", BaseURL: "http://127.0.0.1:9", APIKey: "k"},
		},
		Models: []Model{{ID: "m", Protocol: ProtocolOpenAIChat, Backend: "my-go"}},
	}
	named.Normalize()
	if err := named.Validate(); err != nil {
		t.Fatalf("命名后端应通过：%v", err)
	}
	candidate := named.Candidates("m", ProtocolOpenAIChat)[0]
	if candidate.Type != "opencode-go" || candidate.Prepare == nil || candidate.BaseURL != "http://127.0.0.1:9" {
		t.Errorf("命名后端解析 = %+v", candidate)
	}
	if fmt.Sprint(candidate.Token) != "k" {
		t.Errorf("命名后端凭据 = %q", candidate.Token)
	}
}
