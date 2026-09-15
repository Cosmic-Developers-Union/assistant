package aigateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
)

// DefaultListen 是网关的默认监听地址（只绑环回；对外暴露请显式改）。
const DefaultListen = "127.0.0.1:8780"

// DefaultUpstreamTimeoutMS 是单次上游请求超时缺省值（30 分钟：评审会话可能
// 很长，网关不做比会话本身更短的超时）。
const DefaultUpstreamTimeoutMS = 30 * 60 * 1000

// MaxRequestBodyBytes 是请求体上限（长上下文请求可能很大）。
const MaxRequestBodyBytes = 64 << 20

// 支持的协议面（客户端路径 → 上游协议族；网关只做同协议透传，不做翻译）。
// 配置里的 protocol 别名见 NormalizeProtocol。
const (
	ProtocolAnthropic       = "anthropic-messages" // POST /v1/messages
	ProtocolOpenAIChat      = "openai-compatible"  // POST /v1/chat/completions
	ProtocolOpenAIResponses = "openai-responses"   // POST /v1/responses
)

// DefaultUserAgent 是网关转发时的默认 UA（客户端 UA 缺失或为通用 SDK 名时使用，
// OpenCode Go 要求客户端使用专属 UA）。
const DefaultUserAgent = "assistant-ai-gateway/1.0"

// protocolAliases 把常见叫法归一化。
var protocolAliases = map[string]string{
	"anthropic-messages": ProtocolAnthropic,
	"anthropic":          ProtocolAnthropic,
	"openai-compatible":  ProtocolOpenAIChat,
	"openai-chat":        ProtocolOpenAIChat,
	"chat-completions":   ProtocolOpenAIChat,
	"openai-responses":   ProtocolOpenAIResponses,
	"responses":          ProtocolOpenAIResponses,
}

// NormalizeProtocol 归一化协议名；未识别返回空串。
func NormalizeProtocol(value string) string {
	return protocolAliases[strings.ToLower(strings.TrimSpace(value))]
}

// ProtocolForPath 把客户端请求路径映射到协议。
func ProtocolForPath(path string) (string, bool) {
	switch path {
	case "/v1/messages", "/v1/messages/count_tokens":
		return ProtocolAnthropic, true
	case "/v1/chat/completions":
		return ProtocolOpenAIChat, true
	case "/v1/responses":
		return ProtocolOpenAIResponses, true
	}
	return "", false
}

// SupportedProtocols 返回全部协议名（校验/文档用）。
func SupportedProtocols() []string {
	return []string{ProtocolAnthropic, ProtocolOpenAIChat, ProtocolOpenAIResponses}
}

// Config 是 AI 网关配置（JSON；建议放 <配置目录>/ai-gateway.json，0600）。
//
// 保持简单：只有一张模型表。每个条目声明「虚拟模型 + 协议类型 + 后端类型 +
// api-key」，端点与会话/参数特化都在后端的代码里（见 backend*.go）。同一个 id
// 写多条 = 故障转移链（按顺序尝试）。
type Config struct {
	// Listen 是监听地址（缺省 127.0.0.1:8780）
	Listen string `json:"listen,omitempty"`
	// UserAgent 是转发时使用的网关 UA（缺省 assistant-ai-gateway/1.0）
	UserAgent string `json:"user_agent,omitempty"`
	// Session 是会话机制配置
	Session SessionConfig `json:"session,omitempty"`
	// Residency 是数据驻留：完整保留请求与响应（默认关闭）
	Residency Residency `json:"residency,omitempty"`
	// Keys 是接入密钥（可选；留空表示不鉴权——此时必须监听环回地址）
	Keys []APIKey `json:"keys,omitempty"`
	// Backends 是自定义/命名后端：标准后端在这里给 base_url 与凭据；type 可
	// 指向内置后端类型（缺省 standard）
	Backends map[string]BackendConfig `json:"backends,omitempty"`
	// Models 是模型路由表；至少一条
	Models []Model `json:"models"`

	// chains 是解析后的路由（Normalize 期计算；不参与序列化）
	chains map[string][]ResolvedUpstream
}

