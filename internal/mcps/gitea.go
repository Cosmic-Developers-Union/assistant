// Package mcps 隔离 MCP 服务配置解析、身份选择与进程启动职责，避免运行时依赖仓库脚手架
// 和会话存储实现。
package mcps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/dispatcher"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
)

// DefaultGiteaModule 指定未配置二进制覆盖时使用的官方 Gitea MCP 模块。
const DefaultGiteaModule = "gitea.com/gitea/gitea-mcp@latest"

var defaultGiteaScopes = []string{
	"version",
	"user",
	"search",
	"notification",
	"label",
	"milestone",
	"wiki",
	"timetracking",
	"packages",
	"project",
	"issue",
	"pull_request",
	"actions",
	"repository",
	"file",
	"branch",
	"tag",
	"commit",
	"release",
}

func defaultGiteaScopeList() string {
	return strings.Join(defaultGiteaScopes, ",")
}

// GiteaOptions 控制 `assistant mcp gitea` 的解析与启动。
type GiteaOptions struct {
	// Dir 是项目目录（默认 cwd），用于从 git remote 推导站点。
	Dir string
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

// GiteaSpec 保存解析出的 Gitea 站点与令牌及其来源，供启动器记录诊断日志。
type GiteaSpec struct {
	Host        string
	Token       string
	HostSource  string
	TokenSource string
}

// ResolveGitea 自动检测项目的 Gitea 实例与当前开发者的访问令牌：
//
// host：--host > GITEA_HOST > 检出内的 Gitea remote（多上游时选探测命中的那个）
//
//	> config.json 里唯一启用的 gitea 通道 > 凭据库里唯一登记的站点；
//	仍定不下来（无上游且登记了多个平台）时显式报错，不猜。
//
// token：--token > GITEA_ACCESS_TOKEN > GITEA_ACCESS_TOKEN_FILE >
// credentials.json 中该站点的 (host, purpose=mcp) 登录令牌。
// 不自动读取全局 token 文件（config.json 里没有凭据）。
//
// remote 探测在前是为了解决多上游问题：检出同时挂 GitHub 与多个 Gitea remote
// 时，按「哪个 remote 是 Gitea」选站点。但 MCP 会随任意目录启动（Claude Code
// 等 AI CLI 的 MCP 配置），没有上游的检出不该让 MCP 起不来——登记状态
// （login 写入的 config/credentials）是剩下的唯一事实来源。
func ResolveGitea(ctx context.Context, options GiteaOptions) (GiteaSpec, error) {
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	dir := options.Dir
	if dir == "" {
		dir = "."
	}
	spec := GiteaSpec{}
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
		host, source, hostErr := resolveRegisteredGiteaHost(getenv, dir)
		if hostErr != nil {
			return GiteaSpec{}, hostErr
		}
		spec.Host, spec.HostSource = host, source
	}
	if spec.Host == "" {
		return GiteaSpec{}, fmt.Errorf(
			"无法检测 Gitea 实例：用 --host / GITEA_HOST 指定，先 assistant login add 登记平台，或在带 Gitea remote 的检出内运行")
	}
	// token
	if options.Token != "" {
		spec.Token, spec.TokenSource = strings.TrimSpace(options.Token), "--token"
	} else {
		spec.Token, spec.TokenSource, err = resolveGiteaToken(spec.Host, getenv)
		if err != nil {
			return GiteaSpec{}, err
		}
	}
	if spec.Token == "" {
		return GiteaSpec{}, fmt.Errorf("站点 %s 没有匹配的登录凭据：%s；"+
			"CI / 评审会话也可显式注入 GITEA_ACCESS_TOKEN 或 GITEA_ACCESS_TOKEN_FILE",
			spec.Host, missingGiteaHint(spec.Host))
	}
	return spec, nil
}

// missingGiteaHint 在缺凭据时给出可行动说明。区分两种状态：
//
//   - 已登记身份但没有 mcp 用途令牌（登录中断、手工编辑过凭据库）：点名账号，
//     否则「identity 有、mcp 没有」看起来像工具坏了；
//   - 完全没有身份：直接给出登录命令。
func missingGiteaHint(host string) string {
	login := "assistant login add " + host + " --user <账号>"
	credentialPath, err := credentials.Path()
	if err != nil {
		return "请运行 " + login
	}
	store, err := credentials.Load(credentialPath)
	if err != nil {
		return "请运行 " + login
	}
	if identity, ok := store.IdentityFor(host); ok {
		return fmt.Sprintf("已登记身份 @%s，但没有 (host, %s, mcp) 用途令牌：运行 assistant login add %s --user %s 派生",
			identity.User, identity.User, host, identity.User)
	}
	return "请运行 " + login
}

