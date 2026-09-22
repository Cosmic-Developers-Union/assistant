package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/credentials"
	"assistant/internal/instances"
	"assistant/internal/repoinstall"
)

// 登录只影响目标 (host, user) 的凭据：其他站点的凭据不动，MCP 解析用登录写入的
// mcp 令牌。
func TestLoginCredentialIsolation(t *testing.T) {
	state, server := newLoginServer(t, "developer", false)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	savePlatform(t, path, instances.Instance{
		Host:     server.URL,
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
	})
	store := &credentials.File{}
	store.SetCredential(credentials.Credential{
		Host: "https://other.example.com", User: "bob", Purpose: credentials.PurposeMerge, Token: "other-merge",
	})
	if err := credentials.Save(filepath.Join(dir, "credentials.json"), store); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	command := newLoginCommand(&path)
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetIn(strings.NewReader(" s3cret \n"))
	command.SetArgs([]string{server.URL, "--user", "developer", "--password-stdin"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(state.creates) != 1 {
		t.Fatalf("creates = %v", state.creates)
	}

	reloaded, err := credentials.Load(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if credential, ok := reloaded.CredentialForUser("https://other.example.com", "bob", credentials.PurposeMerge); !ok ||
		credential.Token != "other-merge" {
		t.Fatalf("其他站点凭据被改动：%+v", credential)
	}
	credential, ok := reloaded.CredentialForUser(server.URL, "developer", credentials.PurposeMCP)
	if !ok || credential.Token != "token-"+state.creates[0] {
		t.Fatalf("凭据 = %+v ok=%v", credential, ok)
	}
	if strings.Contains(out.String(), credential.Token) {
		t.Fatal("登录输出泄露令牌")
	}
	spec, err := repoinstall.ResolveMCP(command.Context(), repoinstall.MCPOptions{
		Host: server.URL, ConfigPath: path, Getenv: func(string) string { return "" },
	})
	if err != nil || spec.Token != credential.Token {
		t.Fatalf("MCP 未使用登录写入的令牌：%v", err)
	}
}

func savePlatform(t *testing.T, path string, instance instances.Instance) {
	t.Helper()
	if err := instances.Save(path, &instances.File{Instances: []instances.Instance{instance}}); err != nil {
		t.Fatal(err)
	}
}

func TestLoginListAndRemove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	savePlatform(t, path, instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
		Repos:    []instances.Repo{{Name: "acme/repo"}},
	})
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: "https://gitea.example.com", User: "Ge", IsAdmin: true})
	store.SetCredential(credentials.Credential{Host: "https://gitea.example.com", User: "Ge",
		Purpose: credentials.PurposeMCP, Token: "mcp", TokenName: "assistant-mcp"})
	store.SetCredential(credentials.Credential{Host: "https://gitea.example.com", User: "Ge",
		Purpose: credentials.PurposeAdmin, Token: "admin", TokenName: "assistant-admin"})
	store.SetCredential(credentials.Credential{Host: "https://gitea.example.com", User: "ai",
		Purpose: credentials.PurposeReview, Token: "review", TokenName: "assistant"})
	if err := credentials.Save(filepath.Join(dir, "credentials.json"), store); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	list := newLoginListCommand(&path)
	list.SetOut(&output)
	if err := list.Execute(); err != nil {
		t.Fatalf("login list error = %v", err)
	}
	text := output.String()
	for _, want := range []string{
		"https://gitea.example.com", "repos=1", "reviewer=ai", "merger=merge",
		"identity=@Ge(admin)", "mcp@Ge", "admin@Ge", "review@ai",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("login list 输出缺少 %q：\n%s", want, text)
		}
	}

	output.Reset()
	remove := newLoginRemoveCommand(&path)
	remove.SetOut(&output)
	remove.SetArgs([]string{"https://gitea.example.com"})
	if err := remove.Execute(); err != nil {
		t.Fatalf("login remove error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("移除最后一个平台后应删除配置文件：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "credentials.json")); !os.IsNotExist(err) {
		t.Errorf("凭据清空后应删除凭据文件：%v", err)
	}
}

func TestLoginRemoveKeepsOtherPlatforms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	t.Setenv("ASSISTANT_CREDENTIALS", "")
	file := &instances.File{Instances: []instances.Instance{
		{Host: "https://a.example.com", Reviewer: instances.Account{Name: "ai"}, Merger: instances.Account{Name: "merge"}},
		{Host: "https://b.example.com", Reviewer: instances.Account{Name: "ai"}, Merger: instances.Account{Name: "merge"}},
	}}
	if err := instances.Save(path, file); err != nil {
		t.Fatal(err)
	}
	store := &credentials.File{}
	store.SetCredential(credentials.Credential{Host: "https://a.example.com", User: "alice",
		Purpose: credentials.PurposeMCP, Token: "a-mcp"})
	store.SetCredential(credentials.Credential{Host: "https://b.example.com", User: "bob",
		Purpose: credentials.PurposeMCP, Token: "b-mcp"})
	if err := credentials.Save(filepath.Join(dir, "credentials.json"), store); err != nil {
		t.Fatal(err)
	}
	remove := newLoginRemoveCommand(&path)
	remove.SetOut(&bytes.Buffer{})
	remove.SetArgs([]string{"https://a.example.com"})
	if err := remove.Execute(); err != nil {
		t.Fatalf("login remove error = %v", err)
	}
	loaded, err := instances.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(giteaChannels(loaded)) != 1 || giteaChannels(loaded)[0].Host != "https://b.example.com" {
		t.Fatalf("剩余平台 = %+v", giteaChannels(loaded))
	}
	reloaded, err := credentials.Load(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.CredentialForUser("https://a.example.com", "alice", credentials.PurposeMCP); ok {
		t.Fatal("a 站点的凭据应被删除")
	}
	if _, ok := reloaded.CredentialForUser("https://b.example.com", "bob", credentials.PurposeMCP); !ok {
		t.Fatal("b 站点的凭据不应受影响")
	}
}

// 配置解析不读当前目录 config.json：避免检出内同名文件被当成运行配置。
func TestConfigResolutionIgnoresWorkingDirectory(t *testing.T) {
	t.Setenv("ASSISTANT_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)

	path, file, err := resolveInstanceFile(commandOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if path != "" || file != nil {
		t.Fatalf("不应加载当前目录 config.json：path=%q", path)
	}

	writePath, err := setupConfigWritePath("")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "Cosmic-Developers-Union", "assistant", "config.json")
	if writePath != want {
		t.Fatalf("写配置落点 = %q, want %q", writePath, want)
	}
}
