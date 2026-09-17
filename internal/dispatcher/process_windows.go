//go:build windows

package dispatcher

import (
	"os"

	"golang.org/x/sys/windows"
)

// pidAlive 探测进程存活：Windows 没有 signal 0，用查询句柄判断。
func pidAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = windows.CloseHandle(handle)
	return true
}

// terminateProcess 优雅终止会话进程：Windows 不支持 SIGTERM，直接 Kill。
func terminateProcess(process *os.Process) error {
	return process.Kill()
}

// processSignaled 返回进程是否被信号终止及信号名；Windows 无信号语义。
func processSignaled(state *os.ProcessState) (bool, string) {
	return false, ""
}
