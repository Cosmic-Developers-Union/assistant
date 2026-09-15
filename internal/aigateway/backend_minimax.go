package aigateway

// MiniMax：Anthropic 兼容端点，纯透传（自动 cache，无需额外参数）。
func init() {
	RegisterBackend(Backend{
		Name:      "minimax",
		BaseURL:   "https://api.minimax.io/anthropic",
		Auth:      "bearer",
		Protocols: []string{ProtocolAnthropic},
	})
	RegisterBackend(Backend{
		Name:      "minimax-cn",
		Aliases:   []string{"minimaxi"},
		BaseURL:   "https://api.minimaxi.com/anthropic",
		Auth:      "bearer",
		Protocols: []string{ProtocolAnthropic},
	})
}
