package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/claude"
	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// errAPI 是可按通道注入失败的 API 桩：ListWork 的四路并发检索里任一路失败都
// 必须冒泡成「本轮检测失败」，否则一条待办会带着半份清单被派发出去。
type errAPI struct {
	fakeAPI
	mentionErr   error
	requestedErr error
	triageErr    error
}

func (e *errAPI) ListIssuesMentioning(context.Context, status.Repository, string, string) ([]status.Issue, error) {
	return nil, e.mentionErr
}

func (e *errAPI) ListPullRequestsRequestingReview(context.Context, status.Repository, string) ([]status.PullRequest, error) {
	return nil, e.requestedErr
}

func (e *errAPI) ListTriageIssues(context.Context, status.Repository) ([]status.Issue, error) {
	return nil, e.triageErr
}

// TestListWorkPropagatesEachChannelFailure 钉住四路检索的错误出口：mention /
// review 请求 / 分诊标签任一路失败，ListWork 都必须返回该错误而不是残缺清单
// （残缺清单会被当成「没有待办」，请求静默丢失）。
func TestListWorkPropagatesEachChannelFailure(t *testing.T) {
	repository := status.Repository{Owner: "owner", Name: "repo"}
	cases := []struct {
		name string
		api  API
	}{
		{"mention 通道失败", &errAPI{mentionErr: errors.New("mention 挂了")}},
		{"review 请求通道失败", &errAPI{requestedErr: errors.New("requested 挂了")}},
		{"分诊标签通道失败", &errAPI{triageErr: errors.New("triage 挂了")}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			work, err := ListWork(t.Context(), testCase.api, repository, "ai")
			if err == nil {
				t.Fatalf("ListWork() error = nil, want 通道错误；work = %v", work)
			}
			if work != nil {
				t.Errorf("失败时不应返回残缺清单：%v", work)
			}
		})
	}
}

// pendingAPI 撑起 fakeAPI 里默认返回 nil 的两路 mention 通道。fakeAPI 的
// mentionPulls / mentionIssues 为 nil 时 ListWork 在 workItems 里遍历空切片，
// 通道侧不会报错；但 ListWork 会另外查一次本 PR 的当前 head（确认语义），
// 这里把 pull 一并给出，让派发用的是真实构造出的 WorkItem。
type pendingAPI struct {
	fakeAPI
	pulls []status.Issue
}

func (p *pendingAPI) ListIssuesMentioning(_ context.Context, _ status.Repository, _ string, issueType string) ([]status.Issue, error) {
	if issueType == "pulls" {
		return p.pulls, nil
	}
	return nil, nil
}

// TestRunAllEmptyListReturnsNil 钉住 RunAll 的空输入出口：没有仓库可跑时直接
// 返回 nil（main 允许配置零个仓库，不能因此报错）。
func TestRunAllEmptyListReturnsNil(t *testing.T) {
	if err := RunAll(t.Context(), nil); err != nil {
		t.Errorf("RunAll(nil) = %v, want nil", err)
	}
	if err := RunAll(t.Context(), []Deps{}); err != nil {
		t.Errorf("RunAll(空切片) = %v, want nil", err)
	}
}

// runLoopDeps 组装一份「能真跑一轮 RunLoop」的 Deps：配置落点全在临时目录，
// API 返回一条 review 请求，会话由 RunSession 桩完成，退出靠 cancel 在首个会话
// 回执（OnFinish）之后触发。
type loopHarness struct {
	deps Deps
	api  *pendingAPI
	// ctx 是传给 RunLoop 的派生上下文，cancel 取消它。两者必须配对使用。
	ctx      context.Context
	cancel   context.CancelFunc
	sessions atomic.Int32
	queued   atomic.Int32
	started  atomic.Int32
	finished atomic.Int32
	// lockPath 是配置里的锁文件位置；RunLoop 返回后可再次加锁即说明已释放
	lockPath string
	logDir   string
}

