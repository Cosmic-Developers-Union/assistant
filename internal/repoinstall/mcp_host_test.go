package repoinstall

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/credentials"
	"assistant/internal/instances"
)

// 检出内没有 Gitea remote（乃至不是 git 检出）时：回退 config.json 里唯一启用
// 的 gitea 通道。MCP 随任意目录启动，无上游不该让它起不来。
func TestResolveMCPFallsBackToRegisteredConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.json")
	enabled, disabled := true, false
	if err := instances.Save(configPath, &instances.File{Channels: []instances.Channel{
		{Type: instances.ChannelGitea, Name: "main", Host: "https://gitea.example.com", Enabled: &enabled},
		{Type: instances.ChannelGitea, Name: "extra", Host: "https://disabled.example.com", Enabled: &disabled},
	}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CONFIG", configPath)
	t.Setenv("GITEA_ACCESS_TOKEN", "dev-token")
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(t.TempDir(), "credentials.json"))

	spec, err := ResolveMCP(context.Background(), MCPOptions{
		Dir:   dir,
		Probe: func(string) bool { return false }, // 探测全不中：等同无可用 remote
	})
	if err != nil {
		t.Fatalf("ResolveMCP() error = %v", err)
	}
	if spec.Host != "https://gitea.example.com" || spec.HostSource != "config.json" {
		t.Errorf("host = %q (%s), want config.json 里唯一启用通道", spec.Host, spec.HostSource)
	}
}

// config.json 登记了多个平台：无上游时不猜，显式报错点名 GITEA_HOST。
func TestResolveMCPAmbiguousConfigHosts(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{Channels: []instances.Channel{
		{Type: instances.ChannelGitea, Name: "a", Host: "https://a.example.com"},
		{Type: instances.ChannelGitea, Name: "b", Host: "https://b.example.com"},
	}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CONFIG", configPath)
	t.Setenv("GITEA_ACCESS_TOKEN", "dev-token")
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(t.TempDir(), "credentials.json"))

	_, err := ResolveMCP(context.Background(), MCPOptions{
		Dir:   dir,
		Probe: func(string) bool { return false },
	})
	if err == nil || !strings.Contains(err.Error(), "https://a.example.com") ||
		!strings.Contains(err.Error(), "https://b.example.com") || !strings.Contains(err.Error(), "GITEA_HOST") {
		t.Fatalf("多平台应显式报错：%v", err)
	}
}

// 没有配置文件：回退凭据库里唯一登记过的站点。
func TestResolveMCPFallsBackToCredentialHost(t *testing.T) {
	dir := t.TempDir()
	credentialPath := filepath.Join(t.TempDir(), "credentials.json")
	store := &credentials.File{}
	store.SetIdentity(credentials.Identity{Host: "https://cred.example.com", User: "ge"})
	store.SetCredential(credentials.Credential{Host: "https://cred.example.com", User: "ge", Purpose: credentials.PurposeMCP, Token: "tok"})
	if err := credentials.Save(credentialPath, store); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CONFIG", filepath.Join(t.TempDir(), "config.json")) // 不存在
	t.Setenv("ASSISTANT_CREDENTIALS", credentialPath)
	t.Setenv("GITEA_ACCESS_TOKEN", "")

	spec, err := ResolveMCP(context.Background(), MCPOptions{
		Dir:   dir,
		Probe: func(string) bool { return false },
	})
	if err != nil {
		t.Fatalf("ResolveMCP() error = %v", err)
	}
	if spec.Host != "https://cred.example.com" || spec.HostSource != "credentials.json" {
		t.Errorf("host = %q (%s), want credentials.json 里唯一站点", spec.Host, spec.HostSource)
	}
	if spec.Token != "tok" {
		t.Errorf("token = %q, want 凭据库里的 mcp 令牌", spec.Token)
	}
}

// 什么都不登记：报可行动错误，不猜。
func TestResolveMCPNoHostAnywhere(t *testing.T) {
	t.Setenv("ASSISTANT_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv("GITEA_ACCESS_TOKEN", "")

	_, err := ResolveMCP(context.Background(), MCPOptions{
		Dir:   t.TempDir(),
		Probe: func(string) bool { return false },
	})
	if err == nil || !strings.Contains(err.Error(), "无法检测 Gitea 实例") {
		t.Fatalf("无任何依据应报错：%v", err)
	}
}

// remote 探测仍优先于登记状态：多上游检出里，命中探测的 remote 说了算。
func TestResolveMCPRemoteBeatsRegisteredConfig(t *testing.T) {
	dir := t.TempDir()
	if output, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	if output, err := exec.Command("git", "-C", dir, "remote", "add", "origin",
		"https://remote.example.com/owner/repo.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v %s", err, output)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{Channels: []instances.Channel{
		{Type: instances.ChannelGitea, Host: "https://config.example.com"},
	}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CONFIG", configPath)
	t.Setenv("ASSISTANT_CREDENTIALS", filepath.Join(t.TempDir(), "credentials.json"))
	t.Setenv("GITEA_ACCESS_TOKEN", "dev-token")

	spec, err := ResolveMCP(context.Background(), MCPOptions{
		Dir:   dir,
		Probe: func(host string) bool { return host == "https://remote.example.com" },
	})
	if err != nil {
		t.Fatalf("ResolveMCP() error = %v", err)
	}
	if spec.Host != "https://remote.example.com" || !strings.HasPrefix(spec.HostSource, "remote ") {
		t.Errorf("host = %q (%s), want remote 探测优先", spec.Host, spec.HostSource)
	}
}
