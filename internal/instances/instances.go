// Package instances 管理用户配置文件 config.json：平台（instance）、仓库清单、
// AI 供应商（providers/optimizations）、微信桥与默认 provider——全部由用户手写。
// 凭据不在这里：本地身份与用途令牌由 assistant 管理在 credentials.json
// （internal/credentials，login/setup 派生）。其余命令按 instance × repo 迭代执行。
package instances

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	builtinagents "assistant/internal/agents"
	"assistant/internal/claudecfg"
	"assistant/internal/provider"
)

// 默认机器人账号名：reviewer 是内容评审者，merger 是状态评审者（会签/合并）。
const (
	DefaultReviewerName = "ai"
	DefaultMergerName   = "merge"
)

// File 是 config.json 的根。
type File struct {
	// Schema 是配置文件里的 $schema（JSON Schema 位置，编辑器用来补全与悬停文档）。
	// 它由 `assistant config init` 写成同目录的 ./config.schema.json；这里是用户
	// 配置的一部分，原样保留、不解释。加载器开了 DisallowUnknownFields，所以它必须
	// 有显式字段，否则带 $schema 的配置会被判为未知键。
	Schema    string     `json:"$schema,omitempty"`
	Instances []Instance `json:"instances"`
	// Weixin 是微信（openclaw ilink）对话桥配置：可选；未配置时 daemon 不启动
	// 对话能力。多微信账号请改用 Channels 列表（本块等价于一条匿名 weixin 实例）。
	Weixin *Weixin `json:"weixin,omitempty"`
	// QQ 是 QQ 开放平台机器人（官方 Bot API v2，WebSocket 网关）对话桥配置：
	// 可选；未配置时 daemon 不启动 QQ 通道。多 QQ 机器人请改用 Channels 列表。
	QQ *QQ `json:"qq,omitempty"`
	// Channels 是多实例对话通道列表（推荐写法）：type 决定平台（weixin/qq/
	// telegram），name 是可选实例标签——多开同一平台（多个微信号、多个 QQ 机器
	// 人、多个 Telegram bot）时用它区分。会话键为 type（未命名，与旧版单实例
	// 一致）或 type/name（命名）。列表里有条目即启用。
	Channels []Channel `json:"channels,omitempty"`
	// Agents 是命名 agent 池：每个 agent 一份独立的 provider/model/系统提示词/
	// 执行参数，对话服务端按「会话 /agent 选择 > 通道默认 > default_agent」解析。
	Agents map[string]Agent `json:"agents,omitempty"`
	// DefaultAgent 是通道与会话都未指定时的兜底 agent 名；留空表示内置缺省
	// （weixin 节的对话参数 + default_provider）。
	DefaultAgent string `json:"default_agent,omitempty"`
	// Providers 是多供应商配置（名字 → 定义）：不同供应商的 env/settings/mcp
	// 格式各异，框架原样透传合并进运行时会话配置。主要手写维护；未配置时行为
	// 与内置缺省（Anthropic 官方）一致。
	Providers map[string]Provider `json:"providers,omitempty"`
	// DefaultProvider 是未在 repo/instance/weixin 指定时的兜底 provider 名；
	// 留空表示内置缺省（无覆盖）。
	DefaultProvider string `json:"default_provider,omitempty"`
	// Optimizations 是全局优化点（env/settings/mcp 同 provider 形态）：对所有
	// 会话生效，选中 provider 的同名覆盖其上；适合放跨供应商通用的调优
	// （时长、上下文窗口、遥测开关等）。
	Optimizations Provider `json:"optimizations,omitempty"`
}

// DefaultTelegramAPIBaseURL 是 Telegram Bot API 的默认地址（被墙环境可换成
// 自建反代/代理地址）。
const DefaultTelegramAPIBaseURL = "https://api.telegram.org"

// 通道实例的平台类型。
const (
	ChannelWeixin   = "weixin"
	ChannelQQ       = "qq"
	ChannelTelegram = "telegram"
)

// Channel 是对话通道列表里的一条实例：type 决定平台，name 是可选实例标签
// （多开同一平台时区分用）。字段按 type 生效（union 平铺：weixin 认 base_url/
// bot_token 等，qq 认 app_id/app_secret 等，telegram 认 bot_token/api_base_url）。
type Channel struct {
	// Type 是平台类型：weixin | qq | telegram
	Type string `json:"type"`
	// Name 是实例标签（日志/诊断用，也是会话键的一部分）；缺省 = type
	Name string `json:"name,omitempty"`

	// —— weixin（openclaw ilink）——
	// BaseURL 是 ilink API 根地址（缺省官方地址）
	BaseURL string `json:"base_url,omitempty"`
	// BotToken 是凭据：weixin 为扫码登录的 Bot token；telegram 为 BotFather 发放的 token
	BotToken string `json:"bot_token,omitempty"`
	// LoginUserID / BotID 是 weixin 扫码登录返回的身份信息（诊断用）
	LoginUserID string `json:"login_user_id,omitempty"`
	BotID       string `json:"ilink_bot_id,omitempty"`
	// BotAgent 是 weixin 观测标识（缺省 OpenClaw）
	BotAgent string `json:"bot_agent,omitempty"`
	// ChannelVersion 是 weixin 声明的渠道版本（缺省取 assistant 自身版本）
	ChannelVersion string `json:"channel_version,omitempty"`
	// RouteTag 是 weixin 可选的部署路由标签（SKRouteTag）
	RouteTag string `json:"route_tag,omitempty"`

	// —— qq（开放平台 Bot API v2）——
	AppID     string `json:"app_id,omitempty"`
	AppSecret string `json:"app_secret,omitempty"`

	// —— qq / telegram 共用 ——
	// APIBaseURL 是 Bot API 根地址（qq 缺省 https://api.sgroup.qq.com；
	// telegram 缺省 https://api.telegram.org，可换自建反代）
	APIBaseURL string `json:"api_base_url,omitempty"`
	// Sandbox 预留：沙箱环境开关（当前版本仅透传日志标记）
	Sandbox bool `json:"sandbox,omitzero"`

	// —— 通用 ——
	// AdminUsers 是允许对话的用户白名单（qq/telegram 为 openid/数字 id）；
	// 空时 weixin 只允许 LoginUserID、qq/telegram 全拒；含 "*" 放开所有人
	AdminUsers []string `json:"admin_users,omitempty"`
	// Agent 是该通道实例的默认 agent 名（缺省回退 default_agent）
	Agent string `json:"agent,omitempty"`
	// SplitLimit 是回复切块的 rune 上限（缺省按平台：weixin 1800 / qq 1000 /
	// telegram 4000）
	SplitLimit int `json:"split_limit,omitzero"`
}

