package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/credentials"
	"assistant/internal/instances"
)

// loginServer 模拟登录用到的最小 API 面：
// /api/v1/user（身份）与 /api/v1/users/{user}/tokens（Basic Auth 下列出/创建/删除）。
// 令牌值是 "token-<名字>"，因此可以按名字断言创建了哪些用途令牌。
type loginServer struct {
	t        *testing.T
	user     string
	password string
	totp     string
	login    string
	isAdmin  bool
	creates  []string
	scopes   map[string][]string
	deletes  []string
	tokens   []map[string]any
}

func (s *loginServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/version":
			// 版本探测：status 客户端构造时先探一次（避免 SDK 的 nil 版本 panic 路径）
			fmt.Fprint(w, `{"version":"1.26.0"}`)
		case r.URL.Path == "/api/v1/user":
			if r.Header.Get("Authorization") == "" {
				s.t.Error("身份查询未携带令牌")
			}
			fmt.Fprintf(w, `{"login":%q,"is_admin":%t}`, s.login, s.isAdmin)
		case r.URL.Path == "/api/v1/users/"+s.user+"/tokens":
			authUser, authPassword, ok := r.BasicAuth()
			if !ok {
				s.t.Error("令牌端点必须用账号自己的 Basic Auth")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if authUser != s.user || authPassword != s.password {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if s.totp != "" && r.Header.Get("X-Gitea-OTP") != s.totp {
				s.t.Error("TOTP 未通过 X-Gitea-OTP 传递")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch r.Method {
			case http.MethodGet:
				if s.tokens == nil {
					s.tokens = []map[string]any{}
				}
				if err := json.NewEncoder(w).Encode(s.tokens); err != nil {
					s.t.Error(err)
				}
			case http.MethodPost:
				var body struct {
					Name   string
					Scopes []string
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					s.t.Error(err)
				}
				if s.scopes == nil {
					s.scopes = map[string][]string{}
				}
				s.creates = append(s.creates, body.Name)
				s.scopes[body.Name] = body.Scopes
				w.WriteHeader(http.StatusCreated)
				fmt.Fprintf(w, `{"sha1":"token-%s"}`, body.Name)
			default:
				s.t.Errorf("unexpected method %s", r.Method)
			}
		default:
			if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/users/"+s.user+"/tokens/") {
				s.deletes = append(s.deletes, strings.TrimPrefix(r.URL.Path, "/api/v1/users/"+s.user+"/tokens/"))
				w.WriteHeader(http.StatusNoContent)
				return
			}
			s.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newLoginServer(t *testing.T, login string, isAdmin bool) (*loginServer, *httptest.Server) {
	t.Helper()
	state := &loginServer{t: t, user: login, password: " s3cret ", login: login, isAdmin: isAdmin}
	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)
	return state, server
}

// 管理员登录：派生 mcp + admin 两条用途令牌，写入凭据库，config.json 只留平台条目。
func TestLoginDerivesMCPAndAdminTokens(t *testing.T) {
	state, server := newLoginServer(t, "developer", true)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))

	var out bytes.Buffer
	command := newLoginCommand(&configPath)
	command.SetOut(&out)
	command.SetErr(&out)
	state.totp = "123456"
	command.SetIn(strings.NewReader(" s3cret \n"))
	command.SetArgs([]string{server.URL, "--user", "developer", "--password-stdin", "--totp", "123456"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}

	if len(state.creates) != 2 {
		t.Fatalf("creates = %v, want mcp + admin", state.creates)
	}
	mcpName, adminName := state.creates[0], state.creates[1]
	if !strings.HasPrefix(mcpName, "assistant-mcp-127.0.0.1-") || !strings.HasSuffix(mcpName, "-developer") {
		t.Fatalf("mcp 令牌名 = %q", mcpName)
	}
	if !strings.HasPrefix(adminName, "assistant-admin-127.0.0.1-") || !strings.HasSuffix(adminName, "-developer") {
		t.Fatalf("admin 令牌名 = %q", adminName)
	}
	if got := strings.Join(state.scopes[mcpName], ","); got != strings.Join(credentials.MCPScopes(), ",") {
		t.Fatalf("mcp scopes = %v", state.scopes[mcpName])
	}
	if got := strings.Join(state.scopes[adminName], ","); got != strings.Join(credentials.AdminScopes(), ",") {
		t.Fatalf("admin scopes = %v", state.scopes[adminName])
	}

	store, err := credentials.Load(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := store.IdentityFor(server.URL)
	if !ok || identity.User != "developer" || !identity.IsAdmin {
		t.Fatalf("identity = %+v ok=%v", identity, ok)
	}
	for purpose, wantToken := range map[string]string{
		credentials.PurposeMCP:   "token-" + mcpName,
		credentials.PurposeAdmin: "token-" + adminName,
	} {
		credential, ok := store.CredentialForUser(server.URL, "developer", purpose)
		if !ok || credential.Token != wantToken || credential.Source != credentials.SourceLogin {
			t.Fatalf("%s credential = %+v ok=%v", purpose, credential, ok)
		}
	}

	// config.json 只有平台条目：没有任何凭据字段
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "oauth", "password"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("config.json 不应含凭据字段 %q：%s", forbidden, raw)
		}
	}
	file, err := instances.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Channels) != 1 || file.Channels[0].Host != server.URL {
		t.Fatalf("channels = %+v", file.Channels)
	}

	for _, want := range []string{"已登录", "@developer", "mcp 令牌", "admin 令牌"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("输出缺少 %q：\n%s", want, out.String())
		}
	}
	for _, secret := range []string{" s3cret ", "123456", "token-" + mcpName} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("输出泄露 %q：\n%s", secret, out.String())
		}
	}
	info, err := os.Stat(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("凭据库权限 = %v", info.Mode().Perm())
	}
}

