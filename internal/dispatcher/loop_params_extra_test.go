package dispatcher

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// TestRunLoopVerboseBannerReportsModelAndMirror 固定「verbose 启动横幅透出的
// 运行参数」这条运维契约：会话用哪个模型、基线是否开启镜像同步，都必须能在
// 进程起来的瞬间从日志读出来。参数没被 cfg 正确展开时（例如模型白填、镜像
// 开关被漏读），操作者要在启动阶段就看见，而不是等会话跑歪了回头查配置。
//
// 这两行由 config.Model 与 config.SyncMirror 单独拼装，是本用例要钉住的分支：
// 默认测试配置下 Model 为空、SyncMirror 为假，两条分支都不会被走到。
func TestRunLoopVerboseBannerReportsModelAndMirror(t *testing.T) {
	harness := newLoopHarness(t, func(c *Config) {
		c.Model = "claude-sonnet-test"
		c.SyncMirror = true
	})

	var mutex sync.Mutex
	var lines []string
	harness.deps.Log = func(string) {} // 默认级日志不是本用例的观察对象
	harness.deps.LogVerbose = func(line string) {
		mutex.Lock()
		defer mutex.Unlock()
		lines = append(lines, line)
	}

	// 等两条 verbose 横幅都写进缓冲后再取消：它们正是本用例的断言对象。
	// 「已退出主循环」那行是 RunLoop 返回前才打的，等它等于等自己。
	cancelWhen(t, harness.cancel, func() bool {
		mutex.Lock()
		defer mutex.Unlock()
		return len(lines) >= 3
	})

	if err := RunLoop(harness.ctx, harness.deps); err != nil {
		t.Fatalf("RunLoop() error = %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "model=claude-sonnet-test") {
		t.Errorf("横幅未透出配置的模型名：\n%s", joined)
	}
	if !strings.Contains(joined, "镜像同步开") {
		t.Errorf("开启镜像同步时横幅应显示「开」：\n%s", joined)
	}
}

// TestRunLoopDetectorTickerRedetects 固定「空闲轮询由 ticker 驱动」这条常驻
// 语义：RunLoop 不能只在启动时探一次待办，否则驻留期间新开的 PR 永远进不了
// 队列。这里让首轮检测返回空、第二轮才给出待办——只有主 select 的 ticker
// 分支真的被走到，待办才会被派发、会话才会跑起来。
func TestRunLoopDetectorTickerRedetects(t *testing.T) {
	harness := newLoopHarness(t, func(c *Config) {
		// 缩短轮询间隔，让「第二轮检测」在一次测试的时限内到达
		c.Interval = 10 * time.Millisecond
	})

	pull := status.PullRequest{Index: 7, Title: "待办第二次才出现", HeadSHA: "sha7"}
	harness.api.requestedPulls = []status.PullRequest{pull}
	// 首轮检测返回空清单：ListTriageIssues 空、请求通道也空，让第一轮无事可做
	harness.api.issues = nil
	harness.api.requestedPulls = nil

	// 用包装 API 在第 2 次检测起才放出待办：轮次由 ListPullRequestsRequestingReview
	// 的调用次数决定，不依赖 sleep 赌时序。
	rounds := &atomic.Int32{}
	// 换掉注入的检索：第二轮起才放出待办
	swapped := &secondRoundAPI{inner: harness.api, pull: pull, rounds: rounds}
	harness.deps.ListWork = func(ctx context.Context) ([]WorkItem, error) {
		return ListWork(ctx, swapped, mustRepository("owner/repo"), "ai")
	}

	// 第二轮检测到来即取消。这一刻是「ticker 分支被走到」的唯一可靠观察点：
	// 不能等 RunLoop 返回后再读计数——会话收尾会让用例立刻取消 ctx，读到的
	// 轮次与取消时序竞争。
	reached := make(chan int, 1)
	go func() {
		deadline := time.After(5 * time.Second)
		for {
			if got := rounds.Load(); got >= 2 {
				harness.cancel()
				reached <- int(got)
				return
			}
			select {
			case <-deadline:
				t.Errorf("5s 内检测只跑了 %d 轮，ticker 分支未生效", rounds.Load())
				harness.cancel()
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()

	if err := RunLoop(harness.ctx, harness.deps); err != nil {
		t.Fatalf("RunLoop() error = %v", err)
	}
	select {
	case got := <-reached:
		if got < 2 {
			t.Fatalf("检测只跑了 %d 轮，ticker 分支未生效", got)
		}
	default:
		t.Fatal("第二轮检测始终没有到来")
	}
}

// secondRoundAPI 让首轮检测返回空清单、第二轮才放出待办：单靠调整 fakeAPI 的
// 预置数据无法区分「第几轮」，而这里要钉的正是「ticker 触发了第二轮检测」。
type secondRoundAPI struct {
	// inner 复用既有桩的其余通道（Issue、review 等），只覆写请求通道
	inner  *pendingAPI
	pull   status.PullRequest
	rounds *atomic.Int32
}

// 其余方法直接转发给 inner，保持 pendingAPI 的既有行为
func (a *secondRoundAPI) ListTriageIssues(ctx context.Context, repo status.Repository) ([]status.Issue, error) {
	return a.inner.ListTriageIssues(ctx, repo)
}

func (a *secondRoundAPI) ListIssuesMentioning(ctx context.Context, repo status.Repository, user, issueType string) ([]status.Issue, error) {
	return a.inner.ListIssuesMentioning(ctx, repo, user, issueType)
}

func (a *secondRoundAPI) GetPullRequest(ctx context.Context, repo status.Repository, number int64) (status.PullRequest, error) {
	return a.inner.GetPullRequest(ctx, repo, number)
}

func (a *secondRoundAPI) ListPullReviews(ctx context.Context, repo status.Repository, number int64) ([]status.Review, error) {
	return a.inner.ListPullReviews(ctx, repo, number)
}

func (a *secondRoundAPI) GetIssueLabels(ctx context.Context, repo status.Repository, number int64) ([]status.Label, error) {
	return a.inner.GetIssueLabels(ctx, repo, number)
}

func (a *secondRoundAPI) AuthenticatedUser(ctx context.Context) (string, error) {
	return a.inner.AuthenticatedUser(ctx)
}

func (a *secondRoundAPI) ListPullRequestsRequestingReview(context.Context, status.Repository, string) ([]status.PullRequest, error) {
	if a.rounds.Add(1) < 2 {
		return nil, nil
	}
	return []status.PullRequest{a.pull}, nil
}
