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

	// --repo 未登记 + 唯一实例：用配置实例（login/config 表达平台身份）
	single := &instances.File{Instances: []instances.Instance{
		{Host: "https://other.example.com", Repos: []instances.Repo{{Name: "acme/repo"}}},
	}}
	host, fullName, fromConfig = resolveServerTargetWithProbe(single, "acme/other", dir, probe)
	if host != "https://other.example.com" || fullName != "acme/other" || !fromConfig {
		t.Fatalf("target = %q %q %v, want 唯一实例", host, fullName, fromConfig)
	}

	// --repo 未登记 + 多实例：配置歧义，退回 remote 探测
	multiple := &instances.File{Instances: []instances.Instance{
		{Host: "https://other.example.com", Repos: []instances.Repo{{Name: "acme/repo"}}},
		{Host: "https://third.example.com", Repos: []instances.Repo{{Name: "acme/third"}}},
	}}
	host, fullName, fromConfig = resolveServerTargetWithProbe(multiple, "acme/other", dir, probe)
	if host != "http://gitea.example.com:3000" || fullName != "acme/other" || fromConfig {
		t.Fatalf("target = %q %q %v, want remote 探测", host, fullName, fromConfig)
	}

	// 全不命中：跳过
	host, fullName, _ = resolveServerTargetWithProbe(multiple, "", dir, func(string) bool { return false })
	if host != "" || fullName != "" {
		t.Fatalf("target = %q %q, want empty", host, fullName)
	}
}

// login 只登记平台（repos[] 为空）时，doctor 仍应以该实例凭据检查当前仓库：
// remote host 与实例 host 匹配即可，不要求仓库已登记。
func TestResolveServerTargetMatchesInstanceByHostOnly(t *testing.T) {
	dir := gitRemoteFixture(t)
	file := &instances.File{Instances: []instances.Instance{{
		Host: "http://gitea.example.com:3000",
	}}}
	host, fullName, fromConfig := resolveServerTargetWithProbe(file, "", dir, func(string) bool {
		t.Fatal("host 已命中实例，不应探测")
		return false
	})
	if host != "http://gitea.example.com:3000" || fullName != "acme/repo" || !fromConfig {
		t.Fatalf("target = %q %q %v", host, fullName, fromConfig)
	}
}
