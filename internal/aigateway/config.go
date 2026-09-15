package aigateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
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
const (
	ProtocolAnthropic       = "anthropic"        // POST /v1/messages
	ProtocolOpenAIChat      = "openai-chat"      // POST /v1/chat/completions
	ProtocolOpenAIResponses = "openai-responses" // POST /v1/responses
)

// DefaultUserAgent 是网关转发时的默认 UA（客户端 UA 缺失或为通用 SDK 名时使用，
// OpenCode Go 要求客户端使用专属 UA）。
const DefaultUserAgent = "assistant-ai-gateway/1.0"

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
type Config struct {
	// Listen 是监听地址（缺省 127.0.0.1:8780）
	Listen string `json:"listen,omitempty"`
	// Session 是会话机制配置
	Session SessionConfig `json:"session,omitempty"`
	// UserAgent 是转发时使用的网关 UA（缺省 assistant-ai-gateway/1.0）
	UserAgent string `json:"user_agent,omitempty"`
	// Residency 是数据驻留：完整保留请求与响应（默认关闭）
	Residency Residency `json:"residency,omitempty"`
	// Keys 是接入密钥（虚拟 key，如 assistant 使用）；至少一个
	Keys []APIKey `json:"keys"`
	// Upstreams 是上游厂商接入点（Anthropic Messages 兼容）；至少一个
	Upstreams []Upstream `json:"upstreams"`
	// Routes 是虚拟模型 → 上游优先级列表（按顺序故障转移）；"*" 为兜底。
	// 至少要有 "*" 或覆盖客户端使用的模型。
	Routes map[string][]string `json:"routes"`
}

// SessionConfig 是会话机制的全局配置。
type SessionConfig struct {
	// Secret 是内容派生的 HMAC 密钥（集群内一致；建议随机 32 字节的 base64）。
	// 留空退化为无密钥哈希（仍确定，但不同部署间可预测）。
	Secret string `json:"secret,omitempty"`
	// Headers 覆盖默认的显式会话头候选顺序（不区分大小写）
	Headers []string `json:"headers,omitempty"`
}

// APIKey 是一个接入密钥。
type APIKey struct {
	Name  string `json:"name"`
	Token string `json:"token"`
	// Models 非空时限定可访问的虚拟模型（支持 "*" 通配）
	Models []string `json:"models,omitempty"`
}

// Residency 是数据驻留配置：把每个请求（原始体、改写后的上游体）与响应（流式
// 完整落盘）按请求归档到 <Dir>/<日期>/<id>/，供审计与排障。
type Residency struct {
	Enabled bool `json:"enabled,omitempty"`
	// Dir 是归档根目录（启用时必填）
	Dir string `json:"dir,omitempty"`
}

// Upstream 是一个上游厂商接入点。
type Upstream struct {
	Name string `json:"name"`
	// BaseURL 是上游根地址（Anthropic Messages / OpenAI 兼容，按 Protocols 声明）
	BaseURL string `json:"base_url"`
	// Protocols 是该上游支持的协议（缺省 ["anthropic"]）：路由时按客户端协议
	// 过滤，只做同协议透传，不做跨协议翻译
	Protocols []string `json:"protocols,omitempty"`
	// Token 是上游凭据
	Token string `json:"token"`
	// Auth 是鉴权方式：bearer（Authorization: Bearer，缺省）或 x-api-key
	Auth string `json:"auth,omitempty"`
	// Headers 是附加的静态请求头（如网关标识、自定义 beta）
	Headers map[string]string `json:"headers,omitempty"`
	// Models 是虚拟模型 → 上游模型名映射；未命中时原样透传
	Models map[string]string `json:"models,omitempty"`
	// Session 是该上游的会话注入规则（各厂商载体不同）
	Session SessionSpec `json:"session,omitempty"`
	// TimeoutMS 是单次上游请求超时（缺省 30 分钟）
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
}

