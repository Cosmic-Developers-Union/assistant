// 会话封装：每待办一个 headless claude 子进程（`claude -p` + stream-json）。
//
// 命令面与手工运维一致：`--permission-mode auto`（模型分类器自动批准/拒绝，
// result 的 permission_denials 落日志可观测质量）、`--autocompact auto`（长
// 评审会话自动压缩上下文）。与手工命令的差异只有三处，均为无人值守必需：
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
)

// SessionOutcome 是一次 claude 会话的归集结果。
type SessionOutcome struct {
	// Subtype 是 success 或 error_max_turns / error_during_execution 等
	Subtype           string   `json:"subtype"`
	IsError           bool     `json:"isError"`
	NumTurns          int      `json:"numTurns"`
	CostUSD           float64  `json:"costUsd"`
	DurationMS        int64    `json:"durationMs"`
	SessionID         string   `json:"sessionId"`
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
			"评审协议见 .claude/skills/review/SKILL.md，按其 PR 审查协议执行。仅处理该 PR，不要处理其他待办。\n"+
			"最终以 Gitea 原生 Pull Request Review 提交结论（APPROVED / REQUEST_CHANGES / COMMENT），"+
			"结论正文注明所评审的 head%s。", number, subject, headLine, headNote)
	}
	return fmt.Sprintf("triage issue #%d\n\n对象：%s\n"+
		"分诊规则见 .claude/skills/review/SKILL.md，按其 Issue 分诊规则执行。仅处理该 Issue，不要处理其他待办。\n"+
		"结论按 AGENTS.md 的标签规则落标签并评论。", number, subject)
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
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
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
						onProgress("🔧 " + block.Name)
					}
				}
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
	// MCPConfigPath 是宿主仓库 .mcp.json 的路径：工具面由 dispatcher 宿主控制，
	// 与 PR head 解耦
	MCPConfigPath string
	OnProgress    func(string)
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
		"--max-turns",
		strconv.Itoa(MaxTurns),
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
	if options.MCPConfigPath != "" {
		mcpDir := filepath.Dir(options.MCPConfigPath)
		args = append(args, "-v", mcpDir+":"+mcpDir)
	}
	if config.DockerNetwork != "" {
		args = append(args, "--network", config.DockerNetwork)
	}
	for _, key := range dockerPassthroughEnv() {
		args = append(args, "-e", key)
	}
	args = append(args, config.DockerImage, config.ClaudeBin)
	args = append(args, claudeArgs...)
	return "docker", args, container
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

	bin, args, container := sessionCommand(options)
	command := exec.Command(bin, args...)
	command.Dir = options.Cwd
	stream := &streamWriter{outcome: &outcome, onProgress: options.OnProgress}
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
		_ = command.Process.Signal(syscall.SIGTERM)
		time.AfterFunc(killGrace, func() { _ = command.Process.Kill() })
	})
	waitErr := command.Wait()
	timer.Stop()
	// Wait 已等待 stdio 拷贝完成，冲刷无换行结尾的残余行
	stream.flush()

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
			} else if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				how = fmt.Sprintf("被信号 %s 终止", signalName(status.Signal()))
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
	if FeedStreamLine(w.outcome, string(w.buffer), w.onProgress) {
		w.sawResult = true
	}
	w.buffer = nil
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
