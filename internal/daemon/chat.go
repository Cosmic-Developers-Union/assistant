package daemon

import (
	"bufio"
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
	// Debug 为真时把 claude 的原始 stream 事件、会话命令与结束统计也写进日志
	Debug bool
	// Bare 为真时对话会话用 claude 的 --bare 最小模式：不加载 hooks、插件同步、
	// CLAUDE.md 自动发现与记忆，只认显式传入的 --settings/--mcp-config。对话
	// 上下文本来就全靠显式参数（系统提示词 + 自举 daemon MCP），开关由 NewChat
	// 按 claude 是否支持该参数探测决定。
	Bare bool
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
		directory, err := instances.ClaudeDir()
		if err != nil {
			return nil, err
		}
		config.SessionDir = directory
	}
	// 会话配置根由 assistant 托管（claude 的全局配置、会话记录与状态都写在这里）：
	// 启动时就建好，别把失败留到第一条微信消息
	if err := os.MkdirAll(config.SessionDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建会话配置根 %s: %w", config.SessionDir, err)
	}
	chat := &Chat{
		config:   config,
		sessions: map[string]string{},
		locks:    map[string]*sync.Mutex{},
	}
	if err := chat.load(); err != nil {
		return nil, err
	}
	chat.dropLegacySharedConfig()
	return chat, nil
}

// dropLegacySharedConfig 清理早期把所有会话共用的 settings.json / mcp.json（现在
// 每个会话写进自己的工作目录）。一次性迁移，文件不存在时是空操作。
func (c *Chat) dropLegacySharedConfig() {
	for _, name := range []string{"settings.json", "mcp.json"} {
		path := filepath.Join(c.config.StateDir, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.Remove(path); err != nil {
			c.config.Log("清理旧的共享会话配置失败（%s）：%v", path, err)
			continue
		}
		c.config.Log("已清理旧的共享会话配置：%s（现在每个会话写在自己的工作目录）", path)
	}
}

// StateDir 返回会话状态目录。
func (c *Chat) StateDir() string { return c.config.StateDir }

// Bare 返回对话会话是否使用 claude 的最小模式（--bare）。
func (c *Chat) Bare() bool { return c.config.Bare }

// SessionDir 返回会话配置根（assistant 托管，不是用户的 ~/.claude）。
func (c *Chat) SessionDir() string { return c.config.SessionDir }

// ClaudeBin 返回对话会话使用的 claude 可执行文件。
func (c *Chat) ClaudeBin() string { return c.config.ClaudeBin }

// Handle 处理一条对话消息，返回回复文本；同一会话的消息串行执行。
func (c *Chat) Handle(ctx context.Context, conversationID, text string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("对话未初始化")
	}
	lock := c.conversationLock(conversationID)
	lock.Lock()
	defer lock.Unlock()

	workspace, err := c.workspaceDir(conversationID)
	if err != nil {
		return "", err
	}
	sessionID, fresh := c.session(conversationID)
	if sessionID == "" {
		generated := provider.NewSessionID()
		if generated == "" {
			return "", fmt.Errorf("生成会话 ID 失败（crypto/rand 不可用）")
		}
		sessionID, fresh = generated, true
	}
	if fresh {
		c.config.Log("对话[%s] 新建会话 %s（工作目录 %s，续聊用 cd 该目录后 claude --continue）",
			shortSession(sessionID), sessionID, workspace)
	} else if c.config.Debug {
		c.config.Log("对话[%s] 续接既有会话（工作目录 %s）", shortSession(sessionID), workspace)
	}
	args, err := c.sessionArgs(sessionID, chatSessionTitle(conversationID), fresh, text, workspace)
	if err != nil {
		return "", err
	}
	runCtx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()
	progress := c.progressLogger(sessionID)
	if c.config.Debug {
		c.config.Log("对话[%s] 启动：%s", shortSession(sessionID), truncate(strings.Join(args, " "), 600))
	}
	result, err := c.run(runCtx, args, workspace, progress)
	if err != nil {
		return "", fmt.Errorf("对话会话失败: %w", err)
	}
	if c.config.Debug {
		c.config.Log("对话[%s] 结束：subtype=%s is_error=%t turns=%d cost=$%.4f",
			shortSession(sessionID), result.Subtype, result.IsError, result.NumTurns, result.CostUSD)
	}
	if fresh {
		if err := c.remember(conversationID, sessionID, workspace, result.Model); err != nil {
			c.config.Log("持久化会话映射失败：%v", err)
		}
	}
	if result.IsError {
		if message := strings.TrimSpace(result.Result); message != "" {
			return message, nil
		}
		return result.FailureMessage(), nil
	}
	return strings.TrimSpace(result.Result), nil
}

