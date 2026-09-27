package dispatcher

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/claude"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/gitea"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
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
	issues  []status.Issue
	pull    status.PullRequest
	pullErr error
	reviews []status.Review
	// reviewsErr 非空时作为 review 列表失败（完成判定的错误路径）
	reviewsErr error
	labels     []status.Label
	// 非空时优先作为标签错误
	labelsErr error
	login     string
	loginErr  error
	listCalls atomic.Int32
	// mention 通道：mentioned_by 过滤的返回（mentionPulls / mentionIssues）
	mentionPulls  []status.Issue
	mentionIssues []status.Issue
	// mentionUser 记录 mention 通道的账号参数；两路并发调用，用原子量留痕
	mentionUser atomic.Pointer[string]
	// review 请求通道：requested_reviewers 命中且未在当前 head 上回应的 PR
	requestedPulls    []status.PullRequest
	requestedReviewer atomic.Pointer[string]
}

func (f *fakeAPI) ListIssuesMentioning(_ context.Context, _ status.Repository, user, issueType string) ([]status.Issue, error) {
	f.listCalls.Add(1)
	f.mentionUser.Store(&user)
	if issueType == "pulls" {
		return f.mentionPulls, nil
	}
	return f.mentionIssues, nil
}

func (f *fakeAPI) ListPullRequestsRequestingReview(_ context.Context, _ status.Repository, reviewer string) ([]status.PullRequest, error) {
	f.listCalls.Add(1)
	f.requestedReviewer.Store(&reviewer)
	return f.requestedPulls, nil
}

func (f *fakeAPI) ListTriageIssues(context.Context, status.Repository) ([]status.Issue, error) {
	f.listCalls.Add(1)
	return f.issues, nil
}

func (f *fakeAPI) GetPullRequest(context.Context, status.Repository, int64) (status.PullRequest, error) {
	return f.pull, f.pullErr
}

func (f *fakeAPI) ListPullReviews(context.Context, status.Repository, int64) ([]status.Review, error) {
	return f.reviews, f.reviewsErr
}

func (f *fakeAPI) GetIssueLabels(context.Context, status.Repository, int64) ([]status.Label, error) {
	return f.labels, f.labelsErr
}

func (f *fakeAPI) AuthenticatedUser(context.Context) (string, error) {
	return f.login, f.loginErr
}

// fakeRunner 是 claude.Runner 的测试替身：按脚本交付 stdout 行与退出错误，
// 并记录收到的 Spec，供「组装的 argv/env 对不对」这类断言使用。
//
// 为什么替代 fakeClaude 脚本：Spec 是纯数据，直接断言字符串切片比让 shell
// 把 argv 转存到文件再读回来更准（不必处理引号与空参数），也不会为一个断言
// 启动进程。
type fakeRunner struct {
	// lines 是逐行交付的 stream-json（原样交付，不追加换行）。
	lines []string
	// err 是 Run 的返回值；非空时仍先交付 lines（模拟「跑到一半失败」）。
	err error
	// specs 记录每次收到的 Spec（指针，取最后一条即可）。
	specs []claude.Spec
	// onRun 在交付前调用，可在此写盘（如归档目录、文本记录预置）。
	onRun func(spec claude.Spec)
}

func (f *fakeRunner) Run(_ context.Context, spec claude.Spec, onLine func([]byte)) error {
	f.specs = append(f.specs, spec)
	if f.onRun != nil {
		f.onRun(spec)
	}
	for _, line := range f.lines {
		if onLine != nil {
			onLine([]byte(line))
		}
	}
	return f.err
}

// lastSpec 返回最近一次 Run 收到的 Spec（未调用过则失败）。
func (f *fakeRunner) lastSpec(t *testing.T) claude.Spec {
	t.Helper()
	if len(f.specs) == 0 {
		t.Fatal("Runner 从未被调用")
	}
	return f.specs[len(f.specs)-1]
}

// useFakeRunner 注入假 Runner 并登记还原（测试内调用）。
func useFakeRunner(t *testing.T, runner claude.Runner) {
	t.Helper()
	restore := setSessionRunner(runner)
	t.Cleanup(restore)
}

// wireAPI 把一个 API 桩接到 Deps 的两个注入点上，模拟生产侧的装配
// （cmd/assistant 用 gitea 集成包填同样的两个字段，并做判定类型映射）。
//
// 这样测试走的是真实注入路径：Deps 上不再有 API 字段，平台能力一律经函数注入，
// 测试若绕过它去直接调 ListWork 就失去了对注入本身的覆盖。
func wireAPI(deps *Deps, api API, repository string, reviewer string) {
	deps.ListWork = func(ctx context.Context) ([]WorkItem, error) {
		return ListWork(ctx, api, mustRepository(repository), reviewer)
	}
	deps.Verify = func(ctx context.Context, item WorkItem, since time.Time, expectedHead string) (ItemVerdict, error) {
		return verifyForTest(ctx, api, repository, reviewer, item, since, expectedHead)
	}
}

func mustRepository(name string) status.Repository {
	repository, err := ParseRepository(name)
	if err != nil {
		panic(err)
	}
	return repository
}

// verifyForTest 是完成判定的测试侧映射：调 gitea 集成包并把它的判定类型折成
// 调度引擎的 ItemVerdict——与 cmd/assistant 装配处做的映射一致。
func verifyForTest(ctx context.Context, api API, repository, reviewer string, item WorkItem, since time.Time, expectedHead string) (ItemVerdict, error) {
	verdict, err := gitea.Verify(ctx, api, mustRepository(repository), reviewer, item.Kind, item.Number, since, expectedHead)
	if err != nil {
		return ItemVerdict{}, err
	}
	return ItemVerdict{
		Completed: verdict.Completed,
		HeadMoved: verdict.HeadMoved,
		Reason:    verdict.Reason,
	}, nil
}

// freshReviews 是一条「刚刚提交」的 reviewer review：完成判定的正例。
func freshReviews() []status.Review {
	return []status.Review{{
		User:      "ai",
		Submitted: time.Now(),
	}}
}

// since 是判定测试的会话起点时刻（判定「since 之后有新 review」要用）。
var since = time.Date(2026, 9, 6, 5, 0, 0, 0, time.UTC)
