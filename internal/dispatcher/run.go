// 会话封装：每待办一个 headless claude 子进程（`claude -p` + stream-json）。
//
// 命令面与手工运维一致：`--permission-mode auto`（模型分类器自动批准/拒绝，
// result 的 permission_denials 落日志可观测质量）、`--autocompact auto`（长
// 评审会话自动压缩上下文）。会话配置独立于操作者：claudecfg 生成临时
// `--settings`（环境变量与权限放行），`--setting-sources project` 只加载项目级
// 设置（不读也不写 ~/.claude 用户配置），`--no-session-persistence` 不落会话
// 历史。与手工命令的差异，均为无人值守必需：
//   - `--strict-mcp-config` + `--mcp-config` 显式注入宿主仓库的 gitea MCP：评审
//     要提交 Pull Request Review，而 worktree 是新路径，项目级 MCP 授权状态不可依赖；
//   - `--max-turns` 给 runaway 会话兜底（超时之外的第二道闸）；
//   - stdout 按 stream-json 逐行解析，assistant 文本与工具调用（🔧 名称）实时写
//     待办日志。
//
// 超时两段式：SIGTERM 优雅退出，宽限期后 SIGKILL。stderr 只在失败时留尾部。
package dispatcher

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	builtinagents "github.com/Cosmic-Developers-Union/assistant/internal/agents"
	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
	"github.com/Cosmic-Developers-Union/assistant/internal/provider"
	"github.com/Cosmic-Developers-Union/assistant/skills"
)

// SessionOutcome 是一次 claude 会话的归集结果。
type SessionOutcome struct {
	// Subtype 是 success 或 error_max_turns / error_during_execution 等
	Subtype    string  `json:"subtype"`
	IsError    bool    `json:"isError"`
	NumTurns   int     `json:"numTurns"`
	CostUSD    float64 `json:"costUsd"`
	DurationMS int64   `json:"durationMs"`
	// SessionID 是本会话的稳定 ID（--session-id/--resume 用的那个）
	SessionID string `json:"sessionId"`
	// TranscriptPath 是持久化的文本记录路径（未持久化时为空）
	TranscriptPath string `json:"transcriptPath,omitempty"`
	// ArchivePath 是原始 stream-json 行的归档文件（run.yaml sessions-dir；
	// 未配置归档时为空）
	ArchivePath string `json:"archivePath,omitempty"`
	// Resumed 为真表示本次是续接已存在的会话记录
	Resumed           bool     `json:"resumed,omitempty"`
	Result            string   `json:"result"`
	Errors            []string `json:"errors"`
	PermissionDenials int      `json:"permissionDenials"`
}

// PromptContext 是起始提示词携带的钉定信息（缺省字段安全省略）。
type PromptContext struct {
	Repository string
	Title      string
	// HeadSHA 是 PR 会话钉定的 head：漂移作废规则的锚点——会话以它自查
	// 「作者是否已推送」，结论也回写它
	HeadSHA string
}

// MaxTurns 是 runaway 会话的回合上限。
const MaxTurns = 300

// killGrace 是超时 SIGTERM 后的优雅退出窗口，超此改 SIGKILL。
const killGrace = 5 * time.Second

// stderrTailChars 是失败诊断保留的 stderr 尾部长度。
const stderrTailChars = 2_000

var whitespaceRun = regexp.MustCompile(`\s+`)

// BuildPrompt 构造单待办的起始提示词（「review pr #N」为用户指定的起手式）。
func BuildPrompt(kind string, number int64, ctx PromptContext) string {
	title := ctx.Title
	if title != "" {
		title = strings.TrimSpace(whitespaceRun.ReplaceAllString(title, " "))
		if runes := []rune(title); len(runes) > 160 {
			title = string(runes[:160])
		}
	}
	subject := ctx.Repository
	if subject == "" {
		subject = "?"
	}
	subject = fmt.Sprintf("%s 的 #%d", subject, number)
	if title != "" {
		subject += "「" + title + "」"
	}
	if kind == KindPull {
		headLine := ""
		if ctx.HeadSHA != "" {
			headLine = fmt.Sprintf(
				"本会话钉定的 head：%s（当前目录即该 head 的检出）。若经 MCP 查到的 PR head 与此不同，"+
					"说明作者已推送新代码：不要评审旧代码，以 REQUEST_CHANGES 提交结论说明「head 已更新，需以新 head 重新评审」并结束。\n",
				ctx.HeadSHA,
			)
		}
		headNote := ""
		if ctx.HeadSHA != "" {
			headNote = fmt.Sprintf("（%s）", ctx.HeadSHA)
		}
		return fmt.Sprintf("review pr #%d\n\n对象：%s\n%s"+
			"评审协议（assistant 内置的 review 技能）已作为附加 system 提示词给出，按其 PR 审查协议执行；"+
			"项目评审约定（.assistant/review.md，如存在）附在其后，一并遵守。\n"+
			"仅处理该 PR，不要处理其他待办。"+
			"最终以 Gitea 原生 Pull Request Review 提交结论（APPROVED / REQUEST_CHANGES / COMMENT），"+
			"结论正文注明所评审的 head%s。", number, subject, headLine, headNote)
	}
	return fmt.Sprintf("triage issue #%d\n\n对象：%s\n"+
		"分诊规则与标签体系（assistant 内置的 review 技能）已作为附加 system 提示词给出，按其 Issue 分诊规则执行；"+
		"项目评审约定（.assistant/review.md，如存在）附在其后，一并遵守。\n"+
		"仅处理该 Issue，不要处理其他待办。\n"+
		"结论按附加提示词里的标签规则落标签并评论。", number, subject)
}

