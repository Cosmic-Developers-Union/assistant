// Package provider 是供应商的代码级特化点：大多数供应商只靠 config.json 的
// env/settings/mcp 透传即可，少数需要在**会话启动时**做动态调整（例如 OpenCode
// Go/Zen 要求每个会话一个稳定的 x-opencode-session 请求头，须经
// ANTHROPIC_CUSTOM_HEADERS 注入）。
//
// 一个供应商一个文件：实现 Handler 并在 init 里 Register，即可在评审/分诊/对话
// 会话启动前拿到会话上下文（名字、用途、会话 ID）并修改 Env/Settings/MCP。
// 没有注册 handler 的 provider 原样通过，零开销。
package provider

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"assistant/internal/claudecfg"
)

// Session 是一次会话的上下文：handler 可读元信息并就地修改三层覆盖。
type Session struct {
	// Provider 是配置里选中的 provider 名（原样，便于 handler 判断）
	Provider string
	// Kind 是会话用途：review / triage / chat（空串表示未标注）
	Kind string
	// ID 是会话级稳定标识：评审/分诊每次会话一个 UUID（同一进程内的多轮请求
	// 复用），对话桥复用其稳定会话 UUID（跨轮命中缓存）
	ID string
	// Env / Settings / MCP 是当前生效的覆盖（配置合并结果），handler 可增改
	Env      map[string]string
	Settings map[string]any
	MCP      map[string]any
}

// Handler 是供应商的代码级特化：Prepare 在会话启动前被调用一次。
type Handler interface {
	// Name 是主名字（不区分大小写匹配 provider 名）
	Name() string
	// Prepare 就地调整会话覆盖；返回错误会中止该会话（宁可不跑，不带病跑）
	Prepare(session *Session) error
}

// aliasHandler 可选实现：声明主名字之外的别名。
type aliasHandler interface {
	Aliases() []string
}

var registry = map[string]Handler{}

// Preset 是供应商的开箱即用内置配置：选到该 provider 时自动打底（端点、模型
// 映射、超时、窗口等），用户配置覆盖其上。多数供应商只需用户补一个
// api_key/auth_token（简写映射到 TokenEnv），无需了解任何变量名。
type Preset struct {
	// Name 是主名字；Aliases 是等价别名（都不区分大小写）
	Name    string
	Aliases []string
	// TokenEnvs 是 api_key/auth_token 简写要写入的环境变量名（可多个：如
	// minimax 同时写 ANTHROPIC_AUTH_TOKEN 与其 MCP 需要的 MINIMAX_API_KEY）；
	// 为空默认 ANTHROPIC_AUTH_TOKEN
	TokenEnvs []string
	// RequiresBaseURL 为真时必须由用户（或预设）给出 ANTHROPIC_BASE_URL，
	// 否则在配置校验期报错（如 openai 必须指向翻译代理）
	RequiresBaseURL bool
	// Env / Settings / MCP 是预设默认覆盖
	Env      map[string]string
	Settings map[string]any
	MCP      map[string]any
}

// Overrides 返回预设的覆盖视图。
func (p Preset) Overrides() claudecfg.Overrides {
	return claudecfg.Overrides{Env: p.Env, Settings: p.Settings, MCP: p.MCP}
}

var presets = map[string]Preset{}

// RegisterPreset 注册内置预设（主名与别名索引）；重名直接 panic。
func RegisterPreset(preset Preset) {
	if strings.TrimSpace(preset.Name) == "" {
		panic("provider: RegisterPreset 需要非空名字")
	}
	for _, name := range append([]string{preset.Name}, preset.Aliases...) {
		key := normalizeName(name)
		if key == "" {
			panic("provider: preset 名字不能为空")
		}
		if _, exists := presets[key]; exists {
			panic("provider: preset 名字重复：" + name)
		}
		presets[key] = preset
	}
}

// LookupPreset 按 provider 名查找内置预设。
func LookupPreset(name string) (Preset, bool) {
	preset, ok := presets[normalizeName(name)]
	return preset, ok
}

// HasPreset 报告该 provider 名是否有内置预设（日志/诊断用）。
func HasPreset(name string) bool {
	_, ok := LookupPreset(name)
	return ok
}

// PresetNames 返回所有预设主名（排序，诊断用）。
func PresetNames() []string {
	seen := map[string]bool{}
	names := []string{}
	for _, preset := range presets {
		if seen[preset.Name] {
			continue
		}
		seen[preset.Name] = true
		names = append(names, preset.Name)
	}
	sort.Strings(names)
	return names
}

