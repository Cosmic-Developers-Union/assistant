package instances

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Runtime 是一个可运行单元：从 agents 池选一个 main agent + 若干 subagents，
// 集合若干通道，并携带本次运行的全部落点与参数（原 run.yaml 并入）。每个
// runtime 是一棵独立的 $root 树（受管克隆、状态、评审工作区、对话状态），
// 互不串扰；`assistant run --runtime <名>` 按它装配 daemon。
//
// 路径字段支持 ${VAR:-default} 环境变量展开与 $root/$state-dir 自引用，相对
// 路径锚定到 config.json 所在目录（随配置目录一起挂载/备份）。
type Runtime struct {
	// MainAgent 是主 agent 名（agents 池或内置预设）：本运行时唯一接待所有
	// 对话通道的 agent。缺省 "main"（内置通用主 agent）。
	MainAgent string `json:"main_agent,omitempty"`
	// Subagents 是子代理名清单：以 claude 自定义 agent 注入会话，主模型经
	// 原生 Task 工具按 description 委派。缺省 = 全部内置子代理。
	Subagents []string `json:"subagents,omitempty"`
	// Channels 引用本运行时启用的通道（键 = type 或 type/name，即 Channel.Key）。
	Channels []string `json:"channels,omitempty"`
	// Provider 是本运行时的缺省 provider 名（agent 未指定 provider 时的回退，
	// 优先级高于全局 default_provider）。
	Provider string `json:"provider,omitempty"`

	// —— 运行落点（缺省值见各 accessor）——
	// Root 是数据根：受管克隆、状态、日志、评审工作区、对话状态都落它下面。
	Root string `json:"root,omitempty"`
	// ReposDir 是受管克隆根目录；缺省 $root/repos。
	ReposDir string `json:"repos_dir,omitempty"`
	// ReviewRoot 是评审会话工作区根（PR/Issue 的 worktree 建在它下面）；
	// 缺省 $root/review。
	ReviewRoot string `json:"review_root,omitempty"`
	// ReviewNameTemplate 是评审工作区命名模板，占位符：{instance-name}
	// {username-or-org} {name} {pr|issue} {index}。
	ReviewNameTemplate string `json:"review_name_template,omitempty"`
	// SessionsDir 是 claude 会话 jsonl 记录的归档根；留空不归档。
	SessionsDir string `json:"sessions_dir,omitempty"`
	// SessionsNameTemplate 是会话记录归档命名模板，占位符：{instance-name}
	// {username-or-org} {name} {session-id}。
	SessionsNameTemplate string `json:"sessions_name_template,omitempty"`
	// StateDir 是共享状态目录（SQLite WAL：daemon 内省 + 跨进程互斥锁）；
	// 缺省 $root/state。
	StateDir string `json:"state_dir,omitempty"`
	// StateFile 是 SQLite 状态库文件；缺省 $state-dir/state.sqlite3。写 "off"
	// 显式关闭状态库。
	StateFile string `json:"state_file,omitempty"`
	// ChatDir 是对话状态目录（conversations.json、sessions.json）；缺省
	// $root/chat。
	ChatDir string `json:"chat_dir,omitempty"`
	// ClaudeDir 是 Claude Code 配置根（会话文本记录与会话状态）；$CLAUDE_CONFIG_DIR
	// 优先，缺省 $root/claude。
	ClaudeDir string `json:"claude_dir,omitempty"`

	// —— 运行参数 ——
	// APIListen 是状态 API 监听地址；写 "off" 关闭状态 API。
	APIListen string `json:"api_listen,omitempty"`
	// IntervalMS 是调度引擎的轮询间隔（毫秒；缺省 30000）。
	IntervalMS int64 `json:"interval_ms,omitzero"`
	// SessionTimeoutMS 是调度会话（review/triage）的 claude 超时（毫秒；
	// 缺省 1800000 = 30 分钟）。对话会话的超时在各 agent 上单独配。
	SessionTimeoutMS int64 `json:"session_timeout_ms,omitzero"`
	// Concurrency 是调度引擎的并发会话数（缺省 8）。
	Concurrency int `json:"concurrency,omitzero"`

	// root 是 Root 展开后的绝对路径（resolve 填充）。
	root string
	// stateDir 是 StateDir 展开后的绝对路径（$state-dir 自引用用）。
	stateDir string
}

// 运行时的缺省参数。
const (
	// DefaultRuntimeName 是合成/约定俗成的缺省 runtime 名。
	DefaultRuntimeName = "main"
	// DefaultMainAgent 是 main_agent 的缺省名（内置通用主 agent）。
	DefaultMainAgent = "main"
	// DefaultAPIListen 是状态 API 的缺省监听地址。
	DefaultAPIListen = "127.0.0.1:8770"
	// DefaultIntervalMS 是调度轮询间隔缺省值（30 秒）。
	DefaultIntervalMS = int64(30000)
	// DefaultSessionTimeoutMS 是调度会话超时缺省值（30 分钟）。
	DefaultSessionTimeoutMS = int64(1800000)
	// DefaultConcurrency 是调度并发缺省值。
	DefaultConcurrency = 8
	// OffValue 是布尔语义字段的关闭写法（state_file/api_listen = "off"）。
	OffValue = "off"
)

