package main

import (
	"os/exec"
	"testing"

	"assistant/internal/instances"
)

func gitRemoteFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "git@github.com:acme/repo.git"},
		{"remote", "add", "gitea", "http://gitea.example.com:3000/acme/repo.git"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
	}
	return dir
}

func TestResolveServerTargetPrefersConfig(t *testing.T) {
	dir := gitRemoteFixture(t)
	file := &instances.File{Instances: []instances.Instance{{
		Host:  "http://gitea.example.com:3000",
		Repos: []instances.Repo{{Name: "acme/repo"}},
	}}}
	host, fullName, fromConfig := resolveServerTargetWithProbe(file, "", dir, func(string) bool {
		t.Fatal("配置已命中，不应探测 remote")
		return false
	})
	if host != "http://gitea.example.com:3000" || fullName != "acme/repo" || !fromConfig {
		t.Fatalf("target = %q %q %v", host, fullName, fromConfig)
	}
}

func TestResolveServerTargetProbesRemotes(t *testing.T) {
	dir := gitRemoteFixture(t)
	probe := func(host string) bool { return host == "http://gitea.example.com:3000" }

	// 配置未命中：跳过 GitHub origin，探测选中 gitea remote
	host, fullName, fromConfig := resolveServerTargetWithProbe(nil, "", dir, probe)
	if host != "http://gitea.example.com:3000" || fullName != "acme/repo" || fromConfig {
		t.Fatalf("target = %q %q %v", host, fullName, fromConfig)
	}

	// --repo 指定且配置中没有：用探测到的 host
	file := &instances.File{Instances: []instances.Instance{{Host: "https://other.example.com", Repos: []instances.Repo{{Name: "acme/repo"}}}}}
	host, fullName, _ = resolveServerTargetWithProbe(file, "acme/other", dir, probe)
	if host != "http://gitea.example.com:3000" || fullName != "acme/other" {
		t.Fatalf("target = %q %q", host, fullName)
	}

	// 全不命中：跳过
	host, fullName, _ = resolveServerTargetWithProbe(nil, "", dir, func(string) bool { return false })
	if host != "" || fullName != "" {
		t.Fatalf("target = %q %q, want empty", host, fullName)
	}
}
