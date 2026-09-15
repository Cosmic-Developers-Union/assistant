package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/credentials"
)

func TestAuthStatusAndRemove(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: "https://a.example.com", User: "alice", IsAdmin: true})
	store.SetCredential(credentials.Credential{
		Host: "https://a.example.com", User: "alice", Purpose: credentials.PurposeMCP,
		Token: "abcdefgh-token", TokenName: "assistant-mcp-a.example.com-alice",
		LastEight: credentials.LastEight("abcdefgh-token"), Source: "password", Scopes: credentials.MCPScopes(),
	})
	store.SetCredential(credentials.Credential{
		Host: "https://b.example.com", User: "bob", Purpose: credentials.PurposeMCP,
		Token: "other-token", TokenName: "assistant-mcp-b.example.com-bob", Source: "token-file",
	})
	credentialPath := filepath.Join(dir, "credentials.json")
	if err := credentials.Save(credentialPath, store); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	status := newAuthStatusCommand(&configPath)
	status.SetOut(&out)
	if err := status.Execute(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"https://a.example.com", "@alice", "admin=true", "purpose=mcp",
		"assistant-mcp-a.example.com-alice", "gh-token", "source=password",
		"https://b.example.com", "identity=none", credentialPath,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("auth status 缺少 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "abcdefgh-token") {
		t.Fatalf("auth status 不应显示完整令牌：\n%s", got)
	}

	// 单站点过滤
	out.Reset()
	filtered := newAuthStatusCommand(&configPath)
	filtered.SetOut(&out)
	filtered.SetArgs([]string{"https://b.example.com"})
	if err := filtered.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "a.example.com") || !strings.Contains(out.String(), "b.example.com") {
		t.Fatalf("过滤输出 = %q", out.String())
	}

	// 只清一个站点
	remove := newAuthRemoveCommand(&configPath)
	remove.SetOut(&bytes.Buffer{})
	remove.SetArgs([]string{"https://a.example.com"})
	if err := remove.Execute(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := credentials.Load(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.IdentityFor("https://a.example.com"); ok {
		t.Fatal("应清除 a 的身份")
	}
	if _, ok := reloaded.CredentialForUser("https://b.example.com", "bob", credentials.PurposeMCP); !ok {
		t.Fatal("不应影响 b")
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("auth 不应创建 config.json：%v", err)
	}
}

func TestAuthRemoveLastCredentialDeletesFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: "https://a.example.com", User: "alice", IsAdmin: true})
	store.SetCredential(credentials.Credential{
		Host: "https://a.example.com", User: "alice", Purpose: credentials.PurposeMCP, Token: "t",
	})
	credentialPath := filepath.Join(dir, "credentials.json")
	if err := credentials.Save(credentialPath, store); err != nil {
		t.Fatal(err)
	}
	remove := newAuthRemoveCommand(&configPath)
	remove.SetOut(&bytes.Buffer{})
	remove.SetArgs([]string{"https://a.example.com/", "--user", "alice"})
	if err := remove.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(credentialPath); !os.IsNotExist(err) {
		t.Fatalf("清空后应删除凭据文件：%v", err)
	}
}