// DefaultReviewNameTemplate 是 review_name_template 的缺省值。
const DefaultReviewNameTemplate = "{instance-name}-{username-or-org}--{name}-{pr|issue}-{index}"

// DefaultSessionsNameTemplate 是 sessions_name_template 的缺省值。
const DefaultSessionsNameTemplate = "{instance-name}-{username-or-org}--{name}/{session-id}.jsonl"

// resolve 展开变量并锚定相对路径（幂等；Parse 在 Normalize 后调用）。
func (r *Runtime) resolve(baseDir string) error {
	r.MainAgent = strings.TrimSpace(r.MainAgent)
	r.Provider = strings.TrimSpace(r.Provider)
	r.Subagents = trimNonEmpty(r.Subagents)
	r.Channels = trimNonEmpty(r.Channels)
	r.ReviewNameTemplate = strings.TrimSpace(r.ReviewNameTemplate)
	r.SessionsNameTemplate = strings.TrimSpace(r.SessionsNameTemplate)
	r.APIListen = strings.TrimSpace(r.APIListen)
	if r.Root == "" {
		dataDir, err := defaultDataDir()
		if err != nil {
			return err
		}
		r.root = dataDir
	} else {
		expanded, err := expandEnv(r.Root)
		if err != nil {
			return fmt.Errorf("root: %w", err)
		}
		r.root = absFrom(baseDir, expanded)
	}
	// $root 自引用：其余路径字段里的 $root 指向已解析的数据根
	resolveUnderRoot := func(name string, target *string) error {
		if *target == "" {
			return nil
		}
		expanded, err := expandEnv(*target)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		*target = absFrom(baseDir, expandRoot(expanded, r.root))
		return nil
	}
	for _, field := range []struct {
		name   string
		target *string
	}{
		{"repos_dir", &r.ReposDir}, {"review_root", &r.ReviewRoot},
		{"sessions_dir", &r.SessionsDir}, {"chat_dir", &r.ChatDir}, {"claude_dir", &r.ClaudeDir},
	} {
		if err := resolveUnderRoot(field.name, field.target); err != nil {
			return err
		}
	}
	// $state-dir 自引用（state_file 常写作 $state-dir/state.sqlite3）；"off"
	// 是关闭哨兵，不当路径锚定
	if r.StateDir != "" {
		expanded, err := expandEnv(r.StateDir)
		if err != nil {
			return fmt.Errorf("state_dir: %w", err)
		}
		r.stateDir = absFrom(baseDir, expandRoot(expanded, r.root))
	}
	if r.StateFile != "" && !strings.EqualFold(strings.TrimSpace(r.StateFile), OffValue) {
		expanded, err := expandEnv(r.StateFile)
		if err != nil {
			return fmt.Errorf("state_file: %w", err)
		}
		r.StateFile = absFrom(baseDir,
			strings.ReplaceAll(strings.ReplaceAll(expanded, "$state-dir", r.stateDir), "${state-dir}", r.stateDir))
	}
	return r.validate()
}

// validate 校验运行时定义的结构（数值边界；路径解析在 resolve）。
func (r Runtime) validate() error {
	if r.IntervalMS < 0 {
		return fmt.Errorf("interval_ms 不能为负：%d", r.IntervalMS)
	}
	if r.SessionTimeoutMS < 0 {
		return fmt.Errorf("session_timeout_ms 不能为负：%d", r.SessionTimeoutMS)
	}
	if r.Concurrency < 0 {
		return fmt.Errorf("concurrency 不能为负：%d", r.Concurrency)
	}
	return nil
}

// DataRoot 返回解析后的数据根（绝对路径）。
func (r Runtime) DataRoot() string {
	if r.root != "" {
		return r.root
	}
	if root, err := defaultDataDir(); err == nil {
		return root
	}
	return r.Root
}

// ReposRoot 返回受管克隆根目录：缺省 $root/repos。
func (r Runtime) ReposRoot() string {
	if r.ReposDir != "" {
		return r.ReposDir
	}
	return filepath.Join(r.DataRoot(), "repos")
}

// ReviewRootDir 返回评审工作区根：缺省 $root/review。
func (r Runtime) ReviewRootDir() string {
	if r.ReviewRoot != "" {
		return r.ReviewRoot
	}
	return filepath.Join(r.DataRoot(), "review")
}

