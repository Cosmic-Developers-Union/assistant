package aigateway

// Kimi（Moonshot / Kimi Code）：Anthropic 兼容端点，纯透传。
func init() {
	RegisterBackend(Backend{
		Name:      "kimi",
		Aliases:   []string{"kimi-code", "kimi-coding"},
		BaseURL:   "https://api.kimi.com/coding/",
		Auth:      "bearer",
		Protocols: []string{ProtocolAnthropic},
	})
	RegisterBackend(Backend{
		Name:      "moonshot",
		Aliases:   []string{"kimi-platform", "moonshot-ai"},
		BaseURL:   "https://api.moonshot.cn/anthropic",
		Auth:      "bearer",
		Protocols: []string{ProtocolAnthropic},
	})
}
