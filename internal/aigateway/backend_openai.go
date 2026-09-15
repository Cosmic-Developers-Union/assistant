package aigateway

import "net/http"

// OpenAI 官方：OpenAI 兼容面（chat completions / responses）可直接透传；按其
// 提示缓存约定补 prompt_cache_key（会话稳定值，客户端未带时由会话机制派生）。
func init() {
	RegisterBackend(Backend{
		Name:      "openai",
		Aliases:   []string{"gpt"},
		BaseURL:   "https://api.openai.com",
		Auth:      "bearer",
		Protocols: []string{ProtocolOpenAIChat, ProtocolOpenAIResponses},
		Prepare: func(_ http.Header, body *Body, session Session) {
			if session.ID != "" {
				body.Set("prompt_cache_key", session.ID)
			}
		},
	})
}
