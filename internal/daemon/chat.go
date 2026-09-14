package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"assistant/internal/claudecfg"
	"assistant/internal/instances"
	"assistant/internal/provider"
)

// chatSystemPrompt 是对话会话的附加 system 提示词：角色 + 工具边界。
const chatSystemPrompt = `你是 assistant daemon 的运维对话助手（微信桥）。用户通过微信提问，你用简洁中文回答。
能力边界：通过 daemon MCP 只读查询调度状态——目标仓库、待办队列、进行中的评审/分诊会话、最近会话结果。
必须用工具查询后再回答，不要编造状态；查询不可用（daemon 未运行）时如实说明。
回答保持简短（微信场景），要点用短列表；除非用户要求，不复述原始 JSON。`

// ChatConfig 是对话会话的配置。
type ChatConfig struct {
	// ClaudeBin 是 claude 可执行文件（缺省 PATH 上的 claude）
	ClaudeBin string
	// Model 可选模型覆盖
	Model string
	// Provider 是生效的供应商运行时覆盖（env/settings/mcp 原样透传；零值表示
	// 内置缺省）。只作用于对话会话临时配置，不写入仓库文件。
	Provider claudecfg.Overrides
	// ProviderName 是生效的 provider 名（空串表示内置缺省；仅日志展示）
	ProviderName string
	// Timeout 是单轮对话超时（缺省 3 分钟）
	Timeout time.Duration
	// StateDir 是会话状态目录（缺省 <配置目录>/chat）
	StateDir string
	// SessionDir 是 Claude Code 配置根（缺省 $CLAUDE_CONFIG_DIR 或 ~/.claude）
	SessionDir string
	// SessionProject 是对话会话稳定的项目目录名（缺省 assistant-chat），文本
	// 记录固定落 <SessionDir>/projects/<SessionProject>/，与启动目录无关
	SessionProject string
	// RunClaude 可覆盖 claude 调用（测试注入）；env 是额外进程环境变量；返回 stdout
	RunClaude func(ctx context.Context, bin string, args []string, dir string, env []string) ([]byte, error)
	// Log 输出
	Log func(string, ...any)
}

// Chat 把对话消息桥到稳定的 claude 会话：每个会话（微信用户）一个 session
// UUID，首轮以 --session-id 建立，之后 --resume 延续；工具面只有一个自举的
// daemon MCP（只读状态）。
type Chat struct {
	config ChatConfig

	mu       sync.Mutex
	sessions map[string]string
	locks    map[string]*sync.Mutex
}

// NewChat 创建对话会话管理器（加载已持久化的会话映射）。
func NewChat(config ChatConfig) (*Chat, error) {
	if strings.TrimSpace(config.ClaudeBin) == "" {
		config.ClaudeBin = "claude"
	}
	if config.Timeout <= 0 {
		config.Timeout = 3 * time.Minute
	}
	if config.Log == nil {
		config.Log = func(string, ...any) {}
	}
	if strings.TrimSpace(config.StateDir) == "" {
		directory, err := instances.DefaultConfigDir()
		if err != nil {
			return nil, err
		}
		config.StateDir = filepath.Join(directory, "chat")
	}
	if strings.TrimSpace(config.SessionDir) == "" {
		config.SessionDir = claudecfg.ConfigDir()
	}
	if strings.TrimSpace(config.SessionProject) == "" {
		config.SessionProject = claudecfg.ProjectDirName("assistant-chat")
	}
	if config.RunClaude == nil {
		config.RunClaude = runClaude
	}
	chat := &Chat{
		config:   config,
		sessions: map[string]string{},
		locks:    map[string]*sync.Mutex{},
	}
	if err := chat.load(); err != nil {
		return nil, err
	}
	return chat, nil
}

// StateDir 返回会话状态目录。
func (c *Chat) StateDir() string { return c.config.StateDir }

// Handle 处理一条对话消息，返回回复文本；同一会话的消息串行执行。
func (c *Chat) Handle(ctx context.Context, conversationID, text string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("对话未初始化")
	}
	lock := c.conversationLock(conversationID)
	lock.Lock()
	defer lock.Unlock()

	sessionID, fresh := c.session(conversationID)
	if sessionID == "" {
		generated := provider.NewSessionID()
		if generated == "" {
			return "", fmt.Errorf("生成会话 ID 失败（crypto/rand 不可用）")
		}
		sessionID, fresh = generated, true
	}
	args, err := c.sessionArgs(sessionID, chatSessionTitle(conversationID), fresh, text)
	if err != nil {
		return "", err
	}
	runCtx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()
	output, err := c.config.RunClaude(
		runCtx, c.config.ClaudeBin, args, c.config.StateDir,
		claudecfg.SessionEnv(c.config.SessionDir, c.config.SessionProject))
	if err != nil {
		return "", fmt.Errorf("对话会话失败: %w", err)
	}
	result, err := parseClaudeResult(output)
	if err != nil {
		return "", err
	}
	if fresh {
		if err := c.remember(conversationID, sessionID); err != nil {
			c.config.Log("持久化会话映射失败：%v", err)
		}
	}
	if result.IsError {
		message := strings.TrimSpace(result.Result)
		if message == "" && len(result.Errors) > 0 {
			message = strings.Join(result.Errors, "；")
		}
		if message == "" {
			message = "会话执行失败（" + result.Subtype + "）"
		}
		return message, nil
	}
	return strings.TrimSpace(result.Result), nil
}

