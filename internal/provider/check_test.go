package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
)

// checkEnv 把自检指向 httptest 服务：替换 client 并把 ANTHROPIC_BASE_URL 指向
// 测试服务，杜绝测试意外打到真实端点（默认端点 https://api.anthropic.com）。
// 返回的 baseURL 供调用方在需要时覆盖。
func checkEnv(t *testing.T, handler http.HandlerFunc) (baseURL string, env map[string]string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	previous := checkClient
	checkClient = server.Client()
	t.Cleanup(func() { checkClient = previous })
	return server.URL, map[string]string{"ANTHROPIC_BASE_URL": server.URL}
}

// 凭据被接受：200 视为 OK，且端点、模型与请求体（最小请求）都正确。
func TestCheckCredentialAcceptsValidKey(t *testing.T) {
	var gotPath, gotAPIKey, gotVersion string
	var body map[string]any
	baseURL, env := checkEnv(t, func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		gotAPIKey = request.Header.Get("x-api-key")
		gotVersion = request.Header.Get("anthropic-version")
		raw, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = writer.Write([]byte(`{"ok":true}`))
	})
	env["ANTHROPIC_API_KEY"] = "sk-test"

	check := CheckCredential(t.Context(), claudecfg.Overrides{Env: env})
	if !check.OK() {
		t.Fatalf("OK() = false, check = %+v", check)
	}
	if check.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", check.Status)
	}
	if check.BaseURL != baseURL {
		t.Errorf("BaseURL = %q, want 测试端点 %q", check.BaseURL, baseURL)
	}
	if check.Model != checkModelFallback {
		t.Errorf("Model = %q, want 回退模型 %q", check.Model, checkModelFallback)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("请求路径 = %q, want /v1/messages", gotPath)
	}
	if gotAPIKey != "sk-test" {
		t.Errorf("x-api-key = %q, want sk-test", gotAPIKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", gotVersion)
	}
	// 最小请求：max_tokens=1，避免自检产生实质费用
	if maxTokens, _ := body["max_tokens"].(float64); maxTokens != 1 {
		t.Errorf("max_tokens = %v, want 1（自检不应产生实质费用）", body["max_tokens"])
	}
}

// 自检用 auth token（Bearer）时走 authorization 头而非 x-api-key。
func TestCheckCredentialSendsBearerForAuthToken(t *testing.T) {
	var authorization, apiKey string
	_, env := checkEnv(t, func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("authorization")
		apiKey = request.Header.Get("x-api-key")
		_, _ = writer.Write([]byte(`{}`))
	})
	env["ANTHROPIC_AUTH_TOKEN"] = "auth-token"

	check := CheckCredential(t.Context(), claudecfg.Overrides{Env: env})
	if !check.OK() {
		t.Fatalf("OK() = false, check = %+v", check)
	}
	if authorization != "Bearer auth-token" {
		t.Errorf("authorization = %q, want Bearer auth-token", authorization)
	}
	if apiKey != "" {
		t.Errorf("x-api-key = %q, want 空（auth token 不走 x-api-key）", apiKey)
	}
}

// api_key 优先于 auth_token（与 claudecfg.CredentialSource 同口径）。
func TestCheckCredentialPrefersAPIKeyOverAuthToken(t *testing.T) {
	var apiKey, authorization string
	_, env := checkEnv(t, func(writer http.ResponseWriter, request *http.Request) {
		apiKey = request.Header.Get("x-api-key")
		authorization = request.Header.Get("authorization")
		_, _ = writer.Write([]byte(`{}`))
	})
	env["ANTHROPIC_API_KEY"] = "sk-both"
	env["ANTHROPIC_AUTH_TOKEN"] = "auth-both"

	CheckCredential(t.Context(), claudecfg.Overrides{Env: env})
	if apiKey != "sk-both" {
		t.Errorf("x-api-key = %q, want sk-both", apiKey)
	}
	if authorization != "" {
		t.Errorf("authorization = %q, want 空（api_key 优先）", authorization)
	}
}

