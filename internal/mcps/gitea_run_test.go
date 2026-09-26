package mcps

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// maskGiteaToken 的边界：短令牌全掩码，长令牌只暴露首尾 4 位——令牌不落日志
// 是硬要求，边界算错就等于泄漏。
func TestMaskGiteaToken(t *testing.T) {
	for _, test := range []struct {
		name  string
		token string
		want  string
	}{
		{name: "空", token: "", want: "***"},
		{name: "恰好 8 位全掩码", token: "12345678", want: "***"},
		{name: "7 位全掩码", token: "1234567", want: "***"},
		{name: "9 位露出首尾 4 位", token: "123456789", want: "1234***6789"},
		{name: "长令牌", token: "abcdefghijklmnop", want: "abcd***mnop"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := maskGiteaToken(test.token)
			if got != test.want {
				t.Errorf("maskGiteaToken(%q) = %q, want %q", test.token, got, test.want)
			}
			// 中间段必须被遮住：不能出现完整令牌
			if test.token != "" && strings.Contains(got, test.token) {
				t.Errorf("掩码后仍含完整令牌：%q", got)
			}
		})
	}
}

// runGiteaOptions 构造一份可注入的启动选项：假二进制、假退出、缓冲输出。
// getenv 只认表里给出的键，避免读到宿主环境。
func runGiteaOptions(t *testing.T, env map[string]string, exit func(int)) (GiteaOptions, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	options := GiteaOptions{
		Dir:    t.TempDir(),
		Host:   "https://gitea.example.com",
		Token:  "long-enough-token-value",
		Getenv: func(key string) string { return env[key] },
		Stdin:  strings.NewReader(""),
		Stdout: stdout,
		Stderr: stderr,
		Exit:   exit,
	}
	return options, stdout, stderr
}

