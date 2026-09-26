package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/spf13/cobra"
)

// TestConfigTargetPathPrecedence 断言配置落点的三级优先级：--config 显式路径 >
// ASSISTANT_CONFIG 环境变量 > 平台标准位置。写错优先级会让 `--config a.json`
// 悄悄改到环境变量指向的 b.json，操作者事后才发现动错了文件。
func TestConfigTargetPathPrecedence(t *testing.T) {
	t.Setenv("ASSISTANT_CONFIG", "/tmp/env-config.json")

	got, err := configTargetPath("  /tmp/flag-config.json  ")
	if err != nil {
		t.Fatalf("configTargetPath: %v", err)
	}
	if got != "/tmp/flag-config.json" {
		t.Errorf("--config 应优先且去空白，got %q", got)
	}

	got, err = configTargetPath("   ")
	if err != nil {
		t.Fatalf("configTargetPath: %v", err)
	}
	if got != "/tmp/env-config.json" {
		t.Errorf("空 --config 应回退 ASSISTANT_CONFIG，got %q", got)
	}

	t.Setenv("ASSISTANT_CONFIG", "   ")
	got, err = configTargetPath("")
	if err != nil {
		t.Fatalf("configTargetPath: %v", err)
	}
	want, err := instances.DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("全空应回退平台标准位置 %q，got %q", want, got)
	}
}

// TestPrintConfigChangesRendersEmptyAndLines 断言改动清单的两种渲染：空清单打
// 「（无改动）」而不是什么都不打——静默的成功会让操作者以为命令没跑；有改动时
// 每行统一缩进两格，便于与正文其它输出对齐区分。
func TestPrintConfigChangesRendersEmptyAndLines(t *testing.T) {
	empty := &bytes.Buffer{}
	printConfigChanges(empty, nil)
	if !strings.Contains(empty.String(), "（无改动）") {
		t.Errorf("空清单应打（无改动），got %q", empty.String())
	}

	out := &bytes.Buffer{}
	printConfigChanges(out, []string{"+ providers.minimax", "~ default_provider: \"\" → \"minimax\""})
	want := "  + providers.minimax\n  ~ default_provider: \"\" → \"minimax\"\n"
	if out.String() != want {
		t.Errorf("渲染 = %q, want %q", out.String(), want)
	}
}

// TestReadAPIKeyFromPipeTrimsAndAcceptsEOF 断言管道输入的读取口径：末尾换行要能
// 容忍，且没有换行（EOF）时也要接受——`echo -n <key>` 正是这种没有换行的写法，
// 把它当成读失败会让文档里推荐的用法直接不可用。
func TestReadAPIKeyFromPipeTrimsAndAcceptsEOF(t *testing.T) {
	cases := []struct {
		name  string
		stdin string
		want  string
	}{
		{"带换行", "sk-abc123\n", "sk-abc123"},
		{"无换行（echo -n）", "sk-abc123", "sk-abc123"},
		{"去首尾空白", "   sk-abc123  \n", "sk-abc123"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			command := &cobra.Command{}
			command.SetIn(strings.NewReader(testCase.stdin))
			got, err := readAPIKey(command)
			if err != nil {
				t.Fatalf("readAPIKey: %v", err)
			}
			if got != testCase.want {
				t.Errorf("key = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestReadAPIKeyRejectsEmptyPipe 断言管道里读到空内容时给出带用法示例的错误：
// 空密钥如果被当成合法值写进配置，后续所有请求都会以难以定位的鉴权失败收场。
func TestReadAPIKeyRejectsEmptyPipe(t *testing.T) {
	for _, stdin := range []string{"", "\n", "   \n"} {
		command := &cobra.Command{}
		command.SetIn(strings.NewReader(stdin))
		_, err := readAPIKey(command)
		if err == nil {
			t.Fatalf("stdin = %q 时应报错", stdin)
		}
		if !strings.Contains(err.Error(), "没有读到 api_key") {
			t.Errorf("err = %v, want 含「没有读到 api_key」", err)
		}
		if !strings.Contains(err.Error(), "echo -n <key>") {
			t.Errorf("err = %v, want 含用法示例 echo -n <key>", err)
		}
	}
}

// TestReadAPIKeyReportsReadError 断言底层读错误不会退化为「空密钥」：把读取故障
// 报成「没读到」会把一个 I/O 问题伪装成用户输入问题，掩盖真正的原因。
func TestReadAPIKeyReportsReadError(t *testing.T) {
	command := &cobra.Command{}
	command.SetIn(errorReader{err: errors.New("管道断了")})
	_, err := readAPIKey(command)
	if err == nil || !strings.Contains(err.Error(), "管道断了") {
		t.Errorf("err = %v, want 含底层「管道断了」", err)
	}
}

// errorReader 是一个永远失败的读端，用来驱动 readAPIKey 的读错误分支。
type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

// TestLoadOptionalConfigDistinguishesMissingFromBroken 断言「文件不存在」与
// 「文件坏了」被区别对待：不存在返回 (nil, nil) 表示首次生成；语法坏掉必须上抛，
// 否则 config init 会在一个坏文件上「补全」出一份更坏的配置。
func TestLoadOptionalConfigDistinguishesMissingFromBroken(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	file, err := loadOptionalConfig(missing)
	if err != nil || file != nil {
		t.Fatalf("不存在的文件应得 (nil, nil)，got (%v, %v)", file, err)
	}

	broken := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(broken, []byte("{ 这不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOptionalConfig(broken); err == nil {
		t.Error("语法错误的配置应上抛错误")
	}
}

// TestBackupConfigCopiesContentWithTightMode 断言备份是原样复制且权限收紧到 0600：
// 配置里含 api_key 等密钥，备份若沿用宽松权限等于把密钥泄漏出去。
func TestBackupConfigCopiesContentWithTightMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := []byte(`{"default_provider":"minimax"}`)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := backupConfig(path); err != nil {
		t.Fatalf("backupConfig: %v", err)
	}
	got, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("备份 = %q, want 原样 %q", got, content)
	}
	info, err := os.Stat(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("备份权限 = %#o, want 0600", mode)
	}

	if err := backupConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("源文件不存在时应报错")
	}
}
