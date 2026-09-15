// Package aigateway 是 assistant 套件的自托管 API 网关（cmd/ai-gateway）：
// 对客户端暴露 Anthropic Messages 协议，向上游（Anthropic 兼容厂商）转发，
// 并在两侧之间统一「会话身份」。
//
// 会话机制是本包的核心差异化：不同 Agent 与供应商对“会话”的载体完全不同——
// Claude Code 把 session_id 放在请求体 metadata.user_id 的 JSON 里；opencode
// 走 x-opencode-session 头；Fireworks/Cloudflare 等走 x-session-affinity；
// OpenAI 兼容端点走 prompt_cache_key；不少工具则什么都不带。网关负责：
//
//	抽取（显式头 → metadata → 内容派生）→ 规范化（稳定 UUID）→ 注入（按上游规则）
//
// 内容派生是最后兜底：无客户端配合时，用「客户端身份 + system + 第一条 user
// 消息 + 模型」的 HMAC 计算稳定会话键，多轮对话（无状态 API 会重发历史）自然
// 落到同一键，提示缓存亲和与成本归因不受客户端实现差异影响。
package aigateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// DefaultSessionHeaderNames 是显式会话头的候选顺序（不区分大小写）：覆盖
// 当前主流 Agent 的命名，先到先用。
//   - Claude Code：metadata.user_id（请求体），部分版本也带 x-session-id；
//   - codex：session_id；
//   - dsh（DeepSeek Harness）：x-dsh-session / session_id；
//   - OpenCode：x-opencode-session / x-session-affinity。
var DefaultSessionHeaderNames = []string{
	"x-session-id",
	"x-opencode-session",
	"x-session-affinity",
	"x-claude-session-id",
	"session_id",
	"x-dsh-session",
}

// Session 是一次请求解析出的会话身份。
type Session struct {
	// ID 是规范化后的会话标识（UUID 文本，直接可作请求头/字段值）
	ID string
	// Source 说明来源（诊断/审计）：header:<名字> / metadata.user_id /
	// derived:content / derived:client
	Source string
	// Protocol 是客户端协议（后端特化按它选择注入载体）；由代理在解析后填入
	Protocol string
}

// SessionResolver 解析会话键：显式来源优先，缺失时确定性派生（同样输入必得
// 同样输出），保证无客户端配合时也能获得稳定亲和。
type SessionResolver struct {
	// Secret 是内容派生的 HMAC 密钥；空则退化为无密钥 SHA-256（仍确定）
	Secret []byte
	// HeaderNames 是显式会话头候选；空用 DefaultSessionHeaderNames
	HeaderNames []string
}

// headerNames 返回候选头（含默认）。
func (r *SessionResolver) headerNames() []string {
	if len(r.HeaderNames) > 0 {
		return r.HeaderNames
	}
	return DefaultSessionHeaderNames
}

// Resolve 从请求头与已解析的 JSON 请求体解析会话身份；body 可为 nil。
func (r *SessionResolver) Resolve(header http.Header, body map[string]any, client string) Session {
	if header != nil {
		for _, name := range r.headerNames() {
			if value := strings.TrimSpace(header.Get(name)); value != "" {
				return Session{ID: normalizeSessionID(value), Source: "header:" + strings.ToLower(name)}
			}
		}
	}
	if id := sessionIDFromMetadata(body); id != "" {
		return Session{ID: normalizeSessionID(id), Source: "metadata.user_id"}
	}
	firstUser := firstUserText(body)
	if firstUser != "" {
		return Session{ID: r.derive(client, body, firstUser), Source: "derived:content"}
	}
	return Session{ID: r.derive(client, body, ""), Source: "derived:client"}
}

