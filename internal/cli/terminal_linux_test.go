//go:build linux

package cli

import (
	"bytes"
	"fmt"
	"os"
	"testing"

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
			var output bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetIn(slave)
			cmd.SetErr(&output)
			prompt := newPrompts(cmd)
			_, _ = master.Write([]byte(entry.input))
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