// Key 是通道实例的会话键：未命名 = type（与旧版单实例一致，老用户迁移无感）；
// 命名 = type/name（同一平台的多个实例各有一套会话）。
func (c Channel) Key() string {
	if name := strings.TrimSpace(c.Name); name != "" && name != c.Type {
		return c.Type + "/" + name
	}
	return c.Type
}

// Normalize 填充通道实例默认值并清理空白（必须在 Validate 之前），幂等。
func (c *Channel) Normalize() {
	c.Type = strings.TrimSpace(c.Type)
	c.Name = strings.TrimSpace(c.Name)
	c.Agent = strings.TrimSpace(c.Agent)
	for index, user := range c.AdminUsers {
		c.AdminUsers[index] = strings.TrimSpace(user)
	}
	switch c.Type {
	case ChannelWeixin:
		c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
		if c.BaseURL == "" {
			c.BaseURL = DefaultWeixinBaseURL
		}
		c.BotAgent = strings.TrimSpace(c.BotAgent)
		if c.BotAgent == "" {
			c.BotAgent = "OpenClaw"
		}
	case ChannelQQ:
		c.APIBaseURL = strings.TrimRight(strings.TrimSpace(c.APIBaseURL), "/")
		if c.APIBaseURL == "" {
			c.APIBaseURL = DefaultQQAPIBaseURL
		}
	case ChannelTelegram:
		c.APIBaseURL = strings.TrimRight(strings.TrimSpace(c.APIBaseURL), "/")
		if c.APIBaseURL == "" {
			c.APIBaseURL = DefaultTelegramAPIBaseURL
		}
	}
}

// Validate 校验通道实例（必须在 Normalize 之后调用）。
func (c Channel) Validate() error {
	switch c.Type {
	case ChannelWeixin:
		if strings.TrimSpace(c.BotToken) == "" {
			return fmt.Errorf("type=weixin 需要 bot_token（assistant weixin login 获取；多账号用 --name 写入 channels）")
		}
	case ChannelQQ:
		if c.AppID == "" || c.AppSecret == "" {
			return fmt.Errorf("type=qq 需要 app_id 与 app_secret（q.qq.com 开放平台）")
		}
	case ChannelTelegram:
		if strings.TrimSpace(c.BotToken) == "" {
			return fmt.Errorf("type=telegram 需要 bot_token（@BotFather 发放）")
		}
	default:
		return fmt.Errorf("type 必须是 weixin/qq/telegram：%q", c.Type)
	}
	if c.Name != strings.TrimSpace(c.Name) || strings.ContainsAny(c.Name, "/ \t") {
		return fmt.Errorf("name 不能含空白或 /：%q", c.Name)
	}
	if c.SplitLimit < 0 {
		return fmt.Errorf("split_limit 不能为负：%d", c.SplitLimit)
	}
	return nil
}

// Agent 是命名 agent 池中的一员：对话服务端（multi-agent）的一个可选人格。
// 只配 provider/model 时等价于给不同用户群不同的模型；配 system_prompt 时
// 整体替换对话会话的基础系统提示词。
type Agent struct {
	// Provider 是该 agent 使用的 provider 名（缺省回退 default_provider）
	Provider string `json:"provider,omitempty"`
	// Model 可选模型覆盖（claude --model）
	Model string `json:"model,omitempty"`
	// SystemPrompt 非空时整体替换对话会话的基础系统提示词
	SystemPrompt string `json:"system_prompt,omitempty"`
	// MCP 是该 agent 专属的 MCP server 定义（.mcp.json 形态）：合并进会话
	// MCP 配置，同名 server 覆盖自举（daemon/sessions）与供应商的；适合给
	// 不同 agent 挂不同工具面
	MCP map[string]any `json:"mcp,omitempty"`
	// ClaudeBin 是该 agent 使用的 claude 可执行文件（缺省 PATH 上的 claude）
	ClaudeBin string `json:"claude_bin,omitempty"`
	// SessionTimeoutMS 是该 agent 的单轮对话超时（缺省 180000 = 3 分钟）
	SessionTimeoutMS int64 `json:"session_timeout_ms,omitzero"`
}

