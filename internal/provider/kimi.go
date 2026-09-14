package provider

// Kimi（Kimi Code 订阅端点 api.kimi.com/coding，key 用 ANTHROPIC_API_KEY）。
// 该端点必须显式给出模型名（不配会报模型不存在），预设统一钉 kimi-for-coding
// （各档套餐都可用，262144 窗口）；升级套餐后按 providers.md 换成 k3[1m] 等。
// 用户只需：
//
//	{"api_key": "sk-..."}
//
// Moonshot 开放平台用 provider 名 moonshot（不同 key、不同端点）。
func init() {
	RegisterPreset(Preset{
		Name:      "kimi",
		Aliases:   []string{"kimi-code", "kimi-coding"},
		TokenEnvs: []string{"ANTHROPIC_API_KEY"},
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":              "https://api.kimi.com/coding/",
			"ANTHROPIC_MODEL":                 "kimi-for-coding",
			"ANTHROPIC_DEFAULT_OPUS_MODEL":    "kimi-for-coding",
			"ANTHROPIC_DEFAULT_SONNET_MODEL":  "kimi-for-coding",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL":   "kimi-for-coding",
			"ANTHROPIC_DEFAULT_FABLE_MODEL":   "kimi-for-coding",
			"CLAUDE_CODE_SUBAGENT_MODEL":      "kimi-for-coding",
			"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "262144",
			"CLAUDE_CODE_MAX_CONTEXT_TOKENS":  "262144",
		},
	})
	RegisterPreset(Preset{
		Name:      "moonshot",
		Aliases:   []string{"kimi-platform", "moonshot-ai"},
		TokenEnvs: []string{"ANTHROPIC_AUTH_TOKEN"},
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":              "https://api.moonshot.cn/anthropic",
			"ANTHROPIC_MODEL":                 "kimi-k3",
			"ANTHROPIC_DEFAULT_OPUS_MODEL":    "kimi-k3",
			"ANTHROPIC_DEFAULT_SONNET_MODEL":  "kimi-k3",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL":   "kimi-k3",
			"ANTHROPIC_DEFAULT_FABLE_MODEL":   "kimi-k3",
			"CLAUDE_CODE_SUBAGENT_MODEL":      "kimi-k3",
			"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "262144",
		},
	})
}