// sessionArgs 组装 claude 调用参数：稳定会话 + 自定义标题 + 独立设置 + 自举
// daemon MCP。供应商代码级特化（如 opencode 的会话请求头）在每轮对话启动前
// 应用，复用该会话的稳定 UUID（同一会话多轮命中网关缓存）。
func (c *Chat) sessionArgs(sessionID, title string, fresh bool, text string) ([]string, error) {
	overrides, err := provider.Apply(c.config.ProviderName, "chat", sessionID, c.config.Provider)
	if err != nil {
		return nil, err
	}
	settingsPath, err := c.writeSettings(overrides)
	if err != nil {
		return nil, err
	}
	mcpPath, err := c.writeMCPConfig(overrides)
	if err != nil {
		return nil, err
	}
	args := []string{
		"-p", text,
		"--output-format", "json",
		"--permission-mode", "auto",
		"--strict-mcp-config",
		"--mcp-config", mcpPath,
		"--settings", settingsPath,
		"--setting-sources", claudecfg.SettingSources,
		"--append-system-prompt", chatSystemPrompt,
		"--max-turns", "50",
	}
	if fresh {
		args = append(args, "--session-id", sessionID)
	} else {
		args = append(args, "--resume", sessionID)
	}
	if title != "" {
		args = append(args, "--name", title)
	}
	if c.config.Model != "" {
		args = append(args, "--model", c.config.Model)
	}
	return args, nil
}

// writeSettings 写入对话会话的独立设置（env + 权限放行 + provider 覆盖，含
// daemon MCP 的放行）。
func (c *Chat) writeSettings(overrides claudecfg.Overrides) (string, error) {
	settings := claudecfg.SessionSettingsMap(overrides, "mcp__daemon", "mcp__daemon__*")
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(c.config.StateDir, "settings.json")
	if err := os.MkdirAll(c.config.StateDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// writeMCPConfig 写入自举的 daemon MCP 配置（assistant mcp daemon 自己发现
// 运行中的 daemon，无需地址/令牌参数）；provider 定义的原生 MCP server 一并
// 合并（同名由 provider 覆盖）。
func (c *Chat) writeMCPConfig(overrides claudecfg.Overrides) (string, error) {
	config := map[string]any{
		"mcpServers": map[string]any{
			"daemon": map[string]any{"command": "assistant", "args": []any{"mcp", "daemon"}},
		},
	}
	claudecfg.MergeMCPServers(config, overrides)
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(c.config.StateDir, "mcp.json")
	if err := os.MkdirAll(c.config.StateDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (c *Chat) conversationLock(conversationID string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	lock, ok := c.locks[conversationID]
	if !ok {
		lock = &sync.Mutex{}
		c.locks[conversationID] = lock
	}
	return lock
}

func (c *Chat) session(conversationID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, ok := c.sessions[conversationID]
	return session, !ok
}

func (c *Chat) sessionsPath() string {
	return filepath.Join(c.config.StateDir, "sessions.json")
}

func (c *Chat) load() error {
	data, err := os.ReadFile(c.sessionsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var sessions map[string]string
	if err := json.Unmarshal(data, &sessions); err != nil {
		return fmt.Errorf("解析 %s: %w", c.sessionsPath(), err)
	}
	if sessions != nil {
		c.sessions = sessions
	}
	return nil
}

func (c *Chat) remember(conversationID, sessionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions[conversationID] = sessionID
	if err := os.MkdirAll(c.config.StateDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c.sessions, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.sessionsPath(), append(data, '\n'), 0o600)
}

// chatSessionTitle 生成对话会话显示名（--name）：按会话 ID 哈希取短标识，
// 便于在会话选择器中区分不同微信用户，且跨轮稳定。
func chatSessionTitle(conversationID string) string {
	sum := sha256.Sum256([]byte(conversationID))
	return "chat-" + hex.EncodeToString(sum[:4])
}

// claudeResult 是 claude -p --output-format json 的结果子集。
type claudeResult struct {
	Subtype string   `json:"subtype"`
	IsError bool     `json:"is_error"`
	Result  string   `json:"result"`
	Errors  []string `json:"errors"`
}

func parseClaudeResult(output []byte) (claudeResult, error) {
	var result claudeResult
	if err := json.Unmarshal(bytes.TrimSpace(output), &result); err != nil {
		return result, fmt.Errorf("解析会话输出: %w（输出前 200 字节：%s）", err, truncate(string(output), 200))
	}
	return result, nil
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func runClaude(ctx context.Context, bin string, args []string, dir string, env []string) ([]byte, error) {
	command := exec.CommandContext(ctx, bin, args...)
	command.Dir = dir
	if len(env) > 0 {
		command.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("%s: %s", bin, truncate(message, 400))
	}
	return stdout.Bytes(), nil
}