// Weixin 是微信对话桥（Tencent/openclaw-weixin 兼容 ilink 协议）的配置。
type Weixin struct {
	// Enabled 为 true 时 assistant run 启动对话桥（--weixin 亦可强制开启）
	Enabled bool `json:"enabled,omitempty"`
	// BaseURL 是 ilink API 根地址（缺省官方地址）
	BaseURL string `json:"base_url,omitempty"`
	// BotToken 是扫码登录得到的 Bot token
	BotToken string `json:"bot_token,omitempty"`
	// LoginUserID / BotID 是扫码登录返回的身份信息（诊断用）
	LoginUserID string `json:"login_user_id,omitempty"`
	BotID       string `json:"ilink_bot_id,omitempty"`
	// BotAgent 是观测标识（缺省 OpenClaw）
	BotAgent string `json:"bot_agent,omitempty"`
	// ChannelVersion 是声明的渠道版本（缺省取 assistant 自身版本）
	ChannelVersion string `json:"channel_version,omitempty"`
	// RouteTag 是可选的部署路由标签（SKRouteTag）
	RouteTag string `json:"route_tag,omitempty"`
	// AdminUsers 是允许对话的用户 ID 白名单；空表示只允许扫码登录的用户；
	// 含 "*" 表示放开所有用户（公网平台慎用）
	AdminUsers []string `json:"admin_users,omitempty"`
	// Agent 是该通道对话的默认 agent 名（缺省回退 default_agent）
	Agent string `json:"agent,omitempty"`
	// Provider 覆盖对话会话使用的 provider 名（缺省回退全局 default_provider）
	Provider string `json:"provider,omitempty"`
	// ClaudeBin / Model / SessionTimeout 是对话会话的执行参数（可选覆盖）。
	// 配置了 agent 池时这些字段仍是「内置缺省 agent」的取值。
	ClaudeBin string `json:"claude_bin,omitempty"`
	Model     string `json:"model,omitempty"`
	// SessionTimeoutMS 是单轮对话的 claude 超时（缺省 180000 = 3 分钟）
	SessionTimeoutMS int64 `json:"session_timeout_ms,omitzero"`
}

// DefaultQQAPIBaseURL 是 QQ 开放平台机器人 API（v2）的默认地址。
const DefaultQQAPIBaseURL = "https://api.sgroup.qq.com"

// QQ 是 QQ 开放平台机器人（q.qq.com，官方 Bot API v2）的配置：WebSocket 网关
// 收事件、REST 发消息，凭据是开放平台控制台的 AppID/AppSecret。收发均为被动
// 回复（带 msg_id，15 分钟窗口）。
type QQ struct {
	// Enabled 为 true 时 assistant run 启动 QQ 通道（--qq 亦可强制开启）
	Enabled bool `json:"enabled,omitzero"`
	// AppID 是开放平台机器人的 AppID
	AppID string `json:"app_id,omitempty"`
	// AppSecret 是开放平台机器人的 AppSecret（与 bot_token 同级敏感，config.json
	// 为 0600）
	AppSecret string `json:"app_secret,omitempty"`
	// APIBaseURL 是 Bot API 根地址（缺省官方地址）
	APIBaseURL string `json:"api_base_url,omitempty"`
	// Sandbox 预留：沙箱环境开关（当前版本仅透传日志标记）
	Sandbox bool `json:"sandbox,omitzero"`
	// AdminUsers 是允许对话的用户 openid 白名单；空时拒绝所有用户；含 "*"
	// 表示放开所有用户（群聊场景慎用，任何 @ 机器人的人都会消耗 AI 额度）
	AdminUsers []string `json:"admin_users,omitempty"`
	// Agent 是该通道对话的默认 agent 名（缺省回退 default_agent）
	Agent string `json:"agent,omitempty"`
	// SplitLimit 是回复切块的 rune 上限（缺省 1000；平台对 content 长度有限制）
	SplitLimit int `json:"split_limit,omitzero"`
}

// Instance 是一台 Gitea 站点及其仓库。
//
// 这里**只有配置，没有凭据**：所有令牌按 (host, user, purpose) 存在
// credentials.json（见 internal/credentials）。Reviewer/Merger 只保留账号名——
// 它们的令牌同样在凭据库里。
type Instance struct {
	Host string `json:"host"`
	// Provider 覆盖本实例仓库使用的 provider 名（仓库自身 provider 优先，
	// 缺省回退全局 default_provider）
	Provider string `json:"provider,omitempty"`
	// Reviewer / Merger 是内容评审与状态评审/合并的机器人账号名（缺省 ai / merge）。
	Reviewer Account `json:"reviewer,omitempty"`
	Merger   Account `json:"merger,omitempty"`
	Repos    []Repo  `json:"repos"`
}

// Account 是一个机器人账号（令牌在凭据库里）。
type Account struct {
	Name string `json:"name"`
}

// Repo 是一个被管理的仓库。JSON 里可写 "owner/name" 简写，或 {"name":...,
// "dir":...} 对象：dir 是该仓库的本地检出路径，调度引擎的 review/triage
// 会话需要它（单仓库可省略，退回启动目录的检出）。
type Repo struct {
	Name string `json:"name"`
	Dir  string `json:"dir,omitempty"`
	// Provider 是该仓库专属的 provider 名（最高优先级；缺省回退 instance 与
	// 全局 default_provider）
	Provider string `json:"provider,omitempty"`
}

