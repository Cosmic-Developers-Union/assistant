// Package instances 管理多实例配置文件（config.json）。
//
// 一个 instance 对应一台 Gitea 站点；每个 instance 登记仓库清单与三类凭据：
// admin（高权限，读分支保护 + setup 操作用）、reviewer（内容评审机器人）、
// merger（状态评审/合并机器人）。setup 命令负责创建后两类账号与令牌，并把
// 结果回写本文件；其余命令按 instance × repo 迭代执行。
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
	Instances []Instance `json:"instances"`
	// Weixin 是微信（openclaw ilink）对话桥配置：可选；未配置时 daemon 不启动
	// 对话能力。
	Weixin *Weixin `json:"weixin,omitempty"`
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
	// fileProviders 是 <配置目录>/providers/*.json 加载的供应商（一个文件一个
	// provider，文件名即名字）：不写回 config.json，避免任何保存动作把它们内联。
	fileProviders map[string]Provider
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
	// AdminUsers 是允许对话的用户 ID 白名单；空表示只允许扫码登录的用户
	AdminUsers []string `json:"admin_users,omitempty"`
	// Provider 覆盖对话会话使用的 provider 名（缺省回退全局 default_provider）
	Provider string `json:"provider,omitempty"`
	// ClaudeBin / Model / SessionTimeout 是对话会话的执行参数（可选覆盖）
	ClaudeBin string `json:"claude_bin,omitempty"`
	Model     string `json:"model,omitempty"`
	// SessionTimeoutMS 是单轮对话的 claude 超时（缺省 180000 = 3 分钟）
	SessionTimeoutMS int64 `json:"session_timeout_ms,omitempty"`
}

// Instance 是一台 Gitea 站点及其仓库与凭据。
type Instance struct {
	Host string `json:"host"`
	// Provider 覆盖本实例仓库使用的 provider 名（仓库自身 provider 优先，
	// 缺省回退全局 default_provider）
	Provider string `json:"provider,omitempty"`
	// AdminToken 是高权限令牌：读取分支保护需要 repo admin；setup 也用它建
	// 账号与令牌。留空时运行期分支保护读取自动回退严格模式。
	AdminToken string `json:"admin_token,omitempty"`
	// AdminOAuth 是 OAuth 登录留下的刷新凭据：access token 短期有效不落盘，
	// refresh token 长期有效，运行期按需换取（用 OAuth 初始化时写入）。
	AdminOAuth *OAuthCredential `json:"admin_oauth,omitempty"`
	// MCPToken 是独立的 MCP 长期个人令牌，不复用管理或 tea 令牌。
	MCPToken string `json:"mcp_token,omitempty"`
	// MCPUser 是 MCPToken 所属的 Gitea 账号名：登录时以站点身份校验后落盘，
	// 使令牌与账号绑定（换账号必须重新登录，不靠文件位置或环境推断）。
	MCPUser  string  `json:"mcp_user,omitempty"`
	Reviewer Account `json:"reviewer,omitempty"`
	Merger   Account `json:"merger,omitempty"`
	Repos    []Repo  `json:"repos"`
}

// OAuthCredential 是 OAuth2 刷新凭据。
type OAuthCredential struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
	RefreshToken string `json:"refresh_token"`
}

