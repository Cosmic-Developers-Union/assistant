package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
)

// seedTokenStore 写一份多站点/多账号/多用途的凭据库，供 token 命令族测试共用。
func seedTokenStore(t *testing.T, dir string) {
	t.Helper()
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: "https://gitea.example.com", User: "ge", IsAdmin: true})
	store.SetCredential(credentials.Credential{
		Host: "https://gitea.example.com", User: "ge", Purpose: credentials.PurposeMCP,
		Token: "mcp-secret-token", TokenName: "assistant-mcp-gitea-example-com-ge",
		Scopes: credentials.MCPScopes(), Source: credentials.SourceLogin, CreatedAt: "2026-01-02T03:04:05Z",
	})
	store.SetCredential(credentials.Credential{
		Host: "https://gitea.example.com", User: "ge", Purpose: credentials.PurposeAdmin,
		Token: "admin-secret-token", TokenName: "assistant-admin-gitea-example-com-ge",
		Scopes: credentials.AdminScopes(), Source: credentials.SourceLogin,
	})
	store.SetCredential(credentials.Credential{
		Host: "https://gitea.example.com", User: "ai", Purpose: credentials.PurposeReview,
		Token: "review-secret-token", TokenName: "assistant-review", Source: credentials.SourceSetup,
	})
	store.SetCredential(credentials.Credential{
		Host: "https://other.example.com", User: "bob", Purpose: credentials.PurposeMCP,
		Token: "other-secret-token",
	})
	// other 站点放两个账号且无身份记录：token 命令不许猜，必须 --user 点名
	store.SetCredential(credentials.Credential{
		Host: "https://other.example.com", User: "merge", Purpose: credentials.PurposeMerge,
		Token: "other-merge-token",
	})
	if err := credentials.Save(filepath.Join(dir, "credentials.json"), store); err != nil {
		t.Fatal(err)
	}
}

// list 展示 purpose@user、末 8 位与来源，绝不输出令牌明文；host/--user 过滤生效。
func TestLoginTokenList(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	seedTokenStore(t, dir)

	var out bytes.Buffer
	list := newLoginTokenListCommand()
	list.SetOut(&out)
	list.SetErr(&out)
	if err := list.Execute(); err != nil {
		t.Fatalf("login token list error = %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"https://gitea.example.com", "mcp@ge", "admin@ge", "review@ai", "mcp@bob",
		"assistant-mcp-gitea-example-com-ge", "login", "setup", "2026-01-02T03:04:05Z",
		credentials.LastEight("mcp-secret-token"),
		"凭据库：" + filepath.Join(dir, "credentials.json"),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("token list 输出缺少 %q：\n%s", want, text)
		}
	}
	for _, secret := range []string{"mcp-secret-token", "admin-secret-token", "review-secret-token", "other-secret-token"} {
		if strings.Contains(text, secret) {
			t.Fatalf("token list 泄露令牌明文 %q：\n%s", secret, text)
		}
	}

	// host 过滤：只留该站点的行
	out.Reset()
	list.SetArgs([]string{"https://other.example.com/"})
	if err := list.Execute(); err != nil {
		t.Fatalf("login token list <host> error = %v", err)
	}
	text = out.String()
	if !strings.Contains(text, "mcp@bob") || strings.Contains(text, "mcp@ge") {
		t.Fatalf("host 过滤失效：\n%s", text)
	}

	// --user 过滤
	out.Reset()
	list.SetArgs([]string{"https://gitea.example.com", "--user", "ai"})
	if err := list.Execute(); err != nil {
		t.Fatalf("login token list --user error = %v", err)
	}
	if text = out.String(); !strings.Contains(text, "review@ai") || strings.Contains(text, "mcp@ge") {
		t.Fatalf("--user 过滤失效：\n%s", text)
	}

	// 空库：给可行动提示，不输出任何记录
	empty := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(empty, "credentials.json"))
	out.Reset()
	list.SetArgs(nil)
	if err := list.Execute(); err != nil {
		t.Fatalf("login token list 空库 error = %v", err)
	}
	if text = out.String(); !strings.Contains(text, "凭据库为空") || !strings.Contains(text, "assistant login add") {
		t.Fatalf("空库提示缺失：\n%s", text)
	}
}

