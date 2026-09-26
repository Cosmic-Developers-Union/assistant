package setup

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	gitea "gitea.dev/sdk"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// ---------- 请求录制与通用假服务端 ----------

// recordedRequest 是一次到达假服务端的请求快照：协议契约（路径、方法、认证头、
// 原始请求体）全部留存，供断言「setup 到底对 Gitea 发了什么」。
type recordedRequest struct {
	Method string
	Path   string
	// EscapedPath 保留 URL 编码形态（如 ai%2Fmerge），Path 已被 http 解码；
	// 断言 URL 转义行为时用前者。
	EscapedPath string
	Query       string
	Auth        string
	Basic       string
	Body        string
	// OTP 是 X-Gitea-OTP 头：个人令牌端点在账号开了两步验证时必须带上。
	OTP string
}

// giteaStub 是 setup 适配器的通用假服务端：按「方法 + 路径」注册处理函数，
// 未注册的请求返回 404，并记录所有到达的请求。
// /api/v1/version 始终可答：官方 SDK 在分支保护等接口前会探测服务端版本，
// 缺了它调用会直接失败。
type giteaStub struct {
	t        *testing.T
	mu       sync.Mutex
	routes   map[string]http.HandlerFunc
	requests []recordedRequest
}

func newGiteaStub(t *testing.T) *giteaStub {
	t.Helper()
	stub := &giteaStub{t: t, routes: map[string]http.HandlerFunc{}}
	stub.jsonRoute("GET /api/v1/version", map[string]string{"version": "1.22.0"})
	return stub
}

// handle 注册一条路由；pattern 形如 "GET /api/v1/user"（方法 + 空格 + 路径）。
func (s *giteaStub) handle(pattern string, handler http.HandlerFunc) {
	s.routes[pattern] = handler
}

// jsonStatusRoute 注册一条固定状态码 + JSON 体 + 可选消息头的路由。
func (s *giteaStub) jsonStatusRoute(pattern string, status int, payload any) {
	s.handle(pattern, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_ = json.NewEncoder(writer).Encode(payload)
	})
}

// jsonRoute 注册一条固定返回 JSON 的路由。
func (s *giteaStub) jsonRoute(pattern string, payload any) {
	s.handle(pattern, func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(payload)
	})
}

func (s *giteaStub) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body := make([]byte, 0)
	if request.Body != nil {
		raw, err := readAllLimited(request.Body)
		if err != nil {
			s.t.Errorf("读取请求体失败: %v", err)
		}
		body = raw
	}
	s.mu.Lock()
	s.requests = append(s.requests, recordedRequest{
		Method:      request.Method,
		Path:        request.URL.Path,
		EscapedPath: request.URL.EscapedPath(),
		Query:       request.URL.RawQuery,
		Auth:        request.Header.Get("Authorization"),
		Basic:       basicAuthHeader(request),
		Body:        string(body),
		OTP:         request.Header.Get("X-Gitea-OTP"),
	})
	s.mu.Unlock()
	if handler, ok := s.routes[request.Method+" "+request.URL.Path]; ok {
		handler(writer, request)
		return
	}
	http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
}

// basicAuthHeader 把 Basic Auth 还原成 user:password（无凭据时为空）。
func basicAuthHeader(request *http.Request) string {
	user, password, ok := request.BasicAuth()
	if !ok {
		return ""
	}
	return user + ":" + password
}

// readAllLimited 读完请求体（httptest 的请求体不会超过测试造的数据量）。
func readAllLimited(reader io.Reader) ([]byte, error) {
	buffer := make([]byte, 0, 256)
	chunk := make([]byte, 256)
	for {
		n, err := reader.Read(chunk)
		buffer = append(buffer, chunk[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buffer, nil
			}
			return buffer, err
		}
	}
}

// snapshot 返回已记录请求的副本。
func (s *giteaStub) snapshot() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.requests...)
}

// find 按「方法 + 路径」找第一条已记录的请求。
func (s *giteaStub) find(t *testing.T, method, path string) recordedRequest {
	t.Helper()
	for _, request := range s.snapshot() {
		if request.Method == method && request.Path == path {
			return request
		}
	}
	t.Fatalf("未收到请求 %s %s；实际记录：%+v", method, path, s.snapshot())
	return recordedRequest{}
}

// newStubAdmin 起一个假服务端并返回指向它的适配器（含官方 SDK 客户端，
// 因此 SDK 路径的写操作也可测）。
func newStubAdmin(t *testing.T, stub *giteaStub) (*giteaAdmin, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	sdk, err := gitea.NewClient(server.URL,
		gitea.SetToken("admin-token"),
		gitea.SetHTTPClient(server.Client()),
		gitea.SetUserAgent("assistant-test/1"),
	)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return &giteaAdmin{
		host: server.URL, token: "admin-token", http: server.Client(), sdk: sdk,
		passwords: map[string]string{}, log: func(string, ...any) {},
	}, server
}

// ---------- 纯助手与错误分类 ----------

func TestIsHTTPStatusMatchesWrappedError(t *testing.T) {
	base := &httpError{Method: "GET", Path: "/api/v1/user", Status: http.StatusForbidden, Message: "forbidden"}
	tests := map[string]struct {
		err    error
		status int
		want   bool
	}{
		"直接命中":      {base, http.StatusForbidden, true},
		"包装后命中":     {fmt.Errorf("校验令牌: %w", base), http.StatusForbidden, true},
		"状态码不符":     {base, http.StatusNotFound, false},
		"nil 错误":    {nil, http.StatusForbidden, false},
		"非 HTTP 错误": {errors.New("网络不通"), http.StatusForbidden, false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := isHTTPStatus(test.err, test.status); got != test.want {
				t.Errorf("isHTTPStatus() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestAPIErrorMessageExtraction(t *testing.T) {
	tests := map[string]struct {
		data []byte
		want string
	}{
		"标准 message":     {[]byte(`{"message":"用户已存在"}`), "用户已存在"},
		"message 带空白":    {[]byte(`{"message":"  权限不足  "}`), "权限不足"},
		"非 JSON 原文":      {[]byte("  upstream timeout  "), "upstream timeout"},
		"JSON 无 message": {[]byte(`{"error":"x"}`), ""},
		"空响应体":           {nil, ""},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := apiMessage(test.data); got != test.want {
				t.Errorf("apiMessage() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestHTTPErrorRendersMessage(t *testing.T) {
	withMessage := &httpError{Method: "GET", Path: "/api/v1/user", Status: 500, Message: "boom"}
	if got, want := withMessage.Error(), "GET /api/v1/user: HTTP 500: boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	withoutMessage := &httpError{Method: "PUT", Path: "/api/v1/x", Status: 403}
	if got, want := withoutMessage.Error(), "PUT /api/v1/x: HTTP 403"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// ---------- 构造：NewAdmin / NewRepoClient ----------

// NewAdmin 只接受在线校验通过的管理员令牌：非管理员必须被拒。
func TestNewAdminRejectsNonAdminToken(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/user", map[string]any{"login": "dev", "is_admin": false})
	_, server := newStubAdmin(t, stub)

	_, err := NewAdmin(t.Context(), Options{Host: server.URL, AdminToken: "dev-token"})
	if err == nil || !strings.Contains(err.Error(), "不是管理员") {
		t.Fatalf("NewAdmin() error = %v, want 管理员权限错误", err)
	}
}

// 令牌校验失败（401）时 NewAdmin 必须报错而不是继续构造。
func TestNewAdminPropagatesAuthFailure(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("GET /api/v1/user", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"message":"unauthorized"}`, http.StatusUnauthorized)
	})
	_, server := newStubAdmin(t, stub)

	_, err := NewAdmin(t.Context(), Options{Host: server.URL, AdminToken: "bad"})
	if err == nil || !strings.Contains(err.Error(), "校验管理员令牌") {
		t.Fatalf("NewAdmin() error = %v, want 校验管理员令牌 错误", err)
	}
}

// Gitea 返回缺 login 的响应体：视为协议违约，必须报错。
func TestNewAdminRejectsResponseWithoutLogin(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/user", map[string]any{"is_admin": true})
	_, server := newStubAdmin(t, stub)

	_, err := NewAdmin(t.Context(), Options{Host: server.URL, AdminToken: "admin-token"})
	if err == nil || !strings.Contains(err.Error(), "缺 login") {
		t.Fatalf("NewAdmin() error = %v, want 缺 login 错误", err)
	}
}

// Options 校验先于任何网络调用：非法 host / 缺令牌直接失败。
func TestNewAdminValidatesOptionsBeforeNetwork(t *testing.T) {
	tests := map[string]Options{
		"缺失 host":    {AdminToken: "admin-token"},
		"host 非 URL": {Host: "gitea.example.com", AdminToken: "admin-token"},
		"缺管理员令牌":     {Host: "https://gitea.example.com"},
	}
	for name, options := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAdmin(t.Context(), options); err == nil {
				t.Error("NewAdmin() error = nil, want 校验错误")
			}
		})
	}
}

func TestNewRepoClientValidatesInput(t *testing.T) {
	tests := map[string]struct {
		host  string
		token string
	}{
		"host 非 URL":     {host: "gitea.example.com", token: "t"},
		"host 相对路径":      {host: "/api", token: "t"},
		"host 非 HTTP 协议": {host: "ftp://gitea.example.com", token: "t"},
		"缺令牌":            {host: "https://gitea.example.com"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRepoClient(t.Context(), test.host, test.token, nil); err == nil {
				t.Error("NewRepoClient() error = nil, want 校验错误")
			}
		})
	}
}

// 令牌校验失败时必须报错，不返回半构造的客户端。
func TestNewRepoClientPropagatesAuthFailure(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("GET /api/v1/user", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"message":"unauthorized"}`, http.StatusUnauthorized)
	})
	_, server := newStubAdmin(t, stub)

	_, err := NewRepoClient(t.Context(), server.URL, "expired", nil)
	if err == nil || !strings.Contains(err.Error(), "校验令牌") {
		t.Fatalf("NewRepoClient() error = %v, want 校验令牌 错误", err)
	}
}

// 令牌校验失败（网络层）时必须报错。
func TestNewRepoClientPropagatesNetworkFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close() // 立即关闭：连接必然失败

	_, err := NewRepoClient(t.Context(), server.URL, "t", nil)
	if err == nil || !strings.Contains(err.Error(), "校验令牌") {
		t.Fatalf("NewRepoClient() error = %v, want 校验令牌 错误", err)
	}
}

// 成功路径：host 归一化、Login() 记录归属账号、Token() 返回原令牌。
func TestNewRepoClientRecordsLoginAndToken(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/user", map[string]any{"login": "dev", "is_admin": false})
	_, server := newStubAdmin(t, stub)

	client, err := NewRepoClient(t.Context(), server.URL+"/", "dev-token", nil)
	if err != nil {
		t.Fatalf("NewRepoClient() error = %v", err)
	}
	if client.Login() != "dev" {
		t.Errorf("Login() = %q, want dev", client.Login())
	}
	if client.Token() != "dev-token" {
		t.Errorf("Token() = %q, want dev-token", client.Token())
	}
	request := stub.find(t, http.MethodGet, "/api/v1/user")
	if request.Auth != "token dev-token" {
		t.Errorf("Authorization = %q, want token dev-token", request.Auth)
	}
}

// NewAdmin 成功路径：管理员令牌通过校验，构造出可直接使用的 SDK。
func TestNewAdminAcceptsAdminToken(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/user", map[string]any{"login": "admin", "is_admin": true})
	stub.jsonRoute("GET /api/v1/repos/acme/repo", map[string]any{"default_branch": "main", "empty": false})
	_, server := newStubAdmin(t, stub)

	admin, err := NewAdmin(t.Context(), Options{Host: server.URL, AdminToken: "admin-token"})
	if err != nil {
		t.Fatalf("NewAdmin() error = %v", err)
	}
	if admin.Login() != "admin" || admin.Token() != "admin-token" {
		t.Errorf("Login() = %q Token() = %q", admin.Login(), admin.Token())
	}
	if admin.sdk == nil {
		t.Fatal("NewAdmin() 未构造 SDK 客户端")
	}
	// 构造出的客户端必须真的可用（NewRepoClient 需要 host 不带尾斜杠）。
	if _, _, err := admin.GetRepo(t.Context(), "acme/repo"); err != nil {
		t.Errorf("GetRepo() error = %v", err)
	}
}

// ---------- 账号：AuthenticatedUser / UserExists / CreateUser / EnsurePassword ----------

func TestAuthenticatedUserReadsAdminFlag(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/user", map[string]any{"login": "ops", "is_admin": true})
	admin, _ := newStubAdmin(t, stub)

	login, isAdmin, err := admin.AuthenticatedUser(t.Context())
	if err != nil {
		t.Fatalf("AuthenticatedUser() error = %v", err)
	}
	if login != "ops" || !isAdmin {
		t.Errorf("AuthenticatedUser() = (%q, %v), want (ops, true)", login, isAdmin)
	}
}

// 服务端 5xx 是错误而非「不存在」/「非管理员」，必须原样冒泡。
func TestAuthenticatedUserPropagatesServerError(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("GET /api/v1/user", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"message":"内部错误"}`, http.StatusInternalServerError)
	})
	admin, _ := newStubAdmin(t, stub)

	_, _, err := admin.AuthenticatedUser(t.Context())
	if err == nil {
		t.Fatal("AuthenticatedUser() error = nil, want 5xx 错误")
	}
	if !isHTTPStatus(err, http.StatusInternalServerError) {
		t.Errorf("error = %v, want HTTP 500", err)
	}
	if !strings.Contains(err.Error(), "内部错误") {
		t.Errorf("error = %v, want 服务端 message 透出", err)
	}
}

// 响应体损坏时必须报「解析响应」错误，不能静默当作空账号。
func TestAuthenticatedUserRejectsMalformedJSON(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("GET /api/v1/user", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("{not json"))
	})
	admin, _ := newStubAdmin(t, stub)

	_, _, err := admin.AuthenticatedUser(t.Context())
	if err == nil || !strings.Contains(err.Error(), "解析") {
		t.Fatalf("AuthenticatedUser() error = %v, want 解析响应错误", err)
	}
}

func TestUserExistsDistinguishesMissingFromFailure(t *testing.T) {
	tests := map[string]struct {
		status   int
		want     bool
		wantErr  bool
		wantHint string
	}{
		"存在":    {status: http.StatusOK, want: true},
		"不存在":   {status: http.StatusNotFound, want: false},
		"权限不足":  {status: http.StatusForbidden, wantErr: true, wantHint: "forbidden"},
		"服务端故障": {status: http.StatusInternalServerError, wantErr: true, wantHint: "boom"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stub := newGiteaStub(t)
			status := test.status
			stub.handle("GET /api/v1/users/ai", func(writer http.ResponseWriter, _ *http.Request) {
				if status == http.StatusOK {
					_ = json.NewEncoder(writer).Encode(map[string]any{"login": "ai"})
					return
				}
				http.Error(writer, fmt.Sprintf(`{"message":%q}`, hintFor(status)), status)
			})
			admin, _ := newStubAdmin(t, stub)

			exists, err := admin.UserExists(t.Context(), "ai")
			switch {
			case test.wantErr:
				if err == nil {
					t.Fatal("UserExists() error = nil, want 错误")
				}
				if test.wantHint != "" && !strings.Contains(err.Error(), test.wantHint) {
					t.Errorf("error = %v, want 含 %q", err, test.wantHint)
				}
			default:
				if err != nil {
					t.Fatalf("UserExists() error = %v", err)
				}
				if exists != test.want {
					t.Errorf("UserExists() = %v, want %v", exists, test.want)
				}
			}
		})
	}
}

func hintFor(status int) string {
	switch status {
	case http.StatusForbidden:
		return "forbidden"
	default:
		return "boom"
	}
}

// 账号名按路径转义后再拼进 URL（防止穿路径）。
func TestUserExistsEscapesName(t *testing.T) {
	stub := newGiteaStub(t)
	admin, _ := newStubAdmin(t, stub)

	if _, err := admin.UserExists(t.Context(), "ai/merge"); err != nil {
		t.Fatalf("UserExists() error = %v", err)
	}
	for _, request := range stub.snapshot() {
		if request.Method != http.MethodGet || !strings.Contains(request.EscapedPath, "/users/") {
			continue
		}
		if request.EscapedPath != "/api/v1/users/ai%2Fmerge" {
			t.Errorf("escaped path = %q, want /api/v1/users/ai%%2Fmerge", request.EscapedPath)
		}
		return
	}
	t.Fatalf("未记录到账号查询请求：%+v", stub.snapshot())
}

// 创建账号：协议契约是把 username/email/password/must_change_password 交给
// POST /api/v1/admin/users；成功时密码被记下供 EnsurePassword 复用。
func TestCreateUserSendsExpectedPayload(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("POST /api/v1/admin/users", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": 7, "login": "ai"})
	})
	admin, _ := newStubAdmin(t, stub)

	if err := admin.CreateUser(t.Context(), "ai", "ai@gitea.example.com"); err != nil {
		t.Fatalf("CreateUser() error = %v", err)
	}
	request := stub.find(t, http.MethodPost, "/api/v1/admin/users")
	if request.Auth != "token admin-token" {
		t.Errorf("Authorization = %q, want token admin-token", request.Auth)
	}
	var body struct {
		Username           string `json:"username"`
		Email              string `json:"email"`
		Password           string `json:"password"`
		MustChangePassword *bool  `json:"must_change_password"`
	}
	if err := json.Unmarshal([]byte(request.Body), &body); err != nil {
		t.Fatalf("请求体不是 JSON: %v (%s)", err, request.Body)
	}
	if body.Username != "ai" || body.Email != "ai@gitea.example.com" {
		t.Errorf("body = %+v, want ai/ai@gitea.example.com", body)
	}
	if len(body.Password) < 24 {
		t.Errorf("password 太弱：%q", body.Password)
	}
	if body.MustChangePassword == nil || *body.MustChangePassword {
		t.Errorf("must_change_password = %v, want 显式 false（机器人不登录 UI）", body.MustChangePassword)
	}
	if admin.passwords["ai"] != body.Password {
		t.Errorf("passwords[ai] = %q, want 记录本次创建的密码", admin.passwords["ai"])
	}
}

// 账号已存在：Gitea 返回 422，错误必须带语境与账号名。
func TestCreateUserSurfacesAlreadyExists(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("POST /api/v1/admin/users", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"message":"user already exists [name]"}`, http.StatusUnprocessableEntity)
	})
	admin, _ := newStubAdmin(t, stub)

	err := admin.CreateUser(t.Context(), "ai", "ai@example.com")
	if err == nil {
		t.Fatal("CreateUser() error = nil, want 错误")
	}
	if !strings.Contains(err.Error(), "创建账号 ai") || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want 语境 + 服务端 message", err)
	}
}

