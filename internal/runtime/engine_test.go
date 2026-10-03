package runtime

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type workspaceFunc func(context.Context, Event) (string, func(), error)

func (f workspaceFunc) Prepare(ctx context.Context, ev Event) (string, func(), error) {
	return f(ctx, ev)
}

type runnerFunc func(context.Context, AgentSpec, Event, string, string) (Outcome, error)

func (f runnerFunc) Run(ctx context.Context, s AgentSpec, e Event, d, id string) (Outcome, error) {
	return f(ctx, s, e, d, id)
}

type sourceFunc func(context.Context) ([]Event, error)

func (f sourceFunc) Poll(ctx context.Context) ([]Event, error) { return f(ctx) }

type testSession struct {
	calls               *[]string
	restoreErr, syncErr error
}

func (s testSession) ID(Event) string { return "stable" }
func (s testSession) Dir(context.Context, Event) (string, error) {
	*s.calls = append(*s.calls, "restore")
	return "memory", s.restoreErr
}
func (s testSession) Sync(ctx context.Context, _ Event, _ string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	*s.calls = append(*s.calls, "sync")
	return s.syncErr
}
func testEvent() Event {
	return Event{Bot: "review", Kind: "gitea-review", Host: "https://site", Repo: "acme/repo", Number: 1, Head: "h"}
}
func TestProcessLifecycle(t *testing.T) {
	for _, failure := range []string{"", "restore", "workspace", "runner", "sync", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			calls := []string{}
			sentinel := errors.New("失败")
			session := testSession{calls: &calls}
			if failure == "restore" {
				session.restoreErr = sentinel
			}
			if failure == "sync" {
				session.syncErr = sentinel
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			engine := Engine{Workers: map[string]Worker{"review": {Session: session, Workspace: workspaceFunc(func(context.Context, Event) (string, func(), error) {
				calls = append(calls, "prepare")
				if failure == "workspace" {
					return "", nil, sentinel
				}
				return "tree", func() { calls = append(calls, "cleanup") }, nil
			}), Runner: runnerFunc(func(ctx context.Context, spec AgentSpec, ev Event, dir, id string) (Outcome, error) {
				calls = append(calls, "run")
				if dir != "tree" || spec.SessionDir != "memory" || id != "stable" {
					t.Fatal("流水线参数不正确")
				}
				if failure == "cancel" {
					cancel()
					return Outcome{}, ctx.Err()
				}
				if failure == "runner" {
					return Outcome{}, sentinel
				}
				return Outcome{SawResult: true}, nil
			})}}, Observe: func(record Record) {
				calls = append(calls, "observe")
				if record.Session != "stable" || record.Duration < 0 {
					t.Fatal(record)
				}
				if failure != "" && record.Error == "" {
					t.Fatal("错误未记录")
				}
			}}
			err := engine.Process(ctx, testEvent())
			if failure == "" && err != nil {
				t.Fatal(err)
			}
			if failure != "" && err == nil {
				t.Fatal("错误被吞掉")
			}
			want := []string{"restore", "prepare", "run", "sync", "observe", "cleanup"}
			if failure == "restore" {
				want = []string{"restore", "observe"}
			}
			if failure == "workspace" {
				want = []string{"restore", "prepare", "observe"}
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("%v want %v", calls, want)
			}
			if len(engine.pending) != 0 {
				t.Fatal("失败后未释放待办")
			}
		})
	}
	engine := Engine{}
	if err := engine.Process(t.Context(), testEvent()); err == nil {
		t.Fatal("未知 bot 被接受")
	}
	engine.Workers = map[string]Worker{"review": {}}
	if err := engine.Process(t.Context(), testEvent()); err == nil {
		t.Fatal("缺失依赖被接受")
	}
}
func TestRoundDedupCapacityAndPlatformCompletion(t *testing.T) {
	var active, maxActive, calls atomic.Int32
	root := t.TempDir()
	engine := Engine{Concurrency: 2, Workers: map[string]Worker{"review": {Session: LocalSession{Root: root}, Workspace: DirectoryWorkspace{Root: root}, Runner: runnerFunc(func(ctx context.Context, _ AgentSpec, _ Event, _, _ string) (Outcome, error) {
		current := active.Add(1)
		for {
			old := maxActive.Load()
			if current <= old || maxActive.CompareAndSwap(old, current) {
				break
			}
		}
		calls.Add(1)
		time.Sleep(time.Millisecond)
		active.Add(-1)
		return Outcome{}, nil
	})}}}
	events := []Event{}
	for i := range 5 {
		ev := testEvent()
		ev.Number = int64(i + 1)
		events = append(events, ev, ev)
	}
	engine.Sources = []Source{sourceFunc(func(context.Context) ([]Event, error) { return events, nil }), sourceFunc(func(context.Context) ([]Event, error) { return nil, errors.New("独立来源失败") })}
	if err := engine.Round(t.Context(), false); err == nil {
		t.Fatal("来源错误被吞掉")
	}
	if calls.Load() != 5 || maxActive.Load() > 2 {
		t.Fatalf("calls=%d active=%d", calls.Load(), maxActive.Load())
	}
	// 正常退出仍在平台队列中，下一轮必须重试。
	_ = engine.Round(t.Context(), false)
	if calls.Load() != 10 {
		t.Fatal("本地结局阻止重试")
	}
	events = nil
	_ = engine.Round(t.Context(), false)
	if calls.Load() != 10 {
		t.Fatal("平台移除待办后仍执行")
	}
	events = []Event{testEvent()}
	_ = engine.Round(t.Context(), true)
	if calls.Load() != 10 {
		t.Fatal("演练启动了会话")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := engine.Run(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(t.Context(), 0); err == nil {
		t.Fatal("非法间隔被接受")
	}
}
func TestPendingGuardAndCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	engine := Engine{Workers: map[string]Worker{"review": {Session: LocalSession{Root: t.TempDir()}, Workspace: DirectoryWorkspace{Root: t.TempDir()}, Runner: runnerFunc(func(context.Context, AgentSpec, Event, string, string) (Outcome, error) {
		runs.Add(1)
		close(entered)
		<-release
		return Outcome{}, nil
	})}}}
	var wg sync.WaitGroup
	wg.Go(func() { _ = engine.Process(t.Context(), testEvent()) })
	<-entered
	if err := engine.Process(t.Context(), testEvent()); err != nil {
		t.Fatal(err)
	}
	close(release)
	wg.Wait()
	if runs.Load() != 1 {
		t.Fatal("同一待办并发执行")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	engine.Sources = []Source{sourceFunc(func(context.Context) ([]Event, error) { return []Event{testEvent()}, nil })}
	if err := engine.Round(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后继续执行: %v", err)
	}
}
func TestEngineTimeoutAndRunLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	engine := Engine{Timeout: time.Millisecond, Workers: map[string]Worker{"review": {Session: LocalSession{Root: t.TempDir()}, Workspace: DirectoryWorkspace{Root: t.TempDir()}, Runner: runnerFunc(func(ctx context.Context, _ AgentSpec, _ Event, _, _ string) (Outcome, error) {
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	})}}}
	if err := engine.Process(t.Context(), testEvent()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	engine.Sources = []Source{sourceFunc(func(context.Context) ([]Event, error) {
		if calls.Add(1) == 2 {
			cancel()
		}
		return nil, errors.New("平台暂时不可用")
	})}
	if err := engine.Run(ctx, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("错误使主循环终止")
	}
}