// show 按身份裁决默认账号与缺省用途 mcp，输出记录字段与令牌明文；--user 显式
// 点名其他账号。缺失与未知用途都要报可行动错误。
func TestLoginTokenShow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	seedTokenStore(t, dir)

	var out bytes.Buffer
	show := newLoginTokenShowCommand()
	show.SetOut(&out)
	show.SetErr(&out)

	// 默认：账号取站点当前登录身份，用途缺省 mcp，输出明文与全部记录字段
	show.SetArgs([]string{"https://gitea.example.com/"})
	if err := show.Execute(); err != nil {
		t.Fatalf("login token show error = %v", err)
	}
	// Save 时 scopes 已按字典序规范化
	sortedScopes := append([]string(nil), credentials.MCPScopes()...)
	slices.Sort(sortedScopes)
	text := out.String()
	for _, want := range []string{
		"账号：@ge", "用途：mcp", "令牌名：assistant-mcp-gitea-example-com-ge",
		"权限：" + strings.Join(sortedScopes, ","),
		"来源：login", "创建：2026-01-02T03:04:05Z", "令牌：mcp-secret-token",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("token show 输出缺少 %q：\n%s", want, text)
		}
	}

	// --user 点名其他账号 + purpose 位置参数
	out.Reset()
	show.SetArgs([]string{"https://gitea.example.com", "review", "--user", "ai"})
	if err := show.Execute(); err != nil {
		t.Fatalf("login token show review error = %v", err)
	}
	if text = out.String(); !strings.Contains(text, "令牌：review-secret-token") || !strings.Contains(text, "来源：setup") {
		t.Fatalf("review 令牌展示不符：\n%s", text)
	}

	// 同站点多账号且无身份记录：不猜，要求 --user
	out.Reset()
	show.SetArgs([]string{"https://other.example.com"})
	err := show.Execute()
	if err == nil || !strings.Contains(err.Error(), "--user") {
		t.Fatalf("多账号歧义应要求 --user：%v", err)
	}

	// 记录缺失：点名账号、站点与用途，给出派生入口
	out.Reset()
	show.SetArgs([]string{"https://gitea.example.com", "merge", "--user", "ai"})
	err = show.Execute()
	if err == nil || !strings.Contains(err.Error(), "没有 merge 用途令牌") || !strings.Contains(err.Error(), "assistant login add") {
		t.Fatalf("缺失凭据应报可行动错误：%v", err)
	}

	// 未登记用途：立即报错
	out.Reset()
	show.SetArgs([]string{"https://gitea.example.com", "nope"})
	err = show.Execute()
	if err == nil || !strings.Contains(err.Error(), "未知用途") {
		t.Fatalf("未知用途应报错：%v", err)
	}
}

// show --check 用令牌向站点验证：有效时报告身份与管理员标记。
func TestLoginTokenShowCheck(t *testing.T) {
	state, server := newLoginServer(t, "ge", true)
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	store := &credentials.File{}
	store.SetCredential(credentials.Credential{
		Host: server.URL, User: "ge", Purpose: credentials.PurposeMCP, Token: "any-token",
	})
	if err := credentials.Save(filepath.Join(dir, "credentials.json"), store); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	show := newLoginTokenShowCommand()
	show.SetOut(&out)
	show.SetErr(&out)
	show.SetArgs([]string{server.URL, "--check"})
	if err := show.Execute(); err != nil {
		t.Fatalf("login token show --check error = %v", err)
	}
	if text := out.String(); !strings.Contains(text, "站点校验：有效（@ge，实例管理员）") {
		t.Fatalf("校验结果缺失（state=%+v）：\n%s", state.login, out.String())
	}
}

// refresh：站点上删同名旧令牌、按记录里的权限集重建，本地记录同步更新且保留
// 来源；输出不出新令牌明文。
func TestLoginTokenRefresh(t *testing.T) {
	state, server := newLoginServer(t, "ge", false)
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	derived, err := credentials.TokenName(server.URL, "ge", credentials.PurposeMCP)
	if err != nil {
		t.Fatal(err)
	}
	// 站点上已有同名旧令牌：轮换必须先删它（id=7）再新建
	state.tokens = []map[string]any{{"id": 7, "name": derived, "token_last_eight": "deadbeef"}}
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: server.URL, User: "ge", IsAdmin: false})
	store.SetCredential(credentials.Credential{
		Host: server.URL, User: "ge", Purpose: credentials.PurposeMCP,
		Token: "stale-token", TokenName: derived, Scopes: []string{"read:user"},
		Source: credentials.SourceLogin, CreatedAt: "2020-01-01T00:00:00Z",
	})
	if err := credentials.Save(filepath.Join(dir, "credentials.json"), store); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	refresh := newLoginTokenRefreshCommand()
	refresh.SetOut(&out)
	refresh.SetErr(&out)
	refresh.SetIn(strings.NewReader(" s3cret \n"))
	refresh.SetArgs([]string{server.URL, "--password-stdin"})
	if err := refresh.Execute(); err != nil {
		t.Fatalf("login token refresh error = %v", err)
	}

	if len(state.deletes) != 1 || state.deletes[0] != "7" {
		t.Fatalf("应删除同名旧令牌：%v", state.deletes)
	}
	if len(state.creates) != 1 || state.creates[0] != derived {
		t.Fatalf("creates = %v, want [%s]", state.creates, derived)
	}
	if got := strings.Join(state.scopes[derived], ","); got != "read:user" {
		t.Fatalf("应沿用记录里的权限集：%v", state.scopes[derived])
	}

	reloaded, err := credentials.Load(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	credential, ok := reloaded.CredentialForUser(server.URL, "ge", credentials.PurposeMCP)
	if !ok {
		t.Fatal("刷新后的凭据丢失")
	}
	if credential.Token != "token-"+derived {
		t.Fatalf("令牌未更新：%+v", credential)
	}
	if credential.LastEight != credentials.LastEight("token-"+derived) {
		t.Fatalf("末 8 位未同步：%+v", credential)
	}
	if credential.Source != credentials.SourceLogin {
		t.Fatalf("来源被改动：%+v", credential)
	}
	if credential.CreatedAt == "2020-01-01T00:00:00Z" {
		t.Fatalf("创建时间应刷新：%+v", credential)
	}
	if text := out.String(); strings.Contains(text, "token-"+derived) {
		t.Fatalf("refresh 输出泄露新令牌明文：\n%s", text)
	}
}

