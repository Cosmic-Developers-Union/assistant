//go:build windows

package claude

import "os"

// TerminateProcess 优雅终止会话进程：Windows 不支持 SIGTERM，直接 Kill。
func TerminateProcess(process *os.Process) error {
	return process.Kill()
}

// ProcessSignaled 返回进程是否被信号终止及信号名；Windows 无信号语义。
func ProcessSignaled(*os.ProcessState) (bool, string) {
	return false, ""
}

// terminateProcess 是 execRunner 的缺省终止动作。
func terminateProcess(process *os.Process) error { return TerminateProcess(process) }

// processExited 报告进程是否已退出（跟随 stdout 的收尾条件）。
func processExited(process *os.Process) bool {
	if process == nil {
		return true
	}
	return process.Signal(os.Interrupt) != nil
}
