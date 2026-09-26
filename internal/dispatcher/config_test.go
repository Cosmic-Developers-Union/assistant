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
	if config.Reviewer != "ai" || config.ClaudeBin != "claude" || config.Concurrency != DefaultConcurrency {
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
	if config.Concurrency != DefaultConcurrency {
		t.Errorf("Concurrency = %d, want %d", config.Concurrency, DefaultConcurrency)
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

// ParseDuration 的整数溢出出口：正则收下超长数字串，但 ParseInt 会在 int64
// 溢出处失败——必须回落 false 而不是截断成某个错误的正数时长。
func TestParseDurationRejectsOverflow(t *testing.T) {
	if got, ok := ParseDuration("99999999999999999999"); ok {
		t.Errorf("ParseDuration(溢出数字串) = %v, true; want false", got)
	}
}

// ParseRepository 的格式出口：空 owner / 空 name / name 里再带斜杠（gitea
// 仓库路径只有一段 owner 与一段 name）一律拒绝，不静默截断。
func TestParseRepositoryRejectsMalformed(t *testing.T) {
	for _, raw := range []string{"", "owner", "owner/", "/repo", "a/b/c"} {
		if repository, err := ParseRepository(raw); err == nil {
			t.Errorf("ParseRepository(%q) = %+v, nil; want 报错", raw, repository)
		}
	}
}

// ResolveConfig 的两条独立出口：同步镜像的布尔标志坏值（DISPATCH_SYNC_MIRROR
// 只认 1/true，其余按 false 收敛而非报错）与令牌只从 flags/env 取，二者都
// 不该被 remote 自动检测悄悄补齐。
func TestResolveConfigSyncMirrorEnvAndSessionDirFailure(t *testing.T) {
	clearDispatchEnv(t)
	t.Setenv("GITEA_HOST", "http://h")
	t.Setenv("GITEA_REPOSITORY", "o/r")
	t.Setenv("GITEA_ACCESS_TOKEN", "t")

	// 坏值按 false 收敛（显式 false 与未给出等价），不是配置错误
	t.Setenv("DISPATCH_SYNC_MIRROR", "yes")
	config, err := ResolveConfig(Flags{}, t.TempDir(), os.Getenv, noRemote)
	if err != nil {
		t.Fatalf("ResolveConfig() error = %v", err)
	}
	if config.SyncMirror {
		t.Errorf("SyncMirror = true, want false（非 1/true 即收敛为 false）")
	}
	t.Setenv("DISPATCH_SYNC_MIRROR", "true")
	if config, err = ResolveConfig(Flags{}, t.TempDir(), os.Getenv, noRemote); err != nil || !config.SyncMirror {
		t.Errorf("SyncMirror = %v/%v, want true", config.SyncMirror, err)
	}

	// 会话配置根解析失败：ClaudeDir 锚定当前 cwd（不是 HOME），把 cwd 换到
	// 已删除的目录上，os.Getwd 必失败
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	doomed, err := os.MkdirTemp("", "dispatcher-cwd-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(doomed); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.RemoveAll(doomed); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveConfig(Flags{}, t.TempDir(), os.Getenv, noRemote); err == nil ||
		!strings.Contains(err.Error(), "解析会话配置根") {
		t.Errorf("error = %v, want 解析会话配置根失败", err)
	}
}

// ResolveConfig 的既有失败出口：host / repo 配置齐全但仓库名不合规时，
// ParseRepository 的错误必须透出（而不是当成缺仓库）。
func TestResolveConfigRejectsMalformedRepository(t *testing.T) {
	clearDispatchEnv(t)
	t.Setenv("GITEA_HOST", "http://h")
	t.Setenv("GITEA_REPOSITORY", "justowner")
	t.Setenv("GITEA_ACCESS_TOKEN", "t")
	if _, err := ResolveConfig(Flags{}, t.TempDir(), os.Getenv, noRemote); err == nil ||
		!strings.Contains(err.Error(), "owner/name") {
		t.Errorf("error = %v, want owner/name 格式错误", err)
	}
}
