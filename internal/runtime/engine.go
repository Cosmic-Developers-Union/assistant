package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/claude"
)

// Event 只携带平台事实；Head 与 Title 分别是评审、分诊的记忆锚点。
type Event struct {
	Bot    string `json:"bot"`
	Kind   string `json:"kind"`
	Host   string `json:"host"`
	Repo   string `json:"repo"`
	Number int64  `json:"number"`
	Title  string `json:"title"`
	Head   string `json:"head"`
	Ref    string `json:"ref"`
	Base   string `json:"base"`
	User   string `json:"user,omitempty"`
	Text   string `json:"-"`
}

func (e Event) key() string {
	return fmt.Sprintf("%s/%s/%s/%d/%s", e.Host, e.Repo, e.Kind, e.Number, e.User)
}

// Workspace 准备工作目录并交付清理函数，主循环不知道具体实现。
type Workspace interface {
	Prepare(context.Context, Event) (string, func(), error)
}

// Session 控制会话 id 与落点，恢复和上传错误不会被吞掉。
type Session interface {
	ID(Event) string
	Dir(context.Context, Event) (string, error)
	Sync(context.Context, Event, string) error
}

// AgentRunner 在准备好的目录中执行一次会话，不推断平台完成状态。
type AgentRunner interface {
	Run(context.Context, AgentSpec, Event, string, string) (Outcome, error)
}

// Outcome 记录执行结局与开销；进程正常退出不代表平台待办完成。
type Outcome = claude.Outcome

// Source 发现平台侧尚未完成的事件。
type Source interface {
	Poll(context.Context) ([]Event, error)
}

// Worker 是配置装配后的 bot，没有连接解析或存储策略。
type Worker struct {
	Spec      AgentSpec
	Workspace Workspace
	Session   Session
	Runner    AgentRunner
}

// Record 是观测数据，写入失败不改变下一轮平台检测。
type Record struct {
	Event    Event     `json:"event"`
	Session  string    `json:"session"`
	Started  time.Time `json:"started"`
	Duration int64     `json:"duration_ms"`
	Outcome  Outcome   `json:"outcome"`
	Error    string    `json:"error,omitempty"`
}

// Engine 用 pending 与并发槽保护所有 bot；每轮 barrier 后重新读平台状态。
type Engine struct {
	Workers     map[string]Worker
	Sources     []Source
	Concurrency int
	Timeout     time.Duration
	Observe     func(Record)
	Log         func(string, ...any)
	mu          sync.Mutex
	pending     map[string]bool
}

func (e *Engine) log(format string, args ...any) {
	if e.Log != nil {
		e.Log(format, args...)
	}
}

// Process 按恢复记忆、准备工作区、执行、上传、观测、清理的顺序处理事件。
func (e *Engine) Process(ctx context.Context, ev Event) (failure error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	worker, ok := e.Workers[ev.Bot]
	if !ok {
		return fmt.Errorf("事件引用了不存在的 bot %s", ev.Bot)
	}
	if worker.Workspace == nil || worker.Session == nil || worker.Runner == nil {
		return fmt.Errorf("bot %s 未完整装配", ev.Bot)
	}
	e.mu.Lock()
	if e.pending == nil {
		e.pending = map[string]bool{}
	}
	if e.pending[ev.key()] {
		e.mu.Unlock()
		return nil
	}
	e.pending[ev.key()] = true
	e.mu.Unlock()
	defer func() { e.mu.Lock(); delete(e.pending, ev.key()); e.mu.Unlock() }()
	var cleanup func()
	record := Record{Event: ev, Session: worker.Session.ID(ev), Started: time.Now()}
	defer func() {
		record.Duration = time.Since(record.Started).Milliseconds()
		if failure != nil {
			record.Error = failure.Error()
		}
		if e.Observe != nil {
			e.Observe(record)
		}
		if cleanup != nil {
			cleanup()
		}
	}()
	sessionDir, err := worker.Session.Dir(ctx, ev)
	if err != nil {
		return fmt.Errorf("恢复会话: %w", err)
	}
	dir, cleanup, err := worker.Workspace.Prepare(ctx, ev)
	if err != nil {
		return fmt.Errorf("准备工作区: %w", err)
	}
	spec := worker.Spec
	spec.SessionDir = sessionDir
	runCtx := ctx
	cancel := func() {}
	if e.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, e.Timeout)
	}
	defer cancel()
	e.log("开始 %s %s#%d", ev.Bot, ev.Repo, ev.Number)
	record.Outcome, failure = worker.Runner.Run(runCtx, spec, ev, dir, record.Session)
	// 会话被取消也必须固化已有记忆；收尾有独立上限，避免阻塞退出。
	syncCtx, syncCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer syncCancel()
	if err := worker.Session.Sync(syncCtx, ev, sessionDir); err != nil {
		failure = errors.Join(failure, fmt.Errorf("固化会话: %w", err))
	}
	return failure
}

// Round 对独立来源逐一容错，去重后限流执行；失败不会截断其他待办。
func (e *Engine) Round(ctx context.Context, dryRun bool) error {
	var failures []error
	var events []Event
	for _, source := range e.Sources {
		found, err := source.Poll(ctx)
		if err != nil {
			failures = append(failures, err)
		}
		events = append(events, found...)
	}
	seen := map[string]bool{}
	slots := make(chan struct{}, max(1, e.Concurrency))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, ev := range events {
		if seen[ev.key()] {
			continue
		}
		seen[ev.key()] = true
		if dryRun {
			e.log("待办 %s %s#%d（只读演练）", ev.Bot, ev.Repo, ev.Number)
			continue
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return errors.Join(append(failures, ctx.Err())...)
		case slots <- struct{}{}:
		}
		wg.Go(func() {
			defer func() { <-slots }()
			if err := e.Process(ctx, ev); err != nil {
				mu.Lock()
				failures = append(failures, fmt.Errorf("%s %s#%d: %w", ev.Bot, ev.Repo, ev.Number, err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(failures...)
}

// Run 不读取本地结局来跳过事件，下一轮待办消失才表示完成。
func (e *Engine) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("轮询间隔必须为正")
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := e.Round(ctx, false); err != nil && ctx.Err() == nil {
			e.log("本轮处理失败：%v", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