// 没有任何凭据时不发请求，直接给出可读错误（避免无意义的网络往返）。
func TestCheckCredentialWithoutCredentials(t *testing.T) {
	var called bool
	_, env := checkEnv(t, func(writer http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = writer.Write([]byte(`{}`))
	})
	_ = env
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	check := CheckCredential(t.Context(), claudecfg.Overrides{})
	if check.Err == nil {
		t.Fatal("Err = nil, want 缺少凭据的错误")
	}
	if !strings.Contains(check.Err.Error(), "api_key") {
		t.Errorf("Err = %v, want 指明缺 api_key/auth_token", check.Err)
	}
	if called {
		t.Error("没有凭据时不应发起请求")
	}
	if check.OK() {
		t.Error("OK() = true, want false")
	}
}

// 端点与密钥不配套（401）：状态与 message 都要带出来，供操作者定位。
func TestCheckCredentialReportsRejection(t *testing.T) {
	_, env := checkEnv(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"type":"error","error":{"message":"invalid x-api-key"}}`))
	})
	env["ANTHROPIC_API_KEY"] = "sk-bad"

	check := CheckCredential(t.Context(), claudecfg.Overrides{Env: env})
	if check.OK() {
		t.Fatal("OK() = true, want false（401 不是成功）")
	}
	if check.Status != http.StatusUnauthorized {
		t.Errorf("Status = %d, want 401", check.Status)
	}
	if !strings.Contains(check.Message, "invalid x-api-key") {
		t.Errorf("Message = %q, want 含服务端原因", check.Message)
	}
	// Describe 要能直接给操作者看：端点 + 状态码 + 原因
	described := check.Describe()
	for _, want := range []string{"HTTP 401", "invalid x-api-key"} {
		if !strings.Contains(described, want) {
			t.Errorf("Describe() = %q, 缺少 %q", described, want)
		}
	}
}

// 2xx 之外的状态都不是 OK（含 3xx 重定向与 5xx 服务端错误）。
func TestCredentialCheckOKBoundaries(t *testing.T) {
	for _, test := range []struct {
		status int
		want   bool
	}{
		{200, true},
		{201, true},
		{204, true},
		{299, true},
		{0, false}, // 请求没发出去
		{301, false},
		{400, false},
		{401, false},
		{429, false},
		{500, false},
	} {
		check := CredentialCheck{Status: test.status}
		if got := check.OK(); got != test.want {
			t.Errorf("Status %d: OK() = %t, want %t", test.status, got, test.want)
		}
	}
}

// 网络层失败（连接被拒）与超时都归入 Err，Describe 给出「无法连接」。
func TestCheckCredentialReportsTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close() // 关闭后端口无人监听

	previous := checkClient
	checkClient = &http.Client{Timeout: time.Second}
	t.Cleanup(func() { checkClient = previous })

	check := CheckCredential(t.Context(), claudecfg.Overrides{
		Env: map[string]string{"ANTHROPIC_API_KEY": "sk-x", "ANTHROPIC_BASE_URL": url},
	})
	if check.Err == nil {
		t.Fatal("Err = nil, want 连接失败")
	}
	if check.Status != 0 {
		t.Errorf("Status = %d, want 0（请求未成功送达）", check.Status)
	}
	if !strings.Contains(check.Describe(), "无法连接") {
		t.Errorf("Describe() = %q, want 含「无法连接」", check.Describe())
	}
}

// 已取消的 ctx 同样归入 Err；ctx 为 nil 时不应 panic（调用方可能直接传 nil）。
func TestCheckCredentialHandlesContext(t *testing.T) {
	_, env := checkEnv(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{}`))
	})
	env["ANTHROPIC_API_KEY"] = "sk-x"

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if check := CheckCredential(cancelled, claudecfg.Overrides{Env: env}); check.Err == nil {
		t.Error("已取消的 ctx: Err = nil, want 失败")
	}

	// nil ctx 应当回落为 Background（调用方可能直接传 nil），不应 panic
	var nilCtx context.Context
	if check := CheckCredential(nilCtx, claudecfg.Overrides{Env: env}); check.Err != nil {
		t.Errorf("nil ctx 应当回落为 Background：Err = %v", check.Err)
	}
}

