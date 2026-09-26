//go:build !windows

package dispatcher

import "syscall"

// pidAlive 探测进程存活（signal 0 不实际发送）。
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