// Account 是一个机器人账号及其访问令牌。
type Account struct {
	Name  string `json:"name"`
	Token string `json:"token,omitempty"`
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
	// MergerToken 是该仓库专属的 merger 令牌（仓库级 Actions workflow 会签/
	// 合并用）。每个项目独立令牌，互不影响；由 setup 生成并写入仓库 secret。
	MergerToken string `json:"merger_token,omitempty"`
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
	if r.Dir == "" && r.MergerToken == "" && r.Provider == "" {
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

// Load 读取并校验配置文件：内联 providers 与 <配置目录>/providers/*.json
// （一个文件一个 provider，文件名即名字）合并，重名视为配置错误。
func Load(path string) (*File, error) {
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
	if err := file.loadProviderFiles(filepath.Dir(path)); err != nil {
		return nil, err
	}
	file.Normalize()
	if err := file.Validate(); err != nil {
		return nil, fmt.Errorf("配置 %s 无效: %w", path, err)
	}
	return &file, nil
}

// loadProviderFiles 读取 <dir>/providers/*.json：每个文件是一个 provider 定义
// （与内联 providers 同形态），文件名去 .json 即 provider 名；目录不存在表示
// 没有文件供应商。文件供应商与内联重名时报错，避免两份定义悄悄打架。
func (f *File) loadProviderFiles(dir string) error {
	providersDir := filepath.Join(dir, "providers")
	entries, err := os.ReadDir(providersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 %s: %w", providersDir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		providerName := strings.TrimSuffix(name, ".json")
		filePath := filepath.Join(providersDir, name)
		content, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("读取 %s: %w", filePath, err)
		}
		if strings.TrimSpace(string(content)) == "" {
			return fmt.Errorf("%s 为空：一个文件一个 provider 定义（env/settings/mcp）", filePath)
		}
		var provider Provider
		if err := json.Unmarshal(content, &provider); err != nil {
			return fmt.Errorf("解析 %s: %w", filePath, err)
		}
		if _, inline := f.Providers[providerName]; inline {
			return fmt.Errorf("providers/%s 与 config.json 的 providers.%s 重名：两处只保留一处", name, providerName)
		}
		if f.fileProviders == nil {
			f.fileProviders = map[string]Provider{}
		}
		f.fileProviders[providerName] = provider
	}
	return nil
}

// Save 原子写入配置文件（0600）；先写同目录临时文件再改名，避免半截文件。
func Save(path string, file *File) error {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
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
	f.DefaultProvider = strings.TrimSpace(f.DefaultProvider)
	f.Optimizations = f.Optimizations.normalized()
	if len(f.Providers) > 0 {
		normalized := make(map[string]Provider, len(f.Providers))
		for name, provider := range f.Providers {
			normalized[strings.TrimSpace(name)] = provider.normalized()
		}
		f.Providers = normalized
	}
	if len(f.fileProviders) > 0 {
		normalized := make(map[string]Provider, len(f.fileProviders))
		for name, provider := range f.fileProviders {
			normalized[strings.TrimSpace(name)] = provider.normalized()
		}
		f.fileProviders = normalized
	}
	for index := range f.Instances {
		f.Instances[index].Normalize()
	}
	f.Weixin.Normalize()
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
	for index, user := range w.AdminUsers {
		w.AdminUsers[index] = strings.TrimSpace(user)
	}
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
	if len(f.Instances) == 0 && f.Weixin == nil {
		return fmt.Errorf("instances 不能为空")
	}
	seen := make(map[string]int, len(f.Instances))
	if err := f.validateProviders(); err != nil {
		return err
	}
	if err := f.Weixin.Validate(); err != nil {
		return fmt.Errorf("weixin: %w", err)
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
	for name, provider := range f.fileProviders {
		if err := provider.Validate("providers/" + name + ".json"); err != nil {
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

// providerNames 返回已定义的 provider 名（含文件供应商，排序，报错信息用）。
func (f *File) providerNames() []string {
	names := make([]string, 0, len(f.Providers)+len(f.fileProviders))
	for name := range f.Providers {
		names = append(names, name)
	}
	for name := range f.fileProviders {
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

// LookupProvider 按名取 provider（内联优先，其次 <配置目录>/providers/ 文件）；
// 返回是否找到。
func (f *File) LookupProvider(name string) (Provider, bool) {
	if name == "" {
		return Provider{}, false
	}
	if provider, ok := f.Providers[name]; ok {
		return provider, true
	}
	provider, ok := f.fileProviders[name]
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

// ProviderFileNames 返回文件供应商的名字（测试/诊断用）。
func (f *File) ProviderFileNames() []string {
	names := make([]string, 0, len(f.fileProviders))
	for name := range f.fileProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Validate 校验单个 instance。必须在 Normalize 之后调用。
func (i Instance) Validate() error {
	parsed, err := url.Parse(i.Host)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("host 必须是绝对 HTTP(S) URL：%q", i.Host)
	}
	if i.Reviewer.Name == i.Merger.Name {
		return fmt.Errorf("reviewer 与 merger 不能是同一账号（%s）", i.Reviewer.Name)
	}
	if i.AdminOAuth != nil {
		if i.AdminOAuth.ClientID == "" || i.AdminOAuth.RefreshToken == "" {
			return fmt.Errorf("admin_oauth 需要 client_id 与 refresh_token")
		}
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
