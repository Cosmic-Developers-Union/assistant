package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"assistant/internal/status"
)

func testConfig(overrides ...func(*Config)) Config {
	config := Config{
		Host:           "http://gitea.test",
		AccessToken:    "token",
		Repository:     status.Repository{Owner: "owner", Name: "repo"},
		Interval:       time.Second,
		SessionTimeout: time.Minute,
		LogDir:         "logs",
		WorktreeRoot:   "worktrees",
		LockFile:       "dispatcher.lock",
		BaseBranch:     "main",
		SyncMirror:     false,
		Concurrency:    1,
		Model:          "",
		Reviewer:       "ai",
		ClaudeBin:      "claude",
	}
	for _, override := range overrides {
		override(&config)
	}
	return config
}

// fakeAPI 是 dispatcher.API 的测试桩：各方法返回预置结果并按调用计数。
type fakeAPI struct {
	pulls   []status.Issue
	issues  []status.Issue
	pull    status.PullRequest
	pullErr error
	reviews []status.Review
	labels  []status.Label
	// 非空时优先作为标签错误
	labelsErr error
	login     string
	loginErr  error
	listCalls atomic.Int32
}

func (f *fakeAPI) ListReviewPullRequests(context.Context, status.Repository) ([]status.Issue, error) {
	f.listCalls.Add(1)
	return f.pulls, nil
}

func (f *fakeAPI) ListTriageIssues(context.Context, status.Repository) ([]status.Issue, error) {
	f.listCalls.Add(1)
	return f.issues, nil
}

func (f *fakeAPI) GetPullRequest(context.Context, status.Repository, int64) (status.PullRequest, error) {
	return f.pull, f.pullErr
}

func (f *fakeAPI) ListPullReviews(context.Context, status.Repository, int64) ([]status.Review, error) {
	return f.reviews, nil
}

func (f *fakeAPI) GetIssueLabels(context.Context, status.Repository, int64) ([]status.Label, error) {
	return f.labels, f.labelsErr
}

func (f *fakeAPI) AuthenticatedUser(context.Context) (string, error) {
	return f.login, f.loginErr
}

// fakeClaude 写入一个可执行的 shebang 脚本并返回其路径。
func fakeClaude(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-claude.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}
