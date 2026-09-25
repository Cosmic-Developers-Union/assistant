package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	builtinagents "github.com/Cosmic-Developers-Union/assistant/internal/agents"
	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
	"github.com/Cosmic-Developers-Union/assistant/internal/conversations"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/provider"
	"github.com/Cosmic-Developers-Union/assistant/internal/sessionstore"

	jsonv2 "encoding/json/v2"
)

// chatSessionsHint 只在配置了记录库时追加：sessions MCP 能回查被压缩掉的完整历史。
const chatSessionsHint = `
上下文被压缩或需要回忆更早的对话时，用 sessions MCP 的 session_search/session_read 回查本会话的完整记录。`

// defaultSystemPrompt 返回缺省主 agent 提示词（builtin.json 的 main 预设）：
// main_agent 未配 system_prompt 或未配置 runtime.agent 时使用。
func defaultSystemPrompt() string {
	return builtinagents.Main().SystemPrompt
}

// AgentRuntime 是一个命名 agent 的生效运行时：启动时由 cmd 层从 config.json
// 的 agents 定义解析（provider 链、bare 探测等）后注入。零值字段回退 ChatConfig
// 的内置缺省。
type AgentRuntime struct {
	// Name 是 agent 名（内置缺省为空串）
	Name string
	// Provider 是该 agent 生效的供应商运行时覆盖
	Provider claudecfg.Overrides
	// ProviderName 是生效的 provider 名（空串表示内置缺省；仅日志展示）
	ProviderName string
	// Model 可选模型覆盖
	Model string
	// SystemPrompt 非空时整体替换对话会话的基础系统提示词
	SystemPrompt string
	// ClaudeBin 是该 agent 使用的 claude 可执行文件（空 = ChatConfig.ClaudeBin）
	ClaudeBin string
	// Bare 为真时该 agent 的会话用 claude 的 --bare 最小模式
	Bare bool
	// Timeout 是该 agent 的单轮对话超时（非正数回退 ChatConfig.Timeout）
	Timeout time.Duration
}

// claudeBinOrDefault 返回该 agent 实际使用的 claude 可执行文件。
func (a AgentRuntime) claudeBinOrDefault(defaultBin string) string {
	return firstNonEmptyString(a.ClaudeBin, defaultBin, "claude")
}

// timeoutOrDefault 返回该 agent 实际的单轮超时。
func (a AgentRuntime) timeoutOrDefault(defaultTimeout time.Duration) time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	if defaultTimeout > 0 {
		return defaultTimeout
	}
	return 3 * time.Minute
}

// Turn 是一轮对话请求：Handle 的入参。
type Turn struct {
	// Transport 是消息来源通道（weixin/qq/…），写入会话元数据
	Transport string
	// Text 是用户消息文本
	Text string
}

// SubagentDefinition 是注入会话的子代理：以 claude 自定义 agent（--agents JSON）
// 下发，主模型经原生 Task 工具按 description 委派。MCP 并入所在会话的 MCP
// 配置（子代理共享会话工具面）。
type SubagentDefinition struct {
	// Name 是子代理名（agents 池或内置预设）
	Name string
	// Description 是一句话能力描述：主模型看它决定何时把任务派给谁
	Description string
	// Prompt 是子代理的系统提示词
	Prompt string
	// Model 可选模型覆盖（委派执行沿用主 agent 的 provider）
	Model string
	// MCP 是该子代理需要的 MCP server 定义（并入会话 MCP 配置）
	MCP map[string]any
}

// ChatConfig 是对话会话的配置。
type ChatConfig struct {
	// MainAgent 是本运行时的主 agent（唯一接待所有对话通道的 agent）
	MainAgent AgentRuntime
	// Subagents 是注入会话的子代理（--agents JSON）：主模型按 description 委派
	Subagents []SubagentDefinition
	// StateDir 是会话状态目录（缺省 <配置目录>/chat）
	StateDir string
	// SessionDir 是 Claude Code 配置根（缺省 $CLAUDE_CONFIG_DIR 或 ~/.claude）
	SessionDir string
	// Debug 为真时把 claude 的原始 stream 事件、会话命令与结束统计也写进日志
	Debug bool
	// Remote 是记录库服务端连接（assistant serve）：非空时每轮结束把该会话的最新
	// 记录推上去，agent 在上下文压缩后能用 sessions MCP 回查完整历史
	Remote sessionstore.RemoteConfig
	// AssistantConfig 是本次运行使用的 config.json 绝对路径：注入会话环境
	// （ASSISTANT_CONFIG），会话内的 assistant MCP（daemon/gitea）解析到同一份
	// 配置与端点文件，而不是靠 cwd 猜
	AssistantConfig string
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

	mu            sync.Mutex
	sessions      map[string]string
	locks         map[string]*sync.Mutex
	conversations *conversations.File
	// pushed 记录每个 claude 会话最近一次推送的文件大小（避免每轮重复上传）
	pushed map[string]int64
}