// 权限不足：非管理员令牌调用创建账号必须 403 冒泡。
func TestCreateUserSurfacesPermissionDenied(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("POST /api/v1/admin/users", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"message":"token does not have at least one of required scope"}`, http.StatusForbidden)
	})
	admin, _ := newStubAdmin(t, stub)

	err := admin.CreateUser(t.Context(), "ai", "ai@example.com")
	if err == nil {
		t.Fatal("CreateUser() error = nil, want 权限错误")
	}
	if !strings.Contains(err.Error(), "创建账号 ai") {
		t.Errorf("error = %v, want 语境包装", err)
	}
}

// SDK 路径的响应体损坏时必须报错。
func TestCreateUserSurfacesMalformedResponse(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("POST /api/v1/admin/users", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("not json"))
	})
	admin, _ := newStubAdmin(t, stub)

	if err := admin.CreateUser(t.Context(), "ai", "ai@example.com"); err == nil {
		t.Fatal("CreateUser() error = nil, want 解析错误")
	}
}

// 没有管理员令牌（sdk == nil）时创建账号必须直接拒绝，不发任何请求。
func TestCreateUserWithoutAdminClient(t *testing.T) {
	admin := &giteaAdmin{log: func(string, ...any) {}, passwords: map[string]string{}}
	err := admin.CreateUser(t.Context(), "ai", "ai@example.com")
	if err == nil || !strings.Contains(err.Error(), "缺少管理员令牌") {
		t.Fatalf("CreateUser() error = %v, want 缺少管理员令牌", err)
	}
}

// EnsurePassword：本次创建的密码优先，不触发任何重置请求。
func TestEnsurePasswordPrefersCreatedPassword(t *testing.T) {
	stub := newGiteaStub(t)
	admin, _ := newStubAdmin(t, stub)
	admin.passwords["ai"] = "created-password"

	password, err := admin.EnsurePassword(t.Context(), "ai")
	if err != nil {
		t.Fatalf("EnsurePassword() error = %v", err)
	}
	if password != "created-password" {
		t.Errorf("EnsurePassword() = %q, want created-password", password)
	}
	if len(stub.snapshot()) != 0 {
		t.Errorf("EnsurePassword() 不应发起请求：%+v", stub.snapshot())
	}
}

// EnsurePassword：账号本就存在时回退到管理员重置（PATCH /admin/users/{name}），
// 且 login_name 必须显式给出（Gitea 1.22 起必填，缺失会 422）。
func TestEnsurePasswordFallsBackToReset(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("PATCH /api/v1/admin/users/ai", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	admin, _ := newStubAdmin(t, stub)

	password, err := admin.EnsurePassword(t.Context(), "ai")
	if err != nil {
		t.Fatalf("EnsurePassword() error = %v", err)
	}
	if len(password) < 24 {
		t.Errorf("重置密码太弱：%q", password)
	}
	request := stub.find(t, http.MethodPatch, "/api/v1/admin/users/ai")
	var body struct {
		LoginName          string `json:"login_name"`
		Password           string `json:"password"`
		MustChangePassword *bool  `json:"must_change_password"`
	}
	if err := json.Unmarshal([]byte(request.Body), &body); err != nil {
		t.Fatalf("请求体不是 JSON: %v (%s)", err, request.Body)
	}
	if body.LoginName != "ai" {
		t.Errorf("login_name = %q, want ai", body.LoginName)
	}
	if body.Password != password {
		t.Errorf("password 字段与返回值不一致：%q vs %q", body.Password, password)
	}
	if body.MustChangePassword == nil || *body.MustChangePassword {
		t.Errorf("must_change_password = %v, want false", body.MustChangePassword)
	}
	if admin.passwords["ai"] != password {
		t.Errorf("重置后的密码未被记下：%q", admin.passwords["ai"])
	}
}

// 重置失败（账号不存在 / 服务端故障）必须冒泡。
func TestEnsurePasswordSurfacesResetFailure(t *testing.T) {
	tests := map[string]int{
		"账号不存在": http.StatusNotFound,
		"服务端故障": http.StatusInternalServerError,
	}
	for name, status := range tests {
		t.Run(name, func(t *testing.T) {
			stub := newGiteaStub(t)
			code := status
			stub.handle("PATCH /api/v1/admin/users/ai", func(writer http.ResponseWriter, _ *http.Request) {
				http.Error(writer, `{"message":"edit failed"}`, code)
			})
			admin, _ := newStubAdmin(t, stub)

			if _, err := admin.EnsurePassword(t.Context(), "ai"); err == nil {
				t.Error("EnsurePassword() error = nil, want 错误")
			}
		})
	}
}

func TestResetPasswordWithoutAdminClient(t *testing.T) {
	admin := &giteaAdmin{log: func(string, ...any) {}, passwords: map[string]string{}}
	if _, err := admin.EnsurePassword(t.Context(), "ai"); err == nil ||
		!strings.Contains(err.Error(), "缺少管理员令牌") {
		t.Fatalf("EnsurePassword() error = %v, want 缺少管理员令牌", err)
	}
}

// ---------- ValidateToken ----------

func TestValidateTokenClassifiesResult(t *testing.T) {
	tests := map[string]struct {
		handler http.HandlerFunc
		want    bool
		wantErr bool
	}{
		"令牌归属账号": {
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(writer).Encode(map[string]any{"login": "ai"})
			},
			want: true,
		},
		"令牌属于别的账号": {
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(writer).Encode(map[string]any{"login": "someone-else"})
			},
			want: false,
		},
		"令牌已过期（401）": {
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				http.Error(writer, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			},
			want: false,
		},
		"令牌被禁用（403）": {
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				http.Error(writer, `{"message":"forbidden"}`, http.StatusForbidden)
			},
			want: false,
		},
		"服务端故障（500）": {
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				http.Error(writer, `{"message":"boom"}`, http.StatusInternalServerError)
			},
			wantErr: true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stub := newGiteaStub(t)
			stub.handle("GET /api/v1/user", test.handler)
			admin, _ := newStubAdmin(t, stub)

			ok, err := admin.ValidateToken(t.Context(), "ai", "candidate")
			switch {
			case test.wantErr:
				if err == nil {
					t.Fatal("ValidateToken() error = nil, want 错误")
				}
			default:
				if err != nil {
					t.Fatalf("ValidateToken() error = %v", err)
				}
				if ok != test.want {
					t.Errorf("ValidateToken() = %v, want %v", ok, test.want)
				}
			}
			request := stub.find(t, http.MethodGet, "/api/v1/user")
			if request.Auth != "token candidate" {
				t.Errorf("Authorization = %q, want token candidate", request.Auth)
			}
		})
	}
}

// 网络不通是错误而非「令牌无效」——否则调用方会误判为需要重建令牌。
func TestValidateTokenPropagatesNetworkFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	admin := &giteaAdmin{host: server.URL, http: server.Client(), log: func(string, ...any) {}}

	if _, err := admin.ValidateToken(t.Context(), "ai", "candidate"); err == nil {
		t.Fatal("ValidateToken() error = nil, want 网络错误")
	}
}

// ---------- 仓库：GetRepo / CreateRepo ----------

func TestGetRepoReportsPresence(t *testing.T) {
	tests := map[string]struct {
		status  int
		payload map[string]any
		want    RepoInfo
		wantOK  bool
		wantErr bool
	}{
		"存在且非空": {
			status:  http.StatusOK,
			payload: map[string]any{"default_branch": "main", "empty": false},
			want:    RepoInfo{DefaultBranch: "main"}, wantOK: true,
		},
		"存在但为空仓库": {
			status:  http.StatusOK,
			payload: map[string]any{"default_branch": "", "empty": true},
			want:    RepoInfo{Empty: true}, wantOK: true,
		},
		"不存在": {status: http.StatusNotFound, wantOK: false},
		"无权限": {status: http.StatusForbidden, wantErr: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stub := newGiteaStub(t)
			code, payload := test.status, test.payload
			stub.handle("GET /api/v1/repos/acme/repo", func(writer http.ResponseWriter, _ *http.Request) {
				if code == http.StatusOK {
					_ = json.NewEncoder(writer).Encode(payload)
					return
				}
				http.Error(writer, `{"message":"nope"}`, code)
			})
			admin, _ := newStubAdmin(t, stub)

			info, ok, err := admin.GetRepo(t.Context(), "acme/repo")
			switch {
			case test.wantErr:
				if err == nil {
					t.Fatal("GetRepo() error = nil, want 错误")
				}
			default:
				if err != nil {
					t.Fatalf("GetRepo() error = %v", err)
				}
				if ok != test.wantOK || info != test.want {
					t.Errorf("GetRepo() = (%+v, %v), want (%+v, %v)", info, ok, test.want, test.wantOK)
				}
			}
		})
	}
}

func TestGetRepoRejectsMalformedName(t *testing.T) {
	admin := &giteaAdmin{log: func(string, ...any) {}}
	_, _, err := admin.GetRepo(t.Context(), "no-slash")
	if err == nil || !strings.Contains(err.Error(), "owner/name") {
		t.Fatalf("GetRepo() error = %v, want 仓库名格式错误", err)
	}
}

// 空 owner / 空 name 都必须被拒绝（边界条件）。
func TestGetRepoRejectsBlankSegments(t *testing.T) {
	admin := &giteaAdmin{log: func(string, ...any) {}}
	for _, fullName := range []string{"/repo", "owner/", "", "a/b/c"} {
		if _, _, err := admin.GetRepo(t.Context(), fullName); err == nil {
			t.Errorf("GetRepo(%q) error = nil, want 格式错误", fullName)
		}
	}
}

// CreateRepo：owner 是组织时走 POST /orgs/{owner}/repos。
func TestCreateRepoForOrganization(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/orgs/acme", map[string]any{"id": 1, "name": "acme"})
	stub.handle("POST /api/v1/orgs/acme/repos", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": 2, "name": "repo"})
	})
	admin, _ := newStubAdmin(t, stub)

	if err := admin.CreateRepo(t.Context(), "acme/repo"); err != nil {
		t.Fatalf("CreateRepo() error = %v", err)
	}
	request := stub.find(t, http.MethodPost, "/api/v1/orgs/acme/repos")
	var body struct {
		Name          string `json:"name"`
		Private       bool   `json:"private"`
		AutoInit      bool   `json:"auto_init"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal([]byte(request.Body), &body); err != nil {
		t.Fatalf("请求体不是 JSON: %v (%s)", err, request.Body)
	}
	if body.Name != "repo" || !body.Private || !body.AutoInit || body.DefaultBranch != "main" {
		t.Errorf("body = %+v, want repo/private/auto_init/main", body)
	}
}

