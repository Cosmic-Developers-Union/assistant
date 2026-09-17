// Package runcfg 加载并解析 run.yaml —— `assistant run`（daemon 部署形态）的
// 运行配置：监控哪些站点、哪些仓库、评审工作区落在哪里。
//
// 与 config.json 的分工：run.yaml 完整描述「这次运行」——站点（monitor）、
// 数据落点（root/repos-dir）、评审工作区（review-root 与命名模板）；config.json
// 继续承担账号身份（providers/optimizations/weixin）。run.yaml 存在时 run 完全
// 按它运行，不再读 config.json 的 instances。
//
// YAML 遵循 XDG Base Directory Specification / Windows Known Folders / macOS
// Library Directory conventions：root 缺省指向平台数据目录下的
// Cosmic-Developers-Union/assistant，与受管克隆的既有落点一致，迁移零搬迁。
package runcfg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"assistant/internal/instances"
	"assistant/internal/provider"

	"gopkg.in/yaml.v3"
)

// FileName 是 run 配置文件的标准名字。
const FileName = "run.yaml"

// File 是 run.yaml 的根结构。字段与 run.example.yaml 一一对应。
type File struct {
	// Monitor 是站点 → 监控配置；键是 Gitea 根地址（http(s)://...）。
	Monitor map[string]Monitor `yaml:"monitor"`
	// Root 是本次运行的数据根（受管克隆、状态、日志都落它下面）。支持
	// ${VAR:-default} 环境变量展开与 $root 自引用。
	Root string `yaml:"root"`
	// ReposDir 是受管克隆根目录；缺省 $root/repos。注释口径：
	// $root/repos/{instance-name}/{username-or-org}/{name}
	ReposDir string `yaml:"repos-dir"`
	// ReviewRoot 是评审会话工作区根（PR/Issue 的 worktree 都建在它下面）。
	// 缺省系统临时目录。
	ReviewRoot string `yaml:"review-root"`
	// ReviewNameTemplate 是评审工作区目录命名模板，可用占位符：
	// {instance-name} {username-or-org} {name} {pr|issue} {index}。
	// 缺省 "{instance-name}-{username-or-org}--{name}-{pr|issue}-{index}"。
	ReviewNameTemplate string `yaml:"review-name-template"`
	// Provider 是本次运行的 AI 供应商（type + token 简写）：给出时覆盖
	// config.json 的 default_provider。token 支持 $VAR / ${VAR} 环境变量展开
	// （令牌不落配置文件）。
	Provider *Provider `yaml:"provider"`
	// SessionsDir 是 claude 会话 jsonl 记录的归档根：按 sessions-name-template
	// 展开归档每个会话的原始 stream 记录（内省与回查用）。缺省不归档。
	SessionsDir string `yaml:"sessions-dir"`
	// SessionsNameTemplate 是会话记录归档命名模板，可用占位符：
	// {instance-name} {username-or-org} {name} {session-id}。
	// 缺省 "{instance-name}-{username-or-org}--{name}/{session-id}.jsonl"。
	SessionsNameTemplate string `yaml:"sessions-name-template"`
	// StateDir 是共享状态目录（SQLite WAL）：daemon 内省、跨进程互斥锁的落点。
	StateDir string `yaml:"state-dir"`
	// StateFile 是 SQLite 状态库文件；缺省 $state-dir/state.sqlite3。
	StateFile string `yaml:"state-file"`

	// root 是 Root 展开后的绝对路径（Resolved 使用）。
	root string
	// stateDir 是 StateDir 展开后的绝对路径（$state-dir 自引用用）。
	stateDir string
}

// Provider 是 run.yaml 的 AI 供应商简写：type 选内置预设（如 minimax），
// token 是该供应商的 API key。目前只支持 minimax（国内站点用 minimax-cn）。
type Provider struct {
	// Type 是内置预设名（provider.PresetNames 之一，不区分大小写）。
	Type string `yaml:"type"`
	// Token 是 API key（api_key 简写）：支持 $VAR / ${VAR} 环境变量引用，
	// 缺省回退进程环境变量。
	Token string `yaml:"token"`
}

// Monitor 是一个被监控的 Gitea 站点。
type Monitor struct {
	// Token 是该站点的访问令牌：daemon 检测待办、克隆仓库与会话 MCP 都以它
	// 身份运行（评审以该账号落库）。缺省回退凭据库 purpose=review 的令牌。
	Token string `yaml:"token"`
	// Repos 是要监控的仓库清单（owner/name）。
	Repos []string `yaml:"repos"`
}