// 端点与模型按覆盖优先级解析：端点去尾斜杠；模型取 provider env 的第一个非空值。
func TestCheckCredentialResolvesEndpointAndModel(t *testing.T) {
	for _, test := range []struct {
		name        string
		env         map[string]string
		wantBaseURL string
		wantModel   string
	}{
		{
			name: "自定义端点去尾斜杠 + ANTHROPIC_MODEL 优先",
			env: map[string]string{
				"ANTHROPIC_BASE_URL": "https://gateway.example.com/",
				"ANTHROPIC_MODEL":    "model-a",
			},
			wantBaseURL: "https://gateway.example.com",
			wantModel:   "model-a",
		},
		{
			name: "ANTHROPIC_MODEL 缺省时退到 SONNET 档",
			env: map[string]string{
				"ANTHROPIC_DEFAULT_SONNET_MODEL": "sonnet-x",
				"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "haiku-x",
			},
			wantBaseURL: defaultBaseURL,
			wantModel:   "sonnet-x",
		},
		{
			name:        "只有 HAIKU 档时用它",
			env:         map[string]string{"ANTHROPIC_DEFAULT_HAIKU_MODEL": "haiku-x"},
			wantBaseURL: defaultBaseURL,
			wantModel:   "haiku-x",
		},
		{
			name:        "都没有时用回退模型",
			env:         map[string]string{},
			wantBaseURL: defaultBaseURL,
			wantModel:   checkModelFallback,
		},
		{
			name:        "空白值不算配置（按 TrimSpace 判定）",
			env:         map[string]string{"ANTHROPIC_BASE_URL": "   ", "ANTHROPIC_MODEL": " \t "},
			wantBaseURL: defaultBaseURL,
			wantModel:   checkModelFallback,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 解析结果与请求解耦：把 client 换成「记录是否被调用」的桩，这样
			// 期望默认端点的用例不会真的打到 api.anthropic.com。
			var reached bool
			previous := checkClient
			checkClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				reached = true
				return nil, io.ErrUnexpectedEOF
			})}
			t.Cleanup(func() { checkClient = previous })

			env := make(map[string]string, len(test.env)+1)
			for key, value := range test.env {
				env[key] = value
			}
			env["ANTHROPIC_API_KEY"] = "sk-x"

			check := CheckCredential(t.Context(), claudecfg.Overrides{Env: env})
			if check.BaseURL != test.wantBaseURL {
				t.Errorf("BaseURL = %q, want %q", check.BaseURL, test.wantBaseURL)
			}
			if check.Model != test.wantModel {
				t.Errorf("Model = %q, want %q", check.Model, test.wantModel)
			}
			// 有凭据就应真的发起自检（这里被桩拦下，故 Err 非空）
			if !reached {
				t.Error("有凭据时应当发起自检请求")
			}
		})
	}
}

