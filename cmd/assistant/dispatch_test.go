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
	dir := t.TempDir()
	file := &instances.File{Instances: []instances.Instance{
		{Host: "https://gitea.aicler.com", AdminToken: "token-aicler"},
		{
			Host:       "https://gitea.mms.vincentge.top",
			AdminToken: "token-mms",
			Repos:      []instances.Repo{{Name: "Ge/assistant", Dir: dir}},
		},
	}}
	file.Normalize()
	path := filepath.Join(dir, "config.json")
	if err := instances.Save(path, file); err != nil {
		t.Fatalf("save config: %v", err)
	}

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
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	instance := instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: "ai", Token: "reviewer-token"},
		Merger:   instances.Account{Name: "merge"},
	}
	command := &cobra.Command{}
	target, err := resolveInstanceTarget(command, instance, instances.Repo{Name: "acme/repo"}, &dispatcherOptions{})
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
	dir := t.TempDir()
	instance := instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: "ai", Token: "reviewer-token"},
		Merger:   instances.Account{Name: "merge"},
	}
	command := &cobra.Command{}
	target, err := resolveInstanceTarget(command, instance, instances.Repo{Name: "acme/repo", Dir: dir}, &dispatcherOptions{})
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
	dir := t.TempDir()
	instance := instances.Instance{
		Host:     "https://gitea.example.com",
		Reviewer: instances.Account{Name: "ai", Token: "reviewer-token"},
		Merger:   instances.Account{Name: "merge"},
	}
	command := &cobra.Command{}
	target, err := resolveInstanceTarget(
		command, instance, instances.Repo{Name: "acme/repo"}, &dispatcherOptions{RepoDir: dir})
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