// Normalize 填充默认值并清理空白，幂等。
func (c *Config) Normalize() {
	c.Listen = strings.TrimSpace(c.Listen)
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	c.Session.Secret = strings.TrimSpace(c.Session.Secret)
	c.UserAgent = strings.TrimSpace(c.UserAgent)
	if c.UserAgent == "" {
		c.UserAgent = DefaultUserAgent
	}
	c.Residency.Dir = strings.TrimSpace(c.Residency.Dir)
	for index := range c.Session.Headers {
		c.Session.Headers[index] = strings.TrimSpace(c.Session.Headers[index])
	}
	for index := range c.Keys {
		c.Keys[index].Name = strings.TrimSpace(c.Keys[index].Name)
		c.Keys[index].Token = strings.TrimSpace(c.Keys[index].Token)
		for modelIndex := range c.Keys[index].Models {
			c.Keys[index].Models[modelIndex] = strings.TrimSpace(c.Keys[index].Models[modelIndex])
		}
	}
	for index := range c.Upstreams {
		upstream := &c.Upstreams[index]
		upstream.Name = strings.TrimSpace(upstream.Name)
		upstream.BaseURL = strings.TrimRight(strings.TrimSpace(upstream.BaseURL), "/")
		upstream.Token = strings.TrimSpace(upstream.Token)
		upstream.Auth = strings.ToLower(strings.TrimSpace(upstream.Auth))
		if upstream.Auth == "" {
			upstream.Auth = "bearer"
		}
		if upstream.TimeoutMS <= 0 {
			upstream.TimeoutMS = DefaultUpstreamTimeoutMS
		}
		if len(upstream.Protocols) == 0 {
			upstream.Protocols = []string{ProtocolAnthropic}
		}
		for protocolIndex := range upstream.Protocols {
			upstream.Protocols[protocolIndex] = strings.ToLower(strings.TrimSpace(upstream.Protocols[protocolIndex]))
		}
		if upstream.Headers != nil {
			cleaned := make(map[string]string, len(upstream.Headers))
			for key, value := range upstream.Headers {
				cleaned[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
			upstream.Headers = cleaned
		}
		if upstream.Models != nil {
			cleaned := make(map[string]string, len(upstream.Models))
			for key, value := range upstream.Models {
				cleaned[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
			upstream.Models = cleaned
		}
	}
}

// Validate 校验配置（必须在 Normalize 之后调用）。
func (c *Config) Validate() error {
	if len(c.Keys) == 0 {
		return fmt.Errorf("keys 不能为空（至少一个接入密钥）")
	}
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("upstreams 不能为空（至少一个上游）")
	}
	seenKeys := map[string]bool{}
	for index, key := range c.Keys {
		if key.Name == "" || key.Token == "" {
			return fmt.Errorf("keys[%d] 需要 name 与 token", index)
		}
		if seenKeys[key.Token] {
			return fmt.Errorf("keys[%d] 的 token 与前面重复", index)
		}
		seenKeys[key.Token] = true
	}
	upstreamNames := map[string]bool{}
	for index, upstream := range c.Upstreams {
		if upstream.Name == "" {
			return fmt.Errorf("upstreams[%d] 需要 name", index)
		}
		if upstreamNames[upstream.Name] {
			return fmt.Errorf("upstreams[%d] 的 name 重复：%s", index, upstream.Name)
		}
		upstreamNames[upstream.Name] = true
		parsed, err := url.Parse(upstream.BaseURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("upstreams[%d].base_url 必须是绝对 HTTP(S) URL：%q", index, upstream.BaseURL)
		}
		if upstream.Auth != "bearer" && upstream.Auth != "x-api-key" {
			return fmt.Errorf("upstreams[%d].auth 只支持 bearer / x-api-key：%q", index, upstream.Auth)
		}
		for _, protocol := range upstream.Protocols {
			supported := false
			for _, candidate := range SupportedProtocols() {
				if protocol == candidate {
					supported = true
					break
				}
			}
			if !supported {
				return fmt.Errorf("upstreams[%d].protocols 含未知协议 %q（支持 %s）",
					index, protocol, strings.Join(SupportedProtocols(), "/"))
			}
		}
	}
	if c.Residency.Enabled && c.Residency.Dir == "" {
		return fmt.Errorf("residency.enabled 需要 residency.dir")
	}
	if len(c.Routes) == 0 {
		return fmt.Errorf("routes 不能为空（至少要覆盖客户端使用的模型或 \"*\"）")
	}
	for model, names := range c.Routes {
		if strings.TrimSpace(model) == "" {
			return fmt.Errorf("routes 含空模型名")
		}
		if len(names) == 0 {
			return fmt.Errorf("routes[%s] 的上游列表为空", model)
		}
		for _, name := range names {
			if !upstreamNames[name] {
				return fmt.Errorf("routes[%s] 引用了未定义的上游：%s", model, name)
			}
		}
	}
	return nil
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

// UpstreamByName 查找上游。
func (c *Config) UpstreamByName(name string) (Upstream, bool) {
	for _, upstream := range c.Upstreams {
		if upstream.Name == name {
			return upstream, true
		}
	}
	return Upstream{}, false
}

// UpstreamsFor 返回虚拟模型 + 协议对应的上游链（未命中用 "*" 兜底；只保留
// 声明支持该协议的上游——网关不做跨协议翻译）。
func (c *Config) UpstreamsFor(model, protocol string) []Upstream {
	names, ok := c.Routes[model]
	if !ok {
		names = c.Routes["*"]
	}
	chain := make([]Upstream, 0, len(names))
	for _, name := range names {
		upstream, found := c.UpstreamByName(name)
		if !found || !upstream.SupportsProtocol(protocol) {
			continue
		}
		chain = append(chain, upstream)
	}
	return chain
}

// SupportsProtocol 报告上游是否声明支持该协议。
func (u Upstream) SupportsProtocol(protocol string) bool {
	for _, candidate := range u.Protocols {
		if candidate == protocol {
			return true
		}
	}
	return false
}

// KeyFor 按令牌查找接入密钥；支持 Authorization: Bearer 与 x-api-key 两种
// 载体（Claude Code 用前者）。比较为常量时间（避免时序侧信道）。
func (c *Config) KeyFor(authorization, apiKeyHeader string) (APIKey, bool) {
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