// NewChat 创建对话会话管理器（加载已持久化的会话映射）。
func NewChat(config ChatConfig) (*Chat, error) {
	if strings.TrimSpace(config.MainAgent.ClaudeBin) == "" {
		config.MainAgent.ClaudeBin = "claude"
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
		pushed:   map[string]int64{},
	}
	conversationFile, err := conversations.Load(chat.conversationsPath())
	if err != nil {
		return nil, err
	}
	chat.conversations = conversationFile
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

// conversationsPath 是会话实体映射表的落点（与 chat 状态同目录）。
func (c *Chat) conversationsPath() string {
	return filepath.Join(c.config.StateDir, "conversations.json")
}

// ConversationFor 把「通道 + 通道内用户标识」映射到会话实体 id（必要时创建）。
// 映射层是刻意的：聊天会话不由微信唯一决定——换通道、或把两个通道绑到同一会话，
// 都只改这张表，记录与工作目录跟着会话实体走。
// 旧版本直接用通道用户 id 当键，首次映射时把已有会话映射迁移过去（不丢上下文）。
func (c *Chat) ConversationFor(transport, user string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conversation, created := c.conversations.Ensure(transport, user, "")
	if created {
		if sessionID, ok := c.sessions[user]; ok {
			// 迁移旧键：把 <通道用户 id> 下的 claude 会话挂到新的会话实体上
			delete(c.sessions, user)
			c.sessions[conversation.ID] = sessionID
			if err := c.saveSessionsLocked(); err != nil {
				return "", err
			}
			c.config.Log("会话映射：%s:%s → %s（迁移旧键 %s 的会话 %s）",
				transport, user, conversation.ID, user, sessionID)
		} else {
			c.config.Log("会话映射：%s:%s → %s", transport, user, conversation.ID)
		}
	}
	if err := conversations.Save(c.conversationsPath(), c.conversations); err != nil {
		return "", err
	}
	return conversation.ID, nil
}

// StateDir 返回会话状态目录。
func (c *Chat) StateDir() string { return c.config.StateDir }

// SessionDir 返回会话配置根（assistant 托管，不是用户的 ~/.claude）。
func (c *Chat) SessionDir() string { return c.config.SessionDir }

// Handle 处理一轮对话消息，返回回复文本；同一会话的消息串行执行。对话统一
// 由本运行时的主 agent 接待，专项任务由主模型经 Task 工具委派子代理。
func (c *Chat) Handle(ctx context.Context, conversationID string, turn Turn) (string, error) {
	if c == nil {
		return "", fmt.Errorf("对话未初始化")
	}
	agent := c.config.MainAgent
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
		c.config.Log("对话[%s] 新建会话 %s（agent=%s，工作目录 %s，续聊用 cd 该目录后 claude --continue）",
			shortSession(sessionID), sessionID, agentLabel(agent.Name), workspace)
	} else if c.config.Debug {
		c.config.Log("对话[%s] 续接既有会话（agent=%s，工作目录 %s）",
			shortSession(sessionID), agentLabel(agent.Name), workspace)
	}
	args, err := c.sessionArgs(sessionID, chatSessionTitle(conversationID), fresh, turn.Text, workspace, agent)
	if err != nil {
		return "", err
	}
	runCtx, cancel := context.WithTimeout(ctx, agent.timeoutOrDefault(0))
	defer cancel()
	progress := c.progressLogger(sessionID)
	if c.config.Debug {
		c.config.Log("对话[%s] 启动：%s", shortSession(sessionID), truncate(strings.Join(args, " "), 600))
	}
	result, err := c.run(runCtx, agent.claudeBinOrDefault(c.config.MainAgent.ClaudeBin), args, workspace, progress)
	if err != nil {
		return "", fmt.Errorf("对话会话失败: %w", err)
	}
	if c.config.Debug {
		c.config.Log("对话[%s] 结束：subtype=%s is_error=%t turns=%d cost=$%.4f",
			shortSession(sessionID), result.Subtype, result.IsError, result.NumTurns, result.CostUSD)
	}
	if fresh {
		if err := c.remember(conversationID, sessionID); err != nil {
			c.config.Log("持久化会话映射失败：%v", err)
		}
	}
	c.attachSession(conversationID, sessionID)
	if err := c.writeSessionMetadata(conversationID, sessionID, workspace, result.Model, turn.Transport); err != nil {
		c.config.Log("写入会话元数据失败：%v", err)
	}
	c.pushSession(sessionID)
	if result.IsError {
		if message := strings.TrimSpace(result.Result); message != "" {
			return message, nil
		}
		return result.FailureMessage(), nil
	}
	return strings.TrimSpace(result.Result), nil
}