func (r *Repo) UnmarshalJSON(data []byte) error {
	var shorthand string
	if err := json.Unmarshal(data, &shorthand); err == nil {
		r.Name = shorthand
		r.Dir = ""
		return nil
	}
	type plain Repo
	var value plain
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf(`仓库条目必须是 "owner/name" 或 {"name": "owner/name", "dir": "..."}`)
	}
	*r = Repo(value)
	return nil
}

// MarshalJSON 无扩展字段时写回字符串简写，保持配置文件简洁。
func (r Repo) MarshalJSON() ([]byte, error) {
	if r.Dir == "" && r.Provider == "" {
		return json.Marshal(r.Name)
	}
	type plain Repo
	return json.Marshal(plain(r))
}

// Provider 是一个 agent 运行时供应商（Anthropic 官方、Anthropic 兼容网关、
// Bedrock/Vertex 等）的配置。三段覆盖均原样透传（框架只做合并，不解释供应商
// 语义）：供应商格式各异时也无需框架适配。
//
// 未识别的键原样保留（Provider.extra）：手写扩展不报错、读取-写回不丢失。
type Provider struct {
	// Env 是注入会话的环境变量（可含密钥）：只进运行时 --settings，不写入
	// 仓库文件；provider 自定义的 MCP server 亦缺省继承它（server 自身优先）。
	// 取值为空的键表示移除内置预设的同名默认。
	Env map[string]string `json:"env,omitempty"`
	// Settings 是 Claude Code 原生 settings 片段（如 model、apiKeyHelper、
	// awsAuthRefresh）：合并进运行时会话 settings，provider 取值优先。
	Settings map[string]any `json:"settings,omitempty"`
	// MCP 是原生 MCP server 定义（.mcp.json 形态）：合并进会话 --mcp-config，
	// 同名 server 由 provider 覆盖（仓库既有的 gitea 等不受影响）。
	MCP map[string]any `json:"mcp,omitempty"`
	// APIKey / AuthToken 是开箱即用的令牌简写（写哪个都行，api_key 优先）：
	// 由内置预设决定落到哪个环境变量（Anthropic 官方 ANTHROPIC_API_KEY，
	// 第三方多为 ANTHROPIC_AUTH_TOKEN），多数供应商只写这一个字段即可。
	APIKey    string `json:"api_key,omitempty"`
	AuthToken string `json:"auth_token,omitempty"`
	// extra 保存未识别键，原样保留（校验与写回不丢）。
	extra map[string]json.RawMessage
}

// Token 返回令牌简写（api_key 优先）。
func (p Provider) Token() string {
	if strings.TrimSpace(p.APIKey) != "" {
		return strings.TrimSpace(p.APIKey)
	}
	return strings.TrimSpace(p.AuthToken)
}

// UnmarshalJSON 松弛解析 provider 定义：只识别 env/settings/mcp，其余键原样
// 保留，便于手写扩展与其他供应商格式。
func (p *Provider) UnmarshalJSON(data []byte) error {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("provider 定义必须是 JSON 对象: %w", err)
	}
	p.Env, p.Settings, p.MCP, p.extra = nil, nil, nil, nil
	p.APIKey, p.AuthToken = "", ""
	for key, value := range fields {
		switch key {
		case "env":
			var raw map[string]any
			if err := json.Unmarshal(value, &raw); err != nil {
				return fmt.Errorf("provider.env 必须是对象: %w", err)
			}
			env, err := stringifyEnv(raw)
			if err != nil {
				return err
			}
			p.Env = env
		case "settings":
			if err := json.Unmarshal(value, &p.Settings); err != nil {
				return fmt.Errorf("provider.settings 必须是对象: %w", err)
			}
		case "mcp":
			if err := json.Unmarshal(value, &p.MCP); err != nil {
				return fmt.Errorf("provider.mcp 必须是对象: %w", err)
			}
		case "api_key":
			if err := json.Unmarshal(value, &p.APIKey); err != nil {
				return fmt.Errorf("provider.api_key 必须是字符串: %w", err)
			}
		case "auth_token":
			if err := json.Unmarshal(value, &p.AuthToken); err != nil {
				return fmt.Errorf("provider.auth_token 必须是字符串: %w", err)
			}
		default:
			if p.extra == nil {
				p.extra = map[string]json.RawMessage{}
			}
			p.extra[key] = value
		}
	}
	return nil
}

// MarshalJSON 写回已知三段与保留的扩展键（map 序列化按键排序，输出稳定）。
func (p Provider) MarshalJSON() ([]byte, error) {
	fields := map[string]json.RawMessage{}
	for key, value := range p.extra {
		fields[key] = value
	}
	encode := func(key string, value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		fields[key] = raw
		return nil
	}
	if len(p.Env) > 0 {
		if err := encode("env", p.Env); err != nil {
			return nil, err
		}
	}
	if len(p.Settings) > 0 {
		if err := encode("settings", p.Settings); err != nil {
			return nil, err
		}
	}
	if len(p.MCP) > 0 {
		if err := encode("mcp", p.MCP); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(p.APIKey) != "" {
		if err := encode("api_key", p.APIKey); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(p.AuthToken) != "" {
		if err := encode("auth_token", p.AuthToken); err != nil {
			return nil, err
		}
	}
	return json.Marshal(fields)
}

// stringifyEnv 把 env 值折成字符串：接受字符串、数字与布尔（手写配置常见
// 混合类型），嵌套对象/数组报错。
func stringifyEnv(raw map[string]any) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(raw))
	for key, value := range raw {
		switch typed := value.(type) {
		case string:
			env[key] = typed
		case bool:
			env[key] = strconv.FormatBool(typed)
		case float64:
			env[key] = strconv.FormatFloat(typed, 'f', -1, 64)
		default:
			return nil, fmt.Errorf("provider.env[%s] 必须是标量（字符串/数字/布尔）", key)
		}
	}
	return env, nil
}

