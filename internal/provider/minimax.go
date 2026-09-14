package provider

// MiniMax（Anthropic 兼容端点，订阅 Token Plan 的 key）。预设钉 M3 1M 档 +
// 压缩窗口（M3 自动 cache，无需额外配置）；国际/国内端点分两个名字。用户只需：
//
//	{"api_key": "sk-cp-..."}
//
// 想用 512K 档或 M2.7-highspeed：在用户 env 覆盖 ANTHROPIC_*_MODEL 与
// CLAUDE_CODE_AUTO_COMPACT_WINDOW 即可。
func init() {
	RegisterPreset(Preset{
		Name:     "minimax",
		TokenEnv: "ANTHROPIC_AUTH_TOKEN",
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":              "https://api.minimax.io/anthropic",
			"ANTHROPIC_MODEL":                 "MiniMax-M3[1m]",
			"ANTHROPIC_DEFAULT_OPUS_MODEL":    "MiniMax-M3[1m]",
			"ANTHROPIC_DEFAULT_SONNET_MODEL":  "MiniMax-M3[1m]",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL":   "MiniMax-M3[1m]",
			"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1000000",
		},
	})
	RegisterPreset(Preset{
		Name:     "minimax-cn",
		Aliases:  []string{"minimaxi"},
		TokenEnv: "ANTHROPIC_AUTH_TOKEN",
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":              "https://api.minimaxi.com/anthropic",
			"ANTHROPIC_MODEL":                 "MiniMax-M3[1m]",
			"ANTHROPIC_DEFAULT_OPUS_MODEL":    "MiniMax-M3[1m]",
			"ANTHROPIC_DEFAULT_SONNET_MODEL":  "MiniMax-M3[1m]",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL":   "MiniMax-M3[1m]",
			"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1000000",
		},
	})
}
