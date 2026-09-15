package aigateway

import (
	"net/http"
	"strings"
)

// ToolProfile 是一个客户端工具（harness）的画像：识别方式与转发时的用户
// 代理署名。编写约束（OpenCode Go/Zen）：客户端应发送典型的编程 Agent 流量、
// 使用自身专属 UA（而不是通用 SDK/HTTP 库名）、并为每段对话发送稳定会话 ID。
// 网关能做的确定性部分是后两条：识别工具 → 规范化 UA；会话 ID 由会话机制保证。
//
// 一个工具一个文件（tool_claude.go / tool_codex.go / tool_dsh.go），init 注册。
type ToolProfile struct {
	// Name 是工具名（claude / codex / dsh），用于日志与指标
	Name string
	// Match 判断该请求是否来自本工具
	Match func(header http.Header) bool
	// UserAgent 是客户端 UA 缺失或为通用名时改写成的署名
	UserAgent string
	// Version 从请求头提取客户端版本（可为空）
	Version func(header http.Header) string
}

var toolProfiles []ToolProfile

// RegisterTool 注册工具画像（装配期）。
func RegisterTool(profile ToolProfile) {
	if profile.Name == "" || profile.Match == nil {
		panic("aigateway: RegisterTool 需要 Name 与非空 Match")
	}
	toolProfiles = append(toolProfiles, profile)
}

// DetectTool 识别请求来自哪个工具；识别不出返回空串。
func DetectTool(header http.Header) string {
	for _, profile := range toolProfiles {
		if profile.Match(header) {
			return profile.Name
		}
	}
	return ""
}

// genericUserAgents 是需要被改写的通用 UA 前缀（小写）：SDK/HTTP 库名无法满足
// OpenCode Go 的“专属 UA”约束。
var genericUserAgents = []string{
	"openai/", "openai-python", "openai-node", "python-requests", "python-httpx",
	"httpx/", "node-fetch", "undici", "axios", "okhttp", "go-http-client",
	"curl/", "wget/", "java/", "apache-httpclient", "guzzle", "reqwest/",
}

// NormalizeUserAgent 按工具画像规范化请求的 User-Agent：
//   - 客户端已有专属 UA（如 claude-cli/2.1.270）→ 原样保留（附上网关标识后缀）；
//   - 通用 SDK/库名或缺失 → 换成 <gateway>/<version> (tool:<name>)；
//   - 未识别工具且 UA 缺失 → <gateway>/<version>。
func NormalizeUserAgent(header http.Header, tool, gatewayUA string) {
	if header == nil || strings.TrimSpace(gatewayUA) == "" {
		return
	}
	current := strings.TrimSpace(header.Get("User-Agent"))
	toolProfile := ""
	for _, profile := range toolProfiles {
		if profile.Name == tool {
			toolProfile = profile.UserAgent
			break
		}
	}
	switch {
	case current == "":
		if toolProfile != "" {
			header.Set("User-Agent", toolProfile)
			return
		}
		header.Set("User-Agent", gatewayUA)
	case isGenericUserAgent(current):
		// 通用 SDK/库名不可用于上游约束：换成工具专属署名或网关署名
		if toolProfile != "" {
			header.Set("User-Agent", toolProfile+" "+gatewayUA)
			return
		}
		header.Set("User-Agent", gatewayUA)
	default:
		// 专属 UA 原样保留，仅追加网关标识（便于上游侧观测中转）
		if !strings.Contains(current, gatewayUA) {
			header.Set("User-Agent", current+" "+gatewayUA)
		}
	}
}

func isGenericUserAgent(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range genericUserAgents {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// ToolVersion 从请求头提取工具版本（未识别或缺失返回空串）。
func ToolVersion(name string, header http.Header) string {
	if name == "" || header == nil {
		return ""
	}
	for _, profile := range toolProfiles {
		if profile.Name == name && profile.Version != nil {
			return profile.Version(header)
		}
	}
	return ""
}

// ToolNames 返回已注册工具名（诊断/文档用）。
func ToolNames() []string {
	names := make([]string, 0, len(toolProfiles))
	for _, profile := range toolProfiles {
		names = append(names, profile.Name)
	}
	return names
}