// Overrides 返回 provider 的运行时覆盖视图（claudecfg 消费）。
func (p Provider) Overrides() claudecfg.Overrides {
	return claudecfg.Overrides{Env: p.Env, Settings: p.Settings, MCP: p.MCP}
}

// RepoNames 返回非空仓库名清单。
func (i Instance) RepoNames() []string {
	names := make([]string, 0, len(i.Repos))
	for _, repo := range i.Repos {
		if repo.Name != "" {
			names = append(names, repo.Name)
		}
	}
	return names
}

// FindRepo 按 owner/name 查找仓库配置。
func (i Instance) FindRepo(name string) (Repo, bool) {
	for _, repo := range i.Repos {
		if repo.Name == name {
			return repo, true
		}
	}
	return Repo{}, false
}

// Load 读取并校验配置文件。provider 定义就在 config.json 的 providers 里——
// 用户配置只有这一个落点（凭据在 credentials.json，由 assistant 管理）。
func Load(path string) (*File, error) {
	file, err := Parse(path)
	if err != nil {
		return nil, err
	}
	if err := file.Validate(); err != nil {
		return nil, fmt.Errorf("配置 %s 无效: %w", path, err)
	}
	return file, nil
}

// Parse 读取配置并规范化，但不做语义校验（instances 允许为空）：`config new`
// 生成的空骨架对运行无效，`config init` 却要能在它上面补全——语法与结构错误
// 照报，语义问题留给合并结果写入前的 Validate。
func Parse(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file File
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	file.Normalize()
	return &file, nil
}

// Save 序列化并原子写入配置文件（0600）。
func Save(path string, file *File) error {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return SaveBytes(path, data)
}

