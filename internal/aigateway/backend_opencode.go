package aigateway

import "net/http"

// OpenCode 网关（Zen / Go）：非标准后端——Go/Zen 要求每个会话稳定的
// x-opencode-session（2026-09-06 起，缺失可能被拒），OpenCode 客户端还会带
// x-session-affinity 做缓存亲和；OpenAI 兼容面用 prompt_cache_key，Anthropic
// 面用 metadata.user_id。网关在会话机制之上自动补齐这些载体。
func init() {
	RegisterBackend(Backend{
		Name:      "opencode-go",
		Aliases:   []string{"opencode", "zen-go"},
		BaseURL:   "https://opencode.ai/zen/go",
		Auth:      "bearer",
		Protocols: []string{ProtocolAnthropic, ProtocolOpenAIChat, ProtocolOpenAIResponses},
		Prepare:   prepareOpencode,
	})
	RegisterBackend(Backend{
		Name:      "opencode-zen",
		Aliases:   []string{"zen"},
		BaseURL:   "https://opencode.ai/zen",
		Auth:      "bearer",
		Protocols: []string{ProtocolAnthropic},
		Prepare:   prepareOpencode,
	})
}

func prepareOpencode(header http.Header, body *Body, session Session) {
	if session.ID == "" {
		return
	}
	header.Set("x-opencode-session", session.ID)
	header.Set("x-session-affinity", session.ID)
	switch session.Protocol {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses:
		body.Set("prompt_cache_key", session.ID)
	default:
		body.MetadataSession(session.ID)
	}
}
