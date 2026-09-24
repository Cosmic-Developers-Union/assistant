package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain 把整个测试二进制的工作目录与 XDG 配置目录切到临时目录：显式模式的
// 缺省路径（./config.json、./daemon.json、./data）都锚定 cwd；凭据库走平台标准
// 配置目录（XDG_CONFIG_HOME）。不隔离的话测试会把产物写进源码树或真实 ~/.config。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "assistant-cmd-tests")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(dir); err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg-config"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
