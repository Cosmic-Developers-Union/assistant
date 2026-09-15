package aigateway

import (
	"net/http"
	"strings"
)

// dsh（DeepSeek Harness）：OpenAI Chat Completions 协议；会话在其原生请求头
// 里（部分调用路径会遗漏——OpenCode Go 文档已确认该问题），网关识别其头部并
// 在内容派生兜底之上向 OpenCode 上游自动补 x-opencode-session。
func init() {
	RegisterTool(ToolProfile{
		Name: "dsh",
		Match: func(header http.Header) bool {
			userAgent := strings.ToLower(header.Get("User-Agent"))
			if strings.HasPrefix(userAgent, "dsh/") || strings.Contains(userAgent, "deepseek-harness") {
				return true
			}
			return header.Get("x-dsh-session") != "" ||
				strings.HasPrefix(strings.ToLower(header.Get("x-app")), "dsh")
		},
		UserAgent: "dsh-gateway/assistant",
		Version: func(header http.Header) string {
			userAgent := strings.ToLower(header.Get("User-Agent"))
			if rest, ok := strings.CutPrefix(userAgent, "dsh/"); ok {
				if version, _, found := strings.Cut(rest, " "); found {
					return version
				}
				return rest
			}
			return ""
		},
	})
}
