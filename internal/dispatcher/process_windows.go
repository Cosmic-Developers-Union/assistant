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