// Resolve 组装某 provider 的最终配置层覆盖（未含代码级 handler 的会话调整，
// 那是 Apply 的职责）：
//
//	托管默认 < 内置预设 < 全局优化点 < 用户 provider 覆盖 < api_key/auth_token
//
// 令牌简写按预设的 TokenEnv 落地（无预设时 ANTHROPIC_AUTH_TOKEN）；用户 env
// 里取值为空的键表示移除低层默认（例如不想要预设的模型映射）。
func Resolve(name string, global claudecfg.Overrides, user claudecfg.Overrides, token string) (claudecfg.Overrides, error) {
	preset, hasPreset := LookupPreset(name)
	merged := claudecfg.Overrides{}
	if hasPreset {
		merged = preset.Overrides()
	}
	merged = claudecfg.ComposeOverrides(merged, global)
	merged = claudecfg.ComposeOverrides(merged, user)
	// 用户空值 = 移除该键（预设默认可被显式关闭）
	for key, value := range user.Env {
		if strings.TrimSpace(value) == "" {
			delete(merged.Env, key)
		}
	}
	if strings.TrimSpace(token) != "" {
		tokenEnvs := []string{"ANTHROPIC_AUTH_TOKEN"}
		if hasPreset && len(preset.TokenEnvs) > 0 {
			tokenEnvs = preset.TokenEnvs
		}
		tokenValues := make(map[string]string, len(tokenEnvs))
		for _, name := range tokenEnvs {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				tokenValues[trimmed] = token
			}
		}
		merged = claudecfg.ComposeOverrides(merged, claudecfg.Overrides{Env: tokenValues})
	}
	if hasPreset && preset.RequiresBaseURL && strings.TrimSpace(merged.Env["ANTHROPIC_BASE_URL"]) == "" {
		return claudecfg.Overrides{}, fmt.Errorf(
			"provider %s 需要 ANTHROPIC_BASE_URL：该类供应商没有 Anthropic 兼容端点，请在 provider.env 指向翻译代理", preset.Name)
	}
	return merged, nil
}

// Register 注册 handler（按主名字与小写别名索引）；重名直接 panic——
// 这是装配期错误，应在程序启动前暴露。
func Register(handler Handler) {
	if handler == nil || strings.TrimSpace(handler.Name()) == "" {
		panic("provider: Register 需要非空名字的 handler")
	}
	names := append([]string{handler.Name()}, aliasesOf(handler)...)
	for _, name := range names {
		key := normalizeName(name)
		if key == "" {
			panic("provider: handler 名字不能为空")
		}
		if _, exists := registry[key]; exists {
			panic("provider: handler 名字重复：" + name)
		}
		registry[key] = handler
	}
}

// Lookup 按 provider 名（不区分大小写）查找 handler。
func Lookup(name string) (Handler, bool) {
	handler, ok := registry[normalizeName(name)]
	return handler, ok
}

func aliasesOf(handler Handler) []string {
	if aliased, ok := handler.(aliasHandler); ok {
		return aliased.Aliases()
	}
	return nil
}

func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// Apply 在配置层覆盖（base）之上应用该 provider 的代码级特化，返回最终覆盖。
// sessionID 为空时生成新 UUID；未注册 handler 或 provider 名为空时原样返回。
func Apply(name, kind, sessionID string, base claudecfg.Overrides) (claudecfg.Overrides, error) {
	handler, ok := Lookup(name)
	if !ok {
		return base, nil
	}
	if strings.TrimSpace(sessionID) == "" {
		sessionID = NewSessionID()
	}
	session := &Session{
		Provider: name,
		Kind:     kind,
		ID:       sessionID,
		Env:      cloneStringMap(base.Env),
		Settings: cloneAnyMap(base.Settings),
		MCP:      cloneAnyMap(base.MCP),
	}
	if err := handler.Prepare(session); err != nil {
		return base, fmt.Errorf("provider %s 会话准备失败: %w", name, err)
	}
	return claudecfg.Overrides{Env: session.Env, Settings: session.Settings, MCP: session.MCP}, nil
}

// HeaderEnv 是 Claude Code 接受自定义请求头的环境变量（换行分隔 Key: Value）。
const HeaderEnv = "ANTHROPIC_CUSTOM_HEADERS"

// EnsureHeader 在 ANTHROPIC_CUSTOM_HEADERS 中补一行 "Key: Value"：已存在同名头
// （不区分大小写）或环境变量由操作者显式配置时保留原值；已有其他头原样保留。
func (s *Session) EnsureHeader(key, value string) {
	if s.Env == nil {
		s.Env = map[string]string{}
	}
	if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
		return
	}
	existing := strings.TrimSpace(s.Env[HeaderEnv])
	lines := []string{}
	if existing != "" {
		lines = strings.Split(existing, "\n")
	}
	for _, line := range lines {
		name, _, found := strings.Cut(line, ":")
		if found && strings.EqualFold(strings.TrimSpace(name), key) {
			return
		}
	}
	lines = append(lines, key+": "+value)
	s.Env[HeaderEnv] = strings.Join(lines, "\n")
}

// NewSessionID 生成 v4 UUID（请求头与 claude --session-id 共用格式）。
func NewSessionID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		// crypto/rand 失败极罕见；退化为空串由调用方回退
		return ""
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(buffer)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32])
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return map[string]string{}
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneAnyMap(source map[string]any) map[string]any {
	if len(source) == 0 {
		return map[string]any{}
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		return source
	}
	var clone map[string]any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		return source
	}
	return clone
}
