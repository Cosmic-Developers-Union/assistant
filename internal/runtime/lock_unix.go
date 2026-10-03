//go:build unix

package runtime

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockFile(file *os.File) (func(), error) {
	fd := int(file.Fd())
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, err
	}
	return func() { _ = unix.Flock(fd, unix.LOCK_UN) }, nil
}
