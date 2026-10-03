// Package runtime 将声明式 bot 配置装配成事件处理流水线；平台、工作区、记忆与
// agent 启动相互独立，平台待办是调度的唯一事实源。
package runtime

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"gopkg.in/yaml.v3"
)

// Config 只描述本次运行，不引用开发者凭据库。
type Config struct {
	Connects map[string]Connect `yaml:"connects"`
	MCP      map[string]MCP     `yaml:"mcp"`
	Bots     map[string]Bot     `yaml:"bots"`
	Session  SessionConfig      `yaml:"session"`
	Runtime  Options            `yaml:"runtime"`
}

// Connect 的平台字段在校验时按 type 分组，禁止混用。
type Connect struct {
	Type       string   `yaml:"type"`
	URL        string   `yaml:"url"`
	Token      string   `yaml:"token"`
	AppID      string   `yaml:"app-id"`
	AppSecret  string   `yaml:"app-secret"`
	UserID     string   `yaml:"user-id"`
	AdminUsers []string `yaml:"admin-users"`
}

// MCP 描述 stdio 工具的启动方式。
type MCP struct {
	Cmd  string            `yaml:"cmd" json:"command"`
	Args []string          `yaml:"args" json:"args,omitempty"`
	Env  map[string]string `yaml:"env" json:"env,omitempty"`
}

// Bot 将连接、工作区与 agent 按名字组合。
type Bot struct {
	Kind      string            `yaml:"kind"`
	Use       map[string]string `yaml:"use"`
	With      Parameters        `yaml:"with"`
	Workspace WorkspaceSpec     `yaml:"workspace"`
	Agent     AgentSpec         `yaml:"agent"`
}

// Parameters 是行为参数；聊天 bot 可用相同流水线消费消息。
type Parameters struct {
	Identity string `yaml:"identity"`
	Prompt   string `yaml:"prompt"`
}

// WorkspaceSpec 是工作区实现的声明，事件模板在 Prepare 时展开。
type WorkspaceSpec struct {
	Type string `yaml:"type"`
	Repo string `yaml:"repo"`
	Ref  string `yaml:"ref"`
}

// AgentSpec 是启动一次会话的输入，不承担完成判定。
type AgentSpec struct {
	System     string         `yaml:"system"`
	MCP        []string       `yaml:"mcp"`
	Model      string         `yaml:"model"`
	Bin        string         `yaml:"bin"`
	Prompt     string         `yaml:"-"`
	Servers    map[string]MCP `yaml:"-"`
	SessionDir string         `yaml:"-"`
}

// SessionConfig 选择本地记忆或 S3 副本，远端错误必须向调用者返回。
type SessionConfig struct {
	Store     string `yaml:"store"`
	Endpoint  string `yaml:"endpoint"`
	Bucket    string `yaml:"bucket"`
	Prefix    string `yaml:"prefix"`
	AccessKey string `yaml:"access-key"`
	SecretKey string `yaml:"secret-key"`
}

// Options 提供少量运行参数；缺省值不来自用户登录态。
type Options struct {
	Root        string        `yaml:"root"`
	Interval    time.Duration `yaml:"interval"`
	Timeout     time.Duration `yaml:"timeout"`
	Concurrency int           `yaml:"concurrency"`
	APIListen   string        `yaml:"api-listen"`
	Debug       bool          `yaml:"-"`
}

