package claudecfg

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// probeTimeout 是探测 claude 可执行文件（--version / --help）的超时：探测只
// 用于给出可诊断的启动日志，绝不能拖住 daemon 启动。
const probeTimeout = 20 * time.Second

var (
	probeMu       sync.Mutex
	versionCache  = map[string]string{}
	versionProbed = map[string]bool{}
	bareCache     = map[string]bool{}
)

// ClaudeVersion 返回 claude 的版本行（如 "2.1.270 (Claude Code)"）；路径无效或
// 执行失败返回空串。结果按可执行文件路径缓存。
func ClaudeVersion(bin string) string {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		return ""
	}
	probeMu.Lock()
	if versionProbed[bin] {
		value := versionCache[bin]
		probeMu.Unlock()
		return value
	}
	probeMu.Unlock()

	output, err := probeClaude(bin, "--version")
	value := ""
	if err == nil {
		value = strings.TrimSpace(string(output))
	}
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	probeMu.Lock()
	versionCache[bin] = value
	versionProbed[bin] = true
	probeMu.Unlock()
	return value
}

// SupportsBare 探测 claude 是否支持 --bare（最小模式：跳过 hooks/插件同步/
// CLAUDE.md 自动发现/记忆等，只认显式传入的 settings 与 MCP 配置——正是 daemon
// 想要的确定性）。结果按可执行文件路径缓存；探测不出来时返回 false，调用方
// 退回普通模式。
func SupportsBare(bin string) bool {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		return false
	}
	probeMu.Lock()
	if value, ok := bareCache[bin]; ok {
		probeMu.Unlock()
		return value
	}
	probeMu.Unlock()

	supported := false
	if output, err := probeClaude(bin, "--help"); err == nil {
		supported = strings.Contains(string(output), "--bare")
	}
	probeMu.Lock()
	bareCache[bin] = supported
	probeMu.Unlock()
	return supported
}

// CredentialEnvKeys 是 Claude Code 认的凭据环境变量（api key 走 x-api-key，
// auth token 走 Authorization: Bearer；第三方网关多用后者）。
var CredentialEnvKeys = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// CredentialSource 判断会话覆盖（provider/optimizations 合并结果）是否提供了
// AI 凭据，返回可读来源（如 "provider env ANTHROPIC_AUTH_TOKEN"、"settings
// apiKeyHelper"、"进程环境 ANTHROPIC_API_KEY"）；没有凭据时返回空串。
//
// 会话配置根由 assistant 托管（instances.ClaudeDir），不读用户的 ~/.claude 登录
// 态，所以「没有凭据」= 会话必然认证失败，启动时值得显式告警。
func CredentialSource(overrides Overrides) string {
	for _, key := range CredentialEnvKeys {
		if strings.TrimSpace(overrides.Env[key]) != "" {
			return "provider env " + key
		}
	}
	if helper, ok := overrides.Settings["apiKeyHelper"].(string); ok && strings.TrimSpace(helper) != "" {
		return "settings apiKeyHelper"
	}
	for _, key := range CredentialEnvKeys {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return "进程环境 " + key
		}
	}
	return ""
}

// MissingCredentialHint 是「没有任何 AI 凭据」时的修复提示。
const MissingCredentialHint = "在 config.json 的 providers（或 optimizations）里给所选 provider 配 api_key，" +
	"或给进程注入 ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN；会话不再读 ~/.claude 登录态"

func probeClaude(bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, bin, args...).Output()
}
