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
