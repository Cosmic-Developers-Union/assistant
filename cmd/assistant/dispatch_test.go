package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"assistant/internal/instances"

	"github.com/spf13/cobra"
)

// 未 setup 的实例（无 repos）应跳过并提示，不阻断其他实例。
func TestResolveDispatchTargetsSkipsInstancesWithoutRepos(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	file := &instances.File{Instances: []instances.Instance{
		{Host: "https://gitea.aicler.com"},
		{
			Host:  "https://gitea.mms.vincentge.top",
			Repos: []instances.Repo{{Name: "Ge/assistant", Dir: dir}},
		},
	}}
	file.Normalize()
	path := filepath.Join(dir, "config.json")
	if err := instances.Save(path, file); err != nil {
		t.Fatalf("save config: %v", err)
	}

	withReviewCredentialFor(t, path, "https://gitea.mms.vincentge.top")

	stderr := &bytes.Buffer{}
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	command.SetErr(stderr)
	targets, err := resolveDispatchTargets(command, "", path, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("resolveDispatchTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].config.Repository.FullName() != "Ge/assistant" {
		t.Fatalf("targets = %+v, want 仅 Ge/assistant", targets)
	}
	if !strings.Contains(stderr.String(), "跳过 https://gitea.aicler.com") {
		t.Errorf("stderr = %q, 缺少跳过提示", stderr.String())
	}
}

// 未配置 repo.dir 的仓库使用受管克隆落点：检出、日志/锁都在数据目录（不随
// 当前目录漂移），worktree 固定在 /tmp。
func TestResolveInstanceTargetUsesManagedDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	withReviewCredential(t, "https://gitea.example.com")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	instance := instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
	}
	command := &cobra.Command{}
	target, err := resolveInstanceTarget(command, &instances.File{}, "", instance, instances.Repo{Name: "acme/repo"}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("resolveInstanceTarget: %v", err)
	}
	data := os.Getenv("XDG_DATA_HOME")
	repoDir := filepath.Join(data, "Cosmic-Developers-Union", "assistant", "repos", "gitea.example.com", "acme", "repo")
	stateDir := filepath.Join(data, "Cosmic-Developers-Union", "assistant", "state", "gitea.example.com", "acme", "repo")
	if !target.managed || target.repoDir != repoDir {
		t.Errorf("managed=%v repoDir=%q, want %q", target.managed, target.repoDir, repoDir)
	}
	if target.config.LogDir != filepath.Join(stateDir, "logs") {
		t.Errorf("LogDir = %q", target.config.LogDir)
	}
	if target.config.LockFile != filepath.Join(stateDir, "dispatcher.lock") {
		t.Errorf("LockFile = %q", target.config.LockFile)
	}
	wantWorktree := filepath.Join(os.TempDir(), "agent-dispatcher", "gitea.example.com-acme-repo", "worktrees")
	if target.config.WorktreeRoot != wantWorktree {
		t.Errorf("WorktreeRoot = %q, want %q", target.config.WorktreeRoot, wantWorktree)
	}
	if !target.config.SyncMirror {
		t.Error("受管克隆应恒开镜像同步")
	}
}

// 显式配置 dir 的共享检出保持原语义：日志/锁在检出内，不受管、不自动镜像。
func TestResolveInstanceTargetExplicitDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	withReviewCredential(t, "https://gitea.example.com")
	dir := t.TempDir()
	instance := instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
	}
	command := &cobra.Command{}
	target, err := resolveInstanceTarget(command, &instances.File{}, "", instance, instances.Repo{Name: "acme/repo", Dir: dir}, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("resolveInstanceTarget: %v", err)
	}
	if target.managed || target.repoDir != dir {
		t.Errorf("managed=%v repoDir=%q, want 非受管 %q", target.managed, target.repoDir, dir)
	}
	if target.config.LogDir != filepath.Join(dir, "logs") || target.config.LockFile != filepath.Join(dir, "dispatcher.lock") {
		t.Errorf("LogDir/LockFile = %q/%q", target.config.LogDir, target.config.LockFile)
	}
	if target.config.SyncMirror {
		t.Error("显式 dir 的共享检出不应默认镜像同步")
	}
}

// --repo-dir 是单目标显式覆盖：按共享检出处理，多目标时必须配合 --repo。
func TestRepoDirOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	withReviewCredential(t, "https://gitea.example.com")
	dir := t.TempDir()
	instance := instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
	}
	command := &cobra.Command{}
	target, err := resolveInstanceTarget(
		command, &instances.File{}, "", instance, instances.Repo{Name: "acme/repo"}, &dispatcherOptions{RepoDir: dir})
	if err != nil {
		t.Fatalf("resolveInstanceTarget: %v", err)
	}
	if target.managed || target.repoDir != dir {
		t.Errorf("--repo-dir 覆盖应是非受管检出：managed=%v dir=%q", target.managed, target.repoDir)
	}

	// 配置里两个仓库 + --repo-dir 且未用 --repo 收敛 → 报错
	configPath := filepath.Join(t.TempDir(), "config.json")
	file := &instances.File{Instances: []instances.Instance{{
		Host:     instance.Host,
		Reviewer: instance.Reviewer,
		Merger:   instance.Merger,
		Repos:    []instances.Repo{{Name: "acme/one"}, {Name: "acme/two"}},
	}}}
	file.Normalize()
	if err := instances.Save(configPath, file); err != nil {
		t.Fatal(err)
	}
	_, err = resolveDispatchTargets(command, "", configPath, &dispatcherOptions{RepoDir: dir})
	if err == nil || !strings.Contains(err.Error(), "--repo") {
		t.Fatalf("err = %v, want 提示用 --repo 限定", err)
	}
}