// SessionConfig 是会话机制的全局配置。
type SessionConfig struct {
	// Secret 是内容派生的 HMAC 密钥（集群内一致；建议随机 32 字节的 base64）。
	// 留空退化为无密钥哈希（仍确定，但不同部署间可预测）。
	Secret string `json:"secret,omitempty"`
	// Headers 覆盖默认的显式会话头候选顺序（不区分大小写）
	Headers []string `json:"headers,omitempty"`
}

// Residency 是数据驻留配置：把每个请求（原始体、改写后的上游体）与响应（流式
// 完整落盘）按请求归档到 <Dir>/<日期>/<id>/，供审计与排障。
type Residency struct {
	Enabled bool   `json:"enabled,omitempty"`
	Dir     string `json:"dir,omitempty"`
}

// APIKey 是一个接入密钥。
type APIKey struct {
	Name  string `json:"name"`
	Token string `json:"token"`
	// Models 非空时限定可访问的虚拟模型（支持 "*" 通配）
	Models []string `json:"models,omitempty"`
}

// Model 是模型路由表的一条：虚拟模型 → 后端。
type Model struct {
	// ID 是客户端请求的模型名（OpenAI/Anthropic 请求体里的 model）
	ID string `json:"id"`
	// Protocol 是客户端协议：anthropic-messages / openai-compatible /
	// openai-responses（别名见 NormalizeProtocol）
	Protocol string `json:"protocol"`
	// Backend 是后端类型名（内置，如 opencode-go / zhipu / anthropic /
	// openai），或 backends 里的命名后端；标准后端写 "standard"
	Backend string `json:"backend"`
	// APIKey 是上游凭据（支持 $ENV_VAR / ${ENV_VAR} 展开）；内置后端用它，
	// 命名后端可省略（用 backends 里的）
	APIKey string `json:"api-key,omitempty"`
	// Model 是上游模型名（缺省与 ID 相同，原样透传）
	Model string `json:"model,omitempty"`
	// BaseURL / Auth / Headers 覆盖后端默认（standard 后端必须给 base_url）
	BaseURL string            `json:"base_url,omitempty"`
	Auth    string            `json:"auth,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// TimeoutMS 是单次上游请求超时（缺省 30 分钟）
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
}

// BackendConfig 是 backends 里的命名后端。
type BackendConfig struct {
	// Type 是后端类型（缺省 standard：纯透传，由 base_url 指定端点）
	Type      string            `json:"type,omitempty"`
	BaseURL   string            `json:"base_url,omitempty"`
	APIKey    string            `json:"api-key,omitempty"`
	Auth      string            `json:"auth,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	TimeoutMS int64             `json:"timeout_ms,omitempty"`
}

// ResolvedUpstream 是解析后的可执行上游（Normalize 期算好，运行时不再解释
// 配置；后端特化以函数形式携带）。
type ResolvedUpstream struct {
	Name      string
	Type      string // 后端类型名（standard 表示纯透传）
	BaseURL   string
	Token     string
	Auth      string
	Headers   map[string]string
	Model     string // 上游模型名（空 = 原样）
	TimeoutMS int64
	Prepare   func(header http.Header, body *Body, session Session)
}

