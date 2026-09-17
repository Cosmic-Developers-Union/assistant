//go:build !windows

package dispatcher

import (
	"os"
	"syscall"
)

// pidAlive 探测进程存活（signal 0 不实际发送）。
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// terminateProcess 优雅终止会话进程（SIGTERM，随后由调用方超时 kill 兜底）。
func terminateProcess(process *os.Process) error {
	return process.Signal(syscall.SIGTERM)
}

// processSignaled 返回进程是否被信号终止及信号名；windows 无此概念。
func processSignaled(state *os.ProcessState) (bool, string) {
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return false, ""
	}
	return true, signalName(status.Signal())
}