// SaveBytes 原子写入配置文件（0600）：先写同目录临时文件再改名，避免半截文件。
func SaveBytes(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".config-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

// Normalize 填充默认值并清理空白，幂等。
func (f *File) Normalize() {
	f.Schema = strings.TrimSpace(f.Schema)
	f.DefaultProvider = strings.TrimSpace(f.DefaultProvider)
	f.DefaultAgent = strings.TrimSpace(f.DefaultAgent)
	f.Optimizations = f.Optimizations.normalized()
	if len(f.Providers) > 0 {
		normalized := make(map[string]Provider, len(f.Providers))
		for name, provider := range f.Providers {
			normalized[strings.TrimSpace(name)] = provider.normalized()
		}
		f.Providers = normalized
	}
	if len(f.Agents) > 0 {
		normalized := make(map[string]Agent, len(f.Agents))
		for name, agent := range f.Agents {
			normalized[strings.TrimSpace(name)] = agent.normalized()
		}
		f.Agents = normalized
	}
	for index := range f.Instances {
		f.Instances[index].Normalize()
	}
	for index := range f.Channels {
		f.Channels[index].Normalize()
	}
	f.Weixin.Normalize()
	f.QQ.Normalize()
}

// normalized 清理 provider 内的空白并去掉空 env 键，幂等。
func (p Provider) normalized() Provider {
	if len(p.Env) > 0 {
		env := make(map[string]string, len(p.Env))
		for key, value := range p.Env {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			env[key] = strings.TrimSpace(value)
		}
		if len(env) == 0 {
			env = nil
		}
		p.Env = env
	}
	return p
}

// Validate 校验 provider 定义（必须在 Normalize 之后调用）。
func (p Provider) Validate(name string) error {
	for key := range p.Env {
		if strings.ContainsAny(key, "=\x00 \t\n\r") {
			return fmt.Errorf("providers[%s].env 含非法键名：%q", name, key)
		}
	}
	return nil
}

// DefaultWeixinBaseURL 是 ilink 协议的默认 API 地址。
const DefaultWeixinBaseURL = "https://ilinkai.weixin.qq.com"

// Normalize 填充微信桥默认值并清理空白，幂等。
func (w *Weixin) Normalize() {
	if w == nil {
		return
	}
	w.BaseURL = strings.TrimRight(strings.TrimSpace(w.BaseURL), "/")
	if w.BaseURL == "" {
		w.BaseURL = DefaultWeixinBaseURL
	}
	w.BotToken = strings.TrimSpace(w.BotToken)
	w.BotAgent = strings.TrimSpace(w.BotAgent)
	if w.BotAgent == "" {
		w.BotAgent = "OpenClaw"
	}
	w.ChannelVersion = strings.TrimSpace(w.ChannelVersion)
	w.RouteTag = strings.TrimSpace(w.RouteTag)
	w.Provider = strings.TrimSpace(w.Provider)
	w.ClaudeBin = strings.TrimSpace(w.ClaudeBin)
	w.Model = strings.TrimSpace(w.Model)
	w.Agent = strings.TrimSpace(w.Agent)
	for index, user := range w.AdminUsers {
		w.AdminUsers[index] = strings.TrimSpace(user)
	}
}

// Normalize 填充 QQ 通道默认值并清理空白，幂等。
func (q *QQ) Normalize() {
	if q == nil {
		return
	}
	q.APIBaseURL = strings.TrimRight(strings.TrimSpace(q.APIBaseURL), "/")
	if q.APIBaseURL == "" {
		q.APIBaseURL = DefaultQQAPIBaseURL
	}
	q.AppID = strings.TrimSpace(q.AppID)
	q.AppSecret = strings.TrimSpace(q.AppSecret)
	q.Agent = strings.TrimSpace(q.Agent)
	for index, user := range q.AdminUsers {
		q.AdminUsers[index] = strings.TrimSpace(user)
	}
}

// normalized 清理 agent 定义内的空白，幂等。
func (a Agent) normalized() Agent {
	a.Provider = strings.TrimSpace(a.Provider)
	a.Model = strings.TrimSpace(a.Model)
	a.ClaudeBin = strings.TrimSpace(a.ClaudeBin)
	return a
}

// Normalize 填充 instance 内的默认值并清理空白，幂等。
func (i *Instance) Normalize() {
	i.Host = strings.TrimRight(strings.TrimSpace(i.Host), "/")
	i.Provider = strings.TrimSpace(i.Provider)
	if i.Reviewer.Name == "" {
		i.Reviewer.Name = DefaultReviewerName
	}
	if i.Merger.Name == "" {
		i.Merger.Name = DefaultMergerName
	}
	for repoIndex := range i.Repos {
		i.Repos[repoIndex].Name = strings.TrimSpace(i.Repos[repoIndex].Name)
		i.Repos[repoIndex].Dir = strings.TrimSpace(i.Repos[repoIndex].Dir)
		i.Repos[repoIndex].Provider = strings.TrimSpace(i.Repos[repoIndex].Provider)
	}
}

// Validate 校验 host、账号与仓库。必须在 Normalize 之后调用。
func (f *File) Validate() error {
	if len(f.Instances) == 0 && f.Weixin == nil && f.QQ == nil && len(f.Channels) == 0 {
		return fmt.Errorf("instances 不能为空")
	}
	seen := make(map[string]int, len(f.Instances))
	if err := f.validateProviders(); err != nil {
		return err
	}
	if err := f.validateAgents(); err != nil {
		return err
	}
	if err := f.Weixin.Validate(); err != nil {
		return fmt.Errorf("weixin: %w", err)
	}
	if err := f.QQ.Validate(); err != nil {
		return fmt.Errorf("qq: %w", err)
	}
	if err := f.validateChannels(); err != nil {
		return err
	}
	for index := range f.Instances {
		if err := f.Instances[index].Validate(); err != nil {
			return fmt.Errorf("instances[%d]: %w", index, err)
		}
		if previous, ok := seen[f.Instances[index].Host]; ok {
			return fmt.Errorf("instances[%d] 与 instances[%d] 的 host 重复：%s", index, previous, f.Instances[index].Host)
		}
		seen[f.Instances[index].Host] = index
	}
	return nil
}

// validateAgents 校验 agent 池与所有 agent 引用（default_agent、weixin.agent、
// qq.agent、channels[].agent）：引用名必须是用户 agents 或内置预设（internal/
// agents，随二进制分发）；引用不存在的名字视为配置错误，避免对话轮才静默回退。
func (f *File) validateAgents() error {
	for name, agent := range f.Agents {
		if name == "" {
			return fmt.Errorf("agents 含空名字")
		}
		if agent.Provider == "" {
			continue
		}
		user, ok := f.LookupProvider(agent.Provider)
		if !ok {
			return fmt.Errorf("agents[%s] 引用的 provider %q 未在 providers 中定义（可用：%s）",
				name, agent.Provider, strings.Join(f.providerNames(), "、"))
		}
		// 预设展开（令牌简写、必须的 base_url 等）在配置校验期就报错，
		// 而不是等会话启动
		if _, err := provider.Resolve(agent.Provider, f.Optimizations.Overrides(), user.Overrides(), user.Token()); err != nil {
			return fmt.Errorf("agents[%s]: %w", name, err)
		}
	}
	reference := func(label, name string) error {
		if name == "" {
			return nil
		}
		if _, ok := f.Agents[name]; ok {
			return nil
		}
		if builtinagents.Has(name) {
			return nil
		}
		return fmt.Errorf("%s 引用的 agent %q 未定义（用户 agents：%s；内置：%s）",
			label, name, strings.Join(f.agentNames(), "、"), strings.Join(builtinagents.Names(), "、"))
	}
	if err := reference("default_agent", f.DefaultAgent); err != nil {
		return err
	}
	if f.Weixin != nil {
		if err := reference("weixin.agent", f.Weixin.Agent); err != nil {
			return err
		}
	}
	if f.QQ != nil {
		if err := reference("qq.agent", f.QQ.Agent); err != nil {
			return err
		}
	}
	for index := range f.Channels {
		if err := reference(fmt.Sprintf("channels[%d].agent", index), f.Channels[index].Agent); err != nil {
			return err
		}
	}
	return nil
}

// validateChannels 校验通道实例列表：type 合法、实例键唯一、与旧版单实例块的
// 匿名键不冲突（同一个会话键起两份通道会让消息路由不确定）。
func (f *File) validateChannels() error {
	seen := map[string]int{}
	for index := range f.Channels {
		channel := &f.Channels[index]
		if err := channel.Validate(); err != nil {
			return fmt.Errorf("channels[%d]: %w", index, err)
		}
		if previous, ok := seen[channel.Key()]; ok {
			return fmt.Errorf("channels[%d] 与 channels[%d] 的实例键重复：%s（多开同平台请用 name 区分）",
				index, previous, channel.Key())
		}
		seen[channel.Key()] = index
		if channel.Name == "" {
			switch channel.Type {
			case ChannelWeixin:
				if f.Weixin != nil {
					return fmt.Errorf("channels[%d]: 匿名 weixin 实例与旧版 weixin 节冲突——给实例起 name 或删掉 weixin 节", index)
				}
			case ChannelQQ:
				if f.QQ != nil {
					return fmt.Errorf("channels[%d]: 匿名 qq 实例与旧版 qq 节冲突——给实例起 name 或删掉 qq 节", index)
				}
			}
		}
	}
	return nil
}

// agentNames 返回已定义的 agent 名（排序，报错信息用）。
func (f *File) agentNames() []string {
	names := make([]string, 0, len(f.Agents))
	for name := range f.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateProviders 校验 providers 定义与所有 provider 引用（含 default_provider、
// instance、repo、weixin）：引用不存在的名字视为配置错误，避免静默用错供应商。
func (f *File) validateProviders() error {
	if err := f.Optimizations.Validate("optimizations"); err != nil {
		return err
	}
	for name, provider := range f.Providers {
		if name == "" {
			return fmt.Errorf("providers 含空名字")
		}
		if err := provider.Validate(name); err != nil {
			return err
		}
	}
	reference := func(label, name string) error {
		if name == "" {
			return nil
		}
		user, ok := f.LookupProvider(name)
		if !ok {
			return fmt.Errorf("%s 引用的 provider %q 未在 providers 中定义（可用：%s）",
				label, name, strings.Join(f.providerNames(), "、"))
		}
		// 预设展开（令牌简写、必须的 base_url 等）在配置校验期就报错，
		// 而不是等会话启动
		if _, err := provider.Resolve(name, f.Optimizations.Overrides(), user.Overrides(), user.Token()); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		return nil
	}
	if err := reference("default_provider", f.DefaultProvider); err != nil {
		return err
	}
	if f.Weixin != nil {
		if err := reference("weixin.provider", f.Weixin.Provider); err != nil {
			return err
		}
	}
	for index := range f.Instances {
		instance := &f.Instances[index]
		if err := reference(fmt.Sprintf("instances[%d].provider", index), instance.Provider); err != nil {
			return err
		}
		for repoIndex := range instance.Repos {
			label := fmt.Sprintf("instances[%d].repos[%d].provider", index, repoIndex)
			if err := reference(label, instance.Repos[repoIndex].Provider); err != nil {
				return err
			}
		}
	}
	return nil
}

// providerNames 返回已定义的 provider 名（排序，报错信息用）。
func (f *File) providerNames() []string {
	names := make([]string, 0, len(f.Providers))
	for name := range f.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ProviderName 返回仓库生效的 provider 名：repo > instance > 全局默认；空串
// 表示内置缺省（无覆盖）。
func (f *File) ProviderName(instance *Instance, repo *Repo) string {
	if repo != nil && repo.Provider != "" {
		return repo.Provider
	}
	if instance != nil && instance.Provider != "" {
		return instance.Provider
	}
	return f.DefaultProvider
}

// WeixinProviderName 返回微信对话会话生效的 provider 名：weixin.provider >
// 全局默认。
func (f *File) WeixinProviderName() string {
	if f.Weixin != nil && f.Weixin.Provider != "" {
		return f.Weixin.Provider
	}
	return f.DefaultProvider
}

// AgentProviderName 返回命名 agent 生效的 provider 名：agents[name].provider >
// 全局默认；名字不存在时回退全局默认（Validate 已保证引用存在）。
func (f *File) AgentProviderName(name string) string {
	if agent, ok := f.Agents[name]; ok && agent.Provider != "" {
		return agent.Provider
	}
	return f.DefaultProvider
}

// AgentNames 返回已定义的 agent 名（排序，诊断/展示用）。
func (f *File) AgentNames() []string { return f.agentNames() }

// LookupProvider 按名取 config.json 里定义的 provider；返回是否找到。
func (f *File) LookupProvider(name string) (Provider, bool) {
	if name == "" {
		return Provider{}, false
	}
	provider, ok := f.Providers[name]
	return provider, ok
}

// ProviderNameOf 按名取 provider，未找到返回零值（调用方已完成校验的场景）。
func (f *File) ProviderNameOf(name string) Provider {
	provider, _ := f.LookupProvider(name)
	return provider
}

// EffectiveOverrides 返回实体生效的运行时覆盖：
//
//	托管默认 < 内置预设（provider 包，开箱即用） < 全局 optimizations <
//	用户 provider 覆盖 < api_key/auth_token 简写
//
// 未识别的 provider 名没有预设，行为与纯手写配置一致。
func (f *File) EffectiveOverrides(providerName string) (claudecfg.Overrides, error) {
	user := f.ProviderNameOf(providerName)
	return provider.Resolve(providerName, f.Optimizations.Overrides(), user.Overrides(), user.Token())
}

// ValidateHost 校验站点地址必须是绝对 HTTP(S) URL（instance 与 run.yaml 的
// monitor 共用）。
func ValidateHost(host string) error {
	parsed, err := url.Parse(host)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("host 必须是绝对 HTTP(S) URL：%q", host)
	}
	return nil
}

// Validate 校验单个 instance。必须在 Normalize 之后调用。
func (i Instance) Validate() error {
	if err := ValidateHost(i.Host); err != nil {
		return err
	}
	if i.Reviewer.Name == i.Merger.Name {
		return fmt.Errorf("reviewer 与 merger 不能是同一账号（%s）", i.Reviewer.Name)
	}
	for _, repo := range i.Repos {
		if _, _, err := ParseRepoName(repo.Name); err != nil {
			return err
		}
	}
	return nil
}

// Validate 校验微信桥配置（必须在 Normalize 之后调用）。
func (w *Weixin) Validate() error {
	if w == nil {
		return nil
	}
	parsed, err := url.Parse(w.BaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("weixin.base_url 必须是绝对 HTTP(S) URL：%q", w.BaseURL)
	}
	if w.Enabled && w.BotToken == "" {
		return fmt.Errorf("weixin.enabled 需要 bot_token：先 assistant weixin login")
	}
	return nil
}

// Validate 校验 QQ 通道配置（必须在 Normalize 之后调用）。
func (q *QQ) Validate() error {
	if q == nil {
		return nil
	}
	parsed, err := url.Parse(q.APIBaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("qq.api_base_url 必须是绝对 HTTP(S) URL：%q", q.APIBaseURL)
	}
	if q.Enabled && (q.AppID == "" || q.AppSecret == "") {
		return fmt.Errorf("qq.enabled 需要 app_id 与 app_secret：在 q.qq.com 开放平台创建机器人后填入")
	}
	if q.SplitLimit < 0 {
		return fmt.Errorf("qq.split_limit 不能为负：%d", q.SplitLimit)
	}
	return nil
}

// ParseRepoName 解析 owner/name。
func ParseRepoName(name string) (owner, repository string, err error) {
	owner, repository, ok := strings.Cut(name, "/")
	if !ok || owner == "" || repository == "" || strings.Contains(repository, "/") {
		return "", "", fmt.Errorf("仓库必须使用 owner/name 格式：%q", name)
	}
	return owner, repository, nil
}

// 平台标准配置目录下的命名空间/应用名（Linux: $XDG_CONFIG_HOME 或
// ~/.config；macOS: ~/Library/Application Support；Windows: %AppData%）。
const (
	configNamespace = "Cosmic-Developers-Union"
	configApp       = "assistant"
)

// DefaultConfigDir 返回平台标准配置目录。
func DefaultConfigDir() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, configNamespace, configApp), nil
}

