package daemon

import (
	"os"
	"testing"
)

// TestMain 把整个测试二进制的工作目录切到临时目录：端点文件等缺省路径锚定
// cwd，不隔离的话 Serve 的测试产物会写进源码树。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "assistant-daemon-tests")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
