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

// identityServer 模拟站点上身份登录用到的最小 API 面：
// /api/v1/user（身份）、/api/v1/users/{user}/tokens（Basic Auth 下列出/创建/删除）。
type identityServer struct {
	t           *testing.T
	user        string
	password    string
	totp        string
	login       string
	isAdmin     bool
	creates     int
	deletes     []string
	createdName string
	scopes      []string
	tokens      []map[string]any
	failCreate  bool
}

func (s *identityServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/user":
			// 身份查询用令牌（Bearer/token 前缀由 SDK 决定），此处只要求带上凭据
			if r.Header.Get("Authorization") == "" {
				s.t.Error("身份查询未携带令牌")
			}
			fmt.Fprintf(w, `{"login":%q,"is_admin":%t}`, s.login, s.isAdmin)
		case r.URL.Path == "/api/v1/users/"+s.user+"/tokens":
			authUser, authPassword, ok := r.BasicAuth()
			if !ok || authUser != s.user || (s.password != "" && authPassword != s.password) {
				s.t.Error("令牌端点必须用账号自己的 Basic Auth")
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
				s.creates++
				if s.failCreate {
					w.WriteHeader(http.StatusUnauthorized)
					fmt.Fprint(w, `{"message":"password secret must not be echoed"}`)
					return
				}
				var body struct {
					Name   string
					Scopes []string
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					s.t.Error(err)
				}
				s.createdName, s.scopes = body.Name, body.Scopes
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"sha1":"fresh-mcp-token"}`)
			default:
				s.t.Errorf("unexpected method %s", r.Method)
			}
		default:
			// 删除：/api/v1/users/{user}/tokens/{id}
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

// newIdentityServer 起一个假站点：token 是身份查询接受的令牌明文。
func newIdentityServer(t *testing.T, login string, isAdmin bool, token string) (*identityServer, *httptest.Server) {
	t.Helper()
	state := &identityServer{t: t, user: login, password: " s3cret ", totp: "123456", login: login, isAdmin: isAdmin}
	server := httptest.NewServer(state.handler())
	t.Cleanup(server.Close)
	return state, server
}

func TestIdentityLoginCreatesMCPToken(t *testing.T) {
	state, server := newIdentityServer(t, "developer", true, "fresh-mcp-token")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	// 旧版把 mcp 令牌写在 config.json：登录后应迁移进凭据库并清掉旧字段
	if err := instances.Save(configPath, &instances.File{Instances: []instances.Instance{{
		Host: server.URL, AdminToken: "admin-token",
		Reviewer: instances.Account{Name: "ai", Token: "reviewer-token"},
		Merger:   instances.Account{Name: "merge", Token: "merger-token"},
		MCPToken: "old-mcp-token", MCPUser: "developer",
	}}}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	command := newLoginCommand(&configPath)
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader(" s3cret \n"))
	command.SetArgs([]string{server.URL, "--user", "developer", "--password-stdin", "--totp", "123456"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}

	if state.creates != 1 {
		t.Fatalf("creates = %d, want 1", state.creates)
	}
	if !strings.HasPrefix(state.createdName, "assistant-mcp-127.0.0.1-") || !strings.HasSuffix(state.createdName, "-developer") {
		t.Fatalf("令牌名应按 host+账号派生：%q", state.createdName)
	}
	wantScopes := []string{"read:repository", "write:repository", "read:issue", "write:issue", "read:user"}
	if strings.Join(state.scopes, ",") != strings.Join(wantScopes, ",") {
		t.Fatalf("scopes = %v, want %v（最小权限）", state.scopes, wantScopes)
	}

	credentialPath := filepath.Join(dir, "credentials.json")
	store, err := credentials.Load(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := store.IdentityFor(server.URL)
	if !ok || identity.User != "developer" || !identity.IsAdmin {
		t.Fatalf("identity = %+v ok=%v", identity, ok)
	}
	credential, ok := store.CredentialForUser(server.URL, "developer", credentials.PurposeMCP)
	if !ok || credential.Token != "fresh-mcp-token" || credential.Source != "password" ||
		credential.TokenName != state.createdName || credential.LastEight != "cp-token" {
		t.Fatalf("credential = %+v ok=%v", credential, ok)
	}
	info, err := os.Stat(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("凭据库权限 = %v, want 0600", info.Mode().Perm())
	}

	file, err := instances.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	instance := file.Instances[0]
	if instance.MCPToken != "" || instance.MCPUser != "" {
		t.Fatalf("旧字段应已迁移清理：%+v", instance)
	}
	if instance.AdminToken != "admin-token" || instance.Reviewer.Token != "reviewer-token" ||
		instance.Merger.Token != "merger-token" {
		t.Fatal("身份登录不应改动管理/机器人凭据")
	}
	for _, secret := range []string{"fresh-mcp-token", " s3cret ", "123456", "old-mcp-token"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("输出泄露凭据 %q：%s", secret, out.String())
		}
	}
	if !strings.Contains(out.String(), "实例管理员") {
		t.Fatalf("管理员身份应有能力说明：%s", out.String())
	}
}

// 已有有效凭据时重复登录：不新建令牌、不需要密码。
func TestIdentityLoginReusesValidToken(t *testing.T) {
	state, server := newIdentityServer(t, "developer", true, "existing-token")
	state.password = "" // 复用路径不应发起 Basic Auth 请求
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: server.URL, User: "developer", IsAdmin: true})
	store.SetCredential(credentials.Credential{
		Host: server.URL, User: "developer", Purpose: credentials.PurposeMCP,
		Token: "existing-token", TokenName: "assistant-mcp-old-developer", Source: "password",
	})
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
	if state.creates != 0 {
		t.Fatalf("复用路径不应新建令牌：creates=%d", state.creates)
	}
	reloaded, err := credentials.Load(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := reloaded.CredentialForUser(server.URL, "developer", credentials.PurposeMCP)
	if credential.Token != "existing-token" || credential.TokenName != "assistant-mcp-old-developer" ||
		credential.Source != "reused" {
		t.Fatalf("复用后的凭据 = %+v", credential)
	}
}

// 非管理员身份：记录事实并明确能力边界。
func TestIdentityLoginNonAdminNotice(t *testing.T) {
	state, server := newIdentityServer(t, "developer", false, "fresh-mcp-token")
	_ = state
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	var out bytes.Buffer
	command := newLoginCommand(&configPath)
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader(" s3cret \n"))
	command.SetArgs([]string{server.URL, "--user", "developer", "--password-stdin", "--totp", "123456"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "不是实例管理员") {
		t.Fatalf("非管理员应有明确提示：%s", out.String())
	}
	store, err := credentials.Load(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if identity, ok := store.IdentityFor(server.URL); !ok || identity.IsAdmin {
		t.Fatalf("identity = %+v ok=%v，应为非管理员", identity, ok)
	}
}

func TestIdentityLoginImportsTokenFile(t *testing.T) {
	state, server := newIdentityServer(t, "developer", true, "file-token")
	_ = state
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	tokenPath := filepath.Join(dir, "token")
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	if err := os.WriteFile(tokenPath, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := newLoginCommand(&configPath)
	command.SetOut(&bytes.Buffer{})
	command.SetArgs([]string{server.URL, "--user", "developer", "--token-file", tokenPath})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	store, err := credentials.Load(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	credential, ok := store.CredentialForUser(server.URL, "developer", credentials.PurposeMCP)
	if !ok || credential.Token != "file-token" || credential.Source != "token-file" {
		t.Fatalf("credential = %+v ok=%v", credential, ok)
	}
}

// 录入令牌时 --user 是身份断言：令牌实际属于别的账号必须拒绝且不落盘。
func TestIdentityLoginRejectsIdentityMismatch(t *testing.T) {
	_, server := newIdentityServer(t, "someone-else", false, "other-token")
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	command := newLoginCommand(&configPath)
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{server.URL, "--user", "alice", "--token", "other-token"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "someone-else") {
		t.Fatalf("身份不符应报错并指出实际账号，got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "credentials.json")); !os.IsNotExist(statErr) {
		t.Fatalf("身份不符不应落盘凭据：%v", statErr)
	}
}

// 令牌创建成功但身份校验失败：提示令牌名和不落盘，且不回显服务端响应。
func TestIdentityLoginCreateButVerifyFails(t *testing.T) {
	state, server := newIdentityServer(t, "developer", false, "other-token")
	state.login = "someone-else"
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	var out bytes.Buffer
	command := newLoginCommand(&configPath)
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader(" s3cret \n"))
	command.SetArgs([]string{server.URL, "--user", "developer", "--password-stdin", "--totp", "123456"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), state.createdName) {
		t.Fatalf("应提示已创建的令牌名，got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "credentials.json")); !os.IsNotExist(statErr) {
		t.Fatalf("校验失败不应落盘：%v", statErr)
	}
}