// CreateRepo：owner 不是组织时走 POST /admin/users/{owner}/repos。
func TestCreateRepoForUserOwner(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("GET /api/v1/orgs/xwh", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
	})
	stub.handle("POST /api/v1/admin/users/xwh/repos", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": 2, "name": "repo"})
	})
	admin, _ := newStubAdmin(t, stub)

	if err := admin.CreateRepo(t.Context(), "xwh/repo"); err != nil {
		t.Fatalf("CreateRepo() error = %v", err)
	}
	stub.find(t, http.MethodPost, "/api/v1/admin/users/xwh/repos")
}

// 探测组织的请求本身失败（非 404）时不能猜成用户仓库，必须报错。
func TestCreateRepoSurfacesOrgProbeFailure(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("GET /api/v1/orgs/acme", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	admin, _ := newStubAdmin(t, stub)

	err := admin.CreateRepo(t.Context(), "acme/repo")
	if err == nil || !strings.Contains(err.Error(), "探测组织 acme") {
		t.Fatalf("CreateRepo() error = %v, want 探测组织错误", err)
	}
}

// 组织仓库创建失败：错误必须带仓库全名。
func TestCreateRepoSurfacesOrgCreateFailure(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/orgs/acme", map[string]any{"id": 1})
	stub.handle("POST /api/v1/orgs/acme/repos", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"message":"repo already exists"}`, http.StatusConflict)
	})
	admin, _ := newStubAdmin(t, stub)

	err := admin.CreateRepo(t.Context(), "acme/repo")
	if err == nil || !strings.Contains(err.Error(), "创建组织仓库 acme/repo") {
		t.Fatalf("CreateRepo() error = %v, want 创建组织仓库错误", err)
	}
}

// 用户仓库创建失败：错误必须带仓库全名。
func TestCreateRepoSurfacesUserCreateFailure(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("GET /api/v1/orgs/xwh", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
	})
	stub.handle("POST /api/v1/admin/users/xwh/repos", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"message":"permission denied"}`, http.StatusForbidden)
	})
	admin, _ := newStubAdmin(t, stub)

	err := admin.CreateRepo(t.Context(), "xwh/repo")
	if err == nil || !strings.Contains(err.Error(), "创建用户仓库 xwh/repo") {
		t.Fatalf("CreateRepo() error = %v, want 创建用户仓库错误", err)
	}
}

func TestCreateRepoRejectsBadInput(t *testing.T) {
	admin, _ := newStubAdmin(t, newGiteaStub(t))
	if err := admin.CreateRepo(t.Context(), "no-slash"); err == nil ||
		!strings.Contains(err.Error(), "owner/name") {
		t.Errorf("CreateRepo() error = %v, want 仓库名格式错误", err)
	}
	withoutSDK := &giteaAdmin{log: func(string, ...any) {}}
	if err := withoutSDK.CreateRepo(t.Context(), "acme/repo"); err == nil ||
		!strings.Contains(err.Error(), "缺少管理员令牌") {
		t.Errorf("CreateRepo() error = %v, want 缺少管理员令牌", err)
	}
}

// ---------- AddCollaborator ----------

func TestAddCollaboratorSendsPermission(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("PUT /api/v1/repos/acme/repo/collaborators/merge", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	admin, _ := newStubAdmin(t, stub)

	if err := admin.AddCollaborator(t.Context(), "acme/repo", "merge", "admin"); err != nil {
		t.Fatalf("AddCollaborator() error = %v", err)
	}
	request := stub.find(t, http.MethodPut, "/api/v1/repos/acme/repo/collaborators/merge")
	var body struct {
		Permission string `json:"permission"`
	}
	if err := json.Unmarshal([]byte(request.Body), &body); err != nil {
		t.Fatalf("请求体不是 JSON: %v (%s)", err, request.Body)
	}
	if body.Permission != "admin" {
		t.Errorf("permission = %q, want admin", body.Permission)
	}
}

func TestAddCollaboratorErrors(t *testing.T) {
	tests := map[string]struct {
		fullName string
		status   int
		hint     string
	}{
		"仓库名非法": {fullName: "nope", hint: "owner/name"},
		"权限被拒":  {fullName: "acme/repo", status: http.StatusForbidden, hint: "添加协作者 ai 到 acme/repo"},
		"用户不存在": {fullName: "acme/repo", status: http.StatusNotFound, hint: "添加协作者 ai 到 acme/repo"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stub := newGiteaStub(t)
			if test.status != 0 {
				code := test.status
				stub.handle("PUT /api/v1/repos/acme/repo/collaborators/ai", func(writer http.ResponseWriter, _ *http.Request) {
					http.Error(writer, `{"message":"collaborator failed"}`, code)
				})
			}
			admin, _ := newStubAdmin(t, stub)

			err := admin.AddCollaborator(t.Context(), test.fullName, "ai", "write")
			if err == nil || !strings.Contains(err.Error(), test.hint) {
				t.Fatalf("AddCollaborator() error = %v, want 含 %q", err, test.hint)
			}
		})
	}
}

func TestAddCollaboratorWithoutAdminClient(t *testing.T) {
	admin := &giteaAdmin{log: func(string, ...any) {}}
	if err := admin.AddCollaborator(t.Context(), "acme/repo", "ai", "write"); err == nil ||
		!strings.Contains(err.Error(), "缺少管理员令牌") {
		t.Fatalf("AddCollaborator() error = %v, want 缺少管理员令牌", err)
	}
}

// ---------- EnsureBranchProtection ----------

// 已有同名规则：必须 PATCH，且七个门禁字段逐个显式写入（nil 会被 Edit 语义
// 当作「保持不变」，静默丢失门禁）。
func TestEnsureBranchProtectionEditsExistingRule(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/repos/acme/repo/branch_protections",
		[]map[string]any{{"id": 3, "rule_name": "main", "branch_name": "main"}})
	stub.handle("PATCH /api/v1/repos/acme/repo/branch_protections/main",
		func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": 3, "rule_name": "main"})
		})
	admin, _ := newStubAdmin(t, stub)

	if err := admin.EnsureBranchProtection(t.Context(), "acme/repo", ProtectionOptions{
		Branch: "main", RequiredApprovals: 2, MergerName: "merge",
	}); err != nil {
		t.Fatalf("EnsureBranchProtection() error = %v", err)
	}
	stub.find(t, http.MethodPatch, "/api/v1/repos/acme/repo/branch_protections/main")
	payload := branchProtectionPayload(t, stub, http.MethodPatch, "/api/v1/repos/acme/repo/branch_protections/main")
	assertProtectionGates(t, payload, protectionGates{
		requiredApprovals: 2, merger: "merge", adminOverride: false,
	})
	// SDK 会把未赋值的指针序列化成 null；契约是「不等于 true」，即不启用。
	if payload["enable_status_check"] == true {
		t.Errorf("未提供 StatusCheckContexts 时不应写 enable_status_check：%+v", payload)
	}
}

// 没有同名规则：必须 POST 创建。
func TestEnsureBranchProtectionCreatesMissingRule(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/repos/acme/repo/branch_protections", []map[string]any{})
	stub.handle("POST /api/v1/repos/acme/repo/branch_protections",
		func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": 4, "rule_name": "main"})
		})
	admin, _ := newStubAdmin(t, stub)

	if err := admin.EnsureBranchProtection(t.Context(), "acme/repo", ProtectionOptions{
		Branch: "main", RequiredApprovals: 1, MergerName: "merge",
	}); err != nil {
		t.Fatalf("EnsureBranchProtection() error = %v", err)
	}
	payload := branchProtectionPayload(t, stub, http.MethodPost, "/api/v1/repos/acme/repo/branch_protections")
	if payload["branch_name"] != "main" {
		t.Errorf("branch_name = %v, want main", payload["branch_name"])
	}
	assertProtectionGates(t, payload, protectionGates{
		requiredApprovals: 1, merger: "merge", adminOverride: false,
	})
	if payload["enable_status_check"] == true {
		t.Errorf("未提供 StatusCheckContexts 时不应启用状态检查：%+v", payload)
	}
}

// 已有规则但不属于本次配置的分支：视作缺失，走创建（不能改错分支）。
func TestEnsureBranchProtectionIgnoresOtherBranches(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/repos/acme/repo/branch_protections",
		[]map[string]any{{"id": 3, "rule_name": "release"}})
	stub.handle("POST /api/v1/repos/acme/repo/branch_protections",
		func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": 4})
		})
	admin, _ := newStubAdmin(t, stub)

	if err := admin.EnsureBranchProtection(t.Context(), "acme/repo", ProtectionOptions{
		Branch: "main", RequiredApprovals: 2, MergerName: "merge",
	}); err != nil {
		t.Fatalf("EnsureBranchProtection() error = %v", err)
	}
	stub.find(t, http.MethodPost, "/api/v1/repos/acme/repo/branch_protections")
}

// AllowAdminOverride=true 时不勾选「管理员须遵守分支保护规则」。
func TestEnsureBranchProtectionAllowsAdminOverride(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/repos/acme/repo/branch_protections", []map[string]any{})
	stub.handle("POST /api/v1/repos/acme/repo/branch_protections",
		func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": 4})
		})
	admin, _ := newStubAdmin(t, stub)

	if err := admin.EnsureBranchProtection(t.Context(), "acme/repo", ProtectionOptions{
		Branch: "main", RequiredApprovals: 2, MergerName: "merge", AllowAdminOverride: true,
	}); err != nil {
		t.Fatalf("EnsureBranchProtection() error = %v", err)
	}
	payload := branchProtectionPayload(t, stub, http.MethodPost, "/api/v1/repos/acme/repo/branch_protections")
	if payload["block_admin_merge_override"] != false {
		t.Errorf("block_admin_merge_override = %v, want false", payload["block_admin_merge_override"])
	}
}

// 提供 StatusCheckContexts 时启用状态检查并把 context 原样写入。
func TestEnsureBranchProtectionWritesStatusChecks(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/repos/acme/repo/branch_protections", []map[string]any{})
	stub.handle("POST /api/v1/repos/acme/repo/branch_protections",
		func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": 4})
		})
	admin, _ := newStubAdmin(t, stub)

	contexts := []string{"ci/build", "ci/lint"}
	if err := admin.EnsureBranchProtection(t.Context(), "acme/repo", ProtectionOptions{
		Branch: "main", RequiredApprovals: 2, MergerName: "merge", StatusCheckContexts: contexts,
	}); err != nil {
		t.Fatalf("EnsureBranchProtection() error = %v", err)
	}
	payload := branchProtectionPayload(t, stub, http.MethodPost, "/api/v1/repos/acme/repo/branch_protections")
	if payload["enable_status_check"] != true {
		t.Errorf("enable_status_check = %v, want true", payload["enable_status_check"])
	}
	raw, ok := payload["status_check_contexts"].([]any)
	if !ok || len(raw) != 2 || raw[0] != "ci/build" || raw[1] != "ci/lint" {
		t.Errorf("status_check_contexts = %v, want %v", payload["status_check_contexts"], contexts)
	}
}

// 编辑语义同样要在显式提供时写状态检查。
func TestEnsureBranchProtectionEditsStatusChecks(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/repos/acme/repo/branch_protections",
		[]map[string]any{{"id": 3, "rule_name": "main"}})
	stub.handle("PATCH /api/v1/repos/acme/repo/branch_protections/main",
		func(writer http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": 3})
		})
	admin, _ := newStubAdmin(t, stub)

	if err := admin.EnsureBranchProtection(t.Context(), "acme/repo", ProtectionOptions{
		Branch: "main", RequiredApprovals: 2, MergerName: "merge",
		StatusCheckContexts: []string{"ci/build"},
	}); err != nil {
		t.Fatalf("EnsureBranchProtection() error = %v", err)
	}
	payload := branchProtectionPayload(t, stub, http.MethodPatch, "/api/v1/repos/acme/repo/branch_protections/main")
	if payload["enable_status_check"] != true {
		t.Errorf("enable_status_check = %v, want true", payload["enable_status_check"])
	}
}