// Load 读取并解析 run.yaml：展开变量、填充默认值并校验。
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 run 配置 %s: %w", path, err)
	}
	var file File
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("解析 run 配置 %s: %w", path, err)
	}
	if err := file.resolve(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("run 配置 %s 无效: %w", path, err)
	}
	return &file, nil
}

// resolve 展开变量并填充默认值（幂等）。
func (f *File) resolve(baseDir string) error {
	f.Root = strings.TrimSpace(f.Root)
	f.ReposDir = strings.TrimSpace(f.ReposDir)
	f.ReviewRoot = strings.TrimSpace(f.ReviewRoot)
	f.ReviewNameTemplate = strings.TrimSpace(f.ReviewNameTemplate)
	if f.Root == "" {
		// XDG Base Directory Specification / Windows Known Folders / macOS
		// Library Directory conventions（与 internal/instances 的数据目录同源）
		dataDir, err := defaultDataDir()
		if err != nil {
			return err
		}
		f.root = dataDir
	} else {
		expanded, err := expandEnv(f.Root)
		if err != nil {
			return fmt.Errorf("root: %w", err)
		}
		f.root = absFrom(baseDir, expanded)
	}
	// $root 自引用：其余字段里的 $root 指向已解析的数据根
	if f.ReposDir != "" {
		f.ReposDir = absFrom(baseDir, expandRoot(f.ReposDir, f.root))
	}
	if f.ReviewRoot != "" {
		f.ReviewRoot = absFrom(baseDir, expandRoot(f.ReviewRoot, f.root))
	}
	if f.SessionsDir != "" {
		f.SessionsDir = absFrom(baseDir, expandRoot(f.SessionsDir, f.root))
	}
	// $state-dir 自引用（state-file 常写作 $state-dir/state.sqlite3）
	if f.StateDir != "" {
		expanded, err := expandEnv(f.StateDir)
		if err != nil {
			return fmt.Errorf("state-dir: %w", err)
		}
		f.stateDir = absFrom(baseDir, expandRoot(expanded, f.root))
	}
	if f.StateFile != "" {
		expanded, err := expandEnv(f.StateFile)
		if err != nil {
			return fmt.Errorf("state-file: %w", err)
		}
		f.StateFile = absFrom(baseDir,
			strings.ReplaceAll(strings.ReplaceAll(expanded, "$state-dir", f.stateDir), "${state-dir}", f.stateDir))
	}
	for host, monitor := range f.Monitor {
		monitor.Token = strings.TrimSpace(monitor.Token)
		repos := make([]string, 0, len(monitor.Repos))
		for _, repo := range monitor.Repos {
			repo = strings.TrimSpace(repo)
			if repo != "" {
				repos = append(repos, repo)
			}
		}
		monitor.Repos = repos
		f.Monitor[host] = monitor
	}
	if f.Provider != nil {
		f.Provider.Type = strings.TrimSpace(f.Provider.Type)
		if f.Provider.Type == "" {
			return fmt.Errorf("provider.type 不能为空（目前只支持 minimax；国内站点用 minimax-cn）")
		}
		// token 支持 $VAR / ${VAR} 引用（YAML 单引号字符串不做插值，这里显式展开；
		// 未定义变量展开为空，由预设与进程环境兜底）。先校验 ${…} 引用闭合再展开，
		// 避免裸展开吞掉未闭合报错
		if err := checkBracedRefs(f.Provider.Token); err != nil {
			return fmt.Errorf("provider.token: %w", err)
		}
		f.Provider.Token = strings.TrimSpace(expandBare(f.Provider.Token))
	}
	return f.Validate()
}

// Validate 校验展开后的配置。
func (f *File) Validate() error {
	if len(f.Monitor) == 0 {
		return fmt.Errorf("monitor 不能为空：至少配置一个站点")
	}
	seen := make(map[string]bool, len(f.Monitor))
	for host, monitor := range f.Monitor {
		normalized := strings.TrimRight(strings.TrimSpace(host), "/")
		if normalized != host {
			if _, duplicate := seen[normalized]; duplicate {
				return fmt.Errorf("monitor 含重复站点（规范化后）：%s", normalized)
			}
			continue
		}
		if err := instances.ValidateHost(host); err != nil {
			return fmt.Errorf("monitor[%s]: %w", host, err)
		}
		if seen[host] {
			return fmt.Errorf("monitor 含重复站点：%s", host)
		}
		seen[host] = true
		for _, repo := range monitor.Repos {
			if _, _, err := instances.ParseRepoName(repo); err != nil {
				return fmt.Errorf("monitor[%s].repos: %w", host, err)
			}
		}
	}
	if f.root == "" {
		return fmt.Errorf("root 不能为空")
	}
	if f.Provider != nil && f.Provider.Type != "" && !provider.HasPreset(f.Provider.Type) {
		return fmt.Errorf("provider.type %q 不是内置预设（可用：%s）", f.Provider.Type, strings.Join(provider.PresetNames(), "、"))
	}
	return nil
}

