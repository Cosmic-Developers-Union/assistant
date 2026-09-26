package daemon

import (
	"strings"
	"sync"
)

// logSink 是并发安全的日志收集器：生产代码（RunChannel / handleInbound 的
// goroutine）与测试断言分处不同 goroutine，直接往切片里 append 会被 -race 判为
// 数据竞争（改前就是这样：`logs = append(logs, format)` 在两边同时碰同一片内存）。
//
// 给测试用的 Log 回调一律用它，不要再用裸切片。
type logSink struct {
	mu    sync.Mutex
	lines []string
}

// Log 可直接作为 ChatConfig.Log / ChannelConfig.Log 使用。
func (s *logSink) Log(format string, _ ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, format)
}

// joined 返回已收集日志的拼接结果（读时加锁）。
func (s *logSink) joined() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "\n")
}

// all 返回已收集日志的副本。
func (s *logSink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}