// NewSessionOutcome 返回按失败回退的初始归集。
func NewSessionOutcome() SessionOutcome {
	return SessionOutcome{
		Subtype: "error_during_execution",
		IsError: true,
		Errors:  []string{},
	}
}

// streamEvent 是 stream-json 单条消息的松弛视图：未知字段忽略，缺省字段由
// 消费端回退。指针字段用于区分「未出现」与「零值」。
type streamEvent struct {
	Type              *string           `json:"type"`
	Subtype           *string           `json:"subtype"`
	SessionID         *string           `json:"session_id"`
	Model             *string           `json:"model"`
	IsError           *bool             `json:"is_error"`
	NumTurns          *int              `json:"num_turns"`
	TotalCostUSD      *float64          `json:"total_cost_usd"`
	DurationMS        *int64            `json:"duration_ms"`
	Result            *string           `json:"result"`
	Errors            []string          `json:"errors"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	Message           *struct {
		Content []streamContentBlock `json:"content"`
	} `json:"message"`
	// thinking_tokens 帧载荷：EstimatedTokens 是本段思考的累计估计词元，
	// EstimatedTokensDelta 是相对上一帧的增量（二选一出现；词元数是 CLI 的
	// 估计值，非计费精确值）。
	EstimatedTokens      *int64 `json:"estimated_tokens"`
	EstimatedTokensDelta *int64 `json:"estimated_tokens_delta"`
}

// streamContentBlock 是 message.content 里的一个块：assistant 消息（text /
// tool_use）与 user 消息里的 tool_result 共用此视图。
type streamContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	ID    string          `json:"id"`
	Input json.RawMessage `json:"input"`
	// tool_result 块
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   *bool           `json:"is_error"`
}

// describeToolInput 提炼工具调用入参里最有判断价值的一段（路径/编号/命令/
// 标题等），让日志读者不打开原始记录也能感知「这一步在干什么」。只取摘要，
// 长值截断，密钥类字段打码。
func describeToolInput(name string, raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return ""
	}
	// 按工具常见的关键字段优先取；取不到就汇总前两个键
	keys := []string{"command", "file_path", "path", "pattern", "url", "query", "description",
		"repository", "repo", "owner", "number", "index", "title", "body", "state", "name", "prompt"}
	var parts []string
	for _, key := range keys {
		value, ok := input[key]
		if !ok {
			continue
		}
		text := strings.TrimSpace(fmt.Sprintf("%v", value))
		if text == "" {
			continue
		}
		text = strings.ReplaceAll(text, "\n", " ")
		if runes := []rune(text); len(runes) > 120 {
			text = string(runes[:120]) + "…"
		}
		switch key {
		case "body", "prompt":
			// 长文本只报长度语义，不刷屏
			parts = append(parts, fmt.Sprintf("%s(%d 字)", key, len([]rune(text))))
		default:
			parts = append(parts, text)
		}
		if len(parts) >= 3 {
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

// describeToolResult 折叠 tool_result：错误要显出来，长输出只报规模。
func describeToolResult(block *streamContentBlock) string {
	if block.IsError != nil && *block.IsError {
		text := strings.TrimSpace(block.Text)
		if text == "" && len(block.Content) > 0 {
			text = strings.TrimSpace(string(block.Content))
		}
		text = strings.ReplaceAll(text, "\n", " ")
		if runes := []rune(text); len(runes) > 160 {
			text = string(runes[:160]) + "…"
		}
		return "✗ " + text
	}
	if len(block.Content) > 0 {
		return fmt.Sprintf("（返回 %d 字）", len(block.Content))
	}
	if block.Text != "" {
		return fmt.Sprintf("（返回 %d 字）", len([]rune(block.Text)))
	}
	return "（完成）"
}

// FeedStreamLine 折叠一条 stream-json 输出行，返回它是否为 result 消息。
// 非 JSON / 非对象行静默忽略——CLI 往 stdout 混入杂音时不至于中断整个会话归档。
func FeedStreamLine(outcome *SessionOutcome, line string, onProgress func(string)) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	var event streamEvent
	if err := json.Unmarshal([]byte(trimmed), &event); err != nil {
		return false
	}
	return feedStreamEvent(outcome, &event, onProgress)
}

func feedStreamEvent(outcome *SessionOutcome, event *streamEvent, onProgress func(string)) bool {
	if event.Type != nil && *event.Type == "system" && event.Subtype != nil && *event.Subtype == "init" {
		if event.SessionID != nil {
			outcome.SessionID = *event.SessionID
		}
		if onProgress != nil {
			onProgress(fmt.Sprintf("session=%s model=%s", valueOr(event.SessionID, "?"), valueOr(event.Model, "?")))
		}
		return false
	}
	if event.Type != nil && *event.Type == "assistant" {
		outcome.NumTurns++
		if event.Message != nil {
			for _, block := range event.Message.Content {
				switch {
				case block.Type == "text" && strings.TrimSpace(block.Text) != "":
					if onProgress != nil {
						onProgress(strings.TrimSpace(block.Text))
					}
				case block.Type == "tool_use" && block.Name != "":
					if onProgress != nil {
						// 工具调用带出入参摘要：读者能判断「当前步在干什么」
						if detail := describeToolInput(block.Name, block.Input); detail != "" {
							onProgress(fmt.Sprintf("🔧 %s: %s", block.Name, detail))
						} else {
							onProgress("🔧 " + block.Name)
						}
					}
				}
			}
		}
		return false
	}
	// user 事件承载 tool_result：把每个工具的回执折成一行（错误显式标红），
	// 会话时间线在日志里完整可读
	if event.Type != nil && *event.Type == "user" && event.Message != nil && onProgress != nil {
		for _, block := range event.Message.Content {
			if block.Type == "tool_result" {
				onProgress("  ↳ " + describeToolResult(&block))
			}
		}
		return false
	}
	if event.Type != nil && *event.Type == "result" {
		if event.Subtype != nil {
			outcome.Subtype = *event.Subtype
		}
		if event.IsError != nil {
			outcome.IsError = *event.IsError
		} else {
			outcome.IsError = true
		}
		if event.NumTurns != nil {
			outcome.NumTurns = *event.NumTurns
		}
		outcome.CostUSD = 0
		if event.TotalCostUSD != nil {
			outcome.CostUSD = *event.TotalCostUSD
		}
		outcome.DurationMS = 0
		if event.DurationMS != nil {
			outcome.DurationMS = *event.DurationMS
		}
		if event.SessionID != nil {
			outcome.SessionID = *event.SessionID
		}
		outcome.Errors = append(outcome.Errors, event.Errors...)
		outcome.PermissionDenials = len(event.PermissionDenials)
		if !outcome.IsError && event.Result != nil {
			outcome.Result = *event.Result
		}
		return true
	}
	return false
}

func valueOr(value *string, fallback string) string {
	if value != nil {
		return *value
	}
	return fallback
}

// SessionOptions 是一次会话的输入。
type SessionOptions struct {
	Config Config
	Prompt string
	Cwd    string
	// MCPConfigPath 是宿主基线检出的 .mcp.json（仓库自带的其它 MCP server 的
	// 合并基线，可以不存在）：assistant 指定的 gitea server 恒注入，工具面与
	// 具体仓库内容解耦
	MCPConfigPath string
	// SettingsPath 是独立会话配置（--settings）的路径：由 RunSession 生成，调用
	// 方无需填写（测试可预置固定路径）
	SettingsPath string
	// ProjectDir 是宿主基线检出根：存在 <ProjectDir>/.assistant/review.md 时，
	// 其内容作为附加 system 提示词（--append-system-prompt）注入会话
	ProjectDir string
	// AppendSystemPrompt 是显式附加 system 提示词（测试用；缺省由 ProjectDir
	// 的 .assistant/review.md 提供）
	AppendSystemPrompt string
	// SessionID 是会话 ID：空则随机生成；已存在该 ID 的文本记录时自动改为
	// --resume 续接（重试同一待办保留上下文），否则 --session-id 新建
	SessionID string
	// Title 是会话显示名（--name），便于在会话选择器与 --resume <name> 中识别
	Title string
	// SessionResume 由 RunSession 依据文本记录是否存在决定，调用方无需填写
	SessionResume bool
	OnProgress    func(string)
}

// ReviewConventionsLimit 是 .assistant/review.md 注入会话的上限（字符数），
// 超出部分截断，避免超长文件挤占上下文。
const ReviewConventionsLimit = 32_000

// ReadReviewConventions 读取项目评审约定（<project-root>/.assistant/review.md），
// 作为附加 system 提示词注入会话；文件缺失或为空返回空串。只读基线检出，
// PR 自带的版本不生效（与 .claude/ 的钉定口径一致）。
func ReadReviewConventions(projectDir string) string {
	if projectDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(projectDir, ".assistant", "review.md"))
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return ""
	}
	if runes := []rune(text); len(runes) > ReviewConventionsLimit {
		text = string(runes[:ReviewConventionsLimit]) + "\n\n（.assistant/review.md 过长，已截断）"
	}
	return text
}

// ReviewProtocolPrompt 组装评审/分诊会话的附加 system 提示词：assistant **内置**
// 的 review agent 协议（agents 注册表取值，唯一事实源 skills/review/SKILL.md）
// 打头，项目自有约定（<projectDir>/.assistant/review.md，如存在）附在其后。
// 协议随二进制走，因此仓库没有 install 过、或没有 .claude/skills 都不影响评审。
func ReviewProtocolPrompt(projectDir string) string {
	protocol := ""
	if definition, ok := builtinagents.Lookup("review"); ok {
		protocol = strings.TrimSpace(definition.SystemPrompt)
	}
	if protocol == "" {
		protocol = strings.TrimSpace(skills.ReviewPrompt())
	}
	sections := []string{protocol}
	if conventions := ReadReviewConventions(projectDir); conventions != "" {
		sections = append(sections,
			"## 项目评审约定（.assistant/review.md）\n\n"+conventions)
	}
	return strings.Join(sections, "\n\n")
}

// sessionCommand 组装会话命令：缺省直接跑 claude；Config.DockerImage 非空时
// 跑在容器里——worktree 与 MCP 配置按相同绝对路径挂载（容器内外路径一致，
// claude 的路径参数无需改写），认证环境变量按白名单透传（-e KEY 继承宿主值）。
// 返回的 container 名用于超时终止（SIGTERM docker 客户端不会停容器）。
func sessionCommand(options SessionOptions) (bin string, args []string, container string) {
	config := options.Config
	claudeArgs := []string{
		"-p",
		options.Prompt,
		"--permission-mode",
		"auto",
		"--autocompact",
		"auto",
		"--output-format",
		"stream-json",
		"--verbose",
		"--strict-mcp-config",
		"--mcp-config",
		options.MCPConfigPath,
		"--setting-sources",
		claudecfg.SettingSources,
		"--max-turns",
		strconv.Itoa(MaxTurns),
	}
	// 会话记录持久化：稳定的 ID（同一待办重试续接）+ 自定义标题
	if options.SessionID != "" {
		if options.SessionResume {
			claudeArgs = append(claudeArgs, "--resume", options.SessionID)
		} else {
			claudeArgs = append(claudeArgs, "--session-id", options.SessionID)
		}
	}
	if options.Title != "" {
		claudeArgs = append(claudeArgs, "--name", options.Title)
	}
	if options.SettingsPath != "" {
		claudeArgs = append(claudeArgs, "--settings", options.SettingsPath)
	}
	if options.AppendSystemPrompt != "" {
		claudeArgs = append(claudeArgs, "--append-system-prompt", options.AppendSystemPrompt)
	}
	if config.Model != "" {
		claudeArgs = append(claudeArgs, "--model", config.Model)
	}
	if config.DockerImage == "" {
		return config.ClaudeBin, claudeArgs, ""
	}
	container = fmt.Sprintf("assistant-review-%d-%d", os.Getpid(), time.Now().UnixNano())
	args = []string{
		"run", "--rm", "-i",
		"--name", container,
		"-v", options.Cwd + ":" + options.Cwd,
		"-w", options.Cwd,
	}
	// 会话输入文件（MCP 配置、独立 settings）与文本记录目录按相同绝对路径挂载
	for _, dir := range sessionMountDirs(options) {
		args = append(args, "-v", dir+":"+dir)
	}
	// assistant 二进制：会话里的 gitea MCP 由它启动（claudecfg.AssistantCommand），
	// 按同一绝对路径只读挂进去，评审镜像不必自带 assistant
	if assistant := claudecfg.AssistantCommand(); filepath.IsAbs(assistant) {
		args = append(args, "-v", assistant+":"+assistant+":ro")
	}
	// 文本记录固定目录名靠进程环境传递，容器内显式注入并挂载（挂载点见
	// sessionMountDirs 的 projects 目录）
	if config.SessionDir != "" {
		args = append(args, "-e", "CLAUDE_CONFIG_DIR="+config.SessionDir)
	}
	if config.SessionProject != "" {
		args = append(args, "-e", "CLAUDE_CODE_PROJECT_DIR_NAME="+config.SessionProject)
	}
	if config.DockerNetwork != "" {
		args = append(args, "--network", config.DockerNetwork)
	}
	for _, key := range dockerPassthroughEnv() {
		args = append(args, "-e", key)
	}
	// Gitea 身份与配置来源由宿主显式钉定：docker 从宿主环境继承值（见
	// sessionCredentialEnv），容器内的 MCP 才能拿到与 daemon 相同的评审身份。
	for _, key := range sessionCredentialKeys(config) {
		args = append(args, "-e", key)
	}
	args = append(args, config.DockerImage, config.ClaudeBin)
	args = append(args, claudeArgs...)
	return "docker", args, container
}

// sessionCredentialEnv 返回钉定评审会话 Gitea 身份与配置来源的环境变量：
//   - GITEA_HOST / GITEA_ACCESS_TOKEN 让会话内 `assistant mcp gitea` 以 reviewer
//     （默认 ai）令牌落库 review——完成判定只承认该账号名下的 review；
//   - ASSISTANT_CONFIG 让会话内 MCP/daemon 解析与 dispatcher 相同的 config.json。
//
// 必须显式注入而非依赖继承：config.json 模式下 daemon 进程环境通常没有 Gitea
// 变量，会话 MCP 会回退到开发者个人的 mcp 令牌，评审身份随之漂移。
func sessionCredentialEnv(config Config) []string {
	pairs := make([]string, 0, 3)
	if config.Host != "" {
		pairs = append(pairs, "GITEA_HOST="+config.Host)
	}
	if config.AccessToken != "" {
		pairs = append(pairs, "GITEA_ACCESS_TOKEN="+config.AccessToken)
	}
	if config.ConfigPath != "" {
		pairs = append(pairs, "ASSISTANT_CONFIG="+config.ConfigPath)
	}
	return pairs
}

// sessionCredentialKeys 与 sessionCredentialEnv 同源，只返回键名：docker -e 的
// 值由 docker 从（已注入这些变量的）宿主进程环境继承。
func sessionCredentialKeys(config Config) []string {
	pairs := sessionCredentialEnv(config)
	keys := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		key, _, _ := strings.Cut(pair, "=")
		keys = append(keys, key)
	}
	return keys
}

var (
	dockerEnvPrefixes = []string{"ANTHROPIC_", "CLAUDE_"}
	dockerEnvNames    = []string{
		"DISABLE_TELEMETRY", "DO_NOT_TRACK",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	}
)

// dockerPassthroughEnv 返回需要 -e 透传给评审容器的环境变量名（只传键，值由
// docker 从宿主环境继承）。
func dockerPassthroughEnv() []string {
	var keys []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		match := slices.Contains(dockerEnvNames, key)
		if !match {
			for _, prefix := range dockerEnvPrefixes {
				if strings.HasPrefix(key, prefix) {
					match = true
					break
				}
			}
		}
		if match {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// RunSession 驱动 claude 子进程并归集 stream-json 结果。
func RunSession(options SessionOptions) SessionOutcome {
	config := options.Config
	startedAt := time.Now()
	outcome := NewSessionOutcome()

	// 会话上下文：assistant 内置的评审/分诊协议 + 项目自有约定（.assistant/review.md）
	if options.AppendSystemPrompt == "" {
		options.AppendSystemPrompt = ReviewProtocolPrompt(options.ProjectDir)
	}

	// 稳定的会话 ID 与文本记录位置：同一待办重试复用同一记录（--resume 续接），
	// 记录固定落在 <SessionDir>/projects/<SessionProject>/ 下，与 /tmp worktree
	// 的生命周期解耦
	if options.SessionID == "" {
		options.SessionID = provider.NewSessionID()
	}
	outcome.SessionID = options.SessionID
	options.SessionResume = false
	if transcript := claudecfg.TranscriptPath(config.SessionDir, config.SessionProject, options.SessionID); transcript != "" {
		if _, err := os.Stat(transcript); err == nil {
			options.SessionResume = true
		}
		outcome.TranscriptPath = transcript
	}
	outcome.Resumed = options.SessionResume

	// 独立会话配置写临时目录：环境变量与权限放行随二进制版本走，不依赖仓库
	// 状态与操作者用户配置
	configDir, cleanup, err := createSessionConfigDir()
	if err != nil {
		outcome.Errors = append(outcome.Errors, err.Error())
		return outcome
	}
	defer cleanup()
	// 会话配置根由 assistant 托管（claude 的全局配置与会话状态都写在这里，
	// 不是用户的 ~/.claude）：不存在时先建，避免把失败留给第一次评审
	if config.SessionDir != "" {
		if err := os.MkdirAll(config.SessionDir, 0o755); err != nil {
			outcome.Errors = append(outcome.Errors, fmt.Sprintf("创建会话配置根 %s: %v", config.SessionDir, err))
			return outcome
		}
	}
	// 供应商代码级特化（如 opencode 的会话请求头）：一次会话一个 id
	overrides, err := provider.Apply(config.ProviderName, "review", "", config.Provider)
	if err != nil {
		outcome.Errors = append(outcome.Errors, err.Error())
		return outcome
	}
	settingsPath, err := writeClaudeSessionSettings(configDir, overrides)
	if err != nil {
		outcome.Errors = append(outcome.Errors, err.Error())
		return outcome
	}
	options.SettingsPath = settingsPath
	// 会话 MCP 配置：assistant 指定的 gitea server + 仓库 .mcp.json 的其它 server
	// + provider 原生 server（仓库文件缺失也照常生成）
	mergedMCPPath, err := writeSessionMCPConfig(configDir, options.MCPConfigPath, overrides)
	if err != nil {
		outcome.Errors = append(outcome.Errors, err.Error())
		return outcome
	}
	options.MCPConfigPath = mergedMCPPath

	bin, args, container := sessionCommand(options)
	if config.Debug {
		debugProgress(options.OnProgress, "会话命令："+truncateRunes(strings.Join(append([]string{bin}, args...), " "), 800))
		debugProgress(options.OnProgress, "文本记录："+outcome.TranscriptPath)
		debugProgress(options.OnProgress, "会话配置：")
		for _, line := range claudecfg.EnvLines(config.Provider.Env) {
			debugProgress(options.OnProgress, "  env."+line)
		}
		if keys := claudecfg.SettingKeys(config.Provider.Settings); len(keys) > 0 {
			debugProgress(options.OnProgress, "  settings（值不打印）："+strings.Join(keys, " "))
		}
		debugProgress(options.OnProgress, fmt.Sprintf("  settings = %s", options.SettingsPath))
		debugProgress(options.OnProgress, fmt.Sprintf("  mcp      = %s", options.MCPConfigPath))
		debugProgress(options.OnProgress, fmt.Sprintf("  配置根   = %s", config.SessionDir))
		debugProgress(options.OnProgress, "  MCP 声明：")
		for _, line := range describeSessionMCP(options.MCPConfigPath) {
			debugProgress(options.OnProgress, "    "+line)
		}
	}
	command := exec.Command(bin, args...)
	command.Dir = options.Cwd
	// 文本记录的固定项目目录名必须在进程环境里（settings.env 无效）；同时把
	// Gitea 身份与配置来源显式钉定给会话（见 sessionCredentialEnv）
	env := claudecfg.SessionEnv(config.SessionDir, config.SessionProject)
	env = append(env, sessionCredentialEnv(config)...)
	if len(env) > 0 {
		command.Env = append(os.Environ(), env...)
	}
	stream := &streamWriter{outcome: &outcome, onProgress: options.OnProgress, debug: config.Debug,
		thinking: thinkingTracker{onProgress: options.OnProgress, debug: config.Debug}}
	// 会话记录归档（run.yaml sessions-dir + 命名模板）：原始 stream-json 每行
	// 落盘，外部工具（sqlite3 cli/编辑器）无需经 daemon 即可内省完整会话
	if archiveDir := strings.TrimSpace(config.SessionArchiveDir); archiveDir != "" {
		archivePath := filepath.Join(archiveDir, outcome.SessionID+".jsonl")
		if err := os.MkdirAll(archiveDir, 0o755); err == nil {
			if file, err := os.OpenFile(archivePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
				stream.archiveFile = file
				defer file.Close()
				outcome.ArchivePath = archivePath
			}
		}
	}
	stderr := &tailWriter{}
	command.Stdout = stream
	command.Stderr = stderr

	if err := command.Start(); err != nil {
		// spawn 失败：claudeBin/docker 不存在或不可执行
		outcome.Errors = append(outcome.Errors, err.Error())
		return outcome
	}

	var timedOut atomic.Bool
	timer := time.AfterFunc(config.SessionTimeout, func() {
		timedOut.Store(true)
		// docker 客户端被终止不会停掉容器，按名 kill 兜底
		if container != "" {
			_ = exec.Command("docker", "kill", container).Run()
		}
		_ = terminateProcess(command.Process)
		time.AfterFunc(killGrace, func() { _ = command.Process.Kill() })
	})
	waitErr := command.Wait()
	timer.Stop()
	// Wait 已等待 stdio 拷贝完成，冲刷无换行结尾的残余行
	stream.flush()
	// 残留的思考段（流以 thinking 帧结尾时）冲刷 + 全会话合计
	stream.thinking.finish()

	switch {
	case waitErr != nil && command.ProcessState == nil:
		outcome.Errors = append(outcome.Errors, waitErr.Error())
	case timedOut.Load():
		outcome.Errors = append(
			outcome.Errors,
			fmt.Sprintf("会话超时（>%dms），已 SIGTERM 终止", config.SessionTimeout.Milliseconds()),
		)
	default:
		if exitCode := command.ProcessState.ExitCode(); exitCode != 0 {
			how := ""
			if exitCode > 0 {
				how = fmt.Sprintf("退出码 %d", exitCode)
			} else if signaled, name := processSignaled(command.ProcessState); signaled {
				how = fmt.Sprintf("被信号 %s 终止", name)
			} else {
				how = "异常终止"
			}
			suffix := ""
			if tail := strings.TrimSpace(stderr.String()); tail != "" {
				suffix = "：" + tail
			}
			outcome.Errors = append(outcome.Errors, "claude "+how+suffix)
		}
	}
	if !stream.sawResult {
		outcome.Errors = append(outcome.Errors, "stream 结束但未收到 result 消息")
	}
	if len(outcome.Errors) > 0 {
		outcome.IsError = true
	}
	if config.Debug {
		debugProgress(options.OnProgress, fmt.Sprintf("会话结束：subtype=%s is_error=%t turns=%d cost=$%.4f duration=%dms",
			outcome.Subtype, outcome.IsError, outcome.NumTurns, outcome.CostUSD, outcome.DurationMS))
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			debugProgress(options.OnProgress, "stderr 尾部："+truncateRunes(tail, 600))
		}
	}
	if outcome.DurationMS == 0 {
		outcome.DurationMS = time.Since(startedAt).Milliseconds()
	}
	return outcome
}

// streamWriter 按行折叠 stream-json 输出。
type streamWriter struct {
	outcome    *SessionOutcome
	onProgress func(string)
	buffer     []byte
	sawResult  bool
	// debug 为真时把每行原始 stream 事件也交给 onProgress（排查「会话在干什么」）
	debug bool
	// thinking 聚合 system/thinking_tokens 帧（仅 debug 下喂入）
	thinking thinkingTracker
	// archiveFile 是会话原始 stream-json 行的归档文件（可选；每行原样追加，
	// daemon 重启不丢，外部工具可直接内省）
	archiveFile *os.File
}

func (w *streamWriter) Write(chunk []byte) (int, error) {
	w.buffer = append(w.buffer, chunk...)
	for {
		newline := bytes.IndexByte(w.buffer, '\n')
		if newline < 0 {
			break
		}
		line := string(w.buffer[:newline])
		w.buffer = w.buffer[newline+1:]
		w.reportDebug(line)
		if w.archiveFile != nil {
			_, _ = w.archiveFile.WriteString(line + "\n")
		}
		if FeedStreamLine(w.outcome, line, w.onProgress) {
			w.sawResult = true
		}
	}
	return len(chunk), nil
}

func (w *streamWriter) flush() {
	if len(w.buffer) == 0 {
		return
	}
	w.reportDebug(string(w.buffer))
	if w.archiveFile != nil {
		_, _ = w.archiveFile.WriteString(string(w.buffer) + "\n")
	}
	if FeedStreamLine(w.outcome, string(w.buffer), w.onProgress) {
		w.sawResult = true
	}
	w.buffer = nil
}

// debugProgress 在 --debug 下把细节写进待办日志（前缀 [debug]，便于过滤）。
func debugProgress(onProgress func(string), line string) {
	if onProgress == nil || line == "" {
		return
	}
	onProgress("[debug] " + line)
}

// parseStreamEvent 提炼 stream-json 单行的头部（解析失败 ok=false）。
func parseStreamEvent(line string) (streamEvent, bool) {
	var event streamEvent
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return streamEvent{}, false
	}
	return event, true
}

// reportDebug 在 --debug 下报一行事件时间线：只写 type/subtype，不堆原始 JSON
// （base64 与长 diff 会把日志淹掉；原文在会话文本记录里）。例外是
// system/thinking_tokens 帧：思考期间每约 10ms 一帧，逐帧上报会把时间线淹掉，
// 改由 thinkingTracker 累计、段结束时报一条汇总（含 t/s）。
func (w *streamWriter) reportDebug(line string) {
	if !w.debug || w.onProgress == nil {
		return
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return
	}
	event, ok := parseStreamEvent(trimmed)
	if ok && event.Type != nil && event.Subtype != nil &&
		*event.Type == "system" && *event.Subtype == "thinking_tokens" {
		w.thinking.observe(event)
		return
	}
	w.thinking.flush()
	w.onProgress("[debug] 事件 " + describeStreamEvent(event, ok, trimmed))
}

// describeStreamEvent 由已解析的事件给出类型标注；非 JSON 行只报长度。
func describeStreamEvent(event streamEvent, ok bool, line string) string {
	if !ok {
		return fmt.Sprintf("（非 JSON 行，%d 字）", len([]rune(line)))
	}
	typeText, subtypeText := "", ""
	if event.Type != nil {
		typeText = *event.Type
	}
	if event.Subtype != nil {
		subtypeText = *event.Subtype
	}
	switch {
	case typeText != "" && subtypeText != "":
		return typeText + "/" + subtypeText
	case typeText != "":
		return typeText
	case subtypeText != "":
		return "subtype/" + subtypeText
	}
	return "（未标注类型）"
}

// thinkingTracker 把 system/thinking_tokens 帧折成段级汇总：思考期间 CLI 每约
// 10ms 一帧（估计词元的累计值与增量），逐帧上报毫无信息量。这里只累计，一段
// 思考结束（下一个非 thinking 事件到达或会话收尾）时报一条——词元数、时长、
// t/s（评估思考速度）、帧数；会话收尾再给全会话合计。
type thinkingTracker struct {
	onProgress func(string)
	debug      bool
	// 当前思考段（burst）的累计；flush 后归零
	burstTokens int64
	burstFrames int
	burstStart  time.Time
	burstLast   time.Time
	// 全会话合计（跨段保留）
	totalTokens int64
	totalFrames int
	totalBursts int
	prevCum     int64
	hasPrev     bool
}

// observe 记录一帧 thinking_tokens：优先取增量，缺失时按累计值差值补算。
func (t *thinkingTracker) observe(event streamEvent) {
	now := time.Now()
	if t.burstStart.IsZero() {
		t.burstStart = now
		t.totalBursts++
	}
	switch {
	case event.EstimatedTokensDelta != nil:
		if delta := *event.EstimatedTokensDelta; delta > 0 {
			t.burstTokens += delta
		}
	case event.EstimatedTokens != nil:
		if t.hasPrev && *event.EstimatedTokens > t.prevCum {
			t.burstTokens += *event.EstimatedTokens - t.prevCum
		}
		t.prevCum = *event.EstimatedTokens
		t.hasPrev = true
	}
	t.burstFrames++
	t.totalFrames++
	t.burstLast = now
}

// flush 报出当前思考段的汇总并归零段计数；无进行中的思考段时不动。
func (t *thinkingTracker) flush() {
	if t.burstStart.IsZero() {
		return
	}
	segment := struct {
		tokens, frames int64
		elapsed        time.Duration
	}{tokens: t.burstTokens, frames: int64(t.burstFrames), elapsed: t.burstLast.Sub(t.burstStart)}
	t.totalTokens += segment.tokens
	t.resetBurst()
	if !t.debug || t.onProgress == nil {
		return
	}
	rate := 0.0
	if segment.elapsed > 0 {
		rate = float64(segment.tokens) / segment.elapsed.Seconds()
	}
	t.onProgress(fmt.Sprintf("[debug] thinking 段汇总：≈%d tokens / %.1fs（%.0f t/s，%d 帧）",
		segment.tokens, segment.elapsed.Seconds(), rate, segment.frames))
}

// finish 在会话收尾时冲刷残留思考段，并报全会话合计。
func (t *thinkingTracker) finish() {
	t.flush()
	if t.debug && t.onProgress != nil && t.totalFrames > 0 {
		t.onProgress(fmt.Sprintf("[debug] thinking 合计：≈%d tokens（%d 段，%d 帧）",
			t.totalTokens, t.totalBursts, t.totalFrames))
	}
}

func (t *thinkingTracker) resetBurst() {
	t.burstTokens = 0
	t.burstFrames = 0
	t.burstStart = time.Time{}
	t.burstLast = time.Time{}
}

// describeSessionMCP 读取生成的会话 MCP 配置并展开成逐行说明（debug 日志用；密钥打码）。
func describeSessionMCP(path string) []string {
	if strings.TrimSpace(path) == "" {
		return []string{"（无）"}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{"（读取失败：" + err.Error() + "）"}
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return []string{"（解析失败：" + err.Error() + "）"}
	}
	return claudecfg.MCPServerLines(document)
}

// truncateRunes 按字符截断（日志用）。
func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// tailWriter 只保留 stderr 尾部（失败时留线索）。
type tailWriter struct {
	buffer []byte
}

func (w *tailWriter) Write(chunk []byte) (int, error) {
	w.buffer = append(w.buffer, chunk...)
	if len(w.buffer) > stderrTailChars {
		w.buffer = w.buffer[len(w.buffer)-stderrTailChars:]
	}
	return len(chunk), nil
}

func (w *tailWriter) String() string {
	return string(w.buffer)
}

func signalName(signal syscall.Signal) string {
	switch signal {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGINT:
		return "SIGINT"
	}
	return signal.String()
}