func newLoopHarness(t *testing.T, options ...func(*Config)) *loopHarness {
	t.Helper()
	root := t.TempDir()
	config := testConfig(func(c *Config) {
		c.LockFile = filepath.Join(root, "dispatcher.lock")
		c.LogDir = filepath.Join(root, "logs")
		c.WorktreeRoot = filepath.Join(root, "worktrees")
		c.ClaudeBin = "claude"
		for _, option := range options {
			option(c)
		}
	})
	api := &pendingAPI{fakeAPI: fakeAPI{login: "ai"}}
	harness := &loopHarness{api: api, lockPath: config.LockFile, logDir: config.LogDir}

	// 清理上一次被中断的测试留下的锁。测试进程被 go test 的 panic/超时直接杀掉
	// 时，RunLoop 末尾的 defer ReleaseLock 不会执行，锁连同 TempDir 一起留在
	// /tmp。本用例的名称与文件位置固定，于是「上一轮残留的锁」会被 AcquireLock
	// 按死 PID 接管——日志写进「接管残留锁…」，横幅与「已退出主循环」都不会再
	// 出现；若残留锁恰好记着一个存活的 PID，AcquireLock 还会直接报错返回。
	// 这里在装配阶段先删掉，让「锁的接管/释放」只由用例自身的断言负责。
	_ = os.Remove(config.LockFile)

	// ctx 的取消是 RunLoop 唯一的退出信号：真实进程收到 SIGINT 才会取消，测试里
	// 靠 harness.cancel 触发。因此派生出的 ctx 必须**本身**传给 RunLoop（调用点
	// 一律写 RunLoop(harness.ctx, ...)）。
	//
	// 不要改成「保存 cancel、RunLoop 传 t.Context()」：Go 的 context 取消是单向
	// 向下传播的，取消一个子上下文对它的父或兄弟上下文没有任何影响。那样写的话
	// harness.cancel() 取消的是一个没人等它的上下文，RunLoop 永远收不到信号，
	// 测试会挂到超时，整个包卡死。
	ctx, cancel := context.WithCancel(t.Context())
	harness.ctx = ctx
	harness.cancel = cancel

	harness.deps = Deps{
		Config: config,
		// 平台能力经注入（生产侧由 gitea 集成包填同样两个字段）
		ListWork: func(ctx context.Context) ([]WorkItem, error) {
			return ListWork(ctx, api, mustRepository("owner/repo"), "ai")
		},
		Verify: func(ctx context.Context, item WorkItem, since time.Time, expectedHead string) (ItemVerdict, error) {
			return verifyForTest(ctx, api, "owner/repo", "ai", item, since, expectedHead)
		},
		Log: func(string) {},
		CurrentLogin: func(context.Context) (string, error) {
			return api.AuthenticatedUser(context.Background())
		},
		OnQueue: func(items []WorkItem) {
			harness.queued.Store(int32(len(items)))
		},
		OnStart: func(WorkItem) bool {
			harness.started.Add(1)
			return true
		},
		OnFinish: func(WorkItem, SessionOutcome) {
			harness.finished.Add(1)
		},
		PrepareWorktree: func(int64) (string, error) { return "sha", nil },
		PrepareIssue:    func(string) (string, error) { return filepath.Join(root, "issue-wt"), nil },
		RemoveWorktree:  func(string) error { return nil },
		SyncMirror:      func() (string, error) { return "sha-baseline", nil },
		BuildPrompt: func(kind string, number int64, extra PromptContext) string {
			return fmt.Sprintf("review %s#%d", kind, number)
		},
	}
	harness.deps.RunSession = func(request SessionRequest) SessionOutcome {
		harness.sessions.Add(1)
		return SessionOutcome{Subtype: "success"}
	}
	harness.deps.FollowUpMessages = func(context.Context, WorkItem, time.Time) ([]string, error) {
		return nil, nil
	}
	return harness
}

// cancelAtFirstDetectLog 让 RunLoop 走完一轮「无待办」检测后退出：退出信号只有
// ctx.Done，测试里没有真的 Ctrl-C。首轮 detect 一定会同步打日志——成功时是
// 启动横幅、失败时是「检测失败」——所以「日志出现即取消」在时间上确定可达，
// 不依赖 ticker 轮询，也不 sleep 赌时序。
func cancelAtFirstDetectLog(t *testing.T, cancel context.CancelFunc, activity func() int) {
	t.Helper()
	cancelWhen(t, cancel, func() bool { return activity() > 0 })
}

// cancelAtFirstFinish 让 RunLoop 在首个会话回执之后才取消：这是「派发—会话—
// 收尾—退出—释放锁」这条完整路径唯一可靠的触发点。OnFinish 在 runSessionRecorded
// 里于 RunSession 之后同步执行，此刻 worker 已经离开 ProcessItem 的会话段，取消
// 不会再打断无缓冲 channel 上的派发（那是 context.Canceled 被当成「worker 收尾」
// 而导致的活锁：inFlight 永不清、每轮重复派发）。
func cancelAtFirstFinish(t *testing.T, cancel context.CancelFunc, finished func() int) {
	t.Helper()
	cancelWhen(t, cancel, func() bool { return finished() > 0 })
}