func TestEnsureBranchProtectionErrors(t *testing.T) {
	tests := map[string]struct {
		fullName string
		status   int
		hint     string
	}{
		"仓库名非法": {fullName: "nope", hint: "owner/name"},
		"读取失败":  {fullName: "acme/repo", status: http.StatusInternalServerError, hint: "读取 acme/repo 分支保护"},
		"创建失败":  {fullName: "acme/repo", status: http.StatusForbidden, hint: "读取 acme/repo 分支保护"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stub := newGiteaStub(t)
			if test.status != 0 {
				code := test.status
				stub.handle("GET /api/v1/repos/acme/repo/branch_protections", func(writer http.ResponseWriter, _ *http.Request) {
					http.Error(writer, `{"message":"branch protection failed"}`, code)
				})
			}
			admin, _ := newStubAdmin(t, stub)

			err := admin.EnsureBranchProtection(t.Context(), test.fullName, ProtectionOptions{
				Branch: "main", RequiredApprovals: 2, MergerName: "merge",
			})
			if err == nil || !strings.Contains(err.Error(), test.hint) {
				t.Fatalf("EnsureBranchProtection() error = %v, want 含 %q", err, test.hint)
			}
		})
	}
}

// 创建分支保护失败（读取成功、写入被拒）也必须报错。
func TestEnsureBranchProtectionSurfacesCreateFailure(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/repos/acme/repo/branch_protections", []map[string]any{})
	stub.handle("POST /api/v1/repos/acme/repo/branch_protections", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"message":"permission denied"}`, http.StatusForbidden)
	})
	admin, _ := newStubAdmin(t, stub)

	err := admin.EnsureBranchProtection(t.Context(), "acme/repo", ProtectionOptions{
		Branch: "main", RequiredApprovals: 2, MergerName: "merge",
	})
	if err == nil || !strings.Contains(err.Error(), "创建 acme/repo 分支保护") {
		t.Fatalf("EnsureBranchProtection() error = %v, want 创建分支保护错误", err)
	}
}

// 更新分支保护失败也必须报错。
func TestEnsureBranchProtectionSurfacesEditFailure(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/repos/acme/repo/branch_protections",
		[]map[string]any{{"id": 3, "rule_name": "main"}})
	stub.handle("PATCH /api/v1/repos/acme/repo/branch_protections/main", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"message":"permission denied"}`, http.StatusForbidden)
	})
	admin, _ := newStubAdmin(t, stub)

	err := admin.EnsureBranchProtection(t.Context(), "acme/repo", ProtectionOptions{
		Branch: "main", RequiredApprovals: 2, MergerName: "merge",
	})
	if err == nil || !strings.Contains(err.Error(), "更新 acme/repo 分支保护") {
		t.Fatalf("EnsureBranchProtection() error = %v, want 更新分支保护错误", err)
	}
}

func TestEnsureBranchProtectionWithoutAdminClient(t *testing.T) {
	admin := &giteaAdmin{log: func(string, ...any) {}}
	if err := admin.EnsureBranchProtection(t.Context(), "acme/repo", ProtectionOptions{}); err == nil ||
		!strings.Contains(err.Error(), "缺少管理员令牌") {
		t.Fatalf("EnsureBranchProtection() error = %v, want 缺少管理员令牌", err)
	}
}

type protectionGates struct {
	requiredApprovals int64
	merger            string
	adminOverride     bool
}

// assertProtectionGates 断言分支保护的七个门禁字段：缺一个都会让「双批准 +
// 只允许 merger 合并」的闭环失效。
func assertProtectionGates(t *testing.T, payload map[string]any, want protectionGates) {
	t.Helper()
	if got := payload["required_approvals"]; got != float64(want.requiredApprovals) {
		t.Errorf("required_approvals = %v, want %d", got, want.requiredApprovals)
	}
	if payload["block_on_rejected_reviews"] != true {
		t.Errorf("block_on_rejected_reviews = %v, want true", payload["block_on_rejected_reviews"])
	}
	// 存在 pending 官方评审请求时阻止合并：内容评审者回应后 Gitea 删除请求行，
	// 门禁随之自动解除。
	if payload["block_on_official_review_requests"] != true {
		t.Errorf("block_on_official_review_requests = %v, want true", payload["block_on_official_review_requests"])
	}
	if payload["dismiss_stale_approvals"] != true {
		t.Errorf("dismiss_stale_approvals = %v, want true", payload["dismiss_stale_approvals"])
	}
	if payload["block_on_outdated_branch"] != true {
		t.Errorf("block_on_outdated_branch = %v, want true", payload["block_on_outdated_branch"])
	}
	if payload["enable_merge_whitelist"] != true {
		t.Errorf("enable_merge_whitelist = %v, want true", payload["enable_merge_whitelist"])
	}
	whitelist, ok := payload["merge_whitelist_usernames"].([]any)
	if !ok || len(whitelist) != 1 || whitelist[0] != want.merger {
		t.Errorf("merge_whitelist_usernames = %v, want [%s]", payload["merge_whitelist_usernames"], want.merger)
	}
	if payload["block_admin_merge_override"] != !want.adminOverride {
		t.Errorf("block_admin_merge_override = %v, want %v", payload["block_admin_merge_override"], !want.adminOverride)
	}
}

// branchProtectionPayload 取出分支保护写请求的 JSON 体。
func branchProtectionPayload(t *testing.T, stub *giteaStub, method, path string) map[string]any {
	t.Helper()
	request := stub.find(t, method, path)
	var payload map[string]any
	if err := json.Unmarshal([]byte(request.Body), &payload); err != nil {
		t.Fatalf("分支保护请求体不是 JSON: %v (%s)", err, request.Body)
	}
	return payload
}

// ---------- SetRepoSecret ----------

func TestSetRepoSecretSendsDataField(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("PUT /api/v1/repos/acme/repo/actions/secrets/MERGE_TOKEN", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusCreated)
	})
	admin, _ := newStubAdmin(t, stub)

	if err := admin.SetRepoSecret(t.Context(), "acme/repo", "MERGE_TOKEN", "merge-token-value"); err != nil {
		t.Fatalf("SetRepoSecret() error = %v", err)
	}
	request := stub.find(t, http.MethodPut, "/api/v1/repos/acme/repo/actions/secrets/MERGE_TOKEN")
	if request.Auth != "token admin-token" {
		t.Errorf("Authorization = %q, want token admin-token", request.Auth)
	}
	var body struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal([]byte(request.Body), &body); err != nil {
		t.Fatalf("请求体不是 JSON: %v (%s)", err, request.Body)
	}
	if body.Data != "merge-token-value" {
		t.Errorf("data = %q, want merge-token-value", body.Data)
	}
}

func TestSetRepoSecretErrors(t *testing.T) {
	tests := map[string]struct {
		fullName string
		status   int
		hint     string
	}{
		"仓库名非法":  {fullName: "nope", hint: "owner/name"},
		"无权限写密钥": {fullName: "acme/repo", status: http.StatusForbidden, hint: "写 acme/repo 的 Actions secret MERGE_TOKEN"},
		"仓库不存在":  {fullName: "acme/repo", status: http.StatusNotFound, hint: "写 acme/repo 的 Actions secret MERGE_TOKEN"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stub := newGiteaStub(t)
			if test.status != 0 {
				code := test.status
				stub.handle("PUT /api/v1/repos/acme/repo/actions/secrets/MERGE_TOKEN",
					func(writer http.ResponseWriter, _ *http.Request) {
						http.Error(writer, `{"message":"secret failed"}`, code)
					})
			}
			admin, _ := newStubAdmin(t, stub)

			err := admin.SetRepoSecret(t.Context(), test.fullName, "MERGE_TOKEN", "v")
			if err == nil || !strings.Contains(err.Error(), test.hint) {
				t.Fatalf("SetRepoSecret() error = %v, want 含 %q", err, test.hint)
			}
		})
	}
}

// 网络不通时错误必须带方法与路径。
func TestSetRepoSecretPropagatesNetworkFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	admin := &giteaAdmin{host: server.URL, http: server.Client(), token: "t", log: func(string, ...any) {}}

	err := admin.SetRepoSecret(t.Context(), "acme/repo", "MERGE_TOKEN", "v")
	if err == nil || !strings.Contains(err.Error(), "PUT /api/v1/repos/acme/repo/actions/secrets/MERGE_TOKEN") {
		t.Fatalf("SetRepoSecret() error = %v, want 带方法与路径的网络错误", err)
	}
}

// ---------- ReconcileLabels ----------

