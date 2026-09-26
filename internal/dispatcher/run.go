// 会话封装：每待办一个 headless claude 子进程（`claude -p` + stream-json）。
//
// 命令面与手工运维一致：`--permission-mode auto`（模型分类器自动批准/拒绝，
// result 的 permission_denials 落日志可观测质量）、`--autocompact auto`（长
// 评审会话自动压缩上下文）。会话配置独立于操作者：claudecfg 生成临时
// `--settings`（环境变量与权限放行），`--setting-sources project` 只加载项目级
// 设置（不读也不写 ~/.claude 用户配置）。与手工命令的差异，均为无人值守必需：
//   - `--strict-mcp-config` + `--mcp-config` 显式注入宿主仓库的 gitea MCP：评审
//     要提交 Pull Request Review，而 worktree 是新路径，项目级 MCP 授权状态不可依赖；
//   - `--max-turns` 给 runaway 会话兜底（超时之外的第二道闸）；
//   - stdout 按 stream-json 逐行解析，assistant 文本与工具调用（🔧 名称）实时写
//     待办日志。
//
// 与 CLI 的全部交互（argv/env 组装、进程启动、逐行交付、超时两段式终止、结果
// 归集）都在 internal/claude：本文件只负责「这次会话要传什么」，以及「收到的行
// 对评审语义意味着什么」。
package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	builtinagents "github.com/Cosmic-Developers-Union/assistant/internal/agents"
	"github.com/Cosmic-Developers-Union/assistant/internal/claude"
	"github.com/Cosmic-Developers-Union/assistant/internal/claudecfg"
	"github.com/Cosmic-Developers-Union/assistant/internal/provider"
	"github.com/Cosmic-Developers-Union/assistant/skills"
)

// sessionOutcome 是一次 claude 会话的归集结果。
//
// 为什么内嵌 claude.Outcome：归集字段（subtype/is_error/轮次/花费/结论/错误）
// 的解析归 claude 包，本包只补两个本地字段，避免同一份语义在两处各写一遍。
type sessionOutcome struct {
	claude.Outcome
	// TranscriptPath 是持久化的文本记录路径（未持久化时为空）——本地探测所得，
	// 不来自 CLI 输出
	TranscriptPath string `json:"transcriptPath,omitempty"`
}

// SessionOutcome 是对外（cmd/assistant、statestore）暴露的归集结果：字段名与
// JSON tag 已定型，因此保留独立类型与显式转换（sessionOutcomeOf），不直接把
// claude.Outcome 外泄——CLI 侧字段增删不该自动改变对外契约。
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

// 超时两段式终止（SIGTERM → 宽限期后 SIGKILL）、stderr 尾部长度与容器按名兜底
// 都在 claude 包（claude.KillGrace / claude.StderrTailChars）：执行点只有一个，
// 超时与诊断行为不该有两处定义。

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

// newSessionOutcome 返回按失败回退的初始归集：会话还没跑出结论之前，任何提前
// 退出（配置生成失败、spawn 失败）都必须归到「执行期错误」而不是留空 subtype
// ——留空会被上层当成「没有结论」而丢失失败归因。
func newSessionOutcome() sessionOutcome {
	outcome := claude.NewOutcome()
	outcome.Subtype = "error_during_execution"
	outcome.IsError = true
	return sessionOutcome{Outcome: outcome}
}

// stream-json 的模型与解析（喂行、折叠归集、进度播报）都在 internal/claude：
// 本包与 daemon 共用一份，CLI 改字段名只需改一处。

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