// cancelWhen 轮询 condition 直到成立再取消 ctx。条件是「已经发生的事实」（计数
// 或日志出现），所以取消只会落在检测之间或派发落定之后，不会打断发送中的 select。
//
// 中止路径的清理交给 t.Cleanup。不在这里做：Cleanup 在 t.Errorf 之后也照常执行，
// 而这样写意味着真正的失败会跟着一次多余的 cancel 一起上报，掩盖「自己平台
// 到底有没有取消成功」这条信息；取消是可能失败的，多取消一次没有额外代价。
func cancelWhen(t *testing.T, cancel context.CancelFunc, condition func() bool) {
	t.Helper()
	// 测试失败 / 超时提前中止时也要解除 RunLoop 对 ctx 的等待：否则循环会一直
	// 停在主 select 上，包级超时（panic: test timed out）把整个包连坐拖死，
	// 真正的原因被埋在超时堆栈里。
	t.Cleanup(cancel)
	go func() {
		// 上限 2s 兜底：RunLoop 若先一步因其它原因退出，说明测试已失败，
		// 取消到得太早也无害
		deadline := time.After(2 * time.Second)
		for {
			if condition() {
				cancel()
				return
			}
			select {
			case <-t.Context().Done():
				return
			case <-deadline:
				t.Errorf("RunLoop 未在 2s 内到达取消水位，测试驱动放弃")
				cancel()
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
}

// TestRunLoopDispatchesTodoAndExitsOnContextCancel 端到端跑一遍常驻循环的骨架：
// 加锁 → 建落点 → 横幅 → 检测到待办 → 派发 → 会话 → 会话结束 → 上下文取消
// → 优雅退出 → 释放锁。任一步静默都会让部署形态「看起来在跑」却从不处理待办。
func TestRunLoopDispatchesTodoAndExitsOnContextCancel(t *testing.T) {
	harness := newLoopHarness(t)
	// 待办由 mention 通道进入（workItems 按 IsPull 定型为 PR），这样才能走到
	// Pull 分支：PrepareWorktree → 会话 → 完成判定
	harness.api.pulls = []status.Issue{{Index: 42, Title: "待评审", IsPull: true}}
	var banner strings.Builder
	var mutex sync.Mutex
	harness.deps.Log = func(line string) {
		mutex.Lock()
		defer mutex.Unlock()
		banner.WriteString(line)
		banner.WriteString("\n")
	}
	// 等首个会话回执（OnFinish）之后再取消：此时派发已落定，取消只会让循环在
	// 检测间隔里退出，而不是把 context.Canceled 塞进无缓冲 channel 的发送。
	cancelAtFirstFinish(t, harness.cancel, func() int { return int(harness.finished.Load()) })
	// 兜底：待办若因任何原因没派发出去，超时后就报错并强制取消，让用例以
	// 「会话次数 = 0」这条真实断言失败收场，而不是挂到包级超时。
	go func() {
		select {
		case <-time.After(5 * time.Second):
			t.Error("5s 内未走到首个会话收尾，派发路径未达成")
			harness.cancel()
		case <-harness.ctx.Done():
		}
	}()

	if err := RunLoop(harness.ctx, harness.deps); err != nil {
		t.Fatalf("RunLoop() error = %v", err)
	}
	logs := func() string {
		mutex.Lock()
		defer mutex.Unlock()
		return banner.String()
	}()

	for _, want := range []string{
		"dispatcher 启动：",
		"检测到 1 个待办",
		"#42",
		"会话结束：",
		"已退出主循环",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("日志缺少 %q：\n%s", want, logs)
		}
	}
	if got := harness.queued.Load(); got != 1 {
		t.Errorf("OnQueue 队列长度 = %d, want 1", got)
	}
	if got := harness.sessions.Load(); got != 1 {
		t.Errorf("会话次数 = %d, want 1", got)
	}
	if got := harness.started.Load(); got != 1 {
		t.Errorf("OnStart = %d, want 1", got)
	}
	if got := harness.finished.Load(); got != 1 {
		t.Errorf("OnFinish = %d, want 1", got)
	}
	// 锁必须已释放：残留会让下一次启动被自己挡住
	if _, err := os.Stat(harness.lockPath); !os.IsNotExist(err) {
		t.Errorf("锁文件未清理: err = %v", err)
	}
	if err := AcquireLock(harness.lockPath, func(string) {}); err != nil {
		t.Errorf("退出后应可重新加锁: %v", err)
	}
	_ = os.Remove(harness.lockPath)
}

// TestRunLoopWarnsWhenTokenAccountIsNotReviewer：横幅里的账户是操作者唯一的
// 自检线索——令牌账户与 reviewer 不一致时完成判定永远匹配不上，必须显式告警
// 而不是安静地跑一个永不收敛的循环。
func TestRunLoopWarnsWhenTokenAccountIsNotReviewer(t *testing.T) {
	harness := newLoopHarness(t)
	harness.api.login = "someone-else"
	var lines []string
	var mutex sync.Mutex
	harness.deps.Log = func(line string) {
		mutex.Lock()
		defer mutex.Unlock()
		lines = append(lines, line)
	}
	// 无待办：检测一轮后即取消，走完横幅与退出路径（会话不会被起）
	cancelAtFirstDetectLog(t, harness.cancel, func() int {
		mutex.Lock()
		defer mutex.Unlock()
		return len(lines)
	})

	if err := RunLoop(harness.ctx, harness.deps); err != nil {
		t.Fatalf("RunLoop() error = %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "账户=@someone-else") {
		t.Errorf("横幅应含令牌账户：\n%s", joined)
	}
	if !strings.Contains(joined, "警告：令牌账户 @someone-else ≠ reviewer ai") {
		t.Errorf("账户与 reviewer 不一致应告警：\n%s", joined)
	}
}

// TestRunLoopWithoutCurrentLoginUsesBareBanner：CurrentLogin 为 nil（无状态查询
// 的嵌入形态）时横幅必须退化成不带账户的一行，且不因此 panic 或跳过启动。
func TestRunLoopWithoutCurrentLoginUsesBareBanner(t *testing.T) {
	harness := newLoopHarness(t)
	harness.deps.CurrentLogin = nil
	var lines []string
	var mutex sync.Mutex
	harness.deps.Log = func(line string) {
		mutex.Lock()
		defer mutex.Unlock()
		lines = append(lines, line)
	}
	cancelAtFirstDetectLog(t, harness.cancel, func() int {
		mutex.Lock()
		defer mutex.Unlock()
		return len(lines)
	})

	if err := RunLoop(harness.ctx, harness.deps); err != nil {
		t.Fatalf("RunLoop() error = %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "dispatcher 启动：host=http://gitea.test repo=owner/repo reviewer=ai") {
		t.Errorf("无 CurrentLogin 时横幅应不含账户：\n%s", joined)
	}
	if strings.Contains(joined, "账户=") {
		t.Errorf("无 CurrentLogin 不应出现账户字段：\n%s", joined)
	}
}

// TestRunLoopSurfacesAccountQueryFailure：账户查询失败只是可观测性降级，横幅
// 仍要打出（否则操作者连「谁在跑」都看不到），但失败原因必须显式暴露。
func TestRunLoopSurfacesAccountQueryFailure(t *testing.T) {
	harness := newLoopHarness(t)
	harness.api.loginErr = errors.New("令牌无效")
	var lines []string
	var mutex sync.Mutex
	harness.deps.Log = func(line string) {
		mutex.Lock()
		defer mutex.Unlock()
		lines = append(lines, line)
	}
	// 等「账户查询失败」与随后的启动横幅都写进缓冲后再取消——这两行正是本测试
	// 要断言的内容，等它们到达即保证启动路径已跑完。
	//
	// 不要等「已退出主循环」：那行是 RunLoop **返回前**才打的，而取消是它返回的
	// 前提，等它等于等自己（循环等待 ⇒ 测试挂到超时）。
	cancelWhen(t, harness.cancel, func() bool {
		mutex.Lock()
		defer mutex.Unlock()
		hasFailure, hasBanner := false, false
		for _, line := range lines {
			if strings.Contains(line, "账户查询失败：") {
				hasFailure = true
			}
			if strings.Contains(line, "dispatcher 启动：") {
				hasBanner = true
			}
		}
		return hasFailure && hasBanner
	})

	if err := RunLoop(harness.ctx, harness.deps); err != nil {
		t.Fatalf("RunLoop() error = %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "账户查询失败：令牌无效") {
		t.Errorf("账户查询失败应报出原因：\n%s", joined)
	}
	if !strings.Contains(joined, "dispatcher 启动：") {
		t.Errorf("查询失败仍应打出横幅：\n%s", joined)
	}
}

// TestRunLoopReportsDetectionFailure：检测失败必须按「下一轮重试」语义报出并
// 继续循环（Gitea 短暂不可用时不该退出常驻进程），且不得派发任何待办。
func TestRunLoopReportsDetectionFailure(t *testing.T) {
	hanging := &errAPI{mentionErr: errors.New("gitea 不可用")}
	observed := make(chan string, 8)
	harness := newLoopHarness(t)
	harness.deps.ListWork = func(ctx context.Context) ([]WorkItem, error) {
		return ListWork(ctx, hanging, mustRepository("owner/repo"), "ai")
	}
	harness.deps.Log = func(line string) {
		select {
		case observed <- line:
		default:
		}
	}

	// 等第一条「检测失败」日志到达后取消：不赌 ticker，也不 sleep
	go func() {
		for line := range observed {
			if strings.Contains(line, "检测失败：") {
				harness.cancel()
				return
			}
		}
	}()

	if err := RunLoop(harness.ctx, harness.deps); err != nil {
		t.Fatalf("RunLoop() error = %v", err)
	}
	if got := harness.sessions.Load(); got != 0 {
		t.Errorf("检测失败时不应起会话：%d", got)
	}
}

// TestRunLoopFailsWhenLockIsHeld：锁被存活实例占用时 RunLoop 必须立即失败——
// 单飞是部署形态的硬前提，绕过它会出现两个进程同时评审同一条待办。
func TestRunLoopFailsWhenLockIsHeld(t *testing.T) {
	harness := newLoopHarness(t)
	if err := os.WriteFile(harness.lockPath, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := RunLoop(harness.ctx, harness.deps)
	if err == nil {
		t.Fatal("锁被占用时 RunLoop 应报错")
	}
	if !strings.Contains(err.Error(), "已在运行") {
		t.Errorf("error = %v, want 已在运行", err)
	}
	if got := harness.sessions.Load(); got != 0 {
		t.Errorf("拿不到锁不应起会话：%d", got)
	}
}

// TestRunLoopReleasesLockWhenLogDirCannotBeCreated：LogDir 建不出来时（落点被
// 文件占用）必须释放刚拿到的锁再返回错误，否则残留锁会把后续每次启动都挡在
// 「已在运行」上，故障无法自愈。
func TestRunLoopReleasesLockWhenLogDirCannotBeCreated(t *testing.T) {
	harness := newLoopHarness(t)
	// 用一个普通文件顶掉 LogDir：MkdirAll 必然失败
	if err := os.WriteFile(harness.logDir, []byte("占位\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunLoop(harness.ctx, harness.deps); err == nil {
		t.Fatal("LogDir 建不出来时应报错")
	}
	if _, err := os.Stat(harness.lockPath); !os.IsNotExist(err) {
		t.Errorf("失败路径必须释放锁: err = %v", err)
	}
}

// TestRunLoopReleasesLockWhenWorktreeRootCannotBeCreated：WorktreeRoot 与 LogDir
// 同理——落点不可用要在启动阶段就失败，且不能留下自己的锁。
func TestRunLoopReleasesLockWhenWorktreeRootCannotBeCreated(t *testing.T) {
	harness := newLoopHarness(t)
	if err := os.WriteFile(harness.deps.Config.WorktreeRoot, []byte("占位\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunLoop(harness.ctx, harness.deps); err == nil {
		t.Fatal("WorktreeRoot 建不出来时应报错")
	}
	if _, err := os.Stat(harness.lockPath); !os.IsNotExist(err) {
		t.Errorf("失败路径必须释放锁: err = %v", err)
	}
}

// TestRunAllAggregatesFailureAndStopsSiblings 钉住多仓库监督语义：任一仓库的
// 循环失败，其余循环要被取消（同一进程内的仓库不该在别的仓库已失败时继续
// 花钱起会话），并且所有失败都要经 errors.Join 聚合返回，调用方能据以退出。
func TestRunAllAggregatesFailureAndStopsSiblings(t *testing.T) {
	// 坏循环：LogDir 落点被普通文件占用，RunLoop 在启动阶段即失败
	broken := newLoopHarness(t)
	broken.deps.Config.LogDir = filepath.Join(t.TempDir(), "logs")
	if err := os.WriteFile(broken.deps.Config.LogDir, []byte("占位\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 兄弟循环：检测通道持续失败（永不产出待办），只有 ctx 被取消才会退出
	sibling := newLoopHarness(t)
	failing := &errAPI{mentionErr: errors.New("gitea 不可用")}
	sibling.deps.ListWork = func(ctx context.Context) ([]WorkItem, error) {
		return ListWork(ctx, failing, mustRepository("owner/repo"), "ai")
	}

	err := RunAll(t.Context(), []Deps{broken.deps, sibling.deps})
	if err == nil {
		t.Fatal("至少一个循环失败时 RunAll 应返回错误")
	}
	// 聚合的错误必须带上坏循环的真实原因（errors.Join 保留 %w 链）。
	// 断言用落点路径而非某个动词：mkdir 的实际文案由 os 给出（父路径是普通
	// 文件时是 "not a directory"），钉动词会把测试绑死在标准库文案上。
	if !strings.Contains(err.Error(), broken.deps.Config.LogDir) {
		t.Errorf("error = %v, want 含坏循环的失败落点 %s", err, broken.deps.Config.LogDir)
	}
	// 兄弟循环必须已被 RunAll 的 cancel 停止：残留的 goroutine 会继续持有锁、
	// 继续轮询 Gitea，且在测试结束后泄漏
	if _, statErr := os.Stat(sibling.lockPath); !os.IsNotExist(statErr) {
		t.Errorf("坏循环失败后兄弟循环应被取消并释放锁: err = %v", statErr)
	}
	if got := sibling.sessions.Load(); got != 0 {
		t.Errorf("兄弟循环不应起会话：%d", got)
	}
}

// TestEnsureRepoFailsOnCloneError：克隆失败（远端不可达 / 仓库不存在）必须带上
// 远端 URL 与 git 的 stderr 一起报出——这是受管克隆唯一会暴露问题的地方，
// 只报「exit status 128」对操作者毫无用处。
func TestEnsureRepoFailsOnCloneError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("环境无 git")
	}
	dir := filepath.Join(t.TempDir(), "clone", "repo")
	_, err := EnsureRepo(dir, "file://"+filepath.Join(t.TempDir(), "absent"), "acme/repo", "")
	if err == nil {
		t.Fatal("远端不存在时 EnsureRepo 应报错")
	}
	if !strings.Contains(err.Error(), "克隆 ") || !strings.Contains(err.Error(), "acme/repo.git") {
		t.Errorf("error = %v, want 含克隆目标 URL", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("失败后不应留下半截目录: err = %v", statErr)
	}
}

// TestEnsureRepoMkdirFailure：父路径被普通文件占用时创建克隆目录必然失败，
// 错误要包住底层原因而不是被吞掉。
func TestEnsureRepoMkdirFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	writeFile(t, blocker, "占位\n")
	_, err := EnsureRepo(filepath.Join(blocker, "repo"), "https://gitea.example.com", "acme/repo", "")
	if err == nil {
		t.Fatal("父路径被占用时应报错")
	}
	if !strings.Contains(err.Error(), "创建克隆目录") {
		t.Errorf("error = %v, want 含创建克隆目录", err)
	}
}

// TestSyncMirrorSurfacesStepFailures：镜像同步的每一步（fetch / checkout /
// clean / rev-parse）失败都要冒泡——fail-closed 的前提是错误可见，静默返回
// 空 sha 会让会话带着过期基线开跑。
func TestSyncMirrorSurfacesStepFailures(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("环境无 git")
	}
	base := makeFixture(t)
	seed := filepath.Join(base, "seed")

	if sha, err := SyncMirror(seed, "no-such-branch", ""); err == nil {
		t.Errorf("基线分支不存在时应报错，sha = %q", sha)
	}
	if sha, err := SyncMirror(filepath.Join(base, "not-a-repo"), "main", ""); err == nil {
		t.Errorf("非仓库目录应报错，sha = %q", sha)
	}
}

// TestPinStandardSurfacesStatFailure：源 .claude 存在但 Stat 失败（父目录可进入、
// 目标本身不可读）时必须报错——这种形态最容易发生在权限错误的检出上，
// 当成「没有托管标准」会安静地放过一份无效评审现场。
func TestPinStandardSurfacesStatFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视权限位，跳过")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	// repo/.claude 指向一个不存在的路径：父目录可进入（能 Stat 条目本身），
	// 但链接目标不可达 → Stat 报 ENOENT 之外的错误（ELOOP 循环链更稳）
	if err := os.Symlink(filepath.Join(repo, ".claude"), filepath.Join(repo, ".claude")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude")); err == nil || os.IsNotExist(err) {
		t.Skipf("该平台/文件系统不支持自指链接探测: %v", err)
	}
	if err := PinStandard(repo, filepath.Join(dir, "wt")); err == nil {
		t.Error("源不可 Stat 时应报错，不能当成缺省标准")
	}
}

// TestPrepareBaselineWorktreeSurfacesFailures：基线 worktree 的每个错误出口
// 都要冒泡（fetch head / 建检出），不留半截现场。
func TestPrepareBaselineWorktreeSurfacesFailures(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("环境无 git")
	}
	base := makeFixture(t)
	seed := filepath.Join(base, "seed")

	// 远端分支不存在 → fetch 失败
	if _, err := PrepareBaselineWorktree(seed, "no-such-branch", filepath.Join(base, "wt"), ""); err == nil {
		t.Error("基线分支不存在应报错")
	}
	// 工作树落点被普通文件占用 → worktree add 失败
	blocker := filepath.Join(base, "blocked-wt")
	writeFile(t, blocker, "占位\n")
	if _, err := PrepareBaselineWorktree(seed, "main", blocker, ""); err == nil {
		t.Error("落点被文件占用应报错")
	}
}

// TestPrepareWorktreeSurfacesPinStandardFailure：检出成功后钉标准失败（宿主
// .claude 不可读）必须整条失败——带着 PR 自带的评审标准开跑等于让被评审方
// 决定评审规则。
func TestPrepareWorktreeSurfacesPinStandardFailure(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("环境无 git")
	}
	if os.Geteuid() == 0 {
		t.Skip("root 无视权限位，跳过")
	}
	base := makeFixture(t)
	seed := filepath.Join(base, "seed")
	if err := os.Chmod(filepath.Join(seed, ".claude"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(seed, ".claude"), 0o755) })

	if _, err := PrepareWorktree(seed, 1, filepath.Join(base, "wt-pr-1"), ""); err == nil {
		t.Error("钉标准失败时 PrepareWorktree 应报错")
	}
}

// TestCopyDirSurfacesNestedFailures：递归复制里每一层的失败都要冒泡——半份
// worktree（缺文件、缺链接）会被会话当成完整检出使用。
func TestCopyDirSurfacesNestedFailures(t *testing.T) {
	t.Run("符号链接落点被占用", func(t *testing.T) {
		dir := t.TempDir()
		source := filepath.Join(dir, "src")
		if err := os.MkdirAll(source, 0o755); err != nil {
			t.Fatal(err)
		}
		// 落点已存在时 os.Symlink 报 EEXIST：必须冒泡（半份检出缺链接）
		if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(source, "link")); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(dir, "out")
		if err := os.MkdirAll(destination, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(destination, "link")); err != nil {
			t.Fatal(err)
		}
		if err := copyDir(source, destination); err == nil {
			t.Error("建链落点被占用时报错应冒泡")
		}
	})

	t.Run("子目录递归失败", func(t *testing.T) {
		dir := t.TempDir()
		source := filepath.Join(dir, "src")
		nested := filepath.Join(source, "nested")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(nested, "f.txt"), "x\n")
		destination := filepath.Join(dir, "out")
		// 子目录落点被普通文件占位：递归 copyDir 的 MkdirAll 必然失败
		if err := os.MkdirAll(destination, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(destination, "nested"), "占位\n")
		if err := copyDir(source, destination); err == nil {
			t.Error("子目录落点被文件占用时报错应冒泡")
		}
	})

	t.Run("文件条目复制失败", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root 无视权限位，跳过")
		}
		dir := t.TempDir()
		source := filepath.Join(dir, "src")
		if err := os.MkdirAll(source, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(source, "f.txt"), "内容\n")
		destination := filepath.Join(dir, "out")
		if err := os.MkdirAll(destination, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(destination, 0o755) })
		if err := copyDir(source, destination); err == nil {
			t.Error("目标不可写时报错应冒泡")
		}
	})
}

