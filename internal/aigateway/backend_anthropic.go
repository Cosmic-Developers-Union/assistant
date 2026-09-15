package aigateway

// Anthropic 官方：标准 Anthropic Messages 端点，鉴权走 x-api-key；无厂商特化
// （Claude Code 自己会带 metadata.user_id，原样透传即可）。
func init() {
	RegisterBackend(Backend{
		Name:      "anthropic",
		Aliases:   []string{"claude"},
		BaseURL:   "https://api.anthropic.com",
		Auth:      "x-api-key",
		Protocols: []string{ProtocolAnthropic},
	})
}