// DataRoot 返回解析后的数据根（绝对路径）。
func (f *File) DataRoot() string { return f.root }

// ReposRoot 返回受管克隆根目录：缺省 $root/repos。
func (f *File) ReposRoot() string {
	if f.ReposDir != "" {
		return f.ReposDir
	}
	return filepath.Join(f.root, "repos")
}

// ReviewRootDir 返回评审工作区根：缺省系统临时目录下 agent-dispatcher。
func (f *File) ReviewRootDir() string {
	if f.ReviewRoot != "" {
		return f.ReviewRoot
	}
	return os.TempDir()
}

// ReviewName 返回评审工作区目录名：按命名模板展开占位符。
func (f *File) ReviewName(instanceHost, fullName, kind string, index int64) (string, error) {
	return f.reviewName(instanceHost, fullName, kind, index, "")
}

// ReviewDirBase 返回评审工作区的仓库级目录基名：命名模板去掉 {pr|issue} 与
// {index} 占位符（dispatcher 在 WorktreeRoot 下自行追加 pr-N / issue-N 叶子），
// 其余占位符照常展开。模板无目录分隔符时返回整段基名（带分隔符时保留层级）。
func (f *File) ReviewDirBase(instanceHost, fullName string) (string, error) {
	template := f.ReviewNameTemplate
	if template == "" {
		template = DefaultReviewNameTemplate
	}
	// 去掉逐待办叶子（{pr|issue} 与 {index}，连同紧邻的分隔符）
	base := template
	for _, placeholder := range []string{"{pr|issue}", "${pr|issue}", "{index}", "${index}"} {
		base = strings.ReplaceAll(base, placeholder, "")
	}
	base = strings.Trim(base, "-_/ ")
	if base == "" {
		base = "{instance-name}/{username-or-org}/{name}"
	}
	return f.reviewName(instanceHost, fullName, "", 0, base)
}

// reviewName 按给定模板展开占位符；template 为空时用配置值（或默认模板）。
func (f *File) reviewName(instanceHost, fullName, kind string, index int64, template string) (string, error) {
	if template == "" {
		template = f.ReviewNameTemplate
		if template == "" {
			template = DefaultReviewNameTemplate
		}
	}
	owner, name, err := instances.ParseRepoName(fullName)
	if err != nil {
		return "", err
	}
	slug, err := instances.HostSlug(instanceHost)
	if err != nil {
		return "", err
	}
	kindLabel := kind
	if kind != "pr" && kind != "issue" {
		kindLabel = "item"
	}
	// 占位符支持 {name} 与 ${name} 两种写法（示例配置用了后者，避免 YAML 引号
	// 之外的歧义）
	pairs := make([]string, 0, 10)
	for _, placeholder := range []struct{ key, value string }{
		{"{instance-name}", slug},
		{"{username-or-org}", owner},
		{"{name}", name},
		{"{pr|issue}", kindLabel},
		{"{index}", fmt.Sprintf("%d", index)},
	} {
		pairs = append(pairs, placeholder.key, placeholder.value,
			"${"+placeholder.key[1:], placeholder.value)
	}
	return strings.NewReplacer(pairs...).Replace(template), nil
}

// DefaultReviewNameTemplate 是 review-name-template 的缺省值（与
// run.example.yaml 示例一致）。
const DefaultReviewNameTemplate = "{instance-name}-{username-or-org}--{name}-{pr|issue}-{index}"

// DefaultSessionsNameTemplate 是 sessions-name-template 的缺省值。
const DefaultSessionsNameTemplate = "{instance-name}-{username-or-org}--{name}/{session-id}.jsonl"