// TestCopyFileSurfacesCopyFailure：io.Copy 失败（源是目录，读会报 EISDIR）
// 必须冒泡——复制到一半的文件静默留下会让评审标准不完整。
func TestCopyFileSurfacesCopyFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视权限位，跳过")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "src-dir")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	// 目录以只读方式打开会成功，但 Read 报 EISDIR：io.Copy 在此失败
	if err := copyFile(source, filepath.Join(dir, "out.txt"), 0o644); err == nil {
		t.Error("源为目录时 io.Copy 应失败并冒泡")
	}
}

// TestCreateSessionConfigDirReportsFailure：临时会话配置目录建不出来时必须返回
// 错误并让 RunSession 记为会话失败——拿不到配置目录会继续跑一个没有权限放行
// 与 gitea MCP 的会话，评审事实上已经失效。
func TestCreateSessionConfigDirReportsFailure(t *testing.T) {
	// TMPDIR 指向不存在的路径：MkdirTemp 必然失败
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "absent", "tmp"))
	dir, cleanup, err := createSessionConfigDir()
	if err == nil {
		t.Fatalf("MkdirTemp 失败时应报错，dir = %q", dir)
	}
	if !strings.Contains(err.Error(), "创建会话配置目录") {
		t.Errorf("error = %v, want 含创建会话配置目录", err)
	}
	if dir != "" {
		t.Errorf("失败时 dir 应为空：%q", dir)
	}
	if cleanup == nil {
		t.Error("失败时仍应返回可调用的 cleanup")
	}
	cleanup()
}

