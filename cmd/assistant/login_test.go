package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/instances"
)

func savePlatform(t *testing.T, path string, instance instances.Instance) {
	t.Helper()
	if err := instances.Save(path, &instances.File{Instances: []instances.Instance{instance}}); err != nil {
		t.Fatal(err)
	}
}

func TestLoginListAndRemove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	savePlatform(t, path, instances.Instance{
		Host:       "https://gitea.example.com",
		Reviewer:   instances.Account{Name: "ai"},
		Merger:     instances.Account{Name: "merge"},
		Repos:      []instances.Repo{{Name: "acme/repo"}},
		AdminOAuth: &instances.OAuthCredential{ClientID: "cid", RefreshToken: "rt"},
	})

	var output bytes.Buffer
	list := newLoginListCommand(&path)
	list.SetOut(&output)
	if err := list.Execute(); err != nil {
		t.Fatalf("login list error = %v", err)
	}
	if !strings.Contains(output.String(), "https://gitea.example.com") ||
		!strings.Contains(output.String(), "admin=oauth") ||
		!strings.Contains(output.String(), "repos=1") {
		t.Fatalf("login list 输出 = %q", output.String())
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
}

func TestLoginRemoveKeepsOtherPlatforms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	file := &instances.File{Instances: []instances.Instance{
		{Host: "https://a.example.com", Reviewer: instances.Account{Name: "ai"},
			Merger: instances.Account{Name: "merge"}, AdminToken: "token-a"},
		{Host: "https://b.example.com", Reviewer: instances.Account{Name: "ai"},
			Merger: instances.Account{Name: "merge"}, AdminToken: "token-b"},
	}}
	if err := instances.Save(path, file); err != nil {
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
	if len(loaded.Instances) != 1 || loaded.Instances[0].Host != "https://b.example.com" {
		t.Fatalf("剩余平台 = %+v", loaded.Instances)
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

// 登录令牌解析：显式 --token 优先，其次 tea CLI 配置，无匹配则为空。
func TestResolveLoginToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	content := `logins:
    - name: gitea
      url: https://gitea.example.com
      token: tea-token
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEA_CONFIG", path)

	if token, source, _ := resolveLoginToken("https://gitea.example.com", "explicit-token", os.Getenv); token != "explicit-token" || source != "--token" {
		t.Errorf("显式令牌 = %q %q", token, source)
	}
	token, source, hint := resolveLoginToken("https://gitea.example.com/", "", os.Getenv)
	if token != "tea-token" || !strings.Contains(source, "tea config") || hint != "" {
		t.Errorf("tea 复用 = %q %q hint=%q, want tea-token", token, source, hint)
	}
	if token, _, hint := resolveLoginToken("https://other.example.com", "", os.Getenv); token != "" || hint != "" {
		t.Errorf("未登录站点不应命中：%q hint=%q", token, hint)
	}

	// 有登录条目但没有令牌：给出可行动提示
	noToken := filepath.Join(dir, "no-token.yml")
	if err := os.WriteFile(noToken, []byte("logins:\n  - name: GeX\n    url: https://gitea.aicler.com\n    auth_method: oauth\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEA_CONFIG", noToken)
	token, _, hint = resolveLoginToken("https://gitea.aicler.com", "", os.Getenv)
	if token != "" || !strings.Contains(hint, "没有保存令牌") || !strings.Contains(hint, "--token") {
		t.Errorf("空令牌提示缺失：token=%q hint=%q", token, hint)
	}
}
