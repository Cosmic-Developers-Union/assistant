package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/Cosmic-Developers-Union/assistant/internal/claude"
)

// stubRunner 是测试用的 claude.Runner：把预置的 stdout 逐行喂给 onLine，并记录
// 本次调用的 argv / 工作目录 / 环境。
//
// 它取代了原先的 ChatConfig.RunClaude 旁路——那条路把整段 stdout 一次性交回，
// 绕过了真实的 spawn 与逐行交付路径；只认 Runner 接口后，被测的是生产的同一条
// 路径（逐行 Feed），测试仍不必真的拉起 claude。
type stubRunner struct {
	mu sync.Mutex
	// Output 是本轮要交付的 stdout（按行拆分）。
	Output string
	// Err 非空时 Run 返回它（模拟 spawn 失败 / 非零退出 / 超时）。
	Err error
	// OnRun 非空时在交付输出前调用（测试可按调用次数改写行为）。
	OnRun func(call int)

	// 以下是记录，**一律经访问器读**（calls/dirs/envs/lastArgs/lastDir/lastEnv）：
	// Run 在会话 goroutine 上写它们，测试在另一 goroutine 上读，直接读字段会被
	// -race 判为数据竞争（曾如此：测试轮询 len(stub.Calls) 等会话启动）。
	calls [][]string
	dirs  []string
	envs  [][]string
}

// calls 返回每次调用的 argv 副本（并发安全）。
func (s *stubRunner) callsSnapshot() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.calls...)
}

// callCount 返回已发生的调用次数（测试等待会话启动时轮询它）。
func (s *stubRunner) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// lastArgs 返回最近一次调用的 argv（无调用时返回 nil）。
func (s *stubRunner) lastArgs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return nil
	}
	return append([]string(nil), s.calls[len(s.calls)-1]...)
}

// dirsSnapshot 返回每次调用的工作目录副本（并发安全）。
func (s *stubRunner) dirsSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.dirs...)
}

// envsSnapshot 返回每次调用的注入环境副本（并发安全）。
func (s *stubRunner) envsSnapshot() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.envs...)
}

// lastDir 返回最近一次调用的工作目录。
func (s *stubRunner) lastDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.dirs) == 0 {
		return ""
	}
	return s.dirs[len(s.dirs)-1]
}

// lastEnv 返回最近一次调用的注入环境。
func (s *stubRunner) lastEnv() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.envs) == 0 {
		return nil
	}
	return append([]string(nil), s.envs[len(s.envs)-1]...)
}

func (s *stubRunner) Run(ctx context.Context, spec claude.Spec, onLine func([]byte)) error {
	s.mu.Lock()
	index := len(s.calls)
	s.calls = append(s.calls, append([]string(nil), spec.Args...))
	s.dirs = append(s.dirs, spec.Dir)
	s.envs = append(s.envs, append([]string(nil), spec.Env...))
	output, runErr := s.Output, s.Err
	onRun := s.OnRun
	s.mu.Unlock()

	if onRun != nil {
		onRun(index)
		s.mu.Lock()
		output, runErr = s.Output, s.Err
		s.mu.Unlock()
	}
	if onLine != nil && output != "" {
		for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
			onLine([]byte(line))
		}
	}
	if runErr != nil {
		return runErr
	}
	return ctx.Err()
}

// runnerOutput 构造一个只交付给定 stdout 的 Runner（等价于原先返回整段输出的
// RunClaude 注入）。
func runnerOutput(output string) *stubRunner { return &stubRunner{Output: output} }

// runnerSuccess 构造交付一条成功 result 的 Runner。
//
// result 用 encoding/json 编码而不是字符串拼接：结果文本里常有换行、引号与反斜杠
// （如多行回复），拼接出来的 JSON 不合法，且换行会把一行 stream-json 撕成两行，
// 解析端只会看到半截。
func runnerSuccess(result string) *stubRunner {
	encoded, err := json.Marshal(struct {
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}{Subtype: "success", IsError: false, Result: result})
	if err != nil {
		panic("runnerSuccess 编码 result 失败: " + err.Error())
	}
	return runnerOutput(string(encoded))
}

// runnerFailure 构造返回错误的 Runner（模拟进程层失败，此时没有 stdout）。
func runnerFailure(err error) *stubRunner { return &stubRunner{Err: err} }

// errClaudeStub 是 stubRunner 的缺省失败原因。
var errClaudeStub = errors.New("stub runner failure")
