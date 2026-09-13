package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/dispatcher"
	"assistant/internal/instances"
)

func TestReposAddAndUpdateDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	savePlatform(t, path, instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
	})

	checkout := filepath.Join(dir, "checkout")
	add := newReposAddCommand(&path, &reposOptions{})
	add.SetOut(&bytes.Buffer{})
	add.SetArgs([]string{"acme/repo", "--dir", checkout})
	if err := add.Execute(); err != nil {
		t.Fatalf("repos add: %v", err)
	}
	loaded, err := instances.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, ok := loaded.Instances[0].FindRepo("acme/repo")
	if !ok || repo.Dir != checkout {
		t.Fatalf("登记结果 = %+v ok=%v", repo, ok)
	}

	// 重复 add 同一 dir：幂等
	again := newReposAddCommand(&path, &reposOptions{})
	output := &bytes.Buffer{}
	again.SetOut(output)
	again.SetArgs([]string{"acme/repo", "--dir", checkout})
	if err := again.Execute(); err != nil {
		t.Fatalf("repos add 重复: %v", err)
	}
	if !strings.Contains(output.String(), "已在配置中") {
		t.Errorf("重复 add 输出 = %q", output.String())
	}

	// 更新 dir
	moved := filepath.Join(dir, "moved")
	update := newReposAddCommand(&path, &reposOptions{})
	update.SetOut(&bytes.Buffer{})
	update.SetArgs([]string{"acme/repo", "--dir", moved})
	if err := update.Execute(); err != nil {
		t.Fatalf("repos add 更新: %v", err)
	}
	loaded, err = instances.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ = loaded.Instances[0].FindRepo("acme/repo")
	if repo.Dir != moved {
		t.Fatalf("更新后 dir = %q, want %q", repo.Dir, moved)
	}
}

// 未指定 --host 时，按仓库已登记的实例定位；--dir 缺省时自动登记当前检出根目录。
func TestReposAddDetectsCheckout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	file := &instances.File{Instances: []instances.Instance{
		{Host: "https://a.example.com", Reviewer: instances.Account{Name: "ai"},
			Merger: instances.Account{Name: "merge"}, Repos: []instances.Repo{{Name: "acme/other"}}},
		{Host: "https://b.example.com", Reviewer: instances.Account{Name: "ai"},
			Merger: instances.Account{Name: "merge"}},
	}}
	if err := instances.Save(path, file); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	if output, err := exec.Command("git", "-C", work, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	if output, err := exec.Command("git", "-C", work, "remote", "add", "origin",
		"https://b.example.com/acme/repo.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v %s", err, output)
	}
	t.Chdir(work)

	add := newReposAddCommand(&path, &reposOptions{})
	add.SetOut(&bytes.Buffer{})
	add.SetArgs([]string{"acme/repo"})
	if err := add.Execute(); err != nil {
		t.Fatalf("repos add: %v", err)
	}
	loaded, err := instances.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// remote 指向 b.example.com：登记到对应实例，并自动带上检出根目录
	instance, ok := findInstanceByHost(loaded, "https://b.example.com")
	if !ok {
		t.Fatal("缺少 b.example.com 实例")
	}
	repo, ok := instance.FindRepo("acme/repo")
	if !ok {
		t.Fatal("acme/repo 未登记")
	}
	want, ok := dispatcher.RepoRoot(work)
	if !ok {
		t.Fatal("无法解析检出根目录")
	}
	if repo.Dir != want {
		t.Fatalf("自动登记 dir = %q, want %q", repo.Dir, want)
	}
}

func TestReposRemoveAndErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	file := &instances.File{Instances: []instances.Instance{
		{Host: "https://a.example.com", Reviewer: instances.Account{Name: "ai"},
			Merger: instances.Account{Name: "merge"}, Repos: []instances.Repo{{Name: "acme/repo"}}},
		{Host: "https://b.example.com", Reviewer: instances.Account{Name: "ai"},
			Merger: instances.Account{Name: "merge"}},
	}}
	if err := instances.Save(path, file); err != nil {
		t.Fatal(err)
	}

	remove := newReposRemoveCommand(&path, &reposOptions{})
	remove.SetOut(&bytes.Buffer{})
	remove.SetArgs([]string{"acme/repo"})
	if err := remove.Execute(); err != nil {
		t.Fatalf("repos remove: %v", err)
	}
	loaded, err := instances.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Instances[0].FindRepo("acme/repo"); ok {
		t.Fatal("acme/repo 应已移除")
	}

	// 仓库不在任何平台：给出明确错误
	missing := newReposRemoveCommand(&path, &reposOptions{})
	missing.SetOut(&bytes.Buffer{})
	missing.SetArgs([]string{"acme/missing"})
	if err := missing.Execute(); err == nil || !strings.Contains(err.Error(), "不在任何平台") {
		t.Fatalf("err = %v, want 不在任何平台", err)
	}

	// 多平台且未指定 --host：要求显式指定
	add := newReposAddCommand(&path, &reposOptions{})
	add.SetOut(&bytes.Buffer{})
	add.SetArgs([]string{"acme/new"})
	if err := add.Execute(); err == nil || !strings.Contains(err.Error(), "--host") {
		t.Fatalf("err = %v, want 提示 --host", err)
	}

	// 平台不存在：提示先 login
	unknown := newReposAddCommand(&path, &reposOptions{Host: "https://c.example.com"})
	unknown.SetOut(&bytes.Buffer{})
	unknown.SetArgs([]string{"acme/new"})
	if err := unknown.Execute(); err == nil || !strings.Contains(err.Error(), "assistant login") {
		t.Fatalf("err = %v, want 提示 assistant login", err)
	}
}

func TestReposList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	file := &instances.File{Instances: []instances.Instance{
		{Host: "https://a.example.com", Reviewer: instances.Account{Name: "ai"},
			Merger: instances.Account{Name: "merge"},
			Repos:  []instances.Repo{{Name: "acme/repo", Dir: "/tmp/repo", MergerToken: "mt"}}},
		{Host: "https://b.example.com", Reviewer: instances.Account{Name: "ai"},
			Merger: instances.Account{Name: "merge"}},
	}}
	if err := instances.Save(path, file); err != nil {
		t.Fatal(err)
	}

	output := &bytes.Buffer{}
	list := newReposListCommand(&path, &reposOptions{})
	list.SetOut(output)
	if err := list.Execute(); err != nil {
		t.Fatalf("repos list: %v", err)
	}
	text := output.String()
	for _, want := range []string{"acme/repo", "dir=/tmp/repo", "merger_token=yes", "未登记仓库", "共 1 个仓库"} {
		if !strings.Contains(text, want) {
			t.Errorf("repos list 输出缺少 %q：%s", want, text)
		}
	}

	// --host 过滤
	output.Reset()
	filtered := newReposListCommand(&path, &reposOptions{Host: "https://a.example.com"})
	filtered.SetOut(output)
	if err := filtered.Execute(); err != nil {
		t.Fatalf("repos list --host: %v", err)
	}
	if strings.Contains(output.String(), "b.example.com") {
		t.Errorf("--host 过滤失败：%s", output.String())
	}
}

func TestReposAddWithoutConfig(t *testing.T) {
	t.Setenv("ASSISTANT_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	empty := ""
	add := newReposAddCommand(&empty, &reposOptions{})
	add.SetOut(&bytes.Buffer{})
	add.SetArgs([]string{"acme/repo"})
	if err := add.Execute(); err == nil || !strings.Contains(err.Error(), "assistant login") {
		t.Fatalf("err = %v, want 提示 assistant login", err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "Cosmic-Developers-Union")); !os.IsNotExist(err) {
		t.Fatalf("不应创建配置文件：%v", err)
	}
}