// derive 计算确定性会话键：HMAC(secret, 客户端 + 模型 + system 摘要 + 首条
// user 消息摘要)。多轮请求会重发完整历史，首条 user 消息不变 → 键稳定；
// 压缩/改写首条消息 → 换新键（与提示缓存前缀语义一致）。
func (r *SessionResolver) derive(client string, body map[string]any, firstUser string) string {
	model := ""
	if body != nil {
		model, _ = body["model"].(string)
	}
	parts := []string{
		"aigateway-session",
		strings.TrimSpace(client),
		strings.TrimSpace(model),
		digest(strings.TrimSpace(systemText(body))),
		digest(strings.TrimSpace(firstUser)),
	}
	message := []byte(strings.Join(parts, "|"))
	var sum []byte
	if len(r.Secret) > 0 {
		mac := hmac.New(sha256.New, r.Secret)
		mac.Write(message)
		sum = mac.Sum(nil)
	} else {
		hashed := sha256.Sum256(message)
		sum = hashed[:]
	}
	return uuidFromBytes(sum)
}

// sessionIDFromMetadata 抽取 Anthropic 约定的 metadata.user_id 会话标识：
// Claude Code 传 JSON 字符串（含 session_id/device_id 等），也兼容纯字符串。
func sessionIDFromMetadata(body map[string]any) string {
	if body == nil {
		return ""
	}
	metadata, _ := body["metadata"].(map[string]any)
	if metadata == nil {
		return ""
	}
	raw, ok := metadata["user_id"]
	if !ok {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return ""
		}
		var document map[string]any
		if strings.HasPrefix(trimmed, "{") && json.Unmarshal([]byte(trimmed), &document) == nil {
			if id, _ := document["session_id"].(string); strings.TrimSpace(id) != "" {
				return strings.TrimSpace(id)
			}
			return ""
		}
		return trimmed
	case map[string]any:
		if id, _ := typed["session_id"].(string); strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

// mergeMetadataSession 按 Anthropic 原生格式写入 metadata.user_id 的
// session_id：已有 JSON 字符串时保留其他键（device_id 等），否则新建 JSON。
func mergeMetadataSession(body map[string]any, sessionID string) {
	metadata, _ := body["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	document := map[string]any{}
	if raw, ok := metadata["user_id"].(string); ok && strings.HasPrefix(strings.TrimSpace(raw), "{") {
		_ = json.Unmarshal([]byte(strings.TrimSpace(raw)), &document)
	}
	document["session_id"] = sessionID
	encoded, err := json.Marshal(document)
	if err != nil {
		return
	}
	metadata["user_id"] = string(encoded)
	body["metadata"] = metadata
}

// systemText 把 system 字段（字符串或内容块数组）折成纯文本。
func systemText(body map[string]any) string {
	if body == nil {
		return ""
	}
	return textOf(body["system"])
}

// firstUserText 返回第一条 user 消息的纯文本（无则空串）。
func firstUserText(body map[string]any) string {
	if body == nil {
		return ""
	}
	messages, _ := body["messages"].([]any)
	for _, item := range messages {
		message, _ := item.(map[string]any)
		if message == nil {
			continue
		}
		if role, _ := message["role"].(string); role != "user" {
			continue
		}
		return textOf(message["content"])
	}
	return ""
}

// textOf 从字符串或内容块数组提取文本。
func textOf(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, block := range typed {
			document, _ := block.(map[string]any)
			if document == nil {
				continue
			}
			if text, _ := document["text"].(string); text != "" {
				builder.WriteString(text)
				builder.WriteString("\n")
			}
		}
		return builder.String()
	}
	return ""
}

// normalizeSessionID 规范化显式会话标识：去空白；超长/含控制字符时改为其
// 摘要，保证可以安全地放进头与 JSON 字段。
func normalizeSessionID(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) > 200 || strings.ContainsAny(trimmed, "\x00\n\r") {
		return uuidFromBytes([]byte(trimmed))
	}
	return trimmed
}

// digest 返回短哈希（派生输入用，避免把长文本直接塞进 HMAC 消息）。
func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:16])
}

// uuidFromBytes 把 16 字节摘要折成 UUIDv5 布局文本。
func uuidFromBytes(source []byte) string {
	sum := sha256.Sum256(source)
	bytes := sum[:16]
	bytes[6] = (bytes[6] & 0x0f) | 0x50
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes)
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32])
}
