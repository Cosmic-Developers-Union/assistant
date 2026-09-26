package main

import (
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

// TestRandomTokenIsURLSafeAndUnique 断言自动生成的访问令牌是 32 位十六进制：
// 这个值会被写进 serve.json 并当作 HTTP 头传递，必须只用 URL/头部安全的字符；
// 同时又必须是随机的——两次生成相同意味着令牌可预测，等于没有鉴权。
func TestRandomTokenIsURLSafeAndUnique(t *testing.T) {
	first, err := randomToken()
	if err != nil {
		t.Fatalf("randomToken: %v", err)
	}
	raw, err := hex.DecodeString(first)
	if err != nil {
		t.Fatalf("令牌 %q 不是十六进制：%v", first, err)
	}
	if len(raw) != 16 {
		t.Errorf("令牌熵 = %d 字节（令牌 %q），want 16", len(raw), first)
	}
	if strings.ContainsAny(first, " \t\r\n/+=?&") {
		t.Errorf("令牌 %q 含不安全字符（会被当作头部/URL 传递）", first)
	}

	second, err := randomToken()
	if err != nil {
		t.Fatalf("randomToken 第二次: %v", err)
	}
	if first == second {
		t.Errorf("两次生成同一令牌 %q：随机源没有生效", first)
	}
}

// TestDefaultSessionsRootFollowsWorkingDirectory 断言缺省记录库落在「当前目录的
// data/sessions」：显式模式的约定是一切产物收在配置旁边，锚定 cwd 才能让
// assistant serve 不写配置就落在项目自己的目录里，而不是落进用户主目录。
func TestDefaultSessionsRootFollowsWorkingDirectory(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)

	root, err := defaultSessionsRoot()
	if err != nil {
		t.Fatalf("defaultSessionsRoot: %v", err)
	}
	want := filepath.Join(work, "data", "sessions")
	if root != want {
		t.Errorf("root = %q, want %q", root, want)
	}
}