// writeFakeBin 写一个可执行的假 gitea-mcp：按脚本决定退出码与输出。
func writeFakeBin(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-gitea-mcp")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// RunGitea 把解析出的 host 与令牌通过环境变量注入子进程，并把子进程退出码
// 透传给调用方（不经 error 返回——退出码是 MCP 协议的一部分）。
func TestRunGiteaInjectsCredentialsAndPassesExitCode(t *testing.T) {
	// 假二进制把注入的环境变量回显到 stdout，便于断言注入内容
	binary := writeFakeBin(t, "#!/bin/sh\necho \"HOST=$GITEA_HOST\"\necho \"TOKEN=$GITEA_ACCESS_TOKEN\"\nexit 3\n")

	var exitCode int
	var exitCalled bool
	options, stdout, _ := runGiteaOptions(t, map[string]string{"GITEA_MCP_BIN": binary}, func(code int) {
		exitCalled = true
		exitCode = code
	})

	err := RunGitea(t.Context(), options)
	if err != nil {
		t.Fatalf("RunGitea() error = %v, want nil（退出码走 Exit 透传）", err)
	}
	if !exitCalled {
		t.Fatal("Exit 未被调用，want 透传子进程退出码")
	}
	if exitCode != 3 {
		t.Errorf("exit code = %d, want 3", exitCode)
	}
	output := stdout.String()
	if !strings.Contains(output, "HOST=https://gitea.example.com") {
		t.Errorf("子进程未收到 GITEA_HOST：%q", output)
	}
	if !strings.Contains(output, "TOKEN=long-enough-token-value") {
		t.Errorf("子进程未收到 GITEA_ACCESS_TOKEN：%q", output)
	}
}

// 日志里的令牌必须是掩码形态（运行诊断可读，但不可泄漏凭据）。
func TestRunGiteaMasksTokenInLog(t *testing.T) {
	binary := writeFakeBin(t, "#!/bin/sh\nexit 0\n")
	options, _, _ := runGiteaOptions(t, map[string]string{"GITEA_MCP_BIN": binary}, func(int) {})
	var logs []string
	options.Log = func(format string, args ...any) {
		logs = append(logs, format)
		for _, arg := range args {
			logs = append(logs, arg.(string))
		}
	}

	if err := RunGitea(t.Context(), options); err != nil {
		t.Fatalf("RunGitea() error = %v", err)
	}
	joined := strings.Join(logs, " ")
	if !strings.Contains(joined, "long***alue") {
		t.Errorf("日志未记录掩码令牌：%q", joined)
	}
	if strings.Contains(joined, "long-enough-token-value") {
		t.Errorf("日志泄漏了完整令牌：%q", joined)
	}
}

// 子进程正常退出（0）时不调用 Exit，直接返回 nil。
func TestRunGiteaReturnsNilOnSuccess(t *testing.T) {
	binary := writeFakeBin(t, "#!/bin/sh\nexit 0\n")
	options, _, _ := runGiteaOptions(t, map[string]string{"GITEA_MCP_BIN": binary}, func(int) {
		t.Error("退出码为 0 时不应调用 Exit")
	})

	if err := RunGitea(t.Context(), options); err != nil {
		t.Errorf("RunGitea() error = %v, want nil", err)
	}
}

// 命令不存在等非 ExitError 的失败按普通 error 返回（不是退出码语义）。
func TestRunGiteaReturnsErrorWhenBinaryMissing(t *testing.T) {
	options, _, _ := runGiteaOptions(t, map[string]string{
		"GITEA_MCP_BIN": filepath.Join(t.TempDir(), "does-not-exist"),
	}, func(int) {
		t.Error("二进制缺失不应走退出码透传")
	})

	if err := RunGitea(t.Context(), options); err == nil {
		t.Error("RunGitea() error = nil, want 启动失败")
	}
}

// 缺凭据时在启动前就失败：不带病起一个必然认证失败的 MCP 进程。
func TestRunGiteaFailsBeforeLaunchWithoutToken(t *testing.T) {
	binary := writeFakeBin(t, "#!/bin/sh\nexit 0\n")
	options, _, _ := runGiteaOptions(t, map[string]string{"GITEA_MCP_BIN": binary}, func(int) {})
	options.Token = ""
	t.Setenv("GITEA_ACCESS_TOKEN", "")
	t.Setenv("GITEA_ACCESS_TOKEN_FILE", "")

	if err := RunGitea(t.Context(), options); err == nil {
		t.Fatal("RunGitea() error = nil, want 缺令牌错误")
	}
}

// 子进程 stdout 的输出被透传（MCP 走 stdio，输出必须直达调用方）。
func TestRunGiteaPassesStdoutThrough(t *testing.T) {
	binary := writeFakeBin(t, "#!/bin/sh\necho '{\"jsonrpc\":\"2.0\"}'\nexit 0\n")
	options, stdout, _ := runGiteaOptions(t, map[string]string{"GITEA_MCP_BIN": binary}, func(int) {})

	if err := RunGitea(t.Context(), options); err != nil {
		t.Fatalf("RunGitea() error = %v", err)
	}
	if !strings.Contains(stdout.String(), "jsonrpc") {
		t.Errorf("stdout 未被透传：%q", stdout.String())
	}
}

// giteaCommand 的命令构造：GITEA_MCP_BIN 优先；否则 go run 模块；scopes 可覆盖。
func TestGiteaCommand(t *testing.T) {
	for _, test := range []struct {
		name     string
		env      map[string]string
		wantCmd  string
		wantArgs []string
	}{
		{
			name:     "默认走 go run 官方模块",
			env:      map[string]string{},
			wantCmd:  "go",
			wantArgs: []string{"run", DefaultGiteaModule, "-t", "stdio", "-S", defaultGiteaScopeList()},
		},
		{
			name:     "GITEA_MCP_BIN 优先于模块",
			env:      map[string]string{"GITEA_MCP_BIN": "/usr/local/bin/gitea-mcp"},
			wantCmd:  "/usr/local/bin/gitea-mcp",
			wantArgs: []string{"-t", "stdio", "-S", defaultGiteaScopeList()},
		},
		{
			name:     "GITEA_MCP_MODULE 覆盖模块",
			env:      map[string]string{"GITEA_MCP_MODULE": "example.com/mcp@v1"},
			wantCmd:  "go",
			wantArgs: []string{"run", "example.com/mcp@v1", "-t", "stdio", "-S", defaultGiteaScopeList()},
		},
		{
			name:     "GITEA_MCP_SCOPES 覆盖范围",
			env:      map[string]string{"GITEA_MCP_SCOPES": "issue,user"},
			wantCmd:  "go",
			wantArgs: []string{"run", DefaultGiteaModule, "-t", "stdio", "-S", "issue,user"},
		},
		{
			name:     "空白值不算配置",
			env:      map[string]string{"GITEA_MCP_BIN": "   ", "GITEA_MCP_SCOPES": "  "},
			wantCmd:  "go",
			wantArgs: []string{"run", DefaultGiteaModule, "-t", "stdio", "-S", defaultGiteaScopeList()},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			getenv := func(key string) string { return test.env[key] }
			command, args := giteaCommand(getenv)
			if command != test.wantCmd {
				t.Errorf("command = %q, want %q", command, test.wantCmd)
			}
			if strings.Join(args, " ") != strings.Join(test.wantArgs, " ") {
				t.Errorf("args = %v, want %v", args, test.wantArgs)
			}
		})
	}
}

// errors.Is / errors.As 的错误链：ExitError 之外的错误必须原样返回，
// 不能被误判成退出码语义而静默吞掉。
func TestRunGiteaDoesNotSwallowOtherErrors(t *testing.T) {
	options, _, _ := runGiteaOptions(t, map[string]string{
		"GITEA_MCP_BIN": filepath.Join(t.TempDir(), "missing"),
	}, func(int) { t.Error("不应调用 Exit") })

	err := RunGitea(t.Context(), options)
	if err == nil {
		t.Fatal("error = nil, want 启动失败")
	}
	var exitErr *os.PathError
	if !errors.As(err, &exitErr) {
		t.Errorf("error = %v (%T), want 保留 *os.PathError", err, err)
	}
}