// roundTripFunc 让测试用函数构造 http.RoundTripper（避免真的发网络请求）。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// compactMessage 把响应体压成一行短文本，并在超长时截断。
func TestCompactMessage(t *testing.T) {
	long := strings.Repeat("字", checkBodyLimit+50)
	for _, test := range []struct {
		name  string
		body  string
		want  string
		exact bool
	}{
		{name: "空响应", body: "", want: "（空响应）", exact: true},
		{name: "仅空白视为空", body: "  \n\t ", want: "（空响应）", exact: true},
		{name: "多行折成一行", body: "第一行\n第二行", want: "第一行 第二行", exact: true},
		{name: "连续空白压成单个空格", body: "a   \t  b", want: "a b", exact: true},
		{name: "去掉首尾空白", body: "  hello  ", want: "hello", exact: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := compactMessage(test.body)
			if test.exact && got != test.want {
				t.Errorf("compactMessage(%q) = %q, want %q", test.body, got, test.want)
			}
		})
	}

	// 超长按 rune（而非字节）截断：多字节字符不能被切碎成半个字符
	got := compactMessage(long)
	runes := []rune(got)
	if len(runes) != checkBodyLimit+1 { // 截断后追加省略号
		t.Fatalf("截断长度 = %d runes, want %d", len(runes), checkBodyLimit+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断结果应以省略号结尾：%q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Error("截断产生了无效的 UTF-8 替换字符")
	}

	// 恰好等于上限时不应截断
	exactLen := strings.Repeat("a", checkBodyLimit)
	if got := compactMessage(exactLen); strings.HasSuffix(got, "…") {
		t.Errorf("恰好 %d 字符不应截断：%q", checkBodyLimit, got)
	}
}

// firstNonEmpty 取第一个非空（按 TrimSpace 判定）的值。
func TestFirstNonEmpty(t *testing.T) {
	for _, test := range []struct {
		name   string
		values []string
		want   string
	}{
		{name: "无参数", values: nil, want: ""},
		{name: "全空", values: []string{"", "  ", "\t"}, want: ""},
		{name: "取第一个非空", values: []string{"", "first", "second"}, want: "first"},
		{name: "去掉首尾空白", values: []string{"  padded  "}, want: "padded"},
		{name: "单值", values: []string{"only"}, want: "only"},
		{name: "首个即非空", values: []string{"a", "b"}, want: "a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := firstNonEmpty(test.values...); got != test.want {
				t.Errorf("firstNonEmpty(%q) = %q, want %q", test.values, got, test.want)
			}
		})
	}
}

// credentialTokens 的读取顺序：provider env 优先于进程环境；api_key 与
// auth_token 各自独立取值。
func TestCredentialTokensPrefersOverrides(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "process-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "process-token")

	apiKey, authToken := credentialTokens(claudecfg.Overrides{})
	if apiKey != "process-key" || authToken != "process-token" {
		t.Errorf("进程环境: api_key=%q auth_token=%q", apiKey, authToken)
	}

	apiKey, authToken = credentialTokens(claudecfg.Overrides{Env: map[string]string{
		"ANTHROPIC_API_KEY":    "override-key",
		"ANTHROPIC_AUTH_TOKEN": "override-token",
	}})
	if apiKey != "override-key" || authToken != "override-token" {
		t.Errorf("覆盖优先: api_key=%q auth_token=%q", apiKey, authToken)
	}

	// 只覆盖一个时，另一个仍从进程环境取
	apiKey, authToken = credentialTokens(claudecfg.Overrides{Env: map[string]string{
		"ANTHROPIC_API_KEY": "override-key",
	}})
	if apiKey != "override-key" || authToken != "process-token" {
		t.Errorf("部分覆盖: api_key=%q auth_token=%q", apiKey, authToken)
	}
}

// Describe 覆盖三类结论：网络错误、成功、HTTP 错误。
func TestCredentialCheckDescribe(t *testing.T) {
	connection := CredentialCheck{BaseURL: "https://x.example", Err: io.ErrUnexpectedEOF}
	if got := connection.Describe(); !strings.Contains(got, "无法连接") ||
		!strings.Contains(got, "https://x.example") {
		t.Errorf("网络错误: Describe() = %q", got)
	}

	ok := CredentialCheck{BaseURL: "https://x.example", Model: "m", Status: 200}
	if got := ok.Describe(); !strings.Contains(got, "端点连通") || !strings.Contains(got, "m") {
		t.Errorf("成功: Describe() = %q", got)
	}

	failed := CredentialCheck{BaseURL: "https://x.example", Status: 403, Message: "denied"}
	if got := failed.Describe(); !strings.Contains(got, "HTTP 403") || !strings.Contains(got, "denied") {
		t.Errorf("HTTP 错误: Describe() = %q", got)
	}
}
