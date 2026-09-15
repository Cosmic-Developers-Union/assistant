package aigateway

import (
	"net/http"
	"strings"
)

// Codex CLI（codex-rs）：OpenAI Responses 协议；会话走 session_id 请求头
// （部分版本/代理会遗漏，网关按内容派生兜底并在转发时保留会话头）。
func init() {
	RegisterTool(ToolProfile{
		Name: "codex",
		Match: func(header http.Header) bool {
			if originator := strings.ToLower(header.Get("originator")); strings.HasPrefix(originator, "codex") {
				return true
			}
			userAgent := strings.ToLower(header.Get("User-Agent"))
			return strings.HasPrefix(userAgent, "codex") || strings.Contains(userAgent, "codex_cli_rs")
		},
		UserAgent: "codex_cli_rs/assistant-gateway",
		Version: func(header http.Header) string {
			userAgent := header.Get("User-Agent")
			if rest, ok := strings.CutPrefix(strings.ToLower(userAgent), "codex_cli_rs/"); ok {
				if version, _, found := strings.Cut(rest, " "); found {
					return version
				}
				return rest
			}
			return ""
		},
	})
}
