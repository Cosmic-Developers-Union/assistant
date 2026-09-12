package dispatcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clearDispatchEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"GITEA_HOST", "GITEA_REPOSITORY", "GITEA_ACCESS_TOKEN",
		"DISPATCH_INTERVAL_MS", "DISPATCH_TIMEOUT_MS", "DISPATCH_LOG_DIR",
		"DISPATCH_WORKTREE_ROOT", "DISPATCH_LOCK_FILE", "DISPATCH_BASE_BRANCH",
		"DISPATCH_SYNC_MIRROR", "DISPATCH_CONCURRENCY", "DISPATCH_MODEL",
		"DISPATCH_REVIEWER", "DISPATCH_CLAUDE_BIN",
	} {
		t.Setenv(name, "")
	}
}

var noRemote = func() (GitRemote, bool) { return GitRemote{}, false }

var envRemote = func() (GitRemote, bool) {
	return GitRemote{Host: "http://from-remote:8418", Repository: "remote/repo"}, true
}

func TestParseDuration(t *testing.T) {
	valid := map[string]time.Duration{
		"30s":     30 * time.Second,
		"10m":     10 * time.Minute,
		"1h":      time.Hour,
		"2d":      48 * time.Hour,
		"1800000": 1800 * time.Second,
		"5ms":     5 * time.Millisecond,
		"  30s  ": 30 * time.Second,
	}
	for input, want := range valid {
		got, ok := ParseDuration(input)
		if !ok || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v, true", input, got, ok, want)
		}
	}
	for _, input := range []string{"abc", "1x", "-3s", "", "s"} {
		if got, ok := ParseDuration(input); ok {
			t.Errorf("ParseDuration(%q) = %v, true; want false", input, got)
		}
	}
}

func TestResolveConfigPriorityFlagsOverEnvOverRemote(t *testing.T) {
	clearDispatchEnv(t)
	t.Setenv("GITEA_HOST", "http://from-env:8418/")
	t.Setenv("GITEA_ACCESS_TOKEN", "env-token")
	repoDir := t.TempDir()

	config, err := ResolveConfig(
		Flags{Host: "http://from-flag:9", Timeout: "45m"},
		repoDir, os.Getenv, envRemote,
	)
	if err != nil {
		t.Fatalf("ResolveConfig() error = %v", err)
	}
	if config.Host != "http://from-flag:9" {
		t.Errorf("Host = %q, want flag value", config.Host)
	}
	if config.Repository.FullName() != "remote/repo" {
		t.Errorf("Repository = %q, want remote value", config.Repository.FullName())
	}
	if config.AccessToken != "env-token" {
		t.Errorf("AccessToken = %q, want env value", config.AccessToken)
	}
	if config.SessionTimeout != 45*time.Minute {
		t.Errorf("SessionTimeout = %v, want 45m", config.SessionTimeout)
	}
	if config.Interval != 30*time.Second {
		t.Errorf("Interval = %v, want default 30s", config.Interval)
	}

	// env 覆盖 remote，尾斜杠剥除
	config, err = ResolveConfig(Flags{}, repoDir, os.Getenv, envRemote)
	if err != nil {
		t.Fatalf("ResolveConfig() error = %v", err)
	}
	if config.Host != "http://from-env:8418" {
		t.Errorf("Host = %q, want env value without trailing slash", config.Host)
	}
	if config.Repository.FullName() != "remote/repo" {
		t.Errorf("Repository = %q, want remote value", config.Repository.FullName())
	}
}

func TestResolveConfigFallsBackToRemote(t *testing.T) {
	clearDispatchEnv(t)
	t.Setenv("GITEA_ACCESS_TOKEN", "t")
	config, err := ResolveConfig(Flags{}, t.TempDir(), os.Getenv, envRemote)
	if err != nil {
		t.Fatalf("ResolveConfig() error = %v", err)
	}
	if config.Host != "http://from-remote:8418" || config.Repository.FullName() != "remote/repo" {
		t.Errorf("config = %+v, want remote host/repository", config)
	}
}

func TestResolveConfigErrors(t *testing.T) {
	clearDispatchEnv(t)

	t.Setenv("GITEA_HOST", "http://h")
	t.Setenv("GITEA_REPOSITORY", "o/r")
	if _, err := ResolveConfig(Flags{}, t.TempDir(), os.Getenv, noRemote); err == nil || !strings.Contains(err.Error(), "令牌") {
		t.Errorf("missing token error = %v, want 令牌", err)
	}

	t.Setenv("GITEA_HOST", "")
	t.Setenv("GITEA_ACCESS_TOKEN", "t")
	if _, err := ResolveConfig(Flags{}, t.TempDir(), os.Getenv, noRemote); err == nil || !strings.Contains(err.Error(), "站点") {
		t.Errorf("missing host error = %v, want 站点", err)
	}

	t.Setenv("GITEA_HOST", "http://h")
	t.Setenv("GITEA_REPOSITORY", "")
	if _, err := ResolveConfig(Flags{}, t.TempDir(), os.Getenv, noRemote); err == nil || !strings.Contains(err.Error(), "仓库") {
		t.Errorf("missing repository error = %v, want 仓库", err)
	}
}

