//go:build !windows

package claude

import (
	"os/exec"
	"syscall"
	"testing"
)

// signalName 的映射：常见信号给固定名，其余用系统默认表示（读日志的人据此判断
// 是超时终止还是崩溃）。
func TestSignalName(t *testing.T) {
	for signal, want := range map[int]string{
		int(syscall.SIGTERM): "SIGTERM",
		int(syscall.SIGKILL): "SIGKILL",
		int(syscall.SIGINT):  "SIGINT",
	} {
		if got := signalName(syscall.Signal(signal)); got != want {
			t.Errorf("signalName(%d) = %q, want %q", signal, got, want)
		}
	}
	// SIGUSR1 不在固定表里，回退到系统默认表示（非空即可）
	if got := signalName(syscall.SIGUSR1); got == "" {
		t.Error("未知信号也应有可读表示")
	}
}

// ProcessSignaled 识别被信号终止的进程；正常退出的进程返回 false。
func TestProcessSignaled(t *testing.T) {
	// 真实跑一个被 SIGTERM 终止的进程
	command := exec.Command("sh", "-c", "kill -TERM $$; sleep 5")
	_ = command.Run()
	if command.ProcessState == nil {
		t.Skip("进程未产生退出状态")
	}
	if signaled, name := ProcessSignaled(command.ProcessState); !signaled {
		t.Error("被 SIGTERM 终止的进程应判为 signaled")
	} else if name != "SIGTERM" {
		t.Errorf("信号名 = %q, want SIGTERM", name)
	}

	// 正常退出：不是 signaled
	ok := exec.Command("sh", "-c", "exit 0")
	_ = ok.Run()
	if signaled, _ := ProcessSignaled(ok.ProcessState); signaled {
		t.Error("正常退出不应判为 signaled")
	}
}
