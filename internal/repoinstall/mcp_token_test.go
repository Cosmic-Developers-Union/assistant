package repoinstall

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/credentials"
)

// xdgCredentialPath 返回给定 XDG_CONFIG_HOME 下的凭据库标准落点。
func xdgCredentialPath(t *testing.T, xdgDir string) string {
	t.Helper()
	return filepath.Join(xdgDir, "Cosmic-Developers-Union", "assistant", "credentials.json")
}

// 已登记身份但没有 mcp 令牌（登录中断或手工编辑过凭据库）：错误必须点名账号并
// 给出派生命令，而不是一句"没有凭据"。
func TestMissingMCPHintExplainsIdentityOnlyState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: "https://a.example.com", User: "Ge", IsAdmin: true})
	if err := credentials.Save(xdgCredentialPath(t, dir), store); err != nil {
		t.Fatal(err)
	}
	getenv := func(string) string { return "" }
	_, err := ResolveMCP(context.Background(), MCPOptions{Host: "https://a.example.com", Getenv: getenv})
	if err == nil {
		t.Fatal("缺少 mcp 令牌时应报错")
	}
	for _, want := range []string{
		"@Ge", "没有 (host, Ge, mcp) 用途令牌",
		"assistant login add https://a.example.com --user Ge",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息缺少 %q：\n%v", want, err)
		}
	}
}

// 凭据库是 mcp 令牌的唯一来源：按 host
// 命中凭据库，库中没有的站点不借用他人令牌，ASSISTANT_CREDENTIALS 可显式指定位置。
func TestMCPTokenFromCredentialsStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	store := &credentials.File{}
	store.SetCredential(credentials.Credential{
		Host: "https://a.example.com", User: "alice", Purpose: credentials.PurposeMCP, Token: "store-a",
	})
	if err := credentials.Save(xdgCredentialPath(t, dir), store); err != nil {
		t.Fatal(err)
	}
	getenv := func(string) string { return "" }
	token, source, err := resolveMCPToken("https://a.example.com", getenv)
	if err != nil || token != "store-a" || !strings.Contains(source, "@alice") {
		t.Fatalf("应从凭据库取令牌：token=%q source=%q err=%v", token, source, err)
	}
	// 库中没有的站点不借用其他站点的令牌
	if token, _, err := resolveMCPToken("https://b.example.com", getenv); err != nil || token != "" {
		t.Fatalf("未知站点应无令牌：token=%q err=%v", token, err)
	}
	// ASSISTANT_CREDENTIALS 显式指定凭据库位置
	overrideDir := t.TempDir()
	overridePath := filepath.Join(overrideDir, "credentials.json")
	override := &credentials.File{}
	override.SetCredential(credentials.Credential{
		Host: "https://a.example.com", User: "carol", Purpose: credentials.PurposeMCP, Token: "store-b",
	})
	if err := credentials.Save(overridePath, override); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CREDENTIALS", overridePath)
	if token, source, err := resolveMCPToken("https://a.example.com", getenv); err != nil || token != "store-b" || !strings.Contains(source, "@carol") {
		t.Fatalf("ASSISTANT_CREDENTIALS 覆盖失败：token=%q source=%q err=%v", token, source, err)
	}
}

// 凭据库按 (host, user, purpose) 隔离：站点 / 账号 / 用途互不借用。
func TestMCPTokenIsolation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	store := &credentials.File{}
	store.SetCredential(credentials.Credential{
		Host: "https://a.example.com", User: "alice", Purpose: credentials.PurposeMCP, Token: "token-a",
	})
	store.SetCredential(credentials.Credential{
		Host: "https://b.example.com", User: "bob", Purpose: credentials.PurposeMCP, Token: "token-b",
	})
	store.SetCredential(credentials.Credential{
		Host: "https://admin.example.com", User: "alice", Purpose: credentials.PurposeAdmin, Token: "admin-only",
	})
	if err := credentials.Save(xdgCredentialPath(t, dir), store); err != nil {
		t.Fatal(err)
	}
	getenv := func(string) string { return "" }
	for _, tc := range []struct{ host, want string }{
		{"https://a.example.com/", "token-a"},
		{"https://b.example.com", "token-b"},
		{"https://c.example.com", ""},
		{"https://unknown.example.com", ""},
		{"https://admin.example.com", ""},
	} {
		t.Run(tc.host, func(t *testing.T) {
			token, _, err := resolveMCPToken(tc.host, getenv)
			if err != nil || token != tc.want {
				t.Fatalf("token = %q, err = %v; want %q", token, err, tc.want)
			}
			if tc.want == "" {
				_, err := ResolveMCP(context.Background(), MCPOptions{Host: tc.host, Getenv: getenv})
				if err == nil || !strings.Contains(err.Error(), "assistant login add "+tc.host) {
					t.Fatalf("expected host-specific login guidance, got %v", err)
				}
			}
		})
	}
}

func TestMCPTokenExplicitOverrides(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(" file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, envToken, file, want string
		wantErr                    bool
	}{
		{"env first", "env-token", "/missing", "env-token", false},
		{"file", "", file, "file-token", false},
		{"missing file fails", "", file + ".missing", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string {
				return map[string]string{"GITEA_ACCESS_TOKEN": tc.envToken, "GITEA_ACCESS_TOKEN_FILE": tc.file}[key]
			}
			token, _, err := resolveMCPToken("https://a.example.com", getenv)
			if (err != nil) != tc.wantErr || token != tc.want {
				t.Fatalf("token = %q, err = %v", token, err)
			}
		})
	}
}
