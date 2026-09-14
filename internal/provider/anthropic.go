package provider

// Anthropic 官方：端点与模型档位由 Claude Code 内置默认管理，预设只声明令牌
// 简写落到 ANTHROPIC_API_KEY（第三方网关多为 bearer token）。用户只需：
//
//	{"api_key": "sk-ant-..."}
func init() {
	RegisterPreset(Preset{
		Name:     "anthropic",
		Aliases:  []string{"claude", "official"},
		TokenEnv: "ANTHROPIC_API_KEY",
	})
}
