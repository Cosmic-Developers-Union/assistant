package provider

// MiniMax（Anthropic 兼容端点，订阅 Token Plan 的 key）。预设内容：
//   - 端点：api.minimax.io（国际）/ api.minimaxi.com（国内，provider 名
//     minimax-cn / minimaxi）
//   - 模型：全档 MiniMax-M3[1m]（1M 上下文，自动 cache），压缩窗口 1000000
//   - 官方 coding-plan MCP：uvx minimax-coding-plan-mcp，
//     MINIMAX_API_KEY 由 api_key 简写自动注入，MINIMAX_API_HOST 按平台区分
//
// 用户只需：
//
//	{"api_key": "sk-cp-..."}
//
// 想用 512K 档或 M2.7-highspeed：在用户 env 覆盖 ANTHROPIC_*_MODEL 与
// CLAUDE_CODE_AUTO_COMPACT_WINDOW 即可。
func init() {
	RegisterPreset(Preset{
		Name:      "minimax",
		TokenEnvs: []string{"ANTHROPIC_AUTH_TOKEN", "MINIMAX_API_KEY"},
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":              "https://api.minimax.io/anthropic",
			"ANTHROPIC_MODEL":                 "MiniMax-M3[1m]",
			"ANTHROPIC_DEFAULT_OPUS_MODEL":    "MiniMax-M3[1m]",
			"ANTHROPIC_DEFAULT_SONNET_MODEL":  "MiniMax-M3[1m]",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL":   "MiniMax-M3[1m]",
			"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1000000",
		},
		MCP: map[string]any{
			"MiniMax": map[string]any{
				"command": "uvx",
				"args":    []any{"minimax-coding-plan-mcp"},
				"env":     map[string]any{"MINIMAX_API_HOST": "https://api.minimax.io"},
			},
		},
	})
	RegisterPreset(Preset{
		Name:      "minimax-cn",
		Aliases:   []string{"minimaxi"},
		TokenEnvs: []string{"ANTHROPIC_AUTH_TOKEN", "MINIMAX_API_KEY"},
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":              "https://api.minimaxi.com/anthropic",
			"ANTHROPIC_MODEL":                 "MiniMax-M3[1m]",
			"ANTHROPIC_DEFAULT_OPUS_MODEL":    "MiniMax-M3[1m]",
			"ANTHROPIC_DEFAULT_SONNET_MODEL":  "MiniMax-M3[1m]",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL":   "MiniMax-M3[1m]",
			"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1000000",
		},
		MCP: map[string]any{
			"MiniMax": map[string]any{
				"command": "uvx",
				"args":    []any{"minimax-coding-plan-mcp"},
				"env":     map[string]any{"MINIMAX_API_HOST": "https://api.minimax.cn"},
			},
		},
	})
}
