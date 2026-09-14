package provider

// OpenAI：没有供 Claude Code 使用的 Anthropic Messages 端点，必须自建/第三方
// 翻译代理（LiteLLM、claude-code-router 等）。预设只声明令牌简写与「必须给
// base_url」，模型映射留给用户（OpenAI 模型名与 Claude 档位无默认对应）：
//
//	{"api_key": "sk-...", "env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:4000"}}
func init() {
	RegisterPreset(Preset{
		Name:            "openai",
		Aliases:         []string{"gpt"},
		TokenEnvs:       []string{"ANTHROPIC_AUTH_TOKEN"},
		RequiresBaseURL: true,
	})
}