// 受管仓库未就绪（缺 install 产物）只跳过该仓库：其余照跑，daemon 不退出。
func TestPrepareManagedTargetsSkipsUnprovisioned(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	withReviewCredential(t, "https://gitea.example.com")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	stderr := &bytes.Buffer{}
	command.SetErr(stderr)

	targetFor := func(name string) dispatchTarget {
		instance := instances.Instance{
			Host:     "https://gitea.example.com",
			Reviewer: instances.Account{Name: "ai"},
			Merger:   instances.Account{Name: "merge"},
		}
		target, err := resolveInstanceTarget(command, &instances.File{}, "", instance, instances.Repo{Name: name}, &dispatcherOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return target
	}

	ready := targetFor("acme/ready")
	if err := os.MkdirAll(filepath.Join(ready.repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ready.repoDir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{".mcp.json", ".claude/settings.json"} {
		if err := os.WriteFile(filepath.Join(ready.repoDir, relative), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	missing := targetFor("acme/missing")
	if err := os.MkdirAll(filepath.Join(missing.repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	targets := prepareManagedTargets(command, []dispatchTarget{ready, missing})
	if targets[0].skipReason != "" {
		t.Errorf("已就绪仓库不应跳过：%s", targets[0].skipReason)
	}
	if targets[1].skipReason == "" || !strings.Contains(targets[1].skipReason, ".mcp.json") {
		t.Errorf("缺产物仓库应跳过：%q", targets[1].skipReason)
	}
	if !strings.Contains(stderr.String(), "跳过 acme/missing") || !strings.Contains(stderr.String(), "assistant install") {
		t.Errorf("日志缺少跳过指引：%s", stderr.String())
	}
	if readyList := readyTargets(targets); len(readyList) != 1 || readyList[0].repo.Name != "acme/ready" {
		t.Errorf("readyTargets = %+v", readyList)
	}
	if summary := skipSummary(targets); !strings.Contains(summary, "acme/missing") {
		t.Errorf("skipSummary = %q", summary)
	}
}

// provider 逐级回退写进运行配置（仅运行时覆盖）：repo > instance > 全局默认。
func TestResolveInstanceTargetProviderCascade(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	withReviewCredential(t, "https://gitea.example.com")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	command := &cobra.Command{}
	file := &instances.File{
		DefaultProvider: "global",
		Optimizations:   instances.Provider{Env: map[string]string{"OPTIMIZED": "1", "SCOPE": "optimized"}},
		Providers: map[string]instances.Provider{
			"global":   {Env: map[string]string{"SCOPE": "global"}},
			"instance": {Env: map[string]string{"SCOPE": "instance"}},
			"repo":     {Env: map[string]string{"SCOPE": "repo"}},
		},
	}
	instance := instances.Instance{
		Host:     "https://gitea.example.com",
		Provider: "instance",
		Reviewer: instances.Account{Name: "ai"},
		Merger:   instances.Account{Name: "merge"},
	}

	repo := instances.Repo{Name: "acme/repo", Provider: "repo"}
	target, err := resolveInstanceTarget(command, file, "", instance, repo, &dispatcherOptions{})
	if err != nil {
		t.Fatalf("resolveInstanceTarget: %v", err)
	}
	if target.config.ProviderName != "repo" || target.config.Provider.Env["SCOPE"] != "repo" {
		t.Errorf("repo 级 provider 未生效：%q %+v", target.config.ProviderName, target.config.Provider.Env)
	}
	if target.config.Provider.Env["OPTIMIZED"] != "1" {
		t.Errorf("全局优化点应打底：%+v", target.config.Provider.Env)
	}
	if target.config.Optimizations.Env["SCOPE"] != "optimized" {
		t.Errorf("全局优化点层未记录：%+v", target.config.Optimizations.Env)
	}

	repo.Provider = ""
	target, err = resolveInstanceTarget(command, file, "", instance, repo, &dispatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if target.config.ProviderName != "instance" || target.config.Provider.Env["SCOPE"] != "instance" {
		t.Errorf("instance 级 provider 未生效：%q %+v", target.config.ProviderName, target.config.Provider.Env)
	}

	instance.Provider = ""
	target, err = resolveInstanceTarget(command, file, "", instance, repo, &dispatcherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if target.config.ProviderName != "global" || target.config.Provider.Env["SCOPE"] != "global" {
		t.Errorf("全局 provider 未生效：%q %+v", target.config.ProviderName, target.config.Provider.Env)
	}

	// 引用了未定义的 provider 在配置校验期就会被拒绝
	file.DefaultProvider = "missing"
	file.Instances = []instances.Instance{instance}
	file.Weixin = &instances.Weixin{BotToken: "t"}
	file.Normalize()
	if err := file.Validate(); err == nil {
		t.Error("未定义的 provider 引用应报错")
	}
}