// ClaudeDir 是 assistant 托管的 Claude Code 配置根（会话文本记录、会话状态与
// Claude 自己的全局配置文件都落在这里）：$CLAUDE_CONFIG_DIR 优先，否则
// <配置目录>/claude。它随配置目录一起挂载/备份，**不读也不写用户的 ~/.claude**——
// 会话凭据由 provider 配置（config.json 的 providers/optimizations）提供，会话
// 数据不与用户本人的 Claude 安装互相污染。
func ClaudeDir() (string, error) {
	if value := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); value != "" {
		return value, nil
	}
	directory, err := DefaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "claude"), nil
}

// DefaultConfigPath 是标准配置目录下的 config.json 路径。
func DefaultConfigPath() (string, error) {
	directory, err := DefaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "config.json"), nil
}

// DaemonEndpointPath 返回 daemon 端点文件的落点：
// <配置目录>/daemon.json（含 API 地址/令牌/PID，0600；assistant mcp daemon
// 靠它自举发现运行中的 daemon）。
func DaemonEndpointPath() (string, error) {
	directory, err := DefaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "daemon.json"), nil
}

// DefaultRepoDir 返回受管克隆（基线检出）的默认落点：
// <数据目录>/Cosmic-Developers-Union/assistant/repos/<host>/<owner>/<name>；
// 未配置 repo.dir 的仓库在这里自动 clone 并保持与 origin/<base> 一致，run 与
// 当前目录彻底解耦。数据目录取 XDG_DATA_HOME，缺省 ~/.local/share。
func DefaultRepoDir(host, fullName string) (string, error) {
	root, slug, owner, name, err := managedPathParts(host, fullName)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "repos", slug, owner, name), nil
}