// TestWriteClaudeSessionSettingsReportsWriteFailure：settings.json 写不进去
// （目录不存在 / 不可写）必须冒泡，静默返回空路径会让会话以空配置开跑。
func TestWriteClaudeSessionSettingsReportsWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视权限位，跳过")
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.MkdirAll(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := writeClaudeSessionSettings(dir, claudecfg.Overrides{}); err == nil {
		t.Error("目录不可写时应报错")
	} else if !strings.Contains(err.Error(), "写入会话配置") {
		t.Errorf("error = %v, want 含写入会话配置", err)
	}
	if _, err := writeClaudeSessionSettings(filepath.Join(dir, "absent"), claudecfg.Overrides{}); err == nil {
		t.Error("目录不存在时应报错")
	}
}

// TestWriteSessionMCPConfigReportsWriteFailure：合并后的 MCP 配置写不出去必须
// 冒泡——空路径会让 claude 丢掉 assistant 注入的 gitea server。
func TestWriteSessionMCPConfigReportsWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 无视权限位，跳过")
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.MkdirAll(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := writeSessionMCPConfig(dir, "", claudecfg.Overrides{}); err == nil {
		t.Error("目录不可写时应报错")
	} else if !strings.Contains(err.Error(), "写入合并后的 MCP 配置") {
		t.Errorf("error = %v, want 含写入合并后的 MCP 配置", err)
	}
}

