package provider

// OpenCode 网关（OpenCode Zen / Go）要求每个会话一个稳定的会话标识：
//   - Go 端点自 2026-09-06 起要求 x-opencode-session，缺失可能被拒（官方通知）；
//   - x-session-affinity 是 OpenCode 客户端默认发送的缓存亲和头（同会话路由到
//     同一后端，提升 prompt cache 命中）。
//
// Claude Code 没有原生会话头开关，用 ANTHROPIC_CUSTOM_HEADERS 注入：
// 每次会话（评审/分诊）一个 UUID；微信对话复用其稳定会话 UUID（跨轮命中缓存）。
// 如网关改名/新增要求，只改本文件。
const (
	opencodeSessionHeader  = "x-opencode-session"
	opencodeAffinityHeader = "x-session-affinity"
)

// opencodeHandler 对应 config.json 里名为 opencode（或 zen / opencode-zen /
// opencode-go）的 provider。
type opencodeHandler struct{}

func init() {
	// 开箱即用预设：Zen 网关原生提供 Claude 模型（模型档位无需映射），
	// 用户只需补令牌。
	RegisterPreset(Preset{
		Name:      "opencode",
		Aliases:   []string{"zen", "opencode-zen", "opencode-go"},
		TokenEnvs: []string{"ANTHROPIC_AUTH_TOKEN"},
		Env: map[string]string{
			"ANTHROPIC_BASE_URL": "https://opencode.ai/zen",
		},
	})
	Register(opencodeHandler{})
}

func (opencodeHandler) Name() string { return "opencode" }

func (opencodeHandler) Aliases() []string {
	return []string{"zen", "opencode-zen", "opencode-go"}
}

func (opencodeHandler) Prepare(session *Session) error {
	if session.ID == "" {
		return nil
	}
	session.EnsureHeader(opencodeSessionHeader, session.ID)
	session.EnsureHeader(opencodeAffinityHeader, session.ID)
	return nil
}
