package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// acquireRoot 将不同进程对同一运行根的所有权互斥；退出或崩溃由内核释放锁。
func acquireRoot(root string) (func(), error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("创建运行根: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(root, "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开运行锁: %w", err)
	}
	unlock, err := lockFile(file)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("运行根已被占用或无法锁定: %w", err)
	}
	// 不 unlink：仍持锁的文件一旦被删除，另一个进程可锁住同名的新 inode。
	return func() { unlock(); file.Close() }, nil
}