// setup 的标签收敛走 status 包同一口径：先读现有标签、创建缺失标签、
// 把已存在但非互斥的 scoped 标签设为互斥、删除不在规范体系内的标签。
// 收敛请求由 status 客户端发出（独立 http client），因此按它实际请求的
// 路径注册处理函数。
func TestReconcileLabelsCreatesAndCuratesLabels(t *testing.T) {
	definitions := status.LabelDefinitions()
	var existing status.LabelDefinition
	for _, definition := range definitions {
		if definition.Exclusive {
			existing = definition
			break
		}
	}
	if existing.Name == "" {
		t.Fatal("规范标签体系里没有可用的互斥标签，测试前提失效")
	}
	stub := newGiteaStub(t)
	stub.handle("GET /api/v1/repos/acme/repo/labels", func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode([]map[string]any{
			// 已存在但未标互斥的 scoped 标签：必须被设为互斥。
			{"id": 1, "name": existing.Name, "color": "#0E8A16", "exclusive": false},
			// 非规范标签：必须被删除。
			{"id": 2, "name": "manual-label", "color": "#FFFFFF", "exclusive": false},
		})
	})
	var createdNames []string
	var createdExclusive = map[string]bool{}
	stub.handle("POST /api/v1/repos/acme/repo/labels", func(writer http.ResponseWriter, request *http.Request) {
		// 请求体已被 stub 统一录制，这里回看最后一条记录。
		requests := stub.snapshot()
		body := requests[len(requests)-1].Body
		var payload struct {
			Name      string `json:"name"`
			Color     string `json:"color"`
			Exclusive bool   `json:"exclusive"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Errorf("创建标签请求体不是 JSON: %v (%s)", err, body)
		}
		createdNames = append(createdNames, payload.Name)
		createdExclusive[payload.Name] = payload.Exclusive
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"id": 100 + len(createdNames), "name": payload.Name, "color": payload.Color,
			"exclusive": payload.Exclusive,
		})
	})
	var exclusiveIDs []int64
	stub.handle("PATCH /api/v1/repos/acme/repo/labels/1", func(writer http.ResponseWriter, request *http.Request) {
		requests := stub.snapshot()
		body := requests[len(requests)-1].Body
		var payload struct {
			Exclusive *bool `json:"exclusive"`
		}
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Errorf("设为互斥的请求体不是 JSON: %v (%s)", err, body)
		}
		if payload.Exclusive == nil || !*payload.Exclusive {
			t.Errorf("设为互斥的请求体 = %+v, want exclusive=true", payload)
		}
		exclusiveIDs = append(exclusiveIDs, 1)
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": 1, "name": existing.Name, "exclusive": true})
	})
	var deletedIDs []int64
	stub.handle("DELETE /api/v1/repos/acme/repo/labels/2", func(writer http.ResponseWriter, _ *http.Request) {
		deletedIDs = append(deletedIDs, 2)
		writer.WriteHeader(http.StatusNoContent)
	})
	admin, server := newStubAdmin(t, stub)
	admin.host = server.URL

	if err := admin.ReconcileLabels(t.Context(), "acme/repo", "reviewer-token"); err != nil {
		t.Fatalf("ReconcileLabels() error = %v", err)
	}
	wantCreated := len(definitions) - 1 // 除已存在的那一个之外全部补齐
	if len(createdNames) != wantCreated {
		t.Errorf("创建的标签数 = %d, want %d", len(createdNames), wantCreated)
	}
	if slices.Contains(createdNames, existing.Name) {
		t.Errorf("已存在的标签被重复创建：%v", createdNames)
	}
	if len(exclusiveIDs) != 1 {
		t.Errorf("设为互斥的标签 = %v, want [1]", exclusiveIDs)
	}
	if len(deletedIDs) != 1 || deletedIDs[0] != 2 {
		t.Errorf("删除的标签 = %v, want [2]（manual-label）", deletedIDs)
	}
	// 收敛用的是 reviewer 令牌，不是管理员令牌。
	for _, request := range stub.snapshot() {
		if request.Auth == "token reviewer-token" {
			return
		}
	}
	t.Errorf("收敛请求未使用 reviewer 令牌：%+v", stub.snapshot())
}

// ReconcileLabels：仓库名非法时应直接失败（不发任何请求）。
func TestReconcileLabelsRejectsBadRepoName(t *testing.T) {
	stub := newGiteaStub(t)
	admin, _ := newStubAdmin(t, stub)

	if err := admin.ReconcileLabels(t.Context(), "nope", "t"); err == nil ||
		!strings.Contains(err.Error(), "owner/name") {
		t.Fatalf("ReconcileLabels() error = %v, want 仓库名格式错误", err)
	}
	if len(stub.snapshot()) != 0 {
		t.Errorf("非法仓库名不应发起请求：%+v", stub.snapshot())
	}
}

// ReconcileLabels：令牌非法（host 无法构造客户端）时失败。
func TestReconcileLabelsRejectsBadToken(t *testing.T) {
	admin := &giteaAdmin{host: "://bad", http: http.DefaultClient, log: func(string, ...any) {}}
	if err := admin.ReconcileLabels(t.Context(), "acme/repo", "t"); err == nil {
		t.Fatal("ReconcileLabels() error = nil, want 客户端构造错误")
	}
}

// ReconcileLabels：列标签被拒（403）时错误冒泡。
func TestReconcileLabelsSurfacesPermissionDenied(t *testing.T) {
	stub := newGiteaStub(t)
	stub.handle("GET /api/v1/repos/acme/repo/labels", func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, `{"message":"permission denied"}`, http.StatusForbidden)
	})
	admin, server := newStubAdmin(t, stub)
	admin.host = server.URL

	err := admin.ReconcileLabels(t.Context(), "acme/repo", "reviewer-token")
	if err == nil {
		t.Fatal("ReconcileLabels() error = nil, want 权限错误")
	}
	if !strings.Contains(err.Error(), "list repository labels") {
		t.Errorf("error = %v, want 带操作语境", err)
	}
}

// ---------- Run / Options 编排可达路径 ----------

// 空用户名与空 merger 名会走默认值：but 账号名非法（空白）必须被拒。
func TestRunRejectsBlankAccountNames(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	tests := map[string]func(*Options){
		"reviewer 全空白": func(o *Options) { o.ReviewerName = "  " },
		"merger 全空白":   func(o *Options) { o.MergerName = "  " },
		"reviewer 带斜杠": func(o *Options) { o.ReviewerName = "a/b" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			options := testOptions()
			mutate(&options)
			if _, err := Run(t.Context(), options, admin); err == nil {
				t.Error("Run() error = nil, want 账号名校验错误")
			}
		})
	}
}

// 空仓库清单 + dry-run：不产生任何服务端写操作，也不发明凭据。
func TestRunDryRunWithoutRepos(t *testing.T) {
	admin := newFakeAdmin()
	options := testOptions()
	options.Repos = nil
	options.DryRun = true
	result, err := Run(t.Context(), options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(result.Credentials) != 0 {
		t.Errorf("dry-run 不应产生凭据：%+v", result.Credentials)
	}
	if len(admin.createdRepos) != 0 || len(admin.secrets) != 0 {
		t.Errorf("dry-run 不应写服务端：%v %v", admin.createdRepos, admin.secrets)
	}
}

// 幂等重跑：第二次执行不重复建号，复用已收敛的令牌。
func TestRunIsIdempotentOnRerun(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	first, err := Run(t.Context(), testOptions(), admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	options := testOptions()
	options.ExistingCredentials = first.Credentials
	second, err := Run(t.Context(), options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(admin.createdRepos) != 0 {
		t.Errorf("重跑不应重复建仓库：%v", admin.createdRepos)
	}
	for _, user := range []string{instances.DefaultReviewerName, instances.DefaultMergerName} {
		firstCredential, _ := credentialFor(first, user, credentialPurposeFor(user))
		secondCredential, ok := credentialFor(second, user, credentialPurposeFor(user))
		if !ok {
			t.Fatalf("重跑缺少 %s 的凭据", user)
		}
		if firstCredential.Token != secondCredential.Token {
			t.Errorf("%s 令牌被重建：%q -> %q", user, firstCredential.Token, secondCredential.Token)
		}
	}
}

func credentialPurposeFor(user string) string {
	if user == instances.DefaultReviewerName {
		return credentials.PurposeReview
	}
	return credentials.PurposeMerge
}

// 空仓库（empty=true）或默认分支为空时跳过分支保护与标签收敛。
func TestRunSkipsProtectionForEmptyRepo(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	admin.repos["acme/repo"] = RepoInfo{Empty: true}
	result, err := Run(t.Context(), testOptions(), admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if _, ok := admin.protections["acme/repo"]; ok {
		t.Errorf("空仓库不应配置分支保护：%+v", admin.protections)
	}
	if admin.labels["acme/repo"] {
		t.Error("空仓库不应收敛标签")
	}
	if len(result.Instance.Repos) != 1 {
		t.Errorf("Repos = %+v", result.Instance.Repos)
	}
	// 协作者仍然要加（与分支保护无关）。
	if admin.collaborators["acme/repo/"+instances.DefaultMergerName] != "admin" {
		t.Errorf("协作者未添加：%+v", admin.collaborators)
	}
}

// 创建仓库后仍读不到仓库：必须报错（服务端行为异常，不能假装成功）。
func TestRunFailsWhenCreatedRepoIsInvisible(t *testing.T) {
	admin := &invisibleRepoAdmin{fakeAdmin: newFakeAdmin()}
	options := testOptions()
	options.Repos = []string{"acme/missing"}
	options.CreateRepos = true
	if _, err := Run(t.Context(), options, admin); err == nil ||
		!strings.Contains(err.Error(), "创建后仍读不到仓库") {
		t.Fatalf("Run() error = %v, want 创建后仍读不到仓库", err)
	}
}

// invisibleRepoAdmin 的 CreateRepo 声明成功，但 GetRepo 始终读不到。
type invisibleRepoAdmin struct{ *fakeAdmin }

func (i *invisibleRepoAdmin) CreateRepo(context.Context, string) error { return nil }

// ---------- 令牌端点（Basic Auth）与令牌收敛 ----------

// 建令牌端点只接受账号自己的 Basic Auth：协议契约是「Basic 认证 + 一次一密、
// 不落盘 + 请求体带令牌名与全部机器人 scope」。
func TestCreateTokenBasicSendsBasicAuthAndScopes(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("POST /api/v1/users/reviewer/tokens", map[string]string{"sha1": "fresh-token-value"})
	admin, _ := newStubAdmin(t, stub)

	token, err := admin.createTokenBasic(t.Context(), "reviewer", "pw-secret", ReviewerTokenName)
	if err != nil {
		t.Fatalf("createTokenBasic() error = %v", err)
	}
	if token != "fresh-token-value" {
		t.Errorf("token = %q, want fresh-token-value", token)
	}
	request := stub.find(t, http.MethodPost, "/api/v1/users/reviewer/tokens")
	// 只断 Basic 认证（账号口令），不带管理员令牌——建令牌端点必须走这条路径。
	if request.Basic != "reviewer:pw-secret" {
		t.Errorf("Basic = %q, want reviewer:pw-secret", request.Basic)
	}
	if request.Auth != "Basic "+base64.StdEncoding.EncodeToString([]byte(request.Basic)) {
		t.Errorf("Authorization = %q, want Basic %s 的 base64", request.Auth, request.Basic)
	}
	if !strings.Contains(request.Body, ReviewerTokenName) {
		t.Errorf("请求体缺令牌名：%s", request.Body)
	}
	for _, scope := range credentials.BotScopes() {
		if !strings.Contains(request.Body, scope) {
			t.Errorf("请求体缺 scope %q：%s", scope, request.Body)
		}
	}
}

func TestCreateTokenBasicErrors(t *testing.T) {
	tests := map[string]struct {
		routeStatus int
		payload     any
		wantErr     string
	}{
		"服务端拒绝": {http.StatusForbidden, map[string]string{"message": "denied"}, "为 reviewer 创建令牌"},
		"未返回令牌": {http.StatusCreated, map[string]string{}, "Gitea 未返回令牌"},
		"响应体非法": {http.StatusCreated, "not-json", "为 reviewer 创建令牌"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stub := newGiteaStub(t)
			stub.jsonStatusRoute("POST /api/v1/users/reviewer/tokens", test.routeStatus, test.payload)
			admin, _ := newStubAdmin(t, stub)
			if _, err := admin.createTokenBasic(t.Context(), "reviewer", "pw", ReviewerTokenName); err == nil ||
				!strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("createTokenBasic() error = %v, want 含 %q", err, test.wantErr)
			}
		})
	}
}

func TestCreateTokenBasicPropagatesNetworkFailure(t *testing.T) {
	stub := newGiteaStub(t)
	admin, server := newStubAdmin(t, stub)
	server.Close()
	if _, err := admin.createTokenBasic(t.Context(), "reviewer", "pw", ReviewerTokenName); err == nil {
		t.Fatal("服务端关闭后应报错")
	}
}

// 列表端点分页：满页继续翻页，不满页即终止——两个分支都要走到。
func TestListTokensBasicPaginates(t *testing.T) {
	tests := map[string]struct {
		firstPage int
		wantPages int
	}{
		"满页继续翻页": {50, 2},
		"不满页终止":  {1, 1},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stub := newGiteaStub(t)
			page := 1
			stub.handle("GET /api/v1/users/reviewer/tokens", func(writer http.ResponseWriter, request *http.Request) {
				// 第一页按用例返回；第二页返回不满页，避免无限翻页。
				count := test.firstPage
				if page > 1 {
					count = 1
				}
				page++
				batch := make([]map[string]any, 0, count)
				for index := range count {
					batch = append(batch, map[string]any{"id": index + 1, "name": "other", "token_last_eight": "12345678"})
				}
				_ = json.NewEncoder(writer).Encode(batch)
			})
			admin, _ := newStubAdmin(t, stub)

			admin.logtokens(t, stub, test.wantPages)
		})
	}
}

func TestListTokensBasicSurfacesServerError(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonStatusRoute("GET /api/v1/users/reviewer/tokens", http.StatusUnauthorized, map[string]string{"message": "bad"})
	admin, _ := newStubAdmin(t, stub)
	if _, err := admin.listTokensBasic(t.Context(), "reviewer", "pw"); err == nil {
		t.Fatal("401 应报错")
	}
}

// deleteTokenBasic 把 404 视作「已删除」（幂等清理），其他错误必须上抛。
func TestDeleteTokenBasicTreatsNotFoundAsDeleted(t *testing.T) {
	tests := map[string]struct {
		status  int
		wantErr bool
	}{
		"404 视为已删除": {http.StatusNotFound, false},
		"403 上抛":    {http.StatusForbidden, true},
		"204 正常删除":  {http.StatusNoContent, false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stub := newGiteaStub(t)
			stub.jsonStatusRoute("DELETE /api/v1/users/reviewer/tokens/7", test.status, map[string]string{})
			admin, _ := newStubAdmin(t, stub)
			err := admin.deleteTokenBasic(t.Context(), "reviewer", "pw", 7)
			if test.wantErr && err == nil {
				t.Error("deleteTokenBasic() error = nil, want 错误")
			}
			if !test.wantErr && err != nil {
				t.Errorf("deleteTokenBasic() error = %v, want nil", err)
			}
			request := stub.find(t, http.MethodDelete, "/api/v1/users/reviewer/tokens/7")
			if request.Basic != "reviewer:pw" {
				t.Errorf("Basic = %q, want reviewer:pw", request.Basic)
			}
		})
	}
}

func TestEnsurePasswordSurfacesUnresettableName(t *testing.T) {
	stub := newGiteaStub(t)
	admin, _ := newStubAdmin(t, stub)
	// 路径里的 %2F 由服务端解码后成为 /admin/users/a/b，stub 必然 404。
	if _, err := admin.EnsurePassword(t.Context(), "a/b"); err == nil {
		t.Fatal("不可重置的账号名应报错")
	}
}

func TestConvergeTokenReusesMatchingCredential(t *testing.T) {
	admin := newFakeAdmin()
	admin.users[instances.DefaultReviewerName] = true
	admin.tokens[tokenKey(instances.DefaultReviewerName, ReviewerTokenName)] = "keep-token-1234"

	token, created, err := admin.ConvergeToken(
		t.Context(), instances.DefaultReviewerName, "pw", ReviewerTokenName, "keep-token-1234")
	if err != nil {
		t.Fatalf("ConvergeToken() error = %v", err)
	}
	if created || token != "keep-token-1234" {
		t.Errorf("token = %q created = %v, want 复用现有令牌", token, created)
	}
}

// 失效令牌（末 8 位对不上）必须换新，且只替换本工具名的条目。
func TestConvergeTokenReplacesStaleCredential(t *testing.T) {
	admin := newFakeAdmin()
	admin.users[instances.DefaultReviewerName] = true
	admin.tokens[tokenKey(instances.DefaultReviewerName, ReviewerTokenName)] = "stale-token-9999"

	token, created, err := admin.ConvergeToken(
		t.Context(), instances.DefaultReviewerName, "pw", ReviewerTokenName, "old-token-0000")
	if err != nil {
		t.Fatalf("ConvergeToken() error = %v", err)
	}
	if !created || token == "stale-token-9999" {
		t.Errorf("token = %q created = %v, want 重建", token, created)
	}
	if admin.tokens[tokenKey(instances.DefaultReviewerName, ReviewerTokenName)] != token {
		t.Errorf("新令牌未落库：%+v", admin.tokens)
	}
}

// ---------- 协作者权限归一化 ----------

func TestCollaboratorPermissionNormalizesAccessMode(t *testing.T) {
	tests := map[string]struct {
		permission string
		want       string
	}{
		"owner 归一化为 admin": {string(gitea.AccessModeOwner), "admin"},
		"admin 归一化为 admin": {string(gitea.AccessModeAdmin), "admin"},
		"write 保留":         {string(gitea.AccessModeWrite), "write"},
		"read 保留":          {string(gitea.AccessModeRead), "read"},
		"未知模式原样返回":         {"none", "none"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stub := newGiteaStub(t)
			stub.jsonRoute("GET /api/v1/repos/acme/repo/collaborators/bot/permission",
				map[string]string{"permission": test.permission})
			admin, _ := newStubAdmin(t, stub)

			got, err := admin.collaboratorPermission(t.Context(), "acme", "repo", "bot")
			if err != nil {
				t.Fatalf("collaboratorPermission() error = %v", err)
			}
			if got != test.want {
				t.Errorf("permission(%q) = %q, want %q", test.permission, got, test.want)
			}
		})
	}
}

func TestCollaboratorPermissionSurfacesServerError(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonStatusRoute("GET /api/v1/repos/acme/repo/collaborators/bot/permission",
		http.StatusForbidden, map[string]string{"message": "denied"})
	admin, _ := newStubAdmin(t, stub)
	if _, err := admin.collaboratorPermission(t.Context(), "acme", "repo", "bot"); err == nil ||
		!strings.Contains(err.Error(), "读取 acme/repo 协作者 bot 的权限") {
		t.Errorf("error = %v, want 包装后的权限读取错误", err)
	}
}

// ListCollaborators 走 SDK：列表 + 逐个权限查询（owner 归一化）。
func TestListCollaboratorsReadsEachPermission(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/repos/acme/repo/collaborators", []map[string]any{
		{"login": "ai"}, {"login": "merge"},
	})
	stub.jsonRoute("GET /api/v1/repos/acme/repo/collaborators/ai/permission",
		map[string]string{"permission": string(gitea.AccessModeOwner)})
	stub.jsonRoute("GET /api/v1/repos/acme/repo/collaborators/merge/permission",
		map[string]string{"permission": string(gitea.AccessModeWrite)})
	admin, _ := newStubAdmin(t, stub)

	result, err := admin.ListCollaborators(t.Context(), "acme/repo")
	if err != nil {
		t.Fatalf("ListCollaborators() error = %v", err)
	}
	want := map[string]string{"ai": "admin", "merge": "write"}
	if len(result) != len(want) {
		t.Fatalf("ListCollaborators() = %+v, want 2 条", result)
	}
	for _, collaborator := range result {
		if want[collaborator.Name] != collaborator.Permission {
			t.Errorf("协作者 %+v, want %q", collaborator, want[collaborator.Name])
		}
	}
}

func TestListCollaboratorsErrors(t *testing.T) {
	tests := map[string]struct {
		fullName string
		setup    func(*giteaStub)
	}{
		"仓库名非法": {"acme", func(*giteaStub) {}},
		"列表端点拒绝": {"acme/repo", func(stub *giteaStub) {
			stub.jsonStatusRoute("GET /api/v1/repos/acme/repo/collaborators", http.StatusForbidden,
				map[string]string{"message": "denied"})
		}},
		"权限查询失败": {"acme/repo", func(stub *giteaStub) {
			stub.jsonRoute("GET /api/v1/repos/acme/repo/collaborators", []map[string]any{{"login": "ai"}})
			stub.jsonStatusRoute("GET /api/v1/repos/acme/repo/collaborators/ai/permission",
				http.StatusForbidden, map[string]string{"message": "denied"})
		}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stub := newGiteaStub(t)
			test.setup(stub)
			admin, _ := newStubAdmin(t, stub)
			if _, err := admin.ListCollaborators(t.Context(), test.fullName); err == nil {
				t.Error("ListCollaborators() error = nil, want 错误")
			}
		})
	}
}

func TestListCollaboratorsWithoutAdminClient(t *testing.T) {
	admin := &giteaAdmin{host: "https://gitea.example.com", log: func(string, ...any) {}}
	if _, err := admin.ListCollaborators(t.Context(), "acme/repo"); err == nil ||
		!strings.Contains(err.Error(), "缺少 Gitea 客户端") {
		t.Errorf("error = %v, want 缺少 Gitea 客户端", err)
	}
}

// ListAllRepos 走仓库搜索分页，满页继续翻页。
func TestListAllReposPaginates(t *testing.T) {
	stub := newGiteaStub(t)
	page := 1
	stub.handle("GET /api/v1/repos/search", func(writer http.ResponseWriter, _ *http.Request) {
		count := 50
		if page > 1 {
			count = 1
		}
		page++
		data := make([]map[string]any, 0, count)
		for index := range count {
			data = append(data, map[string]any{"full_name": fmt.Sprintf("acme/repo-%d", index)})
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true, "data": data})
	})
	admin, _ := newStubAdmin(t, stub)

	result, err := admin.ListAllRepos(t.Context())
	if err != nil {
		t.Fatalf("ListAllRepos() error = %v", err)
	}
	if len(result) != 51 {
		t.Errorf("ListAllRepos() 条数 = %d, want 51", len(result))
	}
	searchRequests := 0
	for _, request := range stub.snapshot() {
		if strings.HasPrefix(request.Path, "/api/v1/repos/search") {
			searchRequests++
			if !strings.Contains(request.Query, "private=true") {
				t.Errorf("查询串缺 private=true：%q", request.Query)
			}
		}
	}
	if searchRequests != 2 {
		t.Errorf("搜索请求数 = %d, want 2", searchRequests)
	}
}

func TestListAllReposSurfacesServerError(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonStatusRoute("GET /api/v1/repos/search", http.StatusInternalServerError, map[string]string{"message": "boom"})
	admin, _ := newStubAdmin(t, stub)
	if _, err := admin.ListAllRepos(t.Context()); err == nil ||
		!strings.Contains(err.Error(), "搜索实例仓库") {
		t.Errorf("error = %v, want 搜索实例仓库", err)
	}
}

// ---------- 错误链与边界 ----------

// tokenLastEight：短于等于 8 位原样返回，长令牌取末 8 位（与 Gitea 的
// token_last_eight 对齐的边界条件）。
func TestTokenLastEightBoundaries(t *testing.T) {
	tests := map[string]string{
		"":               "",
		"abc":            "abc",
		"12345678":       "12345678",
		"123456789":      "23456789",
		"0123456789abcd": "6789abcd",
	}
	for token, want := range tests {
		if got := tokenLastEight(token); got != want {
			t.Errorf("tokenLastEight(%q) = %q, want %q", token, got, want)
		}
	}
}

// matchToken：空 keepToken 不匹配任何条目；命中返回条目 ID。
func TestMatchTokenBoundaries(t *testing.T) {
	tokens := []tokenInfo{{ID: 1, TokenLastEight: "12345678"}, {ID: 2, TokenLastEight: "abcdefgh"}}
	if got := matchToken(tokens, ""); got != 0 {
		t.Errorf("matchToken(空令牌) = %d, want 0", got)
	}
	if got := matchToken(tokens, "prefix12345678"); got != 1 {
		t.Errorf("matchToken(命中) = %d, want 1", got)
	}
	if got := matchToken(tokens, "nothinghere"); got != 0 {
		t.Errorf("matchToken(未命中) = %d, want 0", got)
	}
}

// do() 对非 2xx 返回可 errors.As 取出的 *httpError，并带上 Gitea 的 message。
func TestDoClassifiesHTTPErrorAndKeepsMessage(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonStatusRoute("GET /api/v1/user", http.StatusUnprocessableEntity,
		map[string]string{"message": "用户名已存在"})
	admin, _ := newStubAdmin(t, stub)

	_, _, err := admin.AuthenticatedUser(t.Context())
	if err == nil {
		t.Fatal("422 应报错")
	}
	var httpErr *httpError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v, want *httpError", err)
	}
	if httpErr.Status != http.StatusUnprocessableEntity || httpErr.Message != "用户名已存在" {
		t.Errorf("httpError = %+v", httpErr)
	}
	if !isHTTPStatus(err, http.StatusUnprocessableEntity) {
		t.Error("isHTTPStatus 未识别 422")
	}
}

// CustomBasicAuth（令牌认证头）与「Accept 头」是每次调用的固定协议。
func TestDoSetsDefaultHeaders(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/user", map[string]any{"login": "admin", "is_admin": true})
	admin, _ := newStubAdmin(t, stub)

	if _, _, err := admin.AuthenticatedUser(t.Context()); err != nil {
		t.Fatalf("AuthenticatedUser() error = %v", err)
	}
	request := stub.find(t, http.MethodGet, "/api/v1/user")
	if request.Auth != "token admin-token" {
		t.Errorf("Authorization = %q, want token admin-token", request.Auth)
	}
}

// ---------- Run 编排：账号/令牌/密钥错误的传播 ----------

func TestRunCreatesMissingRepo(t *testing.T) {
	admin := newFakeAdmin()
	options := testOptions()
	options.Repos = []string{"acme/fresh"}
	options.CreateRepos = true
	result, err := Run(t.Context(), options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(admin.createdRepos) != 1 || admin.createdRepos[0] != "acme/fresh" {
		t.Errorf("createdRepos = %v", admin.createdRepos)
	}
	if !admin.labels["acme/fresh"] {
		t.Error("新建仓库应收敛标签")
	}
	if len(result.Instance.Repos) != 1 || result.Instance.Repos[0].Name != "acme/fresh" {
		t.Errorf("Repos = %+v", result.Instance.Repos)
	}
}

func TestRunSelectsExistingReposByFullName(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	options := testOptions()
	options.Existing = &instances.Instance{
		Host: "https://gitea.example.com",
		Repos: []instances.Repo{
			{Name: "acme/other", Dir: "/srv/other"},
			{Name: "acme/repo", Dir: "/srv/repo"},
		},
	}
	result, err := Run(t.Context(), options, admin)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(result.Instance.Repos) != 1 || result.Instance.Repos[0].Dir != "/srv/repo" {
		t.Errorf("Repos = %+v, want 复用 acme/repo 的目录", result.Instance.Repos)
	}
}

func TestRunWrapsRepositorySetupFailure(t *testing.T) {
	admin := &failingSetupAdmin{fakeAdmin: newFakeAdmin("acme/repo"), failOn: "添加协作者 acme/repo"}
	options := testOptions()
	options.Repos = []string{"acme/repo"}
	if _, err := Run(t.Context(), options, admin); err == nil ||
		!strings.Contains(err.Error(), "acme/repo: 添加协作者失败") {
		t.Fatalf("Run() error = %v, want 带仓库名的包装错误", err)
	}
}

func TestRunPropagatesAccountCreationFailure(t *testing.T) {
	tests := map[string]struct {
		admin *failingAccountAdmin
	}{
		"建号失败": {admin: &failingAccountAdmin{createUserErr: errors.New("服务端拒绝建号")}},
		"令牌失效未取到新令牌": {admin: &failingAccountAdmin{
			convergeTokenErr: errors.New("令牌端点不可用")}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			test.admin.fakeAdmin = newFakeAdmin("acme/repo")
			options := testOptions()
			if _, err := Run(t.Context(), options, test.admin); err == nil {
				t.Error("Run() error = nil, want 账号准备错误")
			}
		})
	}
}

func TestRunPropagatesMergeSecretSyncFailure(t *testing.T) {
	admin := &failingSecretAdmin{
		fakeAdmin: newFakeAdmin("acme/repo"), secretErr: errors.New("secret 端点拒绝")}
	admin.repos["acme/repo"] = RepoInfo{}
	if _, err := Run(t.Context(), testOptions(), admin); err == nil ||
		!strings.Contains(err.Error(), "同步 Actions 密钥") {
		t.Fatalf("Run() error = %v, want 同步 Actions 密钥", err)
	}
}

func TestRunAuditsExistingCredentials(t *testing.T) {
	tests := map[string]struct {
		admin    func(*fakeAdmin) Admin
		existing string
		wantLog  string
	}{
		"现有令牌校验失败": {
			admin: func(admin *fakeAdmin) Admin {
				return &failingSecretAdmin{fakeAdmin: admin, validateErr: errors.New("服务端不可达")}
			},
			existing: "reviewer-token",
			wantLog:  "现有令牌校验失败",
		},
		"现有令牌已失效": {
			admin:    func(admin *fakeAdmin) Admin { return admin },
			existing: "reviewer-token",
			wantLog:  "现有令牌已失效",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			admin := test.admin(newFakeAdmin("acme/repo"))
			options := testOptions()
			options.ExistingCredentials = []credentials.Credential{{
				Host: "https://gitea.example.com", User: instances.DefaultReviewerName,
				Purpose: credentials.PurposeReview, Token: test.existing,
			}}
			logs := make([]string, 0)
			options.Log = func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
			}
			if _, err := Run(t.Context(), options, admin); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if !slices.ContainsFunc(logs, func(line string) bool { return strings.Contains(line, test.wantLog) }) {
				t.Errorf("日志缺少 %q：%v", test.wantLog, logs)
			}
		})
	}
}

func TestSyncMergeSecretsErrorPaths(t *testing.T) {
	tests := map[string]struct {
		admin   Admin
		dryRun  bool
		wantErr bool
	}{
		"列出实例仓库失败": {
			admin:   &failingListAllAdmin{Admin: newFakeAdmin(), listAllErr: errors.New("搜索端点拒绝")},
			wantErr: true,
		},
		"读取协作者失败则跳过该仓库": {
			admin: &failingCollaboratorAdmin{Admin: newFakeAdmin(), collaboratorErr: errors.New("无权读取")},
		},
		"写 secret 失败": {
			admin:   &failingSecretAdmin{fakeAdmin: newFakeAdmin("acme/repo"), secretErr: errors.New("secret 端点拒绝")},
			wantErr: true,
		},
		"dry-run 只输出计划": {
			admin: &failingSecretAdmin{fakeAdmin: newFakeAdmin("acme/repo")},
			// 先让 merge 成为管理员协作者，dry-run 才走到「跳过写入」分支。
			dryRun: true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if base, ok := test.admin.(*failingSecretAdmin); ok {
				base.repos["acme/repo"] = RepoInfo{}
				if err := base.AddCollaborator(t.Context(), "acme/repo",
					instances.DefaultMergerName, "admin"); err != nil {
					t.Fatalf("AddCollaborator() error = %v", err)
				}
			}
			err := SyncMergeSecrets(t.Context(), test.admin, instances.DefaultMergerName,
				"merge-token", test.dryRun, nil)
			if test.wantErr && err == nil {
				t.Error("SyncMergeSecrets() error = nil, want 错误")
			}
			if !test.wantErr && err != nil {
				t.Errorf("SyncMergeSecrets() error = %v, want nil", err)
			}
			if test.dryRun {
				if secrets := test.admin.(*failingSecretAdmin).secrets; len(secrets) != 0 {
					t.Errorf("dry-run 不应写 secret：%+v", secrets)
				}
			}
		})
	}
}

// logtokens 断言 listTokensBasic 的翻页次数。
func (a *giteaAdmin) logtokens(t *testing.T, stub *giteaStub, wantPages int) {
	t.Helper()
	if _, err := a.listTokensBasic(t.Context(), "reviewer", "pw"); err != nil {
		t.Fatalf("listTokensBasic() error = %v", err)
	}
	pages := 0
	for _, request := range stub.snapshot() {
		if request.Method == http.MethodGet && request.Path == "/api/v1/users/reviewer/tokens" {
			pages++
		}
	}
	if pages != wantPages {
		t.Errorf("翻页次数 = %d, want %d", pages, wantPages)
	}
}

// failingSetupAdmin 让「添加协作者」失败，用于覆盖 setupRepository 的错误传播。
type failingSetupAdmin struct {
	*fakeAdmin
	failOn string
}

func (f *failingSetupAdmin) AddCollaborator(ctx context.Context, fullName, user, permission string) error {
	return errors.New("添加协作者失败")
}

// failingAccountAdmin 注入账号准备阶段的失败，用于覆盖 Run 的错误传播。
type failingAccountAdmin struct {
	*fakeAdmin
	createUserErr    error
	convergeTokenErr error
}

func (f *failingAccountAdmin) CreateUser(ctx context.Context, name, email string) error {
	if f.createUserErr != nil {
		return f.createUserErr
	}
	return f.fakeAdmin.CreateUser(ctx, name, email)
}

func (f *failingAccountAdmin) ConvergeToken(
	ctx context.Context, name, password, tokenName, keepToken string,
) (string, bool, error) {
	if f.convergeTokenErr != nil {
		return "", false, f.convergeTokenErr
	}
	return f.fakeAdmin.ConvergeToken(ctx, name, password, tokenName, keepToken)
}

// failingListAllAdmin 让实例仓库枚举失败。
type failingListAllAdmin struct {
	Admin
	listAllErr error
}

func (f *failingListAllAdmin) ListAllRepos(context.Context) ([]string, error) {
	return nil, f.listAllErr
}

// failingCollaboratorAdmin 让协作者读取失败（镜像「无权读取的仓库」）。
type failingCollaboratorAdmin struct {
	Admin
	collaboratorErr error
}

func (f *failingCollaboratorAdmin) ListCollaborators(context.Context, string) ([]Collaborator, error) {
	return nil, f.collaboratorErr
}

// failingSecretAdmin 让 secret 写入失败，并可注入其他错误。
type failingSecretAdmin struct {
	*fakeAdmin
	secretErr        error
	validateErr      error
	convergeTokenErr error
	createUserErr    error
}

func (f *failingSecretAdmin) SetRepoSecret(ctx context.Context, fullName, name, value string) error {
	if f.secretErr != nil {
		return f.secretErr
	}
	return f.fakeAdmin.SetRepoSecret(ctx, fullName, name, value)
}

func (f *failingSecretAdmin) ValidateToken(ctx context.Context, name, token string) (bool, error) {
	if f.validateErr != nil {
		return false, f.validateErr
	}
	return f.fakeAdmin.ValidateToken(ctx, name, token)
}

func (f *failingSecretAdmin) ConvergeToken(
	ctx context.Context, name, password, tokenName, keepToken string,
) (string, bool, error) {
	if f.convergeTokenErr != nil {
		return "", false, f.convergeTokenErr
	}
	return f.fakeAdmin.ConvergeToken(ctx, name, password, tokenName, keepToken)
}

func (f *failingSecretAdmin) CreateUser(ctx context.Context, name, email string) error {
	if f.createUserErr != nil {
		return f.createUserErr
	}
	return f.fakeAdmin.CreateUser(ctx, name, email)
}

// ---------- 剩余错误路径：SDK 构造、密码重置、标签客户端、熵源边界 ----------

// 两个构造函数的 gitea.NewClient 只会收到 SetToken/SetHTTPClient/SetUserAgent，
// 这三个 option 在 SDK v1.2.0 里恒返回 nil，因此「创建 Gitea 客户端失败」这一分支
// 不可达（除非 SDK 改 option 的实现）。这里只断言不会误报。
func TestAdminConstructorsSucceedOnReachableOptions(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/user", map[string]any{"login": "admin", "is_admin": true})
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	options := testOptions()
	options.Host = server.URL
	options.AdminToken = "admin-token"
	if admin, err := NewAdmin(t.Context(), options); err != nil || admin.sdk == nil {
		t.Fatalf("NewAdmin() = %v, %v; want 构造成功且带 SDK", admin, err)
	}
	if client, err := NewRepoClient(t.Context(), server.URL, "dev-token", nil); err != nil || client.sdk == nil {
		t.Fatalf("NewRepoClient() = %v, %v; want 构造成功且带 SDK", client, err)
	}
}

// resetPassword 在缺少管理员 SDK 时必须明确报错，而不是静默返回空密码。
func TestResetPasswordSurfacesEditUserFailure(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonStatusRoute("PATCH /api/v1/admin/users/reviewer", http.StatusUnprocessableEntity,
		map[string]string{"message": "LoginName: Required"})
	admin, _ := newStubAdmin(t, stub)
	if _, err := admin.resetPassword(t.Context(), "reviewer"); err == nil {
		t.Fatal("resetPassword() error = nil, want 管理员接口错误")
	}
	if password := admin.passwords["reviewer"]; password != "" {
		t.Errorf("失败后不应记录密码：%q", password)
	}
}

// ReconcileLabels 用站点地址直接构造状态客户端：地址拼不出合法请求时，
// 状态机自身的错误原样上报（这里不存在「先校验地址」的防线）。
func TestReconcileLabelsSurfacesUnparsableHost(t *testing.T) {
	admin := &giteaAdmin{host: "://坏地址", log: func(string, ...any) {}}
	err := admin.ReconcileLabels(t.Context(), "acme/repo", "reviewer-token")
	if err == nil || !strings.Contains(err.Error(), "missing protocol scheme") {
		t.Fatalf("ReconcileLabels() error = %v, want 地址解析错误", err)
	}
}

// DeriveEmailDomain 对解析失败的地址回退到 assistant.local。
func TestDeriveEmailDomainFallsBackOnUnparsableHost(t *testing.T) {
	if got := DeriveEmailDomain("://坏地址"); got != "assistant.local" {
		t.Errorf("DeriveEmailDomain() = %q, want assistant.local", got)
	}
}

// RandomPassword 每次都要给出不同且非空的密码（24 字节 base64url）。
func TestRandomPasswordIsUniqueAndNonEmpty(t *testing.T) {
	first, err := RandomPassword()
	if err != nil {
		t.Fatalf("RandomPassword() error = %v", err)
	}
	second, err := RandomPassword()
	if err != nil {
		t.Fatalf("RandomPassword() error = %v", err)
	}
	if first == "" || len(first) != 32 {
		t.Errorf("RandomPassword() = %q, want 32 字符", first)
	}
	if first == second {
		t.Errorf("两次 RandomPassword() 相同：%q", first)
	}
}

// ---------- 个人令牌端点（personal_token.go）的错误与边界 ----------

// 令牌端点只带 Basic 认证与 TOTP 头，不回显服务端响应体。
func TestUserTokenRequestSendsBasicAuthAndTotp(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/users/dev/tokens", []map[string]any{})
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	if _, err := ListUserTokens(t.Context(), server.URL, "dev", "pw", "123456"); err != nil {
		t.Fatalf("ListUserTokens() error = %v", err)
	}
	request := stub.find(t, http.MethodGet, "/api/v1/users/dev/tokens")
	if request.Basic != "dev:pw" {
		t.Errorf("Basic = %q, want dev:pw", request.Basic)
	}
	if got := request.OTP; got != "123456" {
		t.Errorf("X-Gitea-OTP = %q, want 123456", got)
	}
}

// non-2xx 只报状态码，不把响应体里的提示语/凭据带进错误。
func TestUserTokenRequestHidesServerBody(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonStatusRoute("GET /api/v1/users/dev/tokens", http.StatusForbidden,
		map[string]string{"message": "token 内部提示"})
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	_, err := ListUserTokens(t.Context(), server.URL, "dev", "pw", "")
	if err == nil {
		t.Fatal("ListUserTokens() error = nil, want 403")
	}
	if strings.Contains(err.Error(), "token 内部提示") {
		t.Errorf("错误不应回显服务端响应体：%v", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误应带状态码：%v", err)
	}
}

// 响应体不是 JSON 时报解析失败。
func TestUserTokenRequestSurfacesMalformedJSON(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/users/dev/tokens", "not-json")
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	if _, err := ListUserTokens(t.Context(), server.URL, "dev", "pw", ""); err == nil ||
		!strings.Contains(err.Error(), "解析") {
		t.Fatalf("ListUserTokens() error = %v, want 解析失败", err)
	}
}

// 网络不可达时错误要带路径语境。
func TestUserTokenRequestSurfacesNetworkFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close() // 关掉后端口不可达

	_, err := ListUserTokens(t.Context(), server.URL, "dev", "pw", "")
	if err == nil || !strings.Contains(err.Error(), "失败") {
		t.Fatalf("ListUserTokens() error = %v, want 网络失败", err)
	}
}

// 跨页：page=50 满页才继续，短页停止。
func TestListUserTokensFollowsLongPages(t *testing.T) {
	stub := newGiteaStub(t)
	full := make([]map[string]any, 50)
	for index := range full {
		full[index] = map[string]any{"id": index + 1, "name": fmt.Sprintf("tok-%d", index)}
	}
	stub.handle("GET /api/v1/users/dev/tokens", func(writer http.ResponseWriter, request *http.Request) {
		payload := full
		if request.URL.Query().Get("page") != "1" {
			payload = []map[string]any{{"id": 51, "name": "tok-50"}}
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(payload)
	})
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	tokens, err := ListUserTokens(t.Context(), server.URL, "dev", "pw", "")
	if err != nil {
		t.Fatalf("ListUserTokens() error = %v", err)
	}
	if len(tokens) != 51 {
		t.Errorf("令牌数 = %d, want 51", len(tokens))
	}
}

// CreateUserToken 服务端未回 sha1 时报错。
func TestCreateUserTokenSurfacesMissingToken(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("POST /api/v1/users/dev/tokens", map[string]string{})
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	if _, err := CreateUserToken(t.Context(), server.URL, "dev", "pw", "", "assistant",
		[]string{"write:repository"}); err == nil ||
		!strings.Contains(err.Error(), "未返回新建令牌") {
		t.Fatalf("CreateUserToken() error = %v, want 未返回新建令牌", err)
	}
}

// EnsureUserToken 的参数校验与站点地址校验。
func TestEnsureUserTokenValidatesInput(t *testing.T) {
	tests := map[string]struct {
		host     string
		user     string
		password string
		name     string
		scopes   []string
		wantErr  string
	}{
		"站点地址非法": {host: "not-a-url", user: "dev", password: "pw", name: "assistant",
			scopes: []string{"write:repository"}, wantErr: "无效的 Gitea 站点地址"},
		"缺用户名": {host: "https://gitea.example.com", password: "pw", name: "assistant",
			scopes: []string{"write:repository"}, wantErr: "需要用户名、密码、令牌名与 scope"},
		"缺密码": {host: "https://gitea.example.com", user: "dev", name: "assistant",
			scopes: []string{"write:repository"}, wantErr: "需要用户名、密码、令牌名与 scope"},
		"缺令牌名": {host: "https://gitea.example.com", user: "dev", password: "pw",
			scopes: []string{"write:repository"}, wantErr: "需要用户名、密码、令牌名与 scope"},
		"缺 scope": {host: "https://gitea.example.com", user: "dev", password: "pw",
			name: "assistant", wantErr: "需要用户名、密码、令牌名与 scope"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := EnsureUserToken(t.Context(), test.host, test.user, test.password,
				"", test.name, test.scopes)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("EnsureUserToken() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

// EnsureUserToken 列出令牌失败与删除同名旧令牌失败都要带语境。
func TestEnsureUserTokenPropagatesListAndDeleteFailures(t *testing.T) {
	t.Run("列出失败", func(t *testing.T) {
		stub := newGiteaStub(t)
		stub.jsonStatusRoute("GET /api/v1/users/dev/tokens", http.StatusForbidden, nil)
		server := httptest.NewServer(stub)
		t.Cleanup(server.Close)

		if _, _, err := EnsureUserToken(t.Context(), server.URL, "dev", "pw", "", "assistant",
			[]string{"write:repository"}); err == nil || !strings.Contains(err.Error(), "列出 @dev 的令牌失败") {
			t.Fatalf("EnsureUserToken() error = %v, want 列出失败语境", err)
		}
	})
	t.Run("删除失败", func(t *testing.T) {
		stub := newGiteaStub(t)
		stub.jsonRoute("GET /api/v1/users/dev/tokens",
			[]map[string]any{{"id": 7, "name": "assistant"}})
		stub.jsonStatusRoute("DELETE /api/v1/users/dev/tokens/7", http.StatusInternalServerError, nil)
		server := httptest.NewServer(stub)
		t.Cleanup(server.Close)

		if _, _, err := EnsureUserToken(t.Context(), server.URL, "dev", "pw", "", "assistant",
			[]string{"write:repository"}); err == nil || !strings.Contains(err.Error(), "删除同名旧令牌") {
			t.Fatalf("EnsureUserToken() error = %v, want 删除失败语境", err)
		}
	})
}

// 同名旧令牌会被轮换掉，其他命名的令牌不动；replaced 计数只算同名条目。
func TestEnsureUserTokenReplacesOnlySameName(t *testing.T) {
	stub := newGiteaStub(t)
	stub.jsonRoute("GET /api/v1/users/dev/tokens", []map[string]any{
		{"id": 1, "name": "assistant"},
		{"id": 2, "name": "manual"},
	})
	stub.handle("DELETE /api/v1/users/dev/tokens/1", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	stub.jsonRoute("POST /api/v1/users/dev/tokens", map[string]string{"sha1": "brand-new"})
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)

	token, replaced, err := EnsureUserToken(t.Context(), server.URL, "dev", "pw", "", "assistant",
		[]string{"write:repository"})
	if err != nil {
		t.Fatalf("EnsureUserToken() error = %v", err)
	}
	if token != "brand-new" || replaced != 1 {
		t.Errorf("token = %q, replaced = %d, want brand-new/1", token, replaced)
	}
	if slices.ContainsFunc(stub.snapshot(), func(request recordedRequest) bool {
		return request.Method == http.MethodDelete && request.Path == "/api/v1/users/dev/tokens/2"
	}) {
		t.Error("不应删除其他命名的令牌")
	}
}

// ---------- Run 编排的未覆盖分支 ----------

// Run 要求调用者拿到的客户端自己是管理员：非管理员必须在建任何对象前失败。
func TestRunRejectsNonAdminActor(t *testing.T) {
	admin := &nonAdminActor{fakeAdmin: newFakeAdmin("acme/repo")}
	if _, err := Run(t.Context(), testOptions(), admin); err == nil ||
		!strings.Contains(err.Error(), "不是管理员") {
		t.Fatalf("Run() error = %v, want 不是管理员", err)
	}
}

// nonAdminActor 模拟「令牌有效但不是管理员」。
type nonAdminActor struct{ *fakeAdmin }

func (n *nonAdminActor) AuthenticatedUser(context.Context) (string, bool, error) {
	return "not-admin", false, nil
}

// setupRepository 读取仓库失败、创建仓库失败都要原样上报。
func TestRunPropagatesRepoLookupAndCreateFailures(t *testing.T) {
	tests := map[string]struct {
		admin   Admin
		options func(*Options)
		wantErr string
	}{
		"读取仓库失败": {
			admin:   &failingRepoAdmin{fakeAdmin: newFakeAdmin(), getRepoErr: errors.New("读取仓库失败")},
			options: func(*Options) {},
			wantErr: "acme/repo: 读取仓库失败",
		},
		"创建仓库失败": {
			admin:   &failingRepoAdmin{fakeAdmin: newFakeAdmin(), createRepoErr: errors.New("创建仓库失败")},
			options: func(options *Options) { options.CreateRepos = true },
			wantErr: "acme/repo: 创建仓库失败",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			options := testOptions()
			test.options(&options)
			if _, err := Run(t.Context(), options, test.admin); err == nil ||
				!strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Run() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

// failingRepoAdmin 注入仓库读取/创建失败。
type failingRepoAdmin struct {
	*fakeAdmin
	getRepoErr    error
	createRepoErr error
}

func (f *failingRepoAdmin) GetRepo(ctx context.Context, fullName string) (RepoInfo, bool, error) {
	if f.getRepoErr != nil {
		return RepoInfo{}, false, f.getRepoErr
	}
	return f.fakeAdmin.GetRepo(ctx, fullName)
}

func (f *failingRepoAdmin) CreateRepo(ctx context.Context, fullName string) error {
	if f.createRepoErr != nil {
		return f.createRepoErr
	}
	return f.fakeAdmin.CreateRepo(ctx, fullName)
}

// 分支保护与标签收敛失败都必须带仓库名包装上报。
func TestRunPropagatesProtectionAndLabelFailures(t *testing.T) {
	tests := map[string]struct {
		admin   Admin
		wantErr string
	}{
		"分支保护失败": {
			admin:   &failingProtectionAdmin{fakeAdmin: newFakeAdmin("acme/repo"), protectionErr: errors.New("分支保护失败")},
			wantErr: "acme/repo: 分支保护失败",
		},
		"标签收敛失败": {
			admin:   &failingLabelAdmin{fakeAdmin: newFakeAdmin("acme/repo"), labelErr: errors.New("标签收敛失败")},
			wantErr: "acme/repo: 标签收敛失败",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Run(t.Context(), testOptions(), test.admin); err == nil ||
				!strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Run() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

// failingProtectionAdmin 注入分支保护失败（仓库非空且有默认分支才会走到）。
type failingProtectionAdmin struct {
	*fakeAdmin
	protectionErr error
}

func (f *failingProtectionAdmin) GetRepo(ctx context.Context, fullName string) (RepoInfo, bool, error) {
	info, exists, err := f.fakeAdmin.GetRepo(ctx, fullName)
	info.DefaultBranch = "main"
	return info, exists, err
}

func (f *failingProtectionAdmin) EnsureBranchProtection(context.Context, string, ProtectionOptions) error {
	return f.protectionErr
}

// failingLabelAdmin 注入标签收敛失败。
type failingLabelAdmin struct {
	*fakeAdmin
	labelErr error
}

func (f *failingLabelAdmin) GetRepo(ctx context.Context, fullName string) (RepoInfo, bool, error) {
	info, exists, err := f.fakeAdmin.GetRepo(ctx, fullName)
	info.DefaultBranch = "main"
	return info, exists, err
}

func (f *failingLabelAdmin) ReconcileLabels(context.Context, string, string) error {
	return f.labelErr
}

// 标签收敛失败发生在写完 secret 之后，但 Run 仍在返回结果前失败。
func TestRunPropagatesUserExistLookupFailure(t *testing.T) {
	admin := &failingUserExistsAdmin{fakeAdmin: newFakeAdmin("acme/repo"), userErr: errors.New("用户查询失败")}
	if _, err := Run(t.Context(), testOptions(), admin); err == nil ||
		!strings.Contains(err.Error(), "用户查询失败") {
		t.Fatalf("Run() error = %v, want 用户查询失败", err)
	}
}

// 列出实例仓库失败时 SyncMergeSecrets 直接返回错误。
func TestRunPropagatesListAllReposFailure(t *testing.T) {
	admin := &failingListAllAdmin{Admin: newFakeAdmin("acme/repo"), listAllErr: errors.New("搜索端点拒绝")}
	if _, err := Run(t.Context(), testOptions(), admin); err == nil ||
		!strings.Contains(err.Error(), "同步 Actions 密钥") {
		t.Fatalf("Run() error = %v, want 同步 Actions 密钥", err)
	}
}

// failingUserExistsAdmin 注入账号存在性查询失败。
type failingUserExistsAdmin struct {
	*fakeAdmin
	userErr error
}

func (f *failingUserExistsAdmin) UserExists(context.Context, string) (bool, error) {
	return false, f.userErr
}

// ConvergeToken 的「列出/删除/新建」三条失败路径都必须带语境上报。
func TestConvergeTokenErrorPaths(t *testing.T) {
	newAdmin := func(t *testing.T, stub *giteaStub) *giteaAdmin {
		t.Helper()
		admin, _ := newStubAdmin(t, stub)
		return admin
	}
	t.Run("列出令牌失败", func(t *testing.T) {
		stub := newGiteaStub(t)
		stub.jsonStatusRoute("GET /api/v1/users/reviewer/tokens", http.StatusUnauthorized, nil)
		admin := newAdmin(t, stub)
		_, _, err := admin.ConvergeToken(t.Context(), "reviewer", "pw", ReviewerTokenName, "")
		if err == nil || !strings.Contains(err.Error(), "列出 reviewer 的令牌") {
			t.Fatalf("ConvergeToken() error = %v, want 列出失败语境", err)
		}
	})
	t.Run("删除旧令牌失败", func(t *testing.T) {
		stub := newGiteaStub(t)
		stub.jsonRoute("GET /api/v1/users/reviewer/tokens",
			[]map[string]any{{"id": 9, "name": ReviewerTokenName}})
		stub.jsonStatusRoute("DELETE /api/v1/users/reviewer/tokens/9", http.StatusConflict, nil)
		admin := newAdmin(t, stub)
		_, _, err := admin.ConvergeToken(t.Context(), "reviewer", "pw", ReviewerTokenName, "")
		if err == nil || !strings.Contains(err.Error(), "删除 reviewer 的本工具令牌") {
			t.Fatalf("ConvergeToken() error = %v, want 删除失败语境", err)
		}
	})
	t.Run("新建令牌失败", func(t *testing.T) {
		stub := newGiteaStub(t)
		stub.jsonRoute("GET /api/v1/users/reviewer/tokens", []map[string]any{})
		stub.jsonStatusRoute("POST /api/v1/users/reviewer/tokens", http.StatusForbidden, nil)
		admin := newAdmin(t, stub)
		_, _, err := admin.ConvergeToken(t.Context(), "reviewer", "pw", ReviewerTokenName, "")
		if err == nil || !strings.Contains(err.Error(), "为 reviewer 创建令牌") {
			t.Fatalf("ConvergeToken() error = %v, want 建令牌失败语境", err)
		}
	})
}

// ensureAccount 在准备密码失败时必须带账号名包装；dry-run 且现有令牌有效时
// 保留现有令牌。
func TestEnsureAccountPasswordFailureAndDryRunReuse(t *testing.T) {
	t.Run("准备密码失败", func(t *testing.T) {
		admin := &failingEnsurePasswordAdmin{
			fakeAdmin: newFakeAdmin(), passwordErr: errors.New("重置密码失败")}
		_, _, err := ensureAccount(t.Context(), admin, testOptions(),
			func(string, ...any) {}, "reviewer", "")
		if err == nil || !strings.Contains(err.Error(), "准备 reviewer 的密码") {
			t.Fatalf("ensureAccount() error = %v, want 密码失败语境", err)
		}
	})
	t.Run("dry-run 复用有效令牌", func(t *testing.T) {
		admin := newFakeAdmin()
		admin.users["reviewer"] = true
		admin.tokens[tokenKey("reviewer", ReviewerTokenName)] = "keep-token-1234"
		options := testOptions()
		options.DryRun = true
		logs := make([]string, 0)
		token, created, err := ensureAccount(t.Context(), admin, options,
			func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
			"reviewer", "keep-token-1234")
		if err != nil {
			t.Fatalf("ensureAccount() error = %v", err)
		}
		if created || token != "keep-token-1234" {
			t.Errorf("token = %q created = %v, want dry-run 保留现有令牌", token, created)
		}
		if !slices.ContainsFunc(logs, func(line string) bool {
			return strings.Contains(line, "dry-run：保留 reviewer 的现有令牌")
		}) {
			t.Errorf("日志缺少 dry-run 复用提示：%v", logs)
		}
	})
}

// failingEnsurePasswordAdmin 注入密码准备失败。
type failingEnsurePasswordAdmin struct {
	*fakeAdmin
	passwordErr error
}

func (f *failingEnsurePasswordAdmin) EnsurePassword(context.Context, string) (string, error) {
	return "", f.passwordErr
}

// SyncMergeSecrets 读取协作者失败时跳过该仓库，继续处理后面的仓库。
func TestSyncMergeSecretsSkipsUnreadableCollaborators(t *testing.T) {
	admin := &partialCollaboratorAdmin{
		fakeAdmin: newFakeAdmin("acme/hidden", "acme/ok"),
		blocked:   map[string]bool{"acme/hidden": true},
	}
	admin.repos["acme/ok"] = RepoInfo{}
	for _, name := range []string{"acme/hidden", "acme/ok"} {
		if err := admin.fakeAdmin.AddCollaborator(t.Context(), name,
			instances.DefaultMergerName, "admin"); err != nil {
			t.Fatalf("AddCollaborator() error = %v", err)
		}
	}
	logs := make([]string, 0)
	err := SyncMergeSecrets(t.Context(), admin, instances.DefaultMergerName, "merge-token", false,
		func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
	if err != nil {
		t.Fatalf("SyncMergeSecrets() error = %v", err)
	}
	if !slices.ContainsFunc(logs, func(line string) bool {
		return strings.Contains(line, "acme/hidden: 读取协作者失败") && strings.Contains(line, "跳过")
	}) {
		t.Errorf("日志缺少跳过提示：%v", logs)
	}
	if _, ok := admin.secrets["acme/ok/"+ActionsSecretMergeToken]; !ok {
		t.Errorf("可读仓库应写入 secret：%+v", admin.secrets)
	}
}

// partialCollaboratorAdmin 让指定仓库的协作者读取失败。
type partialCollaboratorAdmin struct {
	*fakeAdmin
	blocked map[string]bool
}

func (p *partialCollaboratorAdmin) ListCollaborators(ctx context.Context, fullName string) ([]Collaborator, error) {
	if p.blocked[fullName] {
		return nil, errors.New("无权读取")
	}
	return p.fakeAdmin.ListCollaborators(ctx, fullName)
}

// createThenFailRepoAdmin 让「创建仓库后重新读取」这一步失败，覆盖
// setupRepository 里 CreateRepo 成功但 GetRepo 报错的分支。
type createThenFailRepoAdmin struct {
	*fakeAdmin
	getCalls int
	err      error
}

func (c *createThenFailRepoAdmin) GetRepo(ctx context.Context, fullName string) (RepoInfo, bool, error) {
	c.getCalls++
	if c.getCalls > 1 {
		return RepoInfo{}, false, c.err
	}
	return RepoInfo{}, false, nil
}

// 仓库创建成功但随后读不回来时，错误必须带仓库名原样上报。
func TestRunPropagatesRereadAfterCreateFailure(t *testing.T) {
	admin := &createThenFailRepoAdmin{fakeAdmin: newFakeAdmin(), err: errors.New("读取仓库失败")}
	options := testOptions()
	options.CreateRepos = true

	_, err := Run(t.Context(), options, admin)
	if err == nil || !strings.Contains(err.Error(), "读取仓库失败") {
		t.Fatalf("Run() error = %v, want 包含 %q", err, "读取仓库失败")
	}
	if admin.getCalls != 2 {
		t.Errorf("GetRepo 调用次数 = %d, want 2", admin.getCalls)
	}
}

// 分支保护日志里的「管理员绕过」与「必要检查」两项取决于选项：这里覆盖非默认分支，
// 避免只测到 else 里的缺省文案。
func TestRunLogsAdminOverrideAndStatusChecks(t *testing.T) {
	admin := newFakeAdmin("acme/repo")
	admin.repos["acme/repo"] = RepoInfo{DefaultBranch: "main"}
	options := testOptions()
	options.AllowAdminOverride = true
	options.StatusCheckContexts = []string{"ci/build", "ci/lint"}
	var logs []string
	options.Log = func(format string, arguments ...any) {
		logs = append(logs, fmt.Sprintf(format, arguments...))
	}

	if _, err := Run(t.Context(), options, admin); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !slices.ContainsFunc(logs, func(line string) bool {
		return strings.Contains(line, "管理员绕过 开启（管理员可绕过）")
	}) {
		t.Errorf("日志缺少「管理员可绕过」：%v", logs)
	}
	if !slices.ContainsFunc(logs, func(line string) bool {
		return strings.Contains(line, "必要检查=ci/build, ci/lint")
	}) {
		t.Errorf("日志缺少必要检查清单：%v", logs)
	}
}
