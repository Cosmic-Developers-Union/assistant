package provider

// 智谱 GLM（GLM Coding Plan，Anthropic 兼容端点）。预设内容：
//   - 端点：z.ai（国际）/ bigmodel（国内，provider 名 bigmodel）
//   - 模型：官方默认映射为全档 glm-5.3-flash[1m]（glm-5.3-flash 有绝对优先性），
//     1M 压缩窗口；若要主档用 glm-5.3[1m]，在用户 env 覆盖
//     ANTHROPIC_DEFAULT_{SONNET,OPUS}_MODEL 即可
//   - 官方接入脚本同款：API_TIMEOUT_MS=3000000、关非必要流量
//   - 视觉理解 MCP（官方文档 vision-mcp-server）：npx -y @z_ai/mcp-server@latest，
//     Z_AI_API_KEY 由 api_key 简写自动注入，Z_AI_MODE 按平台区分
//
// 用户只需：
//
//	{"api_key": "your_key"}
func init() {
	RegisterPreset(Preset{
		Name:      "zhipu",
		Aliases:   []string{"glm", "zai", "z.ai"},
		TokenEnvs: []string{"ANTHROPIC_AUTH_TOKEN", "Z_AI_API_KEY"},
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":                       "https://api.z.ai/api/anthropic",
			"API_TIMEOUT_MS":                           "3000000",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
			"ANTHROPIC_MODEL":                          "glm-5.3-flash[1m]",
			"ANTHROPIC_DEFAULT_OPUS_MODEL":             "glm-5.3-flash[1m]",
			"ANTHROPIC_DEFAULT_SONNET_MODEL":           "glm-5.3-flash[1m]",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL":            "glm-5.3-flash[1m]",
			"ANTHROPIC_DEFAULT_FABLE_MODEL":            "glm-5.3-flash[1m]",
			"CLAUDE_CODE_AUTO_COMPACT_WINDOW":          "1000000",
		},
		MCP: map[string]any{
			"zai-mcp-server": map[string]any{
				"command": "npx",
				"args":    []any{"-y", "@z_ai/mcp-server@latest"},
				"env":     map[string]any{"Z_AI_MODE": "ZAI"},
			},
		},
	})
	RegisterPreset(Preset{
		Name:      "bigmodel",
		Aliases:   []string{"zhipu-cn"},
		TokenEnvs: []string{"ANTHROPIC_AUTH_TOKEN", "Z_AI_API_KEY"},
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":                       "https://open.bigmodel.cn/api/anthropic",
			"API_TIMEOUT_MS":                           "3000000",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
			"ANTHROPIC_MODEL":                          "glm-5.3-flash[1m]",
			"ANTHROPIC_DEFAULT_OPUS_MODEL":             "glm-5.3-flash[1m]",
			"ANTHROPIC_DEFAULT_SONNET_MODEL":           "glm-5.3-flash[1m]",
			"ANTHROPIC_DEFAULT_HAIKU_MODEL":            "glm-5.3-flash[1m]",
			"ANTHROPIC_DEFAULT_FABLE_MODEL":            "glm-5.3-flash[1m]",
			"CLAUDE_CODE_AUTO_COMPACT_WINDOW":          "1000000",
		},
		MCP: map[string]any{
			"zai-mcp-server": map[string]any{
				"command": "npx",
				"args":    []any{"-y", "@z_ai/mcp-server@latest"},
				"env":     map[string]any{"Z_AI_MODE": "ZHIPU"},
			},
		},
	})
}