// refresh 兜底与边界：记录没存 scopes 时按用途补缺省；机器人令牌（非当前身份）
// 用 --user 点名刷新并保留 setup 来源；缺失凭据与多账号歧义都报可行动错误。
func TestLoginTokenRefreshEdgeCases(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	state, server := newLoginServer(t, "ai", false)
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: server.URL, User: "ge", IsAdmin: true})
	store.SetCredential(credentials.Credential{
		Host: server.URL, User: "ge", Purpose: credentials.PurposeMCP, Token: "ge-mcp",
	})
	store.SetCredential(credentials.Credential{
		Host: server.URL, User: "ai", Purpose: credentials.PurposeReview,
		Token: "ai-review", TokenName: "assistant-review", Source: credentials.SourceSetup,
	})
	if err := credentials.Save(filepath.Join(dir, "credentials.json"), store); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	refresh := newLoginTokenRefreshCommand()
	refresh.SetOut(&out)
	refresh.SetErr(&out)

	// 当前身份 ge 没有 admin 令牌：报可行动错误，不发任何站点请求
	refresh.SetArgs([]string{server.URL, "admin", "--password-stdin"})
	refresh.SetIn(strings.NewReader(" s3cret \n"))
	if err := refresh.Execute(); err == nil || !strings.Contains(err.Error(), "没有 admin 用途令牌") {
		t.Fatalf("缺失凭据应报可行动错误：%v", err)
	}

	// 记录没存 scopes：按用途补缺省权限集
	refresh.SetArgs([]string{server.URL, "review", "--user", "ai", "--password-stdin"})
	refresh.SetIn(strings.NewReader(" s3cret \n"))
	if err := refresh.Execute(); err != nil {
		t.Fatalf("refresh review error = %v", err)
	}
	if len(state.creates) != 1 || state.creates[0] != "assistant-review" {
		t.Fatalf("creates = %v", state.creates)
	}
	if got := strings.Join(state.scopes["assistant-review"], ","); got != strings.Join(credentials.BotScopes(), ",") {
		t.Fatalf("review scopes = %v", state.scopes["assistant-review"])
	}
	reloaded, err := credentials.Load(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if credential, ok := reloaded.CredentialForUser(server.URL, "ai", credentials.PurposeReview); !ok ||
		credential.Token != "token-assistant-review" || credential.Source != credentials.SourceSetup {
		t.Fatalf("review 凭据 = %+v ok=%v", credential, ok)
	}
	// ge 的 mcp 令牌不受影响
	if credential, _ := reloaded.CredentialForUser(server.URL, "ge", credentials.PurposeMCP); credential.Token != "ge-mcp" {
		t.Fatalf("其他账号凭据被改动：%+v", credential)
	}
}

// 站点上删了同名旧令牌但创建失败：报错必须点破「本地凭据已失效」并给出重新
// 登录的恢复路径，不能只转述创建失败。
func TestLoginTokenRefreshPartialFailureExplainsLockout(t *testing.T) {
	derived, err := credentials.TokenName("https://gitea.example.com", "ge", credentials.PurposeMCP)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/users/ge/tokens" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 7, "name": derived}})
		case r.URL.Path == "/api/v1/users/ge/tokens" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusInternalServerError) // 删除已发生，创建失败
		case strings.HasPrefix(r.URL.Path, "/api/v1/users/ge/tokens/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(dir, "credentials.json"))
	store := &credentials.File{}
	store.SetCredential(credentials.Credential{
		Host: server.URL, User: "ge", Purpose: credentials.PurposeMCP,
		Token: "stale-token", TokenName: derived, Scopes: []string{"read:user"},
	})
	if err := credentials.Save(filepath.Join(dir, "credentials.json"), store); err != nil {
		t.Fatal(err)
	}

	refresh := newLoginTokenRefreshCommand()
	refresh.SetOut(&bytes.Buffer{})
	refresh.SetErr(&bytes.Buffer{})
	refresh.SetIn(strings.NewReader(" s3cret \n"))
	refresh.SetArgs([]string{server.URL, "--user", "ge", "--password-stdin"})
	refreshErr := refresh.Execute()
	if refreshErr == nil {
		t.Fatal("创建失败应报错")
	}
	for _, want := range []string{"已被删除", "assistant login add", "--user ge"} {
		if !strings.Contains(refreshErr.Error(), want) {
			t.Errorf("锁定提示缺少 %q：%v", want, refreshErr)
		}
	}
}
