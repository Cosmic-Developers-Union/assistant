package aigateway

import (
	"net/http"
	"strings"
)

// Claude Code（Anthropic CLI）：Anthropic Messages 协议；会话走请求体
// metadata.user_id（也兼容 x-session-id）。UA 形如 "claude-cli/2.1.270"。
func init() {
	RegisterTool(ToolProfile{
		Name: "claude",
		Match: func(header http.Header) bool {
			userAgent := strings.ToLower(header.Get("User-Agent"))
			if strings.HasPrefix(userAgent, "claude-cli/") || strings.Contains(userAgent, "claude-code") {
				return true
			}
			return header.Get("anthropic-version") != "" && header.Get("x-app") == "cli"
		},
		UserAgent: "claude-cli-gateway/assistant",
		Version: func(header http.Header) string {
			userAgent := strings.TrimSpace(header.Get("User-Agent"))
			if rest, ok := strings.CutPrefix(userAgent, "claude-cli/"); ok {
				if version, _, found := strings.Cut(rest, " "); found {
					return version
				}
				return rest
			}
			return ""
		},
	})
}
