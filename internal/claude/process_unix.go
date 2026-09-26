//go:build !windows

package claude

import (
	"errors"
	"os"
	"syscall"
)

// TerminateProcess 优雅终止会话进程（SIGTERM，随后由超时后的 Kill 兜底）。
func TerminateProcess(process *os.Process) error {
	return process.Signal(syscall.SIGTERM)
}

// ProcessSignaled 返回进程是否被信号终止及信号名；Windows 无此概念。
//
// state 必须非 nil（调用方都是进程已 Wait 过的路径，见 execRunner.Run 里
// ProcessState == nil 的前置分支）。
func ProcessSignaled(state *os.ProcessState) (bool, string) {
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return false, ""
	}
	return true, signalName(status.Signal())
}

// signalName 把信号折成可读名（常见信号给固定名，其余用系统默认表示）。
func signalName(signal syscall.Signal) string {
	switch signal {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGINT:
		return "SIGINT"
	}
	return signal.String()
}

// terminateProcess 是 execRunner 的缺省终止动作。
func terminateProcess(process *os.Process) error { return TerminateProcess(process) }

// processExited 报告进程是否已退出（跟随 stdout 的收尾条件）。
//
// 用 Signal(0) 探测：进程已死时返回 os.ErrProcessDone；仍在运行返回 nil（对无
// 权限的进程可能返回 EPERM，那也说明它活着）。
func processExited(process *os.Process) bool {
	if process == nil {
		return true
	}
	err := process.Signal(syscall.Signal(0))
	return errors.Is(err, os.ErrProcessDone)
}