// 非管理员：只派生 mcp，并明确说明能力边界。
func TestLoginNonAdminOnlyDerivesMCP(t *testing.T) {
	state, server := newLoginServer(t, "developer", false)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))

	var out bytes.Buffer
	command := newLoginCommand(&configPath)
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader(" s3cret \n"))
	command.SetArgs([]string{server.URL, "--user", "developer", "--password-stdin"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(state.creates) != 1 || !strings.HasPrefix(state.creates[0], "assistant-mcp-") {
		t.Fatalf("creates = %v, want 仅 mcp", state.creates)
	}
	store, err := credentials.Load(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if stored, ok := store.CredentialForUser(server.URL, "developer", credentials.PurposeAdmin); ok {
		t.Fatalf("非管理员不应有 admin 令牌：%+v", stored)
	}
	if !strings.Contains(out.String(), "不是实例管理员") {
		t.Fatalf("应说明能力边界：\n%s", out.String())
	}
}

// 已存令牌仍然有效：重复登录不新建令牌、不需要密码。
func TestLoginReusesValidTokenWithoutPassword(t *testing.T) {
	state, server := newLoginServer(t, "developer", true)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: server.URL, User: "developer", IsAdmin: true})
	for purpose, token := range map[string]string{
		credentials.PurposeMCP:   "existing-mcp",
		credentials.PurposeAdmin: "existing-admin",
	} {
		store.SetCredential(credentials.Credential{
			Host: server.URL, User: "developer", Purpose: purpose,
			Token: token, TokenName: "assistant-" + purpose + "-old", Source: credentials.SourceLogin,
		})
	}
	if err := credentials.Save(filepath.Join(dir, "credentials.json"), store); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	command := newLoginCommand(&configPath)
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{server.URL, "--user", "developer"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(state.creates) != 0 {
		t.Fatalf("复用路径不应新建令牌：%v", state.creates)
	}
	reloaded, err := credentials.Load(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if credential, _ := reloaded.CredentialForUser(server.URL, "developer", credentials.PurposeMCP); credential.Token != "existing-mcp" {
		t.Fatalf("凭据被改动：%+v", credential)
	}
}

// --rotate：忽略已存令牌重建（同名旧令牌在站点上删除）。
func TestLoginRotateReplacesTokens(t *testing.T) {
	state, server := newLoginServer(t, "developer", false)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	derived, err := credentials.TokenName(server.URL, "developer", credentials.PurposeMCP)
	if err != nil {
		t.Fatal(err)
	}
	// 站点上已有同名旧令牌：轮换时必须先删掉它再新建
	state.tokens = []map[string]any{{"id": 7, "name": derived, "token_last_eight": "deadbeef"}}
	store := &credentials.File{}
	store.SetCredential(credentials.Credential{
		Host: server.URL, User: "developer", Purpose: credentials.PurposeMCP,
		Token: "stale", TokenName: derived, Source: credentials.SourceLogin,
	})
	if err := credentials.Save(filepath.Join(dir, "credentials.json"), store); err != nil {
		t.Fatal(err)
	}

	command := newLoginCommand(&configPath)
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetIn(strings.NewReader(" s3cret \n"))
	command.SetArgs([]string{server.URL, "--user", "developer", "--password-stdin", "--rotate"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(state.creates) != 1 || !strings.HasPrefix(state.creates[0], "assistant-mcp-") {
		t.Fatalf("creates = %v", state.creates)
	}
	if len(state.deletes) != 1 || state.deletes[0] != "7" {
		t.Fatalf("应删除同名旧令牌：%v", state.deletes)
	}
}

// 无参数：交互式询问 host → username → password。
func TestLoginInteractivePrompts(t *testing.T) {
	state, server := newLoginServer(t, "developer", false)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	t.Chdir(t.TempDir()) // 无 remote、无配置：host 必须靠询问

	var out bytes.Buffer
	command := newLoginCommand(&configPath)
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader(server.URL + "\ndeveloper\n s3cret \n"))
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(state.creates) != 1 {
		t.Fatalf("creates = %v", state.creates)
	}
	prompts := out.String()
	for _, want := range []string{"Gitea 站点地址", "Gitea 账号", "密码"} {
		if !strings.Contains(prompts, want) {
			t.Errorf("交互式提示缺少 %q：\n%s", want, prompts)
		}
	}
	store, err := credentials.Load(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if identity, ok := store.IdentityFor(server.URL); !ok || identity.User != "developer" {
		t.Fatalf("identity = %+v ok=%v", identity, ok)
	}
}

// 口令错误：不落盘、不回显服务端响应体。
func TestLoginBadPasswordKeepsStoreEmpty(t *testing.T) {
	_, server := newLoginServer(t, "developer", false)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	var out bytes.Buffer
	command := newLoginCommand(&configPath)
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader("wrong\n"))
	command.SetArgs([]string{server.URL, "--user", "developer", "--password-stdin"})
	err := command.Execute()
	if err == nil {
		t.Fatal("凭据错误应报错")
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("错误信息不应回显密码：%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "credentials.json")); !os.IsNotExist(statErr) {
		t.Fatalf("失败不应落盘凭据：%v", statErr)
	}
}
