package repoinstall

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"assistant/internal/dispatcher"
	"assistant/internal/status"
)

// MCP 包装层默认值（与 gitea-mcp 的 stdio 用法一致）。
const (
	DefaultMCPModule = "gitea.com/gitea/gitea-mcp@latest"
	DefaultMCPScopes = "wiki,actions,issue,pull_request,commit,branch,tag,search,label,milestone,packages"
)

// MCPOptions 控制 `assistant mcp gitea` 的解析与启动。
type MCPOptions struct {
	// Dir 是项目目录（默认 cwd），用于从 git remote 推导站点。
	Dir string
	// ConfigPath 指定 assistant login 的实例配置；缺省 ASSISTANT_CONFIG / 标准目录。
	ConfigPath string
	// Host / Token 显式覆盖自动检测。
	Host  string
	Token string
	// Probe 覆盖 Gitea 站点探测（测试注入）；默认请求 /api/v1/version。
	Probe func(host string) bool
	// Getenv 便于测试注入；默认 os.Getenv。
	Getenv func(string) string
	// Stdin/Stdout/Stderr 便于测试注入；默认进程标准流。
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Exit 便于测试注入；默认 os.Exit（子进程退出码透传）。
	Exit func(int)
	Log  func(string, ...any)
}

// MCPSpec 是解析结果。
type MCPSpec struct {
	Host        string
	Token       string
	HostSource  string
	TokenSource string
}

// ResolveMCP 自动检测项目的 Gitea 实例与当前开发者的访问令牌：
//
// host：--host > GITEA_HOST > Gitea remote 推导；
// token：--token > GITEA_ACCESS_TOKEN > GITEA_ACCESS_TOKEN_FILE >
// assistant login --mcp 同站点独立凭据。
// 不自动读取未绑定站点的历史全局 token 文件。
func ResolveMCP(ctx context.Context, options MCPOptions) (MCPSpec, error) {
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	dir := options.Dir
	if dir == "" {
		dir = "."
	}
	spec := MCPSpec{}
	var err error
	// host：显式参数/环境变量优先；否则在多个 remote 中探测 Gitea（origin 优先，
	// GitHub/GitLab 等会被跳过）
	if options.Host != "" {
		spec.Host, spec.HostSource = strings.TrimRight(options.Host, "/"), "--host"
	} else if envHost := strings.TrimSpace(getenv("GITEA_HOST")); envHost != "" {
		spec.Host, spec.HostSource = strings.TrimRight(envHost, "/"), "GITEA_HOST"
	} else {
		probe := options.Probe
		if probe == nil {
			probe = func(host string) bool { return status.ProbeGitea(ctx, host) }
		}
		if remote, ok := dispatcher.SelectGiteaRemote(dir, probe); ok {
			spec.Host, spec.HostSource = remote.Host, "remote "+remote.Name
		}
	}
	if spec.Host == "" {
		return MCPSpec{}, fmt.Errorf(
			"无法检测 Gitea 实例：用 --host / GITEA_HOST，或在带 Gitea remote 的检出内运行")
	}
	// token
	if options.Token != "" {
		spec.Token, spec.TokenSource = strings.TrimSpace(options.Token), "--token"
	} else {
		spec.Token, spec.TokenSource, err = resolveMCPToken(spec.Host, options.ConfigPath, getenv)
		if err != nil {
			return MCPSpec{}, err
		}
	}
	if spec.Token == "" {
		return MCPSpec{}, fmt.Errorf("站点 %s 没有匹配的登录凭据：请运行 assistant login %s --mcp；也可显式设置 GITEA_ACCESS_TOKEN / GITEA_ACCESS_TOKEN_FILE（不再自动读取历史全局 token 文件）", spec.Host, spec.Host)
	}
	return spec, nil
}

// RunMCPGitea 解析凭据后启动 gitea-mcp（stdio），并把子进程退出码透传。
func RunMCPGitea(ctx context.Context, options MCPOptions) error {
	spec, err := ResolveMCP(ctx, options)
	if err != nil {
		return err
	}
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	logf := options.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	command, args := mcpCommand(getenv)
	logf("gitea MCP：host=%s（%s） token=%s（%s）",
		spec.Host, spec.HostSource, maskToken(spec.Token), spec.TokenSource)

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = options.Dir
	if cmd.Dir == "" {
		cmd.Dir = "."
	}
	cmd.Env = append(os.Environ(),
		"GITEA_HOST="+spec.Host,
		"GITEA_ACCESS_TOKEN="+spec.Token,
	)
	cmd.Stdin = options.Stdin
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = options.Stdout
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	cmd.Stderr = options.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	err = cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		exit := options.Exit
		if exit == nil {
			exit = os.Exit
		}
		exit(exitErr.ExitCode())
		return nil
	}
	return err
}

// mcpCommand 返回 gitea-mcp 的启动命令：GITEA_MCP_BIN 直接指二进制，
// 否则 `go run <module>`（默认官方模块 @latest）。
func mcpCommand(getenv func(string) string) (string, []string) {
	scopes := strings.TrimSpace(getenv("GITEA_MCP_SCOPES"))
	if scopes == "" {
		scopes = DefaultMCPScopes
	}
	if binary := strings.TrimSpace(getenv("GITEA_MCP_BIN")); binary != "" {
		return binary, []string{"-t", "stdio", "-S", scopes}
	}
	module := strings.TrimSpace(getenv("GITEA_MCP_MODULE"))
	if module == "" {
		module = DefaultMCPModule
	}
	return "go", []string{"run", module, "-t", "stdio", "-S", scopes}
}

func maskToken(token string) string {
	if len(token) <= 8 {
		return "***"
	}
	return token[:4] + "***" + token[len(token)-4:]
}