func TestResolveConfigRejectsBadDurations(t *testing.T) {
	clearDispatchEnv(t)
	t.Setenv("GITEA_HOST", "http://h")
	t.Setenv("GITEA_REPOSITORY", "o/r")
	t.Setenv("GITEA_ACCESS_TOKEN", "t")

	if _, err := ResolveConfig(Flags{Interval: "soon"}, t.TempDir(), os.Getenv, noRemote); err == nil || !strings.Contains(err.Error(), "轮询间隔") {
		t.Errorf("bad interval error = %v, want 轮询间隔", err)
	}
	if _, err := ResolveConfig(Flags{Timeout: "soon"}, t.TempDir(), os.Getenv, noRemote); err == nil || !strings.Contains(err.Error(), "会话超时") {
		t.Errorf("bad timeout error = %v, want 会话超时", err)
	}
}

func TestResolveConfigPathDefaultsAnchorToRepoDir(t *testing.T) {
	clearDispatchEnv(t)
	t.Setenv("GITEA_HOST", "http://h")
	t.Setenv("GITEA_REPOSITORY", "o/r")
	t.Setenv("GITEA_ACCESS_TOKEN", "t")
	repoDir := t.TempDir()

	config, err := ResolveConfig(Flags{}, repoDir, os.Getenv, noRemote)
	if err != nil {
		t.Fatalf("ResolveConfig() error = %v", err)
	}
	if config.LogDir != filepath.Join(repoDir, "logs") {
		t.Errorf("LogDir = %q, want repo dir logs", config.LogDir)
	}
	if config.LockFile != filepath.Join(repoDir, "dispatcher.lock") {
		t.Errorf("LockFile = %q, want repo dir lock", config.LockFile)
	}
	if config.WorktreeRoot != filepath.Join(os.TempDir(), "agent-dispatcher", "worktrees") {
		t.Errorf("WorktreeRoot = %q, want temp worktrees", config.WorktreeRoot)
	}
	if config.BaseBranch != "main" || config.SyncMirror {
		t.Errorf("BaseBranch/SyncMirror = %q/%v, want main/false", config.BaseBranch, config.SyncMirror)
	}
	if config.Reviewer != "ai" || config.ClaudeBin != "claude" || config.Concurrency != 1 {
		t.Errorf("defaults = %+v", config)
	}
}

func TestResolveConfigSyncMirrorAndBaseBranch(t *testing.T) {
	clearDispatchEnv(t)
	t.Setenv("GITEA_HOST", "http://h")
	t.Setenv("GITEA_REPOSITORY", "o/r")
	t.Setenv("GITEA_ACCESS_TOKEN", "t")
	t.Setenv("DISPATCH_SYNC_MIRROR", "1")

	config, err := ResolveConfig(Flags{}, t.TempDir(), os.Getenv, noRemote)
	if err != nil {
		t.Fatalf("ResolveConfig() error = %v", err)
	}
	if !config.SyncMirror {
		t.Error("SyncMirror = false, want true from env")
	}

	explicitFalse := false
	config, err = ResolveConfig(Flags{SyncMirror: &explicitFalse}, t.TempDir(), os.Getenv, noRemote)
	if err != nil {
		t.Fatalf("ResolveConfig() error = %v", err)
	}
	if config.SyncMirror {
		t.Error("SyncMirror = true, want explicit false to win")
	}

	config, err = ResolveConfig(Flags{BaseBranch: "stable"}, t.TempDir(), os.Getenv, noRemote)
	if err != nil {
		t.Fatalf("ResolveConfig() error = %v", err)
	}
	if config.BaseBranch != "stable" {
		t.Errorf("BaseBranch = %q, want stable", config.BaseBranch)
	}
}

func TestResolveConfigConcurrency(t *testing.T) {
	clearDispatchEnv(t)
	t.Setenv("GITEA_HOST", "http://h")
	t.Setenv("GITEA_REPOSITORY", "o/r")
	t.Setenv("GITEA_ACCESS_TOKEN", "t")

	config, err := ResolveConfig(Flags{}, t.TempDir(), os.Getenv, noRemote)
	if err != nil {
		t.Fatalf("ResolveConfig() error = %v", err)
	}
	if config.Concurrency != 1 {
		t.Errorf("Concurrency = %d, want 1", config.Concurrency)
	}

	config, err = ResolveConfig(Flags{Concurrency: "4"}, t.TempDir(), os.Getenv, noRemote)
	if err != nil {
		t.Fatalf("ResolveConfig() error = %v", err)
	}
	if config.Concurrency != 4 {
		t.Errorf("Concurrency = %d, want 4", config.Concurrency)
	}

	for _, bad := range []string{"0", "2.5"} {
		if _, err := ResolveConfig(Flags{Concurrency: bad}, t.TempDir(), os.Getenv, noRemote); err == nil || !strings.Contains(err.Error(), "正整数") {
			t.Errorf("Concurrency %q error = %v, want 正整数", bad, err)
		}
	}
}