// agentLabel 是日志里的 agent 展示名。
func agentLabel(name string) string {
	if strings.TrimSpace(name) == "" {
		return "内置缺省"
	}
	return name
}

// sessionArgs 组装 claude 调用参数：稳定会话 + 自定义标题 + 独立设置 + 自举
// daemon MCP。供应商代码级特化（如 opencode 的会话请求头）在每轮对话启动前
// 应用，复用该会话的稳定 UUID（同一会话多轮命中网关缓存）。
func (c *Chat) sessionArgs(sessionID, title string, fresh bool, text, workspace string, agent AgentRuntime) ([]string, error) {
	overrides, err := provider.Apply(agent.ProviderName, "chat", sessionID, agent.Provider)
	if err != nil {
		return nil, err
	}
	// settings/mcp 落**该会话自己的工作目录**：并发会话（不同通道用户）各写各的，
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
		"--append-system-prompt", c.systemPrompt(agent),
		"--max-turns", "50",
	}
	// 子代理以 claude 自定义 agent 注入：主模型按 description 经原生 Task 工具委派
	if agentsJSON := c.agentsJSON(); agentsJSON != "" {
		args = append(args, "--agents", agentsJSON)
	}
	// 最小模式：不加载 hooks/插件同步/CLAUDE.md 自动发现与记忆，上下文只有上面
	// 显式给的系统提示词、settings（含 provider env）与 daemon MCP
	if agent.Bare {
		args = append(args, "--bare")
	}
	if fresh || c.config.Debug {
		c.logSessionConfig(sessionID, agent, overrides, settingsPath, mcpPath)
	}
	if fresh {
		args = append(args, "--session-id", sessionID)
	} else {
		args = append(args, "--resume", sessionID)
	}
	if title != "" {
		args = append(args, "--name", title)
	}
	if agent.Model != "" {
		args = append(args, "--model", agent.Model)
	}
	return args, nil
}

// systemPrompt 组装对话会话的 system 提示词：主 agent 自带提示词优先，否则
// 内置 main 预设 +（配置了记录库时）历史回查提示。
func (c *Chat) systemPrompt(agent AgentRuntime) string {
	prompt := strings.TrimSpace(agent.SystemPrompt)
	if prompt == "" {
		prompt = defaultSystemPrompt()
	}
	if strings.TrimSpace(c.config.Remote.URL) == "" {
		return prompt
	}
	return prompt + chatSessionsHint
}

// logSessionConfig 逐行打印这一轮的生效配置：每行一个键值，不压成一条长行。
// env 的密钥打码、settings 只列键名；声明的 MCP server 与实际连通状态分别打印
// （后者来自 init 事件）。
func (c *Chat) logSessionConfig(sessionID string, agent AgentRuntime, overrides claudecfg.Overrides, settingsPath, mcpPath string) {
	log := func(format string, arguments ...any) {
		c.config.Log("对话[%s] "+format, append([]any{shortSession(sessionID)}, arguments...)...)
	}
	log("会话配置：agent = %s，model = %s", agentLabel(agent.Name),
		firstNonEmptyString(agent.Model, overrides.Env["ANTHROPIC_MODEL"], "账号默认"))
	log("  权限来源 = %s（--setting-sources）", claudecfg.SettingSources)
	log("  settings = %s", settingsPath)
	log("  mcp      = %s", mcpPath)
	log("  env %d 项：", len(overrides.Env))
	for _, line := range claudecfg.EnvLines(overrides.Env) {
		log("    %s", line)
	}
	log("  settings %d 项（值不打印）：", len(overrides.Settings))
	for _, key := range claudecfg.SettingKeys(overrides.Settings) {
		log("    %s", key)
	}
	log("  MCP 声明：")
	for _, line := range claudecfg.MCPServerLines(c.mcpDocument(overrides)) {
		log("    %s", line)
	}
}