// Normalize 填充默认值、展开环境变量并解析路由，幂等。
func (c *Config) Normalize() {
	c.Listen = strings.TrimSpace(c.Listen)
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	c.UserAgent = strings.TrimSpace(c.UserAgent)
	if c.UserAgent == "" {
		c.UserAgent = DefaultUserAgent
	}
	c.Session.Secret = strings.TrimSpace(c.Session.Secret)
	c.Residency.Dir = strings.TrimSpace(c.Residency.Dir)
	for index := range c.Session.Headers {
		c.Session.Headers[index] = strings.TrimSpace(c.Session.Headers[index])
	}
	for index := range c.Keys {
		c.Keys[index].Name = strings.TrimSpace(c.Keys[index].Name)
		c.Keys[index].Token = strings.TrimSpace(c.Keys[index].Token)
	}
	for name, backend := range c.Backends {
		backend.Type = strings.ToLower(strings.TrimSpace(backend.Type))
		backend.BaseURL = strings.TrimRight(strings.TrimSpace(backend.BaseURL), "/")
		backend.Auth = strings.ToLower(strings.TrimSpace(backend.Auth))
		if backend.Auth == "" {
			backend.Auth = "bearer"
		}
		if backend.TimeoutMS <= 0 {
			backend.TimeoutMS = DefaultUpstreamTimeoutMS
		}
		backend.Headers = trimHeaders(backend.Headers)
		c.Backends[name] = backend
	}
	for index := range c.Models {
		model := &c.Models[index]
		model.ID = strings.TrimSpace(model.ID)
		model.Protocol = NormalizeProtocol(model.Protocol)
		model.Backend = strings.TrimSpace(model.Backend)
		model.Model = strings.TrimSpace(model.Model)
		model.BaseURL = strings.TrimRight(strings.TrimSpace(model.BaseURL), "/")
		model.Auth = strings.ToLower(strings.TrimSpace(model.Auth))
		model.Headers = trimHeaders(model.Headers)
		if model.TimeoutMS <= 0 {
			model.TimeoutMS = DefaultUpstreamTimeoutMS
		}
	}
	c.chains = nil
}

func trimHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}
	trimmed := make(map[string]string, len(headers))
	for key, value := range headers {
		trimmed[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return trimmed
}

// expandEnv 展开 $VAR / ${VAR}（$$ 转义为字面 $）；未设置的变量报错（凭据缺失
// 应在加载期暴露，而不是等请求打到上游 401）。
func expandEnv(value string, getenv func(string) string) (string, error) {
	if !strings.Contains(value, "$") {
		return value, nil
	}
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character != '$' {
			builder.WriteByte(character)
			continue
		}
		if index+1 < len(value) && value[index+1] == '$' {
			builder.WriteByte('$')
			index++
			continue
		}
		name := ""
		if index+1 < len(value) && value[index+1] == '{' {
			end := strings.IndexByte(value[index+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("环境变量写法不完整：%q", value)
			}
			name = value[index+2 : index+2+end]
			index += end + 2
		} else {
			end := index + 1
			for end < len(value) && (value[end] == '_' || value[end] >= 'A' && value[end] <= 'Z' ||
				value[end] >= 'a' && value[end] <= 'z' || value[end] >= '0' && value[end] <= '9') {
				end++
			}
			name = value[index+1 : end]
			index = end - 1
		}
		if name == "" {
			return "", fmt.Errorf("环境变量名称为空：%q", value)
		}
		resolved := getenv(name)
		if strings.TrimSpace(resolved) == "" {
			return "", fmt.Errorf("环境变量 %s 未设置（api-key 支持 $VAR / ${VAR}）", name)
		}
		builder.WriteString(resolved)
	}
	return builder.String(), nil
}

// resolveChains 解析 models 表为可执行链（同 id 多条 = 故障转移链）。
func (c *Config) resolveChains(getenv func(string) string) error {
	chains := map[string][]ResolvedUpstream{}
	for index, model := range c.Models {
		if model.ID == "" {
			return fmt.Errorf("models[%d] 缺少 id", index)
		}
		if model.Protocol == "" {
			return fmt.Errorf("models[%d]（%s）的 protocol 无效（支持 %s）",
				index, model.ID, strings.Join(SupportedProtocols(), " / "))
		}
		if model.Backend == "" {
			return fmt.Errorf("models[%d]（%s）缺少 backend", index, model.ID)
		}
		resolved := ResolvedUpstream{
			Name:      model.Backend,
			BaseURL:   model.BaseURL,
			Auth:      model.Auth,
			Headers:   model.Headers,
			Model:     model.Model,
			TimeoutMS: model.TimeoutMS,
		}
		backend, builtin := LookupBackend(model.Backend)
		if named, exists := c.Backends[model.Backend]; exists {
			// 命名后端：类型可指向内置类型，缺省 standard
			if named.Type != "" {
				inner, found := LookupBackend(named.Type)
				if !found && named.Type != "standard" {
					return fmt.Errorf("models[%d]（%s）：backends.%s.type 未知：%s",
						index, model.ID, model.Backend, named.Type)
				}
				backend, builtin = inner, found
			} else {
				backend, builtin = Backend{}, false
			}
			resolved.Name = model.Backend
			resolved.BaseURL = firstNonEmpty(model.BaseURL, named.BaseURL, backend.BaseURL)
			resolved.Auth = firstNonEmpty(model.Auth, named.Auth, backend.Auth, "bearer")
			resolved.Headers = mergeHeaders(backend.Headers, named.Headers, model.Headers)
			resolved.TimeoutMS = firstNonZero(model.TimeoutMS, named.TimeoutMS, backend.TimeoutMS, DefaultUpstreamTimeoutMS)
			resolved.Token = firstNonEmpty(model.APIKey, named.APIKey)
			resolved.Prepare = backend.Prepare
			resolved.Type = backendTypeName(backend, named.Type)
		} else if builtin {
			resolved.BaseURL = firstNonEmpty(model.BaseURL, backend.BaseURL)
			resolved.Auth = firstNonEmpty(model.Auth, backend.Auth, "bearer")
			resolved.Headers = mergeHeaders(backend.Headers, model.Headers)
			resolved.TimeoutMS = firstNonZero(model.TimeoutMS, backend.TimeoutMS, DefaultUpstreamTimeoutMS)
			resolved.Token = model.APIKey
			resolved.Prepare = backend.Prepare
			resolved.Type = backend.Name
		} else if strings.EqualFold(model.Backend, "standard") {
			resolved.Type = "standard"
			resolved.Auth = firstNonEmpty(model.Auth, "bearer")
			resolved.Token = model.APIKey
		} else {
			return fmt.Errorf("models[%d]（%s）：未知后端 %q（内置：standard、%s；或先在 backends 里定义）",
				index, model.ID, model.Backend, strings.Join(BackendNames(), "、"))
		}
		if !builtin && resolved.Type == "standard" && resolved.BaseURL == "" {
			return fmt.Errorf("models[%d]（%s）：标准后端需要 base_url", index, model.ID)
		}
		if builtin && !backendSupportsProtocol(backend, model.Protocol) {
			return fmt.Errorf("models[%d]（%s）：后端 %s 不支持协议 %s（支持 %s）",
				index, model.ID, backend.Name, model.Protocol, strings.Join(backend.Protocols, " / "))
		}
		token, err := expandEnv(resolved.Token, getenv)
		if err != nil {
			return fmt.Errorf("models[%d]（%s）：%w", index, model.ID, err)
		}
		if strings.TrimSpace(token) == "" {
			return fmt.Errorf("models[%d]（%s）：缺少 api-key", index, model.ID)
		}
		resolved.Token = token
		if resolved.BaseURL != "" {
			parsed, err := url.Parse(resolved.BaseURL)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				return fmt.Errorf("models[%d]（%s）：base_url 必须是绝对 HTTP(S) URL：%q",
					index, model.ID, resolved.BaseURL)
			}
		}
		if resolved.Auth != "bearer" && resolved.Auth != "x-api-key" {
			return fmt.Errorf("models[%d]（%s）：auth 只支持 bearer / x-api-key：%q", index, model.ID, resolved.Auth)
		}
		chains[model.ID] = append(chains[model.ID], resolved)
	}
	c.chains = chains
	return nil
}

