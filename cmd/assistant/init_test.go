package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/instances"

	"github.com/spf13/cobra"
)

func TestMergeRepoIntoInstance(t *testing.T) {
	original := instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
		Repos:    []instances.Repo{{Name: "acme/one", Dir: "/srv/one"}},
	}
	updated := instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
		Repos:    []instances.Repo{{Name: "acme/two"}},
	}
	merged := mergeRepoIntoInstance(original, updated)
	for _, want := range []struct{ name, dir string }{
		{"acme/one", "/srv/one"},
		{"acme/two", ""},
	} {
		repo, ok := merged.FindRepo(want.name)
		if !ok || repo.Dir != want.dir {
			t.Errorf("repo %s = %+v, want dir %q", want.name, repo, want.dir)
		}
	}

	// 已登记仓库：条目被更新而不是重复
	updated.Repos = []instances.Repo{{Name: "acme/one", Dir: "/srv/one-v2"}}
	merged = mergeRepoIntoInstance(original, updated)
	if len(merged.Repos) != 1 {
		t.Fatalf("重复合并应为 1 个仓库：%+v", merged.Repos)
	}
	if repo, _ := merged.FindRepo("acme/one"); repo.Dir != "/srv/one-v2" {
		t.Errorf("repo = %+v, want 更新后的 dir", repo)
	}
}

func TestResolveRepoSetupTargetUsesPlatformByHost(t *testing.T) {
	dir := gitRemoteFixture(t)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := instances.Save(configPath, &instances.File{Instances: []instances.Instance{{
		Host: "http://gitea.example.com:3000",
	}}}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	probe := func(host string) bool { return host == "http://gitea.example.com:3000" }
	target, err := resolveRepoSetupTargetWithProbe(
		context.Background(), &cobra.Command{}, configPath, nil, probe)
	if err != nil {
		t.Fatalf("resolveRepoSetupTarget() error = %v", err)
	}
	if target.FullName != "acme/repo" || target.Host != "http://gitea.example.com:3000" || target.Path != configPath {
		t.Fatalf("target = %+v", target)
	}
	if target.Instance.Host != "http://gitea.example.com:3000" {
		t.Errorf("instance = %+v", target.Instance)
	}
}

func TestResolveRepoSetupTargetRequiresConfig(t *testing.T) {
	t.Setenv("ASSISTANT_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dir := gitRemoteFixture(t)
	t.Chdir(dir)
	_, err := resolveRepoSetupTarget(context.Background(), &cobra.Command{}, "", []string{"acme/repo"})
	if err == nil || !strings.Contains(err.Error(), "config.json") {
		t.Fatalf("error = %v, want 缺少 config.json", err)
	}
}
