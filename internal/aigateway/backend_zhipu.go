package aigateway

// 智谱 GLM（Z.ai / bigmodel）：Anthropic 兼容端点，纯透传（模型名与档位由配置
// 的 model 字段映射；Claude Code 自带的 metadata.user_id 原样保留）。
func init() {
	RegisterBackend(Backend{
		Name:      "zhipu",
		Aliases:   []string{"glm", "zai", "z.ai"},
		BaseURL:   "https://api.z.ai/api/anthropic",
		Auth:      "bearer",
		Protocols: []string{ProtocolAnthropic},
	})
	RegisterBackend(Backend{
		Name:      "bigmodel",
		Aliases:   []string{"zhipu-cn"},
		BaseURL:   "https://open.bigmodel.cn/api/anthropic",
		Auth:      "bearer",
		Protocols: []string{ProtocolAnthropic},
	})
}