// sessionArgs 组装 claude 调用参数：稳定会话 + 自定义标题 + 独立设置 + 自举
// daemon MCP。供应商代码级特化（如 opencode 的会话请求头）在每轮对话启动前
// 应用，复用该会话的稳定 UUID（同一会话多轮命中网关缓存）。
func (c *Chat) sessionArgs(sessionID, title string, fresh bool, text, workspace string) ([]string, error) {
	overrides, err := provider.Apply(c.config.ProviderName, "chat", sessionID, c.config.Provider)
	if err != nil {
		return nil, err
	}
	// settings/mcp 落**该会话自己的工作目录**：并发会话（不同微信用户）各写各的，
	// 会话级 provider 覆盖（如 opencode 的会话请求头）不会被彼此覆盖
	settingsPath, err := c.writeSettings(overrides, workspace)
	if err != nil {
		return nil, err
	}
	mcpPath, err := c.writeMCPConfig(overrides, workspace)
	if err != nil {
		return nil, err
	}
	args := []string{
		"-p", text,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "auto",
		"--strict-mcp-config",
		"--mcp-config", mcpPath,
		"--settings", settingsPath,
		"--setting-sources", claudecfg.SettingSources,
		"--append-system-prompt", chatSystemPrompt,
		"--max-turns", "50",
	}
	// 最小模式：不加载 hooks/插件同步/CLAUDE.md 自动发现与记忆，上下文只有上面
	// 显式给的系统提示词、settings（含 provider env）与 daemon MCP
	if c.config.Bare {
		args = append(args, "--bare")
	}
	if fresh || c.config.Debug {
		// 把「这一轮到底用了什么」直接写进日志：env（密钥打码）、settings/MCP 路径、
		// 声明的 MCP server 与命令；实际是否连上由 init 事件单独报
		c.config.Log("对话[%s] 会话配置：%s；model=%s；权限=%s；settings=%s；mcp=%s",
			shortSession(sessionID), overrides.Describe(),
			firstNonEmptyString(c.config.Model, "账号默认"), claudecfg.SettingSources,
			settingsPath, mcpPath)
		c.config.Log("对话[%s] MCP 声明：%s", shortSession(sessionID),
			claudecfg.DescribeMCPServers(c.mcpDocument(overrides)))
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

// mcpDocument 组装对话会话的 MCP 文档：自举 daemon MCP + provider 定义的原生
// server（同名由 provider 覆盖）。写文件与日志摘要共用，避免两处漂移。
func (c *Chat) mcpDocument(overrides claudecfg.Overrides) map[string]any {
	document := map[string]any{
		"mcpServers": map[string]any{
			claudecfg.MCPServerDaemon: claudecfg.DaemonMCPServer(claudecfg.AssistantCommand()),
		},
	}
	return claudecfg.MergeMCPServers(document, overrides)
}

// writeSettings 写入对话会话的独立设置（env + 权限放行 + provider 覆盖，含
// daemon MCP 的放行）。
func (c *Chat) writeSettings(overrides claudecfg.Overrides, dir string) (string, error) {
	settings := claudecfg.SessionSettingsMap(overrides, "mcp__daemon", "mcp__daemon__*")
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
func (c *Chat) writeMCPConfig(overrides claudecfg.Overrides, dir string) (string, error) {
	config := c.mcpDocument(overrides)
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "mcp.json")
	if err := os.MkdirAll(dir, 0o755); err != nil {
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

func (c *Chat) remember(conversationID, sessionID, workspace, model string) error {
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
	if err := os.WriteFile(c.sessionsPath(), append(data, '\n'), 0o600); err != nil {
		return err
	}
	// 工作目录里留一份可读的会话元数据：一眼看到「这个目录是哪个微信会话、
	// 用的哪个 claude 会话 id、怎么续聊」
	metadata := sessionMetadata{
		ConversationID: conversationID,
		SessionID:      sessionID,
		Title:          chatSessionTitle(conversationID),
		Model:          model,
		Workdir:        workspace,
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	if existing, err := readSessionMetadata(workspace); err == nil && existing.CreatedAt != "" {
		metadata.CreatedAt = existing.CreatedAt
	} else {
		metadata.CreatedAt = metadata.UpdatedAt
	}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workspace, "session.json"), append(encoded, '\n'), 0o600)
}

// sessionMetadata 是会话工作目录里的元数据（诊断/续聊用）。
type sessionMetadata struct {
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"`
	Title          string `json:"title"`
	Model          string `json:"model,omitempty"`
	Workdir        string `json:"workdir"`
	CreatedAt      string `json:"created_at,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

func readSessionMetadata(workspace string) (sessionMetadata, error) {
	var metadata sessionMetadata
	data, err := os.ReadFile(filepath.Join(workspace, "session.json"))
	if err != nil {
		return metadata, err
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return sessionMetadata{}, err
	}
	return metadata, nil
}

// workspaceDir 返回该微信会话的持久工作目录（<StateDir>/<chat-xxxxxxxx>/，按会话
// 稳定派生）：claude 按 cwd 派生项目名，因此同一目录 = 同一项目 = 文本记录稳定，
// 用户 cd 进去 `claude --continue` 就能接上最近的会话。
func (c *Chat) workspaceDir(conversationID string) (string, error) {
	dir := filepath.Join(c.config.StateDir, chatSessionTitle(conversationID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建会话工作目录 %s: %w", dir, err)
	}
	return dir, nil
}

// WorkspaceDir 返回会话工作目录（外部诊断用；不存在时按需创建）。
func (c *Chat) WorkspaceDir(conversationID string) (string, error) {
	return c.workspaceDir(conversationID)
}

// Reset 丢弃该会话的映射：下一条消息新建 claude 会话（工作目录保留，用户显式
// 「重新开始」时才发生）。
func (c *Chat) Reset(conversationID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, conversationID)
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

// run 执行一轮 claude 会话：默认逐行消费 stdout（实时进度进日志）；测试注入的
// RunClaude 返回整段输出时按行折叠，两者归集结果一致。
func (c *Chat) run(ctx context.Context, args []string, dir string, onProgress func(string)) (chatOutcome, error) {
	if c.config.RunClaude != nil {
		output, err := c.config.RunClaude(
			ctx, c.config.ClaudeBin, args, dir, claudecfg.ConfigDirEnv(c.config.SessionDir))
		if err != nil {
			return chatOutcome{}, fmt.Errorf("%s: %s", c.config.ClaudeBin, truncate(err.Error(), 400))
		}
		return parseChatStream(output, onProgress)
	}
	return c.runStreaming(ctx, args, dir, onProgress)
}

// runStreaming 启动 claude 并逐行解析 stream-json：assistant 文本、工具调用与
// API 错误实时进日志（--debug 时连原始事件也写），最后返回归集结果。
func (c *Chat) runStreaming(ctx context.Context, args []string, dir string, onProgress func(string)) (chatOutcome, error) {
	command := exec.CommandContext(ctx, c.config.ClaudeBin, args...)
	command.Dir = dir
	if env := claudecfg.ConfigDirEnv(c.config.SessionDir); len(env) > 0 {
		command.Env = append(os.Environ(), env...)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return chatOutcome{}, err
	}
	stderr := &tailBuffer{limit: 4096}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return chatOutcome{}, fmt.Errorf("启动 %s: %w", c.config.ClaudeBin, err)
	}
	var outcome chatOutcome
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		// 只输出解析后的可读事件行（会话实况/思考/文本/工具/结果/API 错误）；
		// 需要原始 stream-json 时看文本记录，不在日志里堆 JSON
		feedChatStreamLine(&outcome, scanner.Bytes(), onProgress)
	}
	scanErr := scanner.Err()
	waitErr := command.Wait()
	if waitErr != nil && outcome.Subtype == "" && outcome.Result == "" && outcome.APIError == "" {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = waitErr.Error()
		}
		return outcome, fmt.Errorf("%s: %s", c.config.ClaudeBin, truncate(message, 400))
	}
	if scanErr != nil {
		return outcome, fmt.Errorf("读取会话输出: %w", scanErr)
	}
	if outcome.Subtype == "" && outcome.Result == "" && outcome.APIError == "" {
		return outcome, fmt.Errorf("会话没有返回结果（stderr：%s）", truncate(strings.TrimSpace(stderr.String()), 300))
	}
	return outcome, nil
}

// progressLogger 把会话进度写进 daemon 日志：一行一条，便于 grep「用户在问什么、
// claude 在做什么、为什么没有回复」。
func (c *Chat) progressLogger(sessionID string) func(string) {
	return func(line string) {
		c.config.Log("对话[%s] %s", shortSession(sessionID), truncate(line, 300))
	}
}

func shortSession(sessionID string) string {
	if len(sessionID) >= 8 {
		return sessionID[:8]
	}
	if sessionID == "" {
		return "-"
	}
	return sessionID
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// tailBuffer 只保留最后 limit 字节（失败时报 stderr 尾部，不刷屏）。
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if b.limit > 0 && len(b.data) > b.limit {
		b.data = b.data[len(b.data)-b.limit:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// firstNonEmptyString 返回第一个非空串（日志展示用）。
func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