// sessionContainer 组装容器形态的运行参数：worktree、会话输入文件与文本记录
// 目录按相同绝对路径挂载（容器内外路径一致，claude 的路径参数无需改写），认证
// 环境变量按白名单透传（`-e KEY` 继承宿主值）。返回 nil 表示直接跑宿主 claude。
//
// 容器名带进程号与纳秒时间戳：同一宿主可能并行多个待办，名字必须唯一（超时兜底
// 按名 kill 容器，重名会误杀别的会话）。
func sessionContainer(options SessionOptions) *claude.Container {
	config := options.Config
	if config.DockerImage == "" {
		return nil
	}
	return &claude.Container{
		Name:  fmt.Sprintf("assistant-review-%d-%d", os.Getpid(), time.Now().UnixNano()),
		Cwd:   options.Cwd,
		Image: config.DockerImage,
		// assistant 二进制：会话里的 gitea MCP 由它启动（claudecfg.AssistantCommand），
		// 按同一绝对路径只读挂进去，评审镜像不必自带 assistant
		AssistantBin: claudecfg.AssistantCommand(),
		// 文本记录固定目录名靠进程环境传递，容器内显式注入并挂载（挂载点见
		// sessionMountDirs 的 projects 目录）
		ConfigDir:      config.SessionDir,
		ProjectDirName: config.SessionProject,
		Network:        config.DockerNetwork,
		// 会话输入文件（MCP 配置、独立 settings）与文本记录目录
		MountDirs: sessionMountDirs(options),
		// Gitea 身份与配置来源由宿主显式钉定：docker 从宿主环境继承值（见
		// sessionCredentialEnv），容器内的 MCP 才能拿到与 daemon 相同的评审身份。
		PassthroughEnv: append(dockerPassthroughEnv(), sessionCredentialKeys(config)...),
	}
}