// DefaultRepoStateDir 返回受管仓库的状态目录（日志、单飞锁）：
// <数据目录>/Cosmic-Developers-Union/assistant/state/<host>/<owner>/<name>；
// 放在检出之外，避免被 SyncMirror 的 clean -fd 波及。
func DefaultRepoStateDir(host, fullName string) (string, error) {
	root, slug, owner, name, err := managedPathParts(host, fullName)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "state", slug, owner, name), nil
}

func managedPathParts(host, fullName string) (root, slug, owner, name string, err error) {
	home := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if home == "" {
		userHome, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return "", "", "", "", fmt.Errorf("定位用户目录: %w", homeErr)
		}
		home = filepath.Join(userHome, ".local", "share")
	}
	owner, name, err = ParseRepoName(fullName)
	if err != nil {
		return "", "", "", "", err
	}
	slug, err = HostSlug(host)
	if err != nil {
		return "", "", "", "", err
	}
	root = filepath.Join(home, configNamespace, configApp)
	return root, slug, owner, name, nil
}

// HostSlug 把站点地址折成目录/路径名：去 scheme，保留 host[:port]，其余字符
// 替换为 -（受管目录与 worktree 根共用）。
func HostSlug(host string) (string, error) {
	trimmed := strings.TrimSpace(host)
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("无法解析平台地址：%q", host)
	}
	var builder strings.Builder
	for _, character := range parsed.Host {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '.', character == '-':
			builder.WriteRune(character)
		default:
			builder.WriteRune('-')
		}
	}
	return builder.String(), nil
}