// mcpDocument 组装对话会话的 MCP 文档：自举 daemon MCP + provider 定义的原生
// server + 子代理声明的 server（同名依次覆盖：子代理 > provider > 自举）。
// 写文件与日志摘要共用，避免两处漂移。
func (c *Chat) mcpDocument(overrides claudecfg.Overrides) map[string]any {
	servers := map[string]any{
		claudecfg.MCPServerDaemon: claudecfg.DaemonMCPServer(claudecfg.AssistantCommand()),
	}
	if strings.TrimSpace(c.config.Remote.URL) != "" {
		// 记录库可用时才注入：否则这几个工具只会返回「未配置」
		servers[claudecfg.MCPServerSessions] = claudecfg.SessionsMCPServer(claudecfg.AssistantCommand())
	}
	for _, subagent := range c.config.Subagents {
		maps.Copy(servers, subagent.MCP)
	}
	document := map[string]any{"mcpServers": servers}
	return claudecfg.MergeMCPServers(document, overrides)
}

// agentsJSON 渲染子代理定义（claude --agents JSON）：主模型按 description 委派；
// 没有子代理时返回空串（不加旗标）。
func (c *Chat) agentsJSON() string {
	if len(c.config.Subagents) == 0 {
		return ""
	}
	agents := make(map[string]any, len(c.config.Subagents))
	for _, subagent := range c.config.Subagents {
		definition := map[string]any{
			"description": subagent.Description,
			"prompt":      subagent.Prompt,
		}
		if subagent.Model != "" {
			definition["model"] = subagent.Model
		}
		agents[subagent.Name] = definition
	}
	data, err := jsonv2.Marshal(agents)
	if err != nil {
		return ""
	}
	return string(data)
}

