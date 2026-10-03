package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreePinsHeadAndTrustedBaseline(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("需要 Git")
	}
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	bare := filepath.Join(root, "remote", "acme", "repo.git")
	_ = os.MkdirAll(seed, 0o700)
	_ = os.MkdirAll(filepath.Dir(bare), 0o700)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	global := filepath.Join(root, "gitconfig")
	_ = os.WriteFile(global, []byte(fmt.Sprintf("[url \"%s/\"]\n  insteadOf = https://site/\n", filepath.Join(root, "remote"))), 0o600)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	runGit := func(dir string, args ...string) string {
		t.Helper()
		value, err := git(t.Context(), dir, nil, args...)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	runGit(root, "init", "--bare", bare)
	runGit(seed, "init", "-b", "main")
	rules := filepath.Join(seed, ".claude", "settings.json")
	_ = os.MkdirAll(filepath.Dir(rules), 0o700)
	_ = os.WriteFile(rules, []byte("trusted"), 0o600)
	runGit(seed, "add", ".")
	runGit(seed, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "基线")
	runGit(seed, "remote", "add", "origin", bare)
	runGit(seed, "push", "origin", "main")
	runGit(bare, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit(seed, "checkout", "-b", "pr")
	_ = os.WriteFile(rules, []byte("untrusted"), 0o600)
	runGit(seed, "add", ".")
	runGit(seed, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "PR")
	head := runGit(seed, "rev-parse", "HEAD")
	runGit(seed, "push", "origin", "HEAD:refs/pull/1/head")
	ev := testEvent()
	ev.Host = "https://site"
	ev.Ref = "refs/pull/1/head"
	ev.Head = head
	workspace := &WorktreeWorkspace{Root: filepath.Join(root, "data"), Connect: Connect{URL: "https://site", Token: "fake"}, Spec: WorkspaceSpec{Repo: "{{event.repo}}", Ref: "{{event.pr.ref}}"}}
	dir, cleanup, err := workspace.Prepare(t.Context(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if got := runGit(dir, "rev-parse", "HEAD"); got != head {
		t.Fatal("未钉住 head", got)
	}
	data, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if string(data) != "trusted" {
		t.Fatal("采用了 PR 自带的评审规则")
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("worktree 未清理")
	}
	ev.Head = "old"
	if _, _, err := workspace.Prepare(t.Context(), ev); err == nil || !strings.Contains(err.Error(), "head 已变化") {
		t.Fatal("head 漂移仍开始评审", err)
	}
	ev.Kind = "triage"
	ev.Title = "Issue"
	ev.Head = ""
	ev.Base = "main"
	workspace.Spec.Ref = ""
	dir, cleanup, err = workspace.Prepare(t.Context(), ev)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := workspace.Prepare(cancelled, ev); err == nil {
		t.Fatal("取消仍执行 Git")
	}
	// 相同克隆来自不同 bot 时必须共享锁。
	if workspaceLock("root/site") != workspaceLock("root/site") {
		t.Fatal("Git 锁未共享")
	}
}