// Validate 校验配置（必须在 Normalize 之后调用）。
func (c *Config) Validate() error { return c.validate(os.Getenv) }

func (c *Config) validate(getenv func(string) string) error {
	if len(c.Keys) == 0 && !isLoopbackListen(c.Listen) {
		return fmt.Errorf("keys 为空时只能监听环回地址（当前 %s）；对外监听必须配置接入密钥", c.Listen)
	}
	seen := map[string]bool{}
	for index, key := range c.Keys {
		if key.Name == "" || key.Token == "" {
			return fmt.Errorf("keys[%d] 需要 name 与 token", index)
		}
		if seen[key.Token] {
			return fmt.Errorf("keys[%d] 的 token 与前面重复", index)
		}
		seen[key.Token] = true
	}
	if c.Residency.Enabled && c.Residency.Dir == "" {
		return fmt.Errorf("residency.enabled 需要 residency.dir")
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("models 不能为空")
	}
	return c.resolveChains(getenv)
}

func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" || strings.EqualFold(host, "localhost") {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}

func backendTypeName(backend Backend, fallback string) string {
	if backend.Name != "" {
		return backend.Name
	}
	return firstNonEmpty(fallback, "standard")
}

func mergeHeaders(groups ...map[string]string) map[string]string {
	var merged map[string]string
	for _, group := range groups {
		if len(group) == 0 {
			continue
		}
		if merged == nil {
			merged = map[string]string{}
		}
		for key, value := range group {
			merged[key] = value
		}
	}
	return merged
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstNonZero(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

// Load 读取、规范化并校验配置文件。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取网关配置 %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("解析网关配置 %s: %w", path, err)
	}
	config.Normalize()
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("网关配置 %s 无效: %w", path, err)
	}
	return &config, nil
}

// Candidates 返回虚拟模型 + 协议对应的上游链（按配置顺序故障转移；协议不匹配
// 的上游跳过——网关只做同协议透传）。
func (c *Config) Candidates(model, protocol string) []ResolvedUpstream {
	chain := c.chains[model]
	candidates := make([]ResolvedUpstream, 0, len(chain))
	for _, upstream := range chain {
		if upstream.Type == "standard" || upstreamSupportsProtocol(upstream, protocol) {
			candidates = append(candidates, upstream)
		}
	}
	return candidates
}

func upstreamSupportsProtocol(upstream ResolvedUpstream, protocol string) bool {
	backend, ok := LookupBackend(upstream.Type)
	if !ok {
		return true // standard / 命名后端：使用者负责
	}
	return backendSupportsProtocol(backend, protocol)
}

// ModelIDs 返回去重后的虚拟模型名（/v1/models 用）。
func (c *Config) ModelIDs() []string {
	seen := map[string]bool{}
	ids := make([]string, 0, len(c.chains))
	for id := range c.chains {
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// KeyFor 按令牌查找接入密钥（未配置 keys 时全部放行）；支持
// Authorization: Bearer 与 x-api-key 两种载体，比较为常量时间。
func (c *Config) KeyFor(authorization, apiKeyHeader string) (APIKey, bool) {
	if len(c.Keys) == 0 {
		return APIKey{Name: "anonymous"}, true
	}
	token := ""
	if bearer, ok := strings.CutPrefix(strings.TrimSpace(authorization), "Bearer "); ok {
		token = strings.TrimSpace(bearer)
	} else if value := strings.TrimSpace(apiKeyHeader); value != "" {
		token = value
	}
	if token == "" {
		return APIKey{}, false
	}
	for _, key := range c.Keys {
		if constantTimeEqual(key.Token, token) {
			return key, true
		}
	}
	return APIKey{}, false
}

// AllowsModel 报告密钥是否允许访问该虚拟模型。
func (k APIKey) AllowsModel(model string) bool {
	if len(k.Models) == 0 {
		return true
	}
	for _, allowed := range k.Models {
		if allowed == "*" || allowed == model {
			return true
		}
	}
	return false
}