var reference = regexp.MustCompile(`\{\{([^{}]+)\}\}`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ExpandEnv 展开环境引用，未定义或未闭合的引用均报错，不回显密钥值。
func ExpandEnv(value string, lookup func(string) (string, bool)) (string, error) {
	var failure error
	expanded := reference.ReplaceAllStringFunc(value, func(match string) string {
		key := strings.TrimSpace(match[2 : len(match)-2])
		if !envName.MatchString(key) {
			failure = fmt.Errorf("非法环境变量引用 %q", key)
			return ""
		}
		result, ok := lookup(key)
		if !ok || result == "" {
			failure = fmt.Errorf("环境变量 %s 未设置", key)
		}
		return result
	})
	if failure != nil {
		return "", failure
	}
	if strings.Contains(expanded, "{{") || strings.Contains(expanded, "}}") {
		return "", fmt.Errorf("环境变量引用未闭合")
	}
	return expanded, nil
}

// Load 严格读取一个 YAML 文档；路径缺省当前目录 config.yaml。
func Load(path string) (*Config, error) {
	if path == "" {
		path = "config.yaml"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取运行配置 %s: %w", path, err)
	}
	cfg, err := Decode(data, os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("运行配置 %s: %w", path, err)
	}
	if !filepath.IsAbs(cfg.Runtime.Root) {
		cfg.Runtime.Root = filepath.Join(filepath.Dir(path), cfg.Runtime.Root)
	}
	cfg.Runtime.Root, err = filepath.Abs(cfg.Runtime.Root)
	if err != nil {
		return nil, fmt.Errorf("定位运行根: %w", err)
	}
	return cfg, nil
}

// Decode 独立完成解析、引用与校验，便于离线验证配置。
func Decode(data []byte, lookup func(string) (string, bool)) (*Config, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	cfg := new(Config)
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("解析 YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("配置只允许一个 YAML 文档")
	}
	expand := func(field string, value *string, secret bool) error {
		if secret && *value != "" && reference.FindString(*value) != *value {
			return fmt.Errorf("%s 必须使用 {{VAR}} 环境引用", field)
		}
		next, err := ExpandEnv(*value, lookup)
		if err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		*value = next
		return nil
	}
	for name, conn := range cfg.Connects {
		for field, value := range map[string]*string{"url": &conn.URL, "token": &conn.Token, "app-id": &conn.AppID, "app-secret": &conn.AppSecret} {
			if err := expand("connects."+name+"."+field, value, field == "token" || field == "app-secret"); err != nil {
				return nil, err
			}
		}
		cfg.Connects[name] = conn
	}
	for name, server := range cfg.MCP {
		for key, value := range server.Env {
			if !envName.MatchString(key) {
				return nil, fmt.Errorf("mcp.%s.env 含非法变量名", name)
			}
			if err := expand("mcp."+name+".env."+key, &value, secretKey(key)); err != nil {
				return nil, err
			}
			server.Env[key] = value
		}
		cfg.MCP[name] = server
	}
	for field, value := range map[string]*string{"endpoint": &cfg.Session.Endpoint, "bucket": &cfg.Session.Bucket, "prefix": &cfg.Session.Prefix, "access-key": &cfg.Session.AccessKey, "secret-key": &cfg.Session.SecretKey} {
		if err := expand("session."+field, value, field == "access-key" || field == "secret-key"); err != nil {
			return nil, err
		}
	}
	if cfg.Runtime.Root == "" {
		cfg.Runtime.Root = "data"
	}
	if cfg.Runtime.Interval == 0 {
		cfg.Runtime.Interval = 30 * time.Second
	}
	if cfg.Runtime.Timeout == 0 {
		cfg.Runtime.Timeout = 30 * time.Minute
	}
	if cfg.Runtime.Concurrency == 0 {
		cfg.Runtime.Concurrency = 8
	}
	if cfg.Session.Store == "" {
		cfg.Session.Store = "local"
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate 在启动任何进程之前拒绝未知行为、跨平台字段和悬空引用。
func (c *Config) Validate() error {
	if len(c.Bots) == 0 {
		return fmt.Errorf("bots 不能为空")
	}
	if c.Runtime.Interval <= 0 || c.Runtime.Timeout <= 0 || c.Runtime.Concurrency < 1 {
		return fmt.Errorf("runtime 的 interval、timeout、concurrency 必须为正")
	}
	if c.Session.Store != "local" && c.Session.Store != "s3" {
		return fmt.Errorf("session.store 必须是 local 或 s3")
	}
	if c.Session.Store == "s3" && (c.Session.Endpoint == "" || c.Session.Bucket == "" || c.Session.AccessKey == "" || c.Session.SecretKey == "") {
		return fmt.Errorf("s3 会话缺少 endpoint/bucket/access-key/secret-key")
	}
	if c.Session.Store == "local" && (c.Session.Endpoint != "" || c.Session.Bucket != "" || c.Session.AccessKey != "" || c.Session.SecretKey != "" || c.Session.Prefix != "") {
		return fmt.Errorf("local 会话不能配置 S3 字段")
	}
	for name, conn := range c.Connects {
		if !validName(name) {
			return fmt.Errorf("非法连接名 %q", name)
		}
		switch conn.Type {
		case "gitea":
			if err := credentials.ValidateHost(conn.URL); err != nil {
				return fmt.Errorf("connects.%s.url: %w", name, err)
			}
			if conn.Token == "" || conn.AppID != "" || conn.AppSecret != "" || conn.UserID != "" || len(conn.AdminUsers) > 0 {
				return fmt.Errorf("connects.%s 的 gitea 字段不完整或混入其他平台字段", name)
			}
		case "qq":
			if conn.AppID == "" || conn.AppSecret == "" || conn.Token != "" || conn.UserID != "" {
				return fmt.Errorf("connects.%s 的 qq 字段不完整或混入其他平台字段", name)
			}
		case "telegram", "weixin":
			if conn.Token == "" || conn.AppID != "" || conn.AppSecret != "" || (conn.Type == "telegram" && conn.UserID != "") {
				return fmt.Errorf("connects.%s 的消息平台字段不完整或混入其他平台字段", name)
			}
		default:
			return fmt.Errorf("connects.%s.type 不支持 %q", name, conn.Type)
		}
	}
	for name, server := range c.MCP {
		if !validName(name) {
			return fmt.Errorf("非法 MCP 名 %q", name)
		}
		if server.Cmd == "" {
			return fmt.Errorf("mcp.%s.cmd 不能为空", name)
		}
	}
	used := map[string]string{}
	for name, bot := range c.Bots {
		if !validName(name) {
			return fmt.Errorf("非法 bot 名 %q", name)
		}
		platform := "gitea"
		switch bot.Kind {
		case "gitea-review", "triage":
		case "chat":
			platform = ""
		default:
			return fmt.Errorf("bots.%s.kind 不支持 %q", name, bot.Kind)
		}
		if len(bot.Use) != 1 {
			return fmt.Errorf("bots.%s.use 必须绑定一个连接", name)
		}
		for role, ref := range bot.Use {
			conn, ok := c.Connects[ref]
			if !ok || role != conn.Type || (platform != "" && conn.Type != platform) || (platform == "" && conn.Type == "gitea") {
				return fmt.Errorf("bots.%s.use.%s 连接不存在或平台不匹配", name, role)
			}
			key := ref + "/" + bot.Kind
			if other, ok := used[key]; ok {
				return fmt.Errorf("bots.%s 与 %s 重复消费同一连接的 %s 事件", name, other, bot.Kind)
			}
			used[key] = name
		}
		if bot.Kind == "chat" {
			if bot.Workspace.Type != "directory" {
				return fmt.Errorf("bots.%s 的 chat 工作区必须是 directory", name)
			}
		} else {
			if bot.Workspace.Type != "worktree" {
				return fmt.Errorf("bots.%s 工作区必须是 worktree", name)
			}
			if bot.Workspace.Repo == "" {
				return fmt.Errorf("bots.%s.workspace.repo 不能为空", name)
			}
			if err := validateTemplate(bot.Workspace.Repo); err != nil {
				return err
			}
			if strings.HasPrefix(bot.Workspace.Ref, "-") {
				return fmt.Errorf("工作区 ref 不能是选项")
			}
			if !strings.Contains(bot.Workspace.Repo, "{{") {
				if _, _, err := credentials.ParseRepoName(bot.Workspace.Repo); err != nil {
					return err
				}
			}
			if err := validateTemplate(bot.Workspace.Ref); err != nil {
				return err
			}
		}
		for _, ref := range bot.Agent.MCP {
			if _, ok := c.MCP[ref]; !ok {
				return fmt.Errorf("bots.%s.agent.mcp 引用了不存在的 %s", name, ref)
			}
		}
	}
	return nil
}

func validateTemplate(value string) error {
	rest := reference.ReplaceAllStringFunc(value, func(match string) string {
		switch strings.TrimSpace(match[2 : len(match)-2]) {
		case "event.repo", "event.pr.ref", "event.pr.head", "event.base":
			return ""
		default:
			return match
		}
	})
	if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		return fmt.Errorf("工作区模板含未知或未闭合的事件引用")
	}
	return nil
}

func secretKey(key string) bool {
	upper := strings.ToUpper(key)
	return strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") || strings.HasSuffix(upper, "_KEY")
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00 \t\r\n")
}