// sessionArgsOptions 组装传给 claude 的参数面（宿主与容器形态共用）：评审会话的
// 全部「非默认取舍」——权限模式、autocompact、strict-mcp-config、setting-sources、
// max-turns 与 session-id/--resume——都只在这里出现一次。
func sessionArgsOptions(options SessionOptions) claude.ArgsOptions {
	config := options.Config
	args := claude.ArgsOptions{
		Prompt:             options.Prompt,
		SettingsPath:       options.SettingsPath,
		MCPConfigPath:      options.MCPConfigPath,
		SettingSources:     claudecfg.SettingSources,
		AppendSystemPrompt: options.AppendSystemPrompt,
		Name:               options.Title,
		Model:              config.Model,
		PermissionMode:     "auto",
		// 会话记录持久化：稳定的 ID（同一待办重试续接）+ 自定义标题
		Session: claude.Session{ID: options.SessionID, Resume: options.SessionResume},
		// runaway 会话兜底（超时之外的第二道闸）
		MaxTurns:    MaxTurns,
		Autocompact: "auto",
		StrictMCP:   true,
		Verbose:     true,
	}
	if container := sessionContainer(options); container != nil {
		args.Container = container
	}
	return args
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

// RunSession 驱动一次评审/分诊会话并归集 stream-json 结果。
//
// 分成两半：组装（review.md → 附加提示词、文本记录探测、settings/MCP 配置落盘、
// env/argv/Spec）是纯逻辑，可在单测里逐项断言；执行只经过 claude.Runner 这一个
// 执行点（缺省 exec 起真实进程，测试注入假实现喂预置的 stream-json 行）。
func RunSession(options SessionOptions) SessionOutcome {
	config := options.Config
	startedAt := time.Now()
	outcome := newSessionOutcome()

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
		return sessionOutcomeOf(outcome)
	}
	defer cleanup()
	// 会话配置根由 assistant 托管（claude 的全局配置与会话状态都写在这里，
	// 不是用户的 ~/.claude）：不存在时先建，避免把失败留给第一次评审
	if config.SessionDir != "" {
		if err := os.MkdirAll(config.SessionDir, 0o755); err != nil {
			outcome.Errors = append(outcome.Errors, fmt.Sprintf("创建会话配置根 %s: %v", config.SessionDir, err))
			return sessionOutcomeOf(outcome)
		}
	}
	// 供应商代码级特化（如 opencode 的会话请求头）：一次会话一个 id
	overrides, err := provider.Apply(config.ProviderName, "review", "", config.Provider)
	if err != nil {
		outcome.Errors = append(outcome.Errors, err.Error())
		return sessionOutcomeOf(outcome)
	}
	settingsPath, err := writeClaudeSessionSettings(configDir, overrides)
	if err != nil {
		outcome.Errors = append(outcome.Errors, err.Error())
		return sessionOutcomeOf(outcome)
	}
	options.SettingsPath = settingsPath
	// 会话 MCP 配置：assistant 指定的 gitea server + 仓库 .mcp.json 的其它 server
	// + provider 原生 server（仓库文件缺失也照常生成）
	mergedMCPPath, err := writeSessionMCPConfig(configDir, options.MCPConfigPath, overrides)
	if err != nil {
		outcome.Errors = append(outcome.Errors, err.Error())
		return sessionOutcomeOf(outcome)
	}
	options.MCPConfigPath = mergedMCPPath

	spec := runSessionSpec(options)
	if config.Debug {
		debugProgress(options.OnProgress, "会话命令："+truncateRunes(
			strings.Join(append([]string{spec.Bin}, spec.Args...), " "), 800))
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

	stream := newSessionStream(&outcome, options.OnProgress, config.Debug)
	defer stream.close()
	// 会话记录归档（run.yaml sessions-dir + 命名模板）：原始 stream-json 每行
	// 落盘，外部工具（sqlite3 cli/编辑器）无需经 daemon 即可内省完整会话
	if archiveDir := strings.TrimSpace(config.SessionArchiveDir); archiveDir != "" {
		outcome.ArchivePath = stream.archiveTo(archiveDir)
	}

	// 超时终止（SIGTERM → 宽限期后 SIGKILL）、容器按名兜底、spawn 失败与退出码
	// 归因都在 Runner 里；这里只管「拿到结论没有」。
	runErr := sessionRunner().Run(context.Background(), spec, stream.consume)

	// 无换行结尾的末行由 Runner 在返回前冲刷，这里不必再兜底
	// 残留的思考段（流以 thinking 帧结尾时）冲刷 + 全会话合计
	stream.thinking.finish()
	if runErr != nil {
		outcome.Errors = append(outcome.Errors, runErr.Error())
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
	}
	if outcome.DurationMS == 0 {
		outcome.DurationMS = time.Since(startedAt).Milliseconds()
	}
	return sessionOutcomeOf(outcome)
}

// sessionOutcomeOf 把内部归集折进对外类型：TranscriptPath 是本包本地探测所得
// （claude 包不知道文本记录落在哪），其余字段一一对应。
func sessionOutcomeOf(outcome sessionOutcome) SessionOutcome {
	return SessionOutcome{
		Subtype:           outcome.Subtype,
		IsError:           outcome.IsError,
		NumTurns:          outcome.NumTurns,
		CostUSD:           outcome.CostUSD,
		DurationMS:        outcome.DurationMS,
		SessionID:         outcome.SessionID,
		TranscriptPath:    outcome.TranscriptPath,
		ArchivePath:       outcome.ArchivePath,
		Resumed:           outcome.Resumed,
		Result:            outcome.Result,
		Errors:            outcome.Errors,
		PermissionDenials: outcome.PermissionDenials,
	}
}

// SessionRunner 是可替换的执行点：生产用 claude.NewExecRunner()（真实子进程），
// 测试注入假实现喂预置的 stream-json 行或注入失败。做成包级变量而不是
// RunSession 的参数，是为了保持后者对 cmd/assistant 与调度引擎的既有签名。
var (
	SessionRunner   claude.Runner = claude.NewExecRunner()
	sessionRunnerMu sync.RWMutex
)

// sessionRunner 取当前执行点（并发读需要加锁：测试会在跑会话时替换它）。
func sessionRunner() claude.Runner {
	sessionRunnerMu.RLock()
	defer sessionRunnerMu.RUnlock()
	return SessionRunner
}

// setSessionRunner 替换执行点并返回还原函数（仅测试使用）。
func setSessionRunner(runner claude.Runner) func() {
	sessionRunnerMu.Lock()
	previous := SessionRunner
	SessionRunner = runner
	sessionRunnerMu.Unlock()
	return func() {
		sessionRunnerMu.Lock()
		SessionRunner = previous
		sessionRunnerMu.Unlock()
	}
}

// runSessionSpec 把参数面折成一次 CLI 调用的输入（纯函数：不碰进程与文件系统）。
func runSessionSpec(options SessionOptions) claude.Spec {
	config := options.Config
	// 文本记录的固定项目目录名必须在进程环境里（settings.env 无效）；同时把
	// Gitea 身份与配置来源显式钉定给会话（见 sessionCredentialEnv）
	env := claudecfg.SessionEnv(config.SessionDir, config.SessionProject)
	env = append(env, sessionCredentialEnv(config)...)
	return claude.BuildSpec(sessionArgsOptions(options), config.ClaudeBin, options.Cwd, env, config.SessionTimeout)
}

// sessionStream 消费 Runner 交付的 stream-json 行：归档原始行、按需报事件时间线、
// 折叠进归集结果。它对应此前直接挂在子进程 stdout 上的 streamWriter——改为 Runner
// 逐行交付后，切行与「无换行结尾的末行」都由 claude 包负责。
type sessionStream struct {
	outcome    *sessionOutcome
	onProgress func(string)
	sawResult  bool
	// debug 为真时把每行原始 stream 事件也交给 onProgress（排查「会话在干什么」）
	debug bool
	// thinking 聚合 system/thinking_tokens 帧
	thinking thinkingTracker
	// archiveFile 是会话原始 stream-json 行的归档文件（可选；未配置归档时为空）
	archiveFile *os.File
}

func newSessionStream(outcome *sessionOutcome, onProgress func(string), debug bool) *sessionStream {
	return &sessionStream{
		outcome:    outcome,
		onProgress: onProgress,
		debug:      debug,
		thinking:   thinkingTracker{onProgress: onProgress, debug: debug},
	}
}

// archiveTo 打开归档文件（<dir>/<SessionID>.jsonl）并返回其路径；目录或文件不可用
// 时返回空串——归档是附加能力，不能因为盘上出问题就中断会话。
func (s *sessionStream) archiveTo(dir string) string {
	archivePath := filepath.Join(dir, s.outcome.SessionID+".jsonl")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	file, err := os.OpenFile(archivePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return ""
	}
	s.archiveFile = file
	return archivePath
}

// close 关闭归档文件（未开归档时不动）。
func (s *sessionStream) close() {
	if s.archiveFile != nil {
		_ = s.archiveFile.Close()
	}
}

// consume 消费一行输出：归档原文、按需报事件时间线、折叠进归集结果。
func (s *sessionStream) consume(line []byte) {
	text := string(line)
	s.reportDebug(text)
	if s.archiveFile != nil {
		_, _ = s.archiveFile.WriteString(text + "\n")
	}
	if claude.Feed(&s.outcome.Outcome, line, claude.ProgressTerse, s.onProgress) {
		s.sawResult = true
	}
}

// debugProgress 在 --debug 下把细节写进待办日志（前缀 [debug]，便于过滤）。
func debugProgress(onProgress func(string), line string) {
	if onProgress == nil || line == "" {
		return
	}
	onProgress("[debug] " + line)
}

// reportDebug 在 --debug 下报一行事件时间线：只写 type/subtype，不堆原始 JSON
// （base64 与长 diff 会把日志淹掉；原文在会话文本记录与归档里）。例外是
// system/thinking_tokens 帧：思考期间每约 10ms 一帧，逐帧上报会把时间线淹掉，
// 改由 thinkingTracker 累计、段结束时报一条汇总（含 t/s）。
func (s *sessionStream) reportDebug(line string) {
	if !s.debug || s.onProgress == nil {
		return
	}
	event, ok := claude.ParseLine([]byte(line))
	if ok && event.IsSystem("thinking_tokens") {
		s.thinking.observe(event)
		return
	}
	s.thinking.flush()
	s.onProgress("[debug] 事件 " + describeStreamEvent(event, ok, line))
}

// describeStreamEvent 由已解析的事件给出类型标注；非 JSON 行只报长度。
func describeStreamEvent(event claude.Event, ok bool, line string) string {
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
func (t *thinkingTracker) observe(event claude.Event) {
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