// resolveRegisteredGiteaHost 从登记状态推导站点：config.json 的 gitea 通道（唯一时）
// > 凭据库里登记过的站点（唯一时）。多平台不猜——站点选择必须是显式决定，用
// GITEA_HOST（如 MCP 配置的 env）指定。
func resolveRegisteredGiteaHost(getenv func(string) string, dir string) (string, string, error) {
	if hosts := configuredGiteaHosts(getenv, dir); len(hosts) > 0 {
		if len(hosts) == 1 {
			return hosts[0], "config.json", nil
		}
		return "", "", ambiguousGiteaHostError(hosts)
	}
	if hosts := credentialGiteaHosts(); len(hosts) > 0 {
		if len(hosts) == 1 {
			return hosts[0], "credentials.json", nil
		}
		return "", "", ambiguousGiteaHostError(hosts)
	}
	return "", "", nil
}

func ambiguousGiteaHostError(hosts []string) error {
	return fmt.Errorf("无法检测 Gitea 实例：检出内没有 Gitea remote，登记状态又有多个平台（%s）；请用 GITEA_HOST 显式指定（如 MCP 配置的 env）",
		strings.Join(hosts, "、"))
}

// configuredGiteaHosts 读配置里启用的 gitea 通道站点：ASSISTANT_CONFIG > 检出内的
// config.json。配置缺失或损坏时返回空——MCP 只是借用登记状态做兜底，配置
// 本身的问题由 validate/config 命令负责报错，不在 MCP 启动路径上拦人。
func configuredGiteaHosts(getenv func(string) string, dir string) []string {
	path := strings.TrimSpace(getenv("ASSISTANT_CONFIG"))
	if path == "" {
		path = filepath.Join(dir, "config.json")
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	file, err := instances.Load(path)
	if err != nil {
		return nil
	}
	var hosts []string
	for _, channel := range file.Channels {
		if channel.Type == instances.ChannelGitea && channel.IsEnabled() && channel.Host != "" {
			hosts = append(hosts, strings.TrimRight(channel.Host, "/"))
		}
	}
	return dedupeGiteaHosts(hosts)
}

// credentialGiteaHosts 读凭据库里登记过的站点。凭据库缺失/损坏时返回空：登录状态
// 的问题由 login/validate 负责，MCP 不在这里拦人。
func credentialGiteaHosts() []string {
	path, err := credentials.Path()
	if err != nil {
		return nil
	}
	store, err := credentials.Load(path)
	if err != nil {
		return nil
	}
	return store.Hosts()
}

// dedupeGiteaHosts 按规范化形式去重，保留首个原始写法（展示用）。
func dedupeGiteaHosts(hosts []string) []string {
	seen := map[string]bool{}
	kept := hosts[:0]
	for _, host := range hosts {
		key := credentials.NormalizeHost(host)
		if seen[key] {
			continue
		}
		seen[key] = true
		kept = append(kept, host)
	}
	return kept
}

// RunGitea 解析凭据后启动 gitea-mcp（stdio），并把子进程退出码透传。
func RunGitea(ctx context.Context, options GiteaOptions) error {
	spec, err := ResolveGitea(ctx, options)
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
	command, args := giteaCommand(getenv)
	logf("gitea MCP：host=%s（%s） token=%s（%s）",
		spec.Host, spec.HostSource, maskGiteaToken(spec.Token), spec.TokenSource)

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
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exit := options.Exit
		if exit == nil {
			exit = os.Exit
		}
		exit(exitErr.ExitCode())
		return nil
	}
	return err
}

// giteaCommand 返回 gitea-mcp 的启动命令：GITEA_MCP_BIN 直接指二进制，
// 否则 `go run <module>`（默认官方模块 @latest）。
func giteaCommand(getenv func(string) string) (string, []string) {
	scopes := strings.TrimSpace(getenv("GITEA_MCP_SCOPES"))
	if scopes == "" {
		scopes = defaultGiteaScopeList()
	}
	if binary := strings.TrimSpace(getenv("GITEA_MCP_BIN")); binary != "" {
		return binary, []string{"-t", "stdio", "-S", scopes}
	}
	module := strings.TrimSpace(getenv("GITEA_MCP_MODULE"))
	if module == "" {
		module = DefaultGiteaModule
	}
	return "go", []string{"run", module, "-t", "stdio", "-S", scopes}
}

func maskGiteaToken(token string) string {
	if len(token) <= 8 {
		return "***"
	}
	return token[:4] + "***" + token[len(token)-4:]
}
