package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"assistant/internal/status"
)

// planDeps 构造 dry-run 用依赖：副作用函数一律「即失败」桩。
func planDeps(t *testing.T, api API, config Config, logs *[]string) Deps {
	t.Helper()
	deps := Deps{
		Config:      config,
		API:         api,
		RepoDir:     "/repo",
		Log:         func(line string) { *logs = append(*logs, line) },
		BuildPrompt: BuildPrompt,
		PrepareWorktree: func(int64) (string, error) {
			t.Fatal("dry-run 不得创建 worktree")
			return "", nil
		},
		RemoveWorktree: func(string) error {
			t.Fatal("dry-run 不得移除 worktree")
			return nil
		},
		RunSession: func(string, string, func(string)) SessionOutcome {
			t.Fatal("dry-run 不得启动会话")
			return SessionOutcome{}
		},
	}
	if config.SyncMirror {
		deps.SyncMirror = func() (string, error) { return "abc1234", nil }
	}
	return deps
}

func TestDryRunPassListsPlanWithoutSideEffects(t *testing.T) {
	var logs []string
	api := &fakeAPI{
		pulls:  []status.Issue{{Index: 67, Title: "fix: draft 状态", IsPull: true}},
		issues: []status.Issue{{Index: 66, Title: "ci: 分层重构"}},
	}
	deps := planDeps(t, api, testConfig(), &logs)
	if err := DryRunPass(context.Background(), deps); err != nil {
		t.Fatalf("DryRunPass() error = %v", err)
	}
	output := strings.Join(logs, "\n")
	worktreeDir := filepath.Join("worktrees", "pr-67")
	for _, want := range []string{
		"pull#67",
		"issue#66",
		"git fetch --quiet origin refs/pull/67/head",
		"git worktree add --quiet --detach " + worktreeDir,
		"cp -r /repo/.claude " + worktreeDir + "/.claude",
		"cd " + worktreeDir,
		"claude -p 'review pr #67",
		"--strict-mcp-config",
		"curl -s -H",
		"/pulls/67/reviews?limit=50",
		"head 漂移即本轮作废",
		"至多 2 个会话",
		"git worktree remove --force " + worktreeDir,
		"cwd=宿主检出根",
		"triage issue #66",
		"/issues/66/labels",
		"60000ms（1 分钟",
		"未创建 worktree、未启动会话、未提交 review",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, output)
		}
	}
}

func TestDryRunPassWithoutWork(t *testing.T) {
	var logs []string
	deps := planDeps(t, &fakeAPI{}, testConfig(), &logs)
	if err := DryRunPass(context.Background(), deps); err != nil {
		t.Fatalf("DryRunPass() error = %v", err)
	}
	if len(logs) != 1 || logs[0] != "当前无待办" {
		t.Errorf("logs = %v, want [当前无待办]", logs)
	}
}

func TestDryRunPassWithSyncMirror(t *testing.T) {
	var logs []string
	config := testConfig(func(config *Config) { config.SyncMirror = true })
	deps := planDeps(t, &fakeAPI{}, config, &logs)
	if err := DryRunPass(context.Background(), deps); err != nil {
		t.Fatalf("DryRunPass() error = %v", err)
	}
	output := strings.Join(logs, "\n")
	for _, want := range []string{"每轮先同步镜像（--sync-mirror）", "checkout -f -B main origin/main", "clean -fd"} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, output)
		}
	}

	var plain []string
	if err := DryRunPass(context.Background(), planDeps(t, &fakeAPI{}, testConfig(), &plain)); err != nil {
		t.Fatalf("DryRunPass() error = %v", err)
	}
	if strings.Contains(strings.Join(plain, "\n"), "同步镜像") {
		t.Errorf("plain dry-run should not mention mirror: %v", plain)
	}
}

