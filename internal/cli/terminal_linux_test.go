//go:build linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

func terminalPair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skip("当前环境无伪终端", err)
	}
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}
func TestTerminalPromptsAndHiddenPassword(t *testing.T) {
	for _, entry := range []struct {
		input  string
		secret bool
		want   string
		fail   bool
	}{{"account\n", false, "account", false}, {"\n", false, "", true}, {"password\n", true, "password", false}, {"\n", true, "", true}, {"\x04", false, "", true}} {
		t.Run(fmt.Sprintf("secret=%t/input=%q", entry.secret, entry.input), func(t *testing.T) {
			master, slave := terminalPair(t)
			output := &promptNotification{ready: make(chan struct{})}
			cmd := &cobra.Command{}
			cmd.SetContext(t.Context())
			cmd.SetIn(slave)
			cmd.SetErr(output)
			prompt := newPrompts(cmd)
			go func() {
				<-output.ready
				_, _ = master.Write([]byte(entry.input))
			}()
			var result string
			var err error
			if entry.secret {
				result, err = prompt.secret("", "密码")
			} else {
				result, err = prompt.value("", "账号")
			}
			if (err != nil) != entry.fail || result != entry.want {
				t.Fatal(result, err)
			}
			if bytes.Contains(output.Bytes(), []byte("password")) {
				t.Fatal("密码被回显到日志")
			}
		})
	}
}

type promptNotification struct {
	bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (w *promptNotification) Write(data []byte) (int, error) {
	n, err := w.Buffer.Write(data)
	w.once.Do(func() { close(w.ready) })
	return n, err
}

func TestTerminalPromptCancellationRestoresState(t *testing.T) {
	for _, secret := range []bool{false, true} {
		for _, interrupt := range []bool{false, true} {
			t.Run(fmt.Sprintf("密码=%t/键盘取消=%t", secret, interrupt), func(t *testing.T) {
				master, slave := terminalPair(t)
				before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				output := &promptNotification{ready: make(chan struct{})}
				cmd := &cobra.Command{}
				cmd.SetContext(ctx)
				cmd.SetIn(slave)
				cmd.SetErr(output)
				prompt := newPrompts(cmd)
				done := make(chan error, 1)
				go func() {
					var err error
					if secret {
						_, err = prompt.secret("", "密码")
					} else {
						_, err = prompt.value("", "实例名")
					}
					done <- err
				}()
				select {
				case <-output.ready:
				case <-time.After(3 * time.Second):
					t.Fatal("未显示输入提示")
				}
				if interrupt {
					_, _ = master.Write([]byte("\x03"))
				} else {
					cancel()
				}
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatal("取消未传回命令", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("输入取消后仍阻塞")
				}
				after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
				if err != nil || *before != *after {
					t.Fatal("取消后终端状态未恢复", err)
				}
				// 释放上下文取消时仍在等待读取的 goroutine，避免测试遗留输入。
				_, _ = master.Write([]byte("\n"))
			})
		}
	}
}

func TestInstancePromptSignalExit(t *testing.T) {
	if os.Getenv("ASSISTANT_TEST_PROMPT_PROCESS") == "1" {
		cmd := NewRootCommand(os.Stdout, os.Stderr)
		cmd.SetArgs(strings.Fields(os.Getenv("ASSISTANT_TEST_PROMPT_ARGS")))
		os.Exit(Execute(cmd))
	}
	for _, secret := range []bool{false, true} {
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
			t.Run(fmt.Sprintf("密码=%t/信号=%s", secret, sig), func(t *testing.T) {
				master, slave := terminalPair(t)
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "credentials.json")
				args := "instance add gitea"
				label := "实例名: "
				if secret {
					args += " --name work --url https://example.com --username dev"
					label = "密码: "
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				process := exec.CommandContext(ctx, executable, "-test.run=^TestInstancePromptSignalExit$")
				process.Env = append(os.Environ(), "ASSISTANT_TEST_PROMPT_PROCESS=1", "ASSISTANT_TEST_PROMPT_ARGS="+args, "ASSISTANT_CREDENTIALS="+path)
				process.Stdin, process.Stdout, process.Stderr = slave, slave, slave
				if err := process.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = process.Process.Kill() })
				ready := make(chan struct{})
				go func() {
					var output string
					buffer := make([]byte, 1024)
					for !strings.Contains(output, label) {
						n, err := master.Read(buffer)
						output += string(buffer[:n])
						if err != nil {
							return
						}
					}
					close(ready)
				}()
				select {
				case <-ready:
				case <-ctx.Done():
					t.Fatal("子进程未显示输入提示")
				}
				if err := process.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				err = process.Wait()
				exit, ok := errors.AsType[*exec.ExitError](err)
				want := 128 + int(sig)
				if !ok || exit.ExitCode() != want || ctx.Err() != nil {
					t.Fatal("单次信号未及时退出", err, ctx.Err())
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("取消后写入凭据", err)
				}
			})
		}
	}
}
