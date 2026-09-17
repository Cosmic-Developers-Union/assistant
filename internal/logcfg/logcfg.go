// Package logcfg 是 assistant 的三级日志约定实现：
//
//	default = What happened?   状态、结果、警告、错误
//	verbose = What is happening? 步骤、目标、进度、外部调用、状态变化
//	debug   = Why is it happening? 内部决策、参数、分支、重试、底层错误、诊断上下文
//
// 各包以 Verb 函数（Debugf/Verbosef/Infof…）写日志；命令层在启动时根据
// --verbose/--debug 旗标决定放行哪几级。三级各自独立开关：debug 开含
// verbose 含默认。
package logcfg

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Level 是日志级别；数值越大越细节。
type Level int

const (
	// LevelInfo 是默认级别：What happened。
	LevelInfo Level = iota
	// LevelVerbose 是 --verbose 级别：What is happening。
	LevelVerbose
	// LevelDebug 是 --debug 级别：Why is it happening。
	LevelDebug
)

// Logger 是分级日志器：Info 恒输出，Verbose/Debug 按放行级别输出。并发安全。
type Logger struct {
	mu      sync.Mutex
	writer  io.Writer
	level   Level
	prefix  string
	clock   func() time.Time
	verbose bool
	debug   bool
}

// New 构造 Logger。verbose/debug 为命令旗标（--debug 蕴含 --verbose）。
func New(writer io.Writer, prefix string, verbose, debug bool) *Logger {
	if debug {
		verbose = true
	}
	return &Logger{
		writer:  writer,
		level:   LevelInfo,
		prefix:  prefix,
		clock:   time.Now,
		verbose: verbose,
		debug:   debug,
	}
}

// Enabled 报告该级别当前是否放行（调用方可用它跳过昂贵的参数拼装）。
func (l *Logger) Enabled(level Level) bool {
	if l == nil {
		return false
	}
	switch level {
	case LevelInfo:
		return true
	case LevelVerbose:
		return l.verbose
	case LevelDebug:
		return l.debug
	}
	return false
}

func (l *Logger) write(level Level, format string, args ...any) {
	if !l.Enabled(level) {
		return
	}
	message := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.writer, "[%s %s] %s\n",
		l.prefix, l.clock().UTC().Format("2006-01-02T15:04:05.000Z"), message)
}

// Infof 输出默认级日志（What happened）：状态、结果、警告、错误。
func (l *Logger) Infof(format string, args ...any) {
	if l == nil {
		return
	}
	l.write(LevelInfo, format, args...)
}

// Verbosef 输出 verbose 级日志（What is happening）：步骤、进度、外部调用。
func (l *Logger) Verbosef(format string, args ...any) {
	if l == nil {
		return
	}
	l.write(LevelVerbose, format, args...)
}

// Debugf 输出 debug 级日志（Why is it happening）：内部决策、参数、诊断。
func (l *Logger) Debugf(format string, args ...any) {
	if l == nil {
		return
	}
	l.write(LevelDebug, format, args...)
}

// Warnf 是 Info 级的警告（警告属于 What happened，任何级别都可见）。
func (l *Logger) Warnf(format string, args ...any) {
	l.Infof(format, args...)
}

// ToFunc 把 Logger 折成单一回调（既有以 func(string) 注入的接口沿用；
// verbose/debug 行的调用方自行前缀区分时由该回调过滤）。
func (l *Logger) ToFunc(level Level) func(string) {
	return func(line string) { l.write(level, "%s", line) }
}
