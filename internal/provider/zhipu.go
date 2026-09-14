package provider

// 智谱 GLM（Z.ai Coding Plan，Anthropic 兼容端点）。内置默认按官方接入脚本：
// 长超时 + 关非必要流量；模型映射不动（网关会自动把 Claude 档位映射到当前
// 套餐模型，官方建议不要硬编码）。用户只需：
//
//	{"api_key": "your_zai_key"}
//
// 国内 bigmodel 端点用 provider 名 bigmodel（或覆盖 ANTHROPIC_BASE_URL）。
func init() {
	RegisterPreset(Preset{
		Name:     "zhipu",
		Aliases:  []string{"glm", "zai", "z.ai"},
		TokenEnv: "ANTHROPIC_AUTH_TOKEN",
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":                       "https://api.z.ai/api/anthropic",
			"API_TIMEOUT_MS":                           "3000000",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		},
	})
	RegisterPreset(Preset{
		Name:     "bigmodel",
		Aliases:  []string{"zhipu-cn"},
		TokenEnv: "ANTHROPIC_AUTH_TOKEN",
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":                       "https://open.bigmodel.cn/api/anthropic",
			"API_TIMEOUT_MS":                           "3000000",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		},
	})
}