// TestParseStreamEventRejectsNonJSON：非 JSON 行必须有明确的「解析失败」出口，
// 而不是零值事件——零值会被下游当成一条无类型事件计入时间线。解析本身归
// claude 包（ParseLine），这里钉的是 dispatch 侧依赖的那条契约。
func TestParseStreamEventRejectsNonJSON(t *testing.T) {
	if _, ok := claude.ParseLine([]byte("这不是 JSON")); ok {
		t.Error("非 JSON 行应返回 ok=false")
	}
	if event, ok := claude.ParseLine([]byte(`{"type":"assistant","subtype":"text"}`)); !ok {
		t.Error("合法 JSON 应返回 ok=true")
	} else if event.Type == nil || *event.Type != "assistant" {
		t.Errorf("解析结果 = %+v, want type=assistant", event)
	}
}

// TestDescribeStreamEventLabelsEachShape 钉住事件标注的四种形态：非 JSON 只报
// 长度、双标注拼 type/subtype、单标注各自退化、无标注显式写明——时间线是
// 排障的第一手材料，含糊的标注等于没有标注。
func TestDescribeStreamEventLabelsEachShape(t *testing.T) {
	str := func(value string) *string { return &value }
	cases := []struct {
		name  string
		event claude.Event
		ok    bool
		line  string
		want  string
	}{
		{"非 JSON 行", claude.Event{}, false, "乱码", "（非 JSON 行，2 字）"},
		{"type/subtype", claude.Event{Type: str("assistant"), Subtype: str("text")}, true, "{}", "assistant/text"},
		{"仅 type", claude.Event{Type: str("result")}, true, "{}", "result"},
		{"仅 subtype", claude.Event{Subtype: str("error")}, true, "{}", "subtype/error"},
		{"无标注", claude.Event{}, true, "{}", "（未标注类型）"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := describeStreamEvent(testCase.event, testCase.ok, testCase.line); got != testCase.want {
				t.Errorf("describeStreamEvent() = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestThinkingTrackerFlushDisabledStaysSilent：未开 --debug 时思考段汇总不得上报
// （默认日志只留操作者需要的信息），但累计值仍要结算，否则合计会漏掉最后一段。
func TestThinkingTrackerFlushDisabledStaysSilent(t *testing.T) {
	reported := 0
	tracker := &thinkingTracker{
		debug:      false,
		onProgress: func(string) { reported++ },
	}
	// 手工构造一个进行中的思考段（observe 需要 system/thinking_tokens 帧，
	// observe 的入口在别处测；这里直接驱动 flush 的出口）
	tracker.burstStart = time.Now().Add(-time.Second)
	tracker.burstLast = time.Now()
	tracker.burstTokens = 100
	tracker.burstFrames = 7
	tracker.flush()

	if reported != 0 {
		t.Errorf("未开 debug 不应上报思考段：%d", reported)
	}
	if tracker.totalTokens != 100 {
		t.Errorf("totalTokens = %d, want 100（累计仍要结算）", tracker.totalTokens)
	}
	if !tracker.burstStart.IsZero() {
		t.Error("flush 后应归零思考段")
	}
}

// TestFeedEventToolUseWithoutDetail：工具调用拿不到出入参摘要时要退化成
// 「🔧 名称」，不能整行丢掉——读者至少要知道当前步调用了哪个工具。折叠与摘要
// 规则都归 claude 包（FeedEvent），这里钉的是 dispatch 依赖的那些出口。
func TestFeedEventToolUseWithoutDetail(t *testing.T) {
	event := claude.Event{
		Type: ptr("assistant"),
		Message: &claude.Message{Content: []claude.ContentBlock{
			{Type: "tool_use", Name: "Read", Input: json.RawMessage(`"不是对象"`)},
		}},
	}
	var lines []string
	outcome := claude.NewOutcome()
	if claude.FeedEvent(&outcome, event, claude.ProgressTerse, func(line string) { lines = append(lines, line) }) {
		t.Error("工具调用不应被当成 result")
	}
	if len(lines) != 1 || lines[0] != "🔧 Read" {
		t.Errorf("进度行 = %v, want [🔧 Read]", lines)
	}
	if outcome.NumTurns != 1 {
		t.Errorf("NumTurns = %d, want 1（assistant 事件计一回）", outcome.NumTurns)
	}
}

// TestFeedEventToolUseWithDetail：有出入参摘要时走「🔧 名称: 摘要」，
// 该分支才是常态——详见 claude.DescribeToolInput 的测试。
func TestFeedEventToolUseWithDetail(t *testing.T) {
	event := claude.Event{
		Type: ptr("assistant"),
		Message: &claude.Message{Content: []claude.ContentBlock{
			{Type: "tool_use", Name: "Bash", Input: json.RawMessage(`{"command":"ls -la"}`)},
		}},
	}
	var lines []string
	outcome := claude.NewOutcome()
	claude.FeedEvent(&outcome, event, claude.ProgressTerse, func(line string) { lines = append(lines, line) })
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "🔧 Bash: ") {
		t.Errorf("进度行 = %v, want 带摘要的 🔧 Bash: …", lines)
	}
}

// TestReadReviewConventionsReturnsRunes：项目评审约定的截断按**字符**计数
// （不是字节），中文约定按字节截断会把最后一个字劈成乱码。
func TestReadReviewConventionsReturnsRunes(t *testing.T) {
	dir := t.TempDir()
	if got := ReadReviewConventions(""); got != "" {
		t.Errorf("空项目目录应返回空串：%q", got)
	}
	if got := ReadReviewConventions(dir); got != "" {
		t.Errorf("缺 .assistant/review.md 应返回空串：%q", got)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".assistant"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".assistant", "review.md"), "   \n\t\n")
	if got := ReadReviewConventions(dir); got != "" {
		t.Errorf("全空白约定应返回空串：%q", got)
	}

	// 超长约定（中文）：必须完整落在字符边界上并附截断说明
	long := strings.Repeat("约定", ReviewConventionsLimit)
	writeFile(t, filepath.Join(dir, ".assistant", "review.md"), long)
	got := ReadReviewConventions(dir)
	if !strings.Contains(got, "已截断") {
		t.Errorf("超长约定应附截断说明：%q", got[:60])
	}
	body, _, _ := strings.Cut(got, "\n\n（")
	if runes := []rune(body); len(runes) != ReviewConventionsLimit {
		t.Errorf("截断长度 = %d 字符, want %d", len(runes), ReviewConventionsLimit)
	}
	if strings.ContainsRune(body, '�') {
		t.Error("截断不得劈坏多字节字符")
	}
}

// TestReviewProtocolPromptIncludesConventions：协议打头 + 项目约定附后，顺序
// 是不可换的（协议是唯一事实源，约定只能补充）；没有约定时不留多余空段。
func TestReviewProtocolPromptIncludesConventions(t *testing.T) {
	withoutConventions := ReviewProtocolPrompt("")
	if withoutConventions == "" {
		t.Fatal("协议不应为空")
	}
	if strings.Contains(withoutConventions, "## 项目评审约定") {
		t.Error("无项目约定时不应出现约定段")
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".assistant"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".assistant", "review.md"), "本项目只评审 Go 代码\n")
	withConventions := ReviewProtocolPrompt(dir)
	if !strings.HasSuffix(withConventions, "## 项目评审约定（.assistant/review.md）\n\n本项目只评审 Go 代码") {
		t.Errorf("项目约定应附在协议之后：\n%s", withConventions)
	}
	if !strings.HasPrefix(withConventions, withoutConventions) {
		t.Error("附录不得改动协议主体")
	}
}

// TestSessionSpecInjectsSessionDirAndProjectName：容器形态下文本记录目录名
// 靠进程环境传递给 docker（`-e KEY`），漏掉会让容器内 claude 写进容器默认目录，
// 会话记录散落；宿主形态则必须走 SessionEnv 而不是容器专用注入。
func TestSessionSpecInjectsSessionDirAndProjectName(t *testing.T) {
	config := testConfig(func(c *Config) {
		c.DockerImage = "review:latest"
		c.SessionDir = "/host/sessions"
		c.SessionProject = "/host/projects"
	})
	options := SessionOptions{
		Config:        config,
		Prompt:        "评审这个 PR",
		Cwd:           "/tmp/wt",
		MCPConfigPath: "/tmp/wt/.mcp.json",
	}
	spec := runSessionSpec(options)
	if spec.Bin != "docker" {
		t.Errorf("容器形态 bin = %q, want docker", spec.Bin)
	}
	if spec.Container == "" {
		t.Error("容器形态应带容器名（超时终止要用）")
	}
	joined := strings.Join(spec.Args, " ")
	for _, want := range []string{
		"-e CLAUDE_CONFIG_DIR=/host/sessions",
		"-e CLAUDE_CODE_PROJECT_DIR_NAME=/host/projects",
		// Cwd 本身既挂载又作工作目录；.mcp.json 与 Cwd 同目录被去重，故此处
		// 只断言 Cwd 挂载与 projects 目录挂载都在场
		"-v /tmp/wt:/tmp/wt",
		"/host/sessions/projects",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("容器命令缺少 %q：\n%s", want, joined)
		}
	}
	// 宿主形态：不带 docker，直接跑 claude，环境变量走 SessionEnv（不是容器 -e）
	hostConfig := testConfig(func(c *Config) {
		c.SessionDir = "/host/sessions"
		c.SessionProject = "/host/projects"
	})
	hostSpec := runSessionSpec(SessionOptions{
		Config: hostConfig,
		Prompt: "评审这个 PR",
		Cwd:    "/tmp/wt",
	})
	if hostSpec.Bin != "claude" || hostSpec.Container != "" {
		t.Errorf("宿主形态 bin=%q container=%q, want claude / 空", hostSpec.Bin, hostSpec.Container)
	}
	if strings.Contains(strings.Join(hostSpec.Args, " "), "CLAUDE_CONFIG_DIR") {
		t.Errorf("宿主形态不应注入容器环境变量：%v", hostSpec.Args)
	}
	configDir, ok := claude.EnvValue(hostSpec.Env, "CLAUDE_CONFIG_DIR")
	if !ok || configDir != "/host/sessions" {
		t.Errorf("宿主形态应把配置根写进进程环境：%v", hostSpec.Env)
	}
	if project, ok := claude.EnvValue(hostSpec.Env, "CLAUDE_CODE_PROJECT_DIR_NAME"); !ok || project != "/host/projects" {
		t.Errorf("宿主形态应把文本记录目录名写进进程环境：%v", hostSpec.Env)
	}
}

// ptr 是测试里构造 claude.Event 指针字段的助手（生产侧字段全是指针，
// 便于区分「字段缺失」与「字段为零值」）。
func ptr[T any](value T) *T { return &value }
