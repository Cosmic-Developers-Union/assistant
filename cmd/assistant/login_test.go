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
