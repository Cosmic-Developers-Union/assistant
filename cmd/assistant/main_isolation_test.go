package main

import (
	"os"
	"testing"
)

// TestMain 把整个测试二进制的工作目录切到临时目录：显式模式的缺省路径
// （./config.json、./credentials.json、./daemon.json、./data）都锚定 cwd，
// 不隔离的话测试会把产物写进源码树。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "assistant-cmd-tests")
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