func TestProcessItemRoutesProgressToConsoleAndLog(t *testing.T) {
	logDir := t.TempDir()
	var logs []string
	api := &fakeAPI{labels: []status.Label{}}
	deps := Deps{
		Config:      testConfig(func(config *Config) { config.LogDir = logDir }),
		API:         api,
		RepoDir:     "/repo",
		Log:         func(line string) { logs = append(logs, line) },
		BuildPrompt: BuildPrompt,
		PrepareWorktree: func(int64) (string, error) {
			t.Fatal("issue 待办不建 worktree")
			return "", nil
		},
		RemoveWorktree: func(string) error {
			t.Fatal("issue 待办不清理 worktree")
			return nil
		},
		RunSession: func(_ string, _ string, onProgress func(string)) SessionOutcome {
			onProgress("session=s-1 model=test")
			onProgress("开始审查")
			onProgress("🔧 Read")
			onProgress("🔧 Bash")
			return SessionOutcome{
				Subtype:    "success",
				NumTurns:   4,
				CostUSD:    0.1,
				DurationMS: 100,
				SessionID:  "s-1",
				Result:     "done",
				Errors:     []string{},
			}
		},
	}
	ProcessItem(context.Background(), deps, WorkItem{Kind: KindIssue, Number: 66, Title: "ci: 分层重构"})

	consoleOutput := strings.Join(logs, "\n")
	for _, want := range []string{
		"issue#66 session=s-1 model=test",
		"issue#66 🔧 #1 Read",
		"issue#66 🔧 #2 Bash",
		"issue#66 完成",
	} {
		if !strings.Contains(consoleOutput, want) {
			t.Errorf("console output missing %q:\n%s", want, consoleOutput)
		}
	}
	if strings.Contains(consoleOutput, "开始审查") {
		t.Errorf("assistant text should not reach console:\n%s", consoleOutput)
	}
	detail := readLogFile(t, logDir, "issue-66-")
	for _, want := range []string{
		"[progress] session=s-1 model=test",
		"[progress] 开始审查",
		"[progress] 🔧 Read",
		"[progress] 🔧 Bash",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail log missing %q:\n%s", want, detail)
		}
	}
}

func TestProcessItemHeadDriftInvalidatesWithoutRetry(t *testing.T) {
	logDir := t.TempDir()
	var logs []string
	sessions := 0
	api := &fakeAPI{pull: status.PullRequest{HeadSHA: "sha-new"}}
	deps := Deps{
		Config:          testConfig(func(config *Config) { config.LogDir = logDir }),
		API:             api,
		RepoDir:         "/repo",
		Log:             func(line string) { logs = append(logs, line) },
		BuildPrompt:     BuildPrompt,
		PrepareWorktree: func(int64) (string, error) { return "sha-old", nil },
		RemoveWorktree:  func(string) error { return nil },
		RunSession: func(string, string, func(string)) SessionOutcome {
			sessions++
			return SessionOutcome{
				Subtype:    "success",
				NumTurns:   3,
				CostUSD:    0.05,
				DurationMS: 50,
				SessionID:  "s-9",
				Result:     "reviewed stale head",
				Errors:     []string{},
			}
		},
	}
	ProcessItem(context.Background(), deps, WorkItem{Kind: KindPull, Number: 67, Title: "fix: something"})

	if sessions != 1 {
		t.Errorf("sessions = %d, want 1（漂移后不再烧第 2 个会话）", sessions)
	}
	output := strings.Join(logs, "\n")
	for _, want := range []string{
		"head 已推进 sha-old → sha-new",
		"本轮放行，下一轮以新 head 重开",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("console output missing %q:\n%s", want, output)
		}
	}
}

func TestAcquireLockCreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "run", "dispatcher.lock")
	if err := AcquireLock(lockFile, func(string) {}); err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}
	raw, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock = %q, want pid %d", got, os.Getpid())
	}
}

func TestAcquireLockTakesOverDeadPID(t *testing.T) {
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "dispatcher.lock")
	// PID 上限之外的数值在任何平台都不会存活
	if err := os.WriteFile(lockFile, []byte("2147483647\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs []string
	if err := AcquireLock(lockFile, func(line string) { logs = append(logs, line) }); err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}
	raw, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock = %q, want pid %d", got, os.Getpid())
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "接管残留锁") {
		t.Errorf("logs = %v, want 接管残留锁", logs)
	}
}

func TestAcquireLockRejectsLiveInstance(t *testing.T) {
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "dispatcher.lock")
	// PID 1 在 Linux 上恒存在且不属于当前进程：锁被存活实例持有，必须拒绝
	if err := os.WriteFile(lockFile, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := AcquireLock(lockFile, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "已在运行") {
		t.Errorf("AcquireLock() error = %v, want 已在运行", err)
	}
}

func TestReleaseLockRemovesFile(t *testing.T) {
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "dispatcher.lock")
	if err := AcquireLock(lockFile, func(string) {}); err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}
	ReleaseLock(lockFile)
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Errorf("lock file should be removed, err = %v", err)
	}
}

func readLogFile(t *testing.T, dir, prefix string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			return string(content)
		}
	}
	t.Fatalf("no log file with prefix %q in %s", prefix, dir)
	return ""
}