// SessionArchivePath 返回一个会话记录（stream-json 原始行）的归档路径：
// sessions-dir + sessions-name-template 展开占位符。未配置 sessions-dir 时
// 返回空串（不归档）。
func (f *File) SessionArchivePath(instanceHost, fullName, sessionID string) (string, error) {
	if f.SessionsDir == "" {
		return "", nil
	}
	owner, name, err := instances.ParseRepoName(fullName)
	if err != nil {
		return "", err
	}
	slug, err := instances.HostSlug(instanceHost)
	if err != nil {
		return "", err
	}
	template := f.SessionsNameTemplate
	if template == "" {
		template = DefaultSessionsNameTemplate
	}
	pairs := make([]string, 0, 8)
	for _, placeholder := range []struct{ key, value string }{
		{"{instance-name}", slug},
		{"{username-or-org}", owner},
		{"{name}", name},
		{"{session-id}", sessionID},
	} {
		pairs = append(pairs, placeholder.key, placeholder.value,
			"${"+placeholder.key[1:], placeholder.value)
	}
	relative := strings.NewReplacer(pairs...).Replace(template)
	if strings.Contains(relative, "..") {
		return "", fmt.Errorf("sessions-name-template 展开含 ..：%q", relative)
	}
	return filepath.Join(f.SessionsDir, relative), nil
}

// SessionArchiveDir 返回本仓库的会话记录归档目录（模板去掉文件名部分）。
func (f *File) SessionArchiveDir(instanceHost, fullName string) (string, error) {
	path, err := f.SessionArchivePath(instanceHost, fullName, "{session-id}")
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

// StatePath 返回 SQLite 状态库路径：显式 state-file 优先；只配了 state-dir 时
// 用 <state-dir>/state.sqlite3；都没配返回空串（状态库未启用）。
func (f *File) StatePath() string {
	if f.StateFile != "" {
		return f.StateFile
	}
	if f.stateDir != "" {
		return filepath.Join(f.stateDir, "state.sqlite3")
	}
	return ""
}

// TargetPath 返回受管克隆（或显式指定仓库目录）在本次运行下的落点：
// <repos-root>/{host-slug}/{owner}/{name}。与既有数据目录布局一致，迁移零搬迁。
func (f *File) TargetPath(instanceHost, fullName string) (string, error) {
	slug, err := instances.HostSlug(instanceHost)
	if err != nil {
		return "", err
	}
	owner, name, err := instances.ParseRepoName(fullName)
	if err != nil {
		return "", err
	}
	return filepath.Join(f.ReposRoot(), slug, owner, name), nil
}

// RepoStatePath 返回仓库状态目录（日志、单飞锁）的落点：
// <root>/state/{host-slug}/{owner}/{name}。
func (f *File) RepoStatePath(instanceHost, fullName string) (string, error) {
	slug, err := instances.HostSlug(instanceHost)
	if err != nil {
		return "", err
	}
	owner, name, err := instances.ParseRepoName(fullName)
	if err != nil {
		return "", err
	}
	return filepath.Join(f.root, "state", slug, owner, name), nil
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

// absFrom 把相对路径锚定到 run.yaml 所在目录；绝对路径原样返回。
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

// defaultDataDir 返回平台数据目录下的应用数据根（XDG / Known Folders / Library）。
func defaultDataDir() (string, error) {
	base := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("定位用户目录: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "Cosmic-Developers-Union", "assistant"), nil
}

// DefaultPath 返回 run.yaml 的标准落点：<配置目录>/run.yaml（与 config.json
// 同目录，随容器挂载一起迁移）。
func DefaultPath() (string, error) {
	directory, err := instances.DefaultConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, FileName), nil
}

// ResolvePath 按显式参数 → ASSISTANT_RUN → config.json 同目录 → 标准配置目录
// 的顺序定位 run.yaml；都不存在返回空串（调用方回退 config.json 语义）。
// configPath 是 config.json 的路径（可为空）：run.yaml 与它同目录共存，随配置
// 目录一起挂载/备份。
func ResolvePath(explicit, configPath string) (string, error) {
	if path := strings.TrimSpace(explicit); path != "" {
		return path, nil
	}
	if path := strings.TrimSpace(os.Getenv("ASSISTANT_RUN")); path != "" {
		return path, nil
	}
	candidates := []string{}
	if directory := strings.TrimSpace(filepath.Dir(configPath)); configPath != "" && directory != "" && directory != "." {
		candidates = append(candidates, filepath.Join(directory, FileName))
	}
	if standard, err := DefaultPath(); err == nil {
		candidates = append(candidates, standard)
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	return "", nil
}