// ChatStateDir 返回对话状态目录：缺省 $root/chat。
func (r Runtime) ChatStateDir() string {
	if r.ChatDir != "" {
		return r.ChatDir
	}
	return filepath.Join(r.DataRoot(), "chat")
}

// ClaudeConfigDir 返回 Claude Code 配置根：显式 claude_dir > $CLAUDE_CONFIG_DIR >
// $root/claude。不读也不写用户的 ~/.claude。
func (r Runtime) ClaudeConfigDir() string {
	if r.ClaudeDir != "" {
		return r.ClaudeDir
	}
	if value := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); value != "" {
		return value
	}
	return filepath.Join(r.DataRoot(), "claude")
}

// StatePath 返回 SQLite 状态库路径：state_file = "off" 返回空串（状态库关闭）；
// 显式 state_file 优先；缺省 <state-dir|  $root/state>/state.sqlite3（默认开启）。
func (r Runtime) StatePath() string {
	if strings.EqualFold(strings.TrimSpace(r.StateFile), OffValue) {
		return ""
	}
	if r.StateFile != "" {
		return r.StateFile
	}
	stateDir := r.stateDir
	if stateDir == "" {
		stateDir = filepath.Join(r.DataRoot(), "state")
	}
	return filepath.Join(stateDir, "state.sqlite3")
}

// ListenAddr 返回状态 API 监听地址：api_listen = "off" 返回空串（关闭）；
// 缺省 DefaultAPIListen。
func (r Runtime) ListenAddr() string {
	if strings.EqualFold(strings.TrimSpace(r.APIListen), OffValue) {
		return ""
	}
	if r.APIListen != "" {
		return r.APIListen
	}
	return DefaultAPIListen
}

// Interval 缺省间隔（毫秒）。
func (r Runtime) Interval() int64 {
	if r.IntervalMS > 0 {
		return r.IntervalMS
	}
	return DefaultIntervalMS
}

// Timeout 缺省调度会话超时（毫秒）。
func (r Runtime) Timeout() int64 {
	if r.SessionTimeoutMS > 0 {
		return r.SessionTimeoutMS
	}
	return DefaultSessionTimeoutMS
}

// Workers 缺省调度并发数。
func (r Runtime) Workers() int {
	if r.Concurrency > 0 {
		return r.Concurrency
	}
	return DefaultConcurrency
}

// ReviewName 返回评审工作区目录名：按命名模板展开占位符。
func (r Runtime) ReviewName(instanceHost, fullName, kind string, index int64) (string, error) {
	return expandTemplateName(r.reviewTemplate(), instanceHost, fullName, kind, index)
}

// ReviewDirBase 返回评审工作区的仓库级目录基名：命名模板去掉 {pr|issue} 与
// {index} 占位符（dispatcher 在其下自行追加 pr-N / issue-N 叶子）。
func (r Runtime) ReviewDirBase(instanceHost, fullName string) (string, error) {
	template := r.reviewTemplate()
	for _, placeholder := range []string{"{pr|issue}", "${pr|issue}", "{index}", "${index}"} {
		template = strings.ReplaceAll(template, placeholder, "")
	}
	template = strings.Trim(template, "-_/ ")
	if template == "" {
		template = "{instance-name}/{username-or-org}/{name}"
	}
	return expandTemplateName(template, instanceHost, fullName, "", 0)
}

func (r Runtime) reviewTemplate() string {
	if r.ReviewNameTemplate != "" {
		return r.ReviewNameTemplate
	}
	return DefaultReviewNameTemplate
}

// SessionArchivePath 返回一个会话记录（stream-json 原始行）的归档路径；未配置
// sessions_dir 时返回空串（不归档）。
func (r Runtime) SessionArchivePath(instanceHost, fullName, sessionID string) (string, error) {
	if r.SessionsDir == "" {
		return "", nil
	}
	owner, name, err := ParseRepoName(fullName)
	if err != nil {
		return "", err
	}
	slug, err := HostSlug(instanceHost)
	if err != nil {
		return "", err
	}
	template := r.SessionsNameTemplate
	if template == "" {
		template = DefaultSessionsNameTemplate
	}
	relative := strings.NewReplacer(
		"{instance-name}", slug, "${instance-name}", slug,
		"{username-or-org}", owner, "${username-or-org}", owner,
		"{name}", name, "${name}", name,
		"{session-id}", sessionID, "${session-id}", sessionID,
	).Replace(template)
	if strings.Contains(relative, "..") {
		return "", fmt.Errorf("sessions_name_template 展开含 ..：%q", relative)
	}
	return filepath.Join(r.SessionsDir, relative), nil
}