// writeSettings 写入对话会话的独立设置（env + 权限放行 + provider 覆盖，含
// daemon MCP 的放行）。
func (c *Chat) writeSettings(overrides claudecfg.Overrides, dir string) (string, error) {
	settings := claudecfg.SessionSettingsMap(overrides,
		"mcp__daemon", "mcp__daemon__*", "mcp__sessions", "mcp__sessions__*")
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

// saveSessionsLocked 写会话映射表（调用方已持锁）。
func (c *Chat) saveSessionsLocked() error {
	if err := os.MkdirAll(c.config.StateDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c.sessions, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.sessionsPath(), append(data, '\n'), 0o600)
}

func (c *Chat) remember(conversationID, sessionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions[conversationID] = sessionID
	return c.saveSessionsLocked()
}

// attachSession 把 claude 会话挂到会话实体下（幂等；续接/迁移的会话也要挂，
// 记录库按会话实体归档时才对得上）。
func (c *Chat) attachSession(conversationID, sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.conversations.Attach(conversationID, sessionID); err != nil {
		c.config.Log("挂载会话到会话实体失败（%s → %s）：%v", conversationID, sessionID, err)
		return
	}
	if err := conversations.Save(c.conversationsPath(), c.conversations); err != nil {
		c.config.Log("持久化会话实体失败：%v", err)
	}
}

// writeSessionMetadata 写会话工作目录里的 session.json：每轮都写（续接的会话也要有，
// 记录库归档依赖这里的 conversation/transport/title），创建时间沿用首次的值。
func (c *Chat) writeSessionMetadata(conversationID, sessionID, workspace, model, transport string) error {
	metadata := sessionMetadata{
		ConversationID: conversationID,
		Transport:      transport,
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
	Transport      string `json:"transport,omitempty"`
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
func (c *Chat) run(ctx context.Context, bin string, args []string, dir string, onProgress func(string)) (chatOutcome, error) {
	if c.config.RunClaude != nil {
		output, err := c.config.RunClaude(ctx, bin, args, dir, c.sessionEnv())
		if err != nil {
			return chatOutcome{}, fmt.Errorf("%s: %s", bin, truncate(err.Error(), 400))
		}
		return parseChatStream(output, onProgress)
	}
	return c.runStreaming(ctx, bin, args, dir, onProgress)
}

// runStreaming 启动 claude 并逐行解析 stream-json：assistant 文本、工具调用与
// API 错误实时进日志（--debug 时连原始事件也写），最后返回归集结果。
func (c *Chat) runStreaming(ctx context.Context, bin string, args []string, dir string, onProgress func(string)) (chatOutcome, error) {
	command := exec.CommandContext(ctx, bin, args...)
	command.Dir = dir
	if env := c.sessionEnv(); len(env) > 0 {
		command.Env = append(os.Environ(), env...)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return chatOutcome{}, err
	}
	stderr := &tailBuffer{limit: 4096}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return chatOutcome{}, fmt.Errorf("启动 %s: %w", bin, err)
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
		return outcome, fmt.Errorf("%s: %s", bin, truncate(message, 400))
	}
	if scanErr != nil {
		return outcome, fmt.Errorf("读取会话输出: %w", scanErr)
	}
	if outcome.Subtype == "" && outcome.Result == "" && outcome.APIError == "" {
		return outcome, fmt.Errorf("会话没有返回结果（stderr：%s）", truncate(strings.TrimSpace(stderr.String()), 300))
	}
	return outcome, nil
}

// pushSession 把该会话的最新记录推给记录库（配置了才推）：让 agent 在同一轮对话里
// 就能通过 sessions MCP 查到自己被压缩掉的历史。推送失败只记日志，不影响回复。
func (c *Chat) pushSession(sessionID string) {
	if strings.TrimSpace(c.config.Remote.URL) == "" || strings.TrimSpace(sessionID) == "" {
		return
	}
	options := sessionstore.CollectOptions{
		Root:    c.config.SessionDir,
		ChatDir: c.config.StateDir,
	}
	session, ok, err := sessionstore.SessionFor(options, sessionID)
	if err != nil || !ok {
		if err != nil {
			c.config.Log("归档会话失败（%s）：%v", shortSession(sessionID), err)
		}
		return
	}
	size := int64(session.Meta.Bytes)
	c.mu.Lock()
	previous := c.pushed[sessionID]
	c.mu.Unlock()
	if previous != 0 && previous == size {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := sessionstore.NewClient(c.config.Remote.URL, c.config.Remote.Token)
	if _, err := client.Push(ctx, sessionstore.Batch{Sessions: []sessionstore.Session{session}}); err != nil {
		c.config.Log("归档会话失败（%s）：%v", shortSession(sessionID), err)
		return
	}
	c.mu.Lock()
	c.pushed[sessionID] = int64(len(strings.Join(session.Lines, "\n")) + 1)
	c.mu.Unlock()
	c.config.Log("已归档会话 %s（%d 行，来源 %s）到 %s", session.Key.String(), len(session.Lines), session.Source, c.config.Remote.URL)
}

// sessionEnv 组装会话进程的额外环境变量：托管的 CLAUDE_CONFIG_DIR + 显式的
// ASSISTANT_CONFIG（会话内 assistant MCP 解析同一份配置与 daemon 端点）。
func (c *Chat) sessionEnv() []string {
	env := claudecfg.ConfigDirEnv(c.config.SessionDir)
	if path := strings.TrimSpace(c.config.AssistantConfig); path != "" {
		env = append(env, "ASSISTANT_CONFIG="+path)
	}
	return env
}

// progressLogger 把会话进度写进 daemon 日志：一行一条，便于 grep「用户在问什么、
// claude 在做什么、为什么没有回复」。
func (c *Chat) progressLogger(sessionID string) func(string) {
	return func(text string) {
		// 多行事件（初始化实况、工具入参 JSON、工具结果）按行输出，每行都带前缀：
		// 日志里不出现几百字的长行
		for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			c.config.Log("对话[%s] %s", shortSession(sessionID), truncate(line, 300))
		}
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