// SessionArchiveDir 返回本仓库的会话记录归档目录（模板去掉文件名部分）。
func (r Runtime) SessionArchiveDir(instanceHost, fullName string) (string, error) {
	path, err := r.SessionArchivePath(instanceHost, fullName, "{session-id}")
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

// TargetPath 返回受管克隆在本次运行下的落点：<repos-root>/{host-slug}/{owner}/{name}。
func (r Runtime) TargetPath(instanceHost, fullName string) (string, error) {
	slug, err := HostSlug(instanceHost)
	if err != nil {
		return "", err
	}
	owner, name, err := ParseRepoName(fullName)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.ReposRoot(), slug, owner, name), nil
}

// RepoStatePath 返回仓库状态目录（日志、单飞锁）的落点：
// $root/state/{host-slug}/{owner}/{name}。
func (r Runtime) RepoStatePath(instanceHost, fullName string) (string, error) {
	slug, err := HostSlug(instanceHost)
	if err != nil {
		return "", err
	}
	owner, name, err := ParseRepoName(fullName)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.DataRoot(), "state", slug, owner, name), nil
}

// expandTemplateName 按模板展开评审/归档占位符（支持 {name} 与 ${name} 写法）。
func expandTemplateName(template, instanceHost, fullName, kind string, index int64) (string, error) {
	owner, name, err := ParseRepoName(fullName)
	if err != nil {
		return "", err
	}
	slug, err := HostSlug(instanceHost)
	if err != nil {
		return "", err
	}
	kindLabel := kind
	if kind != "pr" && kind != "issue" {
		kindLabel = "item"
	}
	return strings.NewReplacer(
		"{instance-name}", slug, "${instance-name}", slug,
		"{username-or-org}", owner, "${username-or-org}", owner,
		"{name}", name, "${name}", name,
		"{pr|issue}", kindLabel, "${pr|issue}", kindLabel,
		"{index}", fmt.Sprintf("%d", index), "${index}", fmt.Sprintf("%d", index),
	).Replace(template), nil
}

// expandEnv 展开 ${VAR} 与 ${VAR:-default} 形式的环境变量引用；未闭合的引用
// 报错（静默留原样最难排查）。
func expandEnv(text string) (string, error) {
	var builder strings.Builder
	for {
		start := strings.Index(text, "${")
		if start < 0 {
			builder.WriteString(text)
			return builder.String(), nil
		}
		end := strings.Index(text[start:], "}")
		if end < 0 {
			return "", fmt.Errorf("未闭合的 ${ 引用：%q", text[start:])
		}
		end += start
		builder.WriteString(text[:start])
		expression := text[start+2 : end]
		name, defaultValue, hasDefault := strings.Cut(expression, ":-")
		name = strings.TrimSpace(name)
		if name == "" {
			return "", fmt.Errorf("空的变量名：%q", text[start:end+1])
		}
		value, ok := os.LookupEnv(name)
		if (!ok || value == "") && hasDefault {
			// 缺省段里允许再引用变量（例：${XDG_DATA_HOME:-$HOME/.local/share}）
			value = expandBare(defaultValue)
		}
		builder.WriteString(value)
		text = text[end+1:]
	}
}

// expandRoot 把字符串里的 $root 替换为已解析的数据根。
func expandRoot(text, root string) string {
	return strings.ReplaceAll(text, "$root", root)
}

// expandBare 展开 $VAR 形式（不带花括号）的变量引用；未定义展开为空串。
func expandBare(text string) string {
	return os.Expand(text, func(name string) string { return os.Getenv(name) })
}

// checkBracedRefs 校验 ${…} 引用闭合（expandBare 不报错，未闭合引用会被它
// 静默消费，这里先行拦截）。
func checkBracedRefs(text string) error {
	for {
		start := strings.Index(text, "${")
		if start < 0 {
			return nil
		}
		end := strings.Index(text[start:], "}")
		if end < 0 {
			return fmt.Errorf("未闭合的 ${ 引用：%q", text[start:])
		}
		text = text[start+end+1:]
	}
}

// ExpandSecret 对支持凭据引用的字段做 ${VAR}/$VAR 展开：gitea 通道 token 等
// 新字段允许引用环境变量（令牌不落配置文件）；先校验闭合再展开。
func ExpandSecret(text string) (string, error) {
	text = strings.TrimSpace(text)
	if !strings.Contains(text, "${") {
		return text, nil
	}
	if err := checkBracedRefs(text); err != nil {
		return "", err
	}
	return expandBare(text), nil
}

// absFrom 把相对路径锚定到 config.json 所在目录；绝对路径原样返回。
func absFrom(baseDir, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(baseDir, path))
}

// defaultDataDir 返回平台数据目录下的应用数据根（XDG / Windows Known Folders /
// macOS Library），与受管克隆既有落点一致。
func defaultDataDir() (string, error) {
	base := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("定位用户目录: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, configNamespace, configApp), nil
}

// trimNonEmpty 去除字符串清单各项空白并丢弃空项。
func trimNonEmpty(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			kept = append(kept, value)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}
