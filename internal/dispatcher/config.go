// Package dispatcher 是评审会话调度引擎：长驻 loop 检测 Gitea 待办，为每个
// 待办自动拉起一个 headless claude 评审会话，直到 review 完成。
//
// 配置装配优先级：命令行参数 > 环境变量 > git remote 自动检测 / 默认值。
// 环境变量与 gitea-assistant 同名约定（GITEA_HOST / GITEA_ACCESS_TOKEN /
// GITEA_REPOSITORY），dispatcher 与评审会话共享同一套凭据。
package dispatcher

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"assistant/internal/claudecfg"
	"assistant/internal/instances"
	"assistant/internal/status"
)

// DefaultConcurrency 是每轮待办处理的会话并发数缺省值（--concurrency /
// DISPATCH_CONCURRENCY 可覆盖）。
const DefaultConcurrency = 8

// Config 是调度引擎的完整运行配置。
type Config struct {
	// Host 是 Gitea 站点根地址（如 http://gitea.example.com:3000），不带尾部斜杠
	Host string
	// AccessToken 决定评审身份：会话提交的 review 以该令牌账号落库
	AccessToken string
	// ConfigPath 是本次运行使用的 config.json 路径（环境变量单实例模式为空）：
	// 评审会话以 ASSISTANT_CONFIG 继承，使会话内 `assistant mcp` 解析同一份配置。
	ConfigPath string
	Repository status.Repository
	// Interval 是空闲轮询间隔
	Interval time.Duration
	// SessionTimeout 是单个评审会话的超时，超时杀会话
	SessionTimeout time.Duration
	// LogDir 是会话结构化日志归档目录
	LogDir string
	// WorktreeRoot 是 PR 评审会话 worktree 根目录（每个 PR 一个子目录）
	WorktreeRoot string
	// LockFile 是单飞锁文件路径
	LockFile string
	// BaseBranch 是镜像同步的基线分支（SyncMirror 开启时强制对齐 origin/<BaseBranch>）
	BaseBranch string
	// SyncMirror 为真时每轮检测前把宿主检出强制对齐 origin 基线
	SyncMirror bool
	// Concurrency 是每轮待办处理的会话并发数（轮间 barrier 保证同一待办同时至多一个会话）
	Concurrency int
	// Model 是评审会话使用的模型；留空用账号默认
	Model string
	// Reviewer 是完成判定匹配的 reviewer 账号，默认 ai
	Reviewer string
	// ClaudeBin 是 claude 可执行文件（PATH 名或绝对路径）
	ClaudeBin string
	// Debug 为真时把会话命令行、claude 原始 stream 事件与 stderr 尾部写进待办日志
	Debug bool
	// Provider 是生效的供应商运行时覆盖（env/settings/mcp 原样透传；零值表示
	// 内置缺省）。只作用于会话临时配置，不写入仓库文件。已经叠加全局优化点。
	Provider claudecfg.Overrides
	// ProviderName 是生效的 provider 名（空串表示内置缺省；仅日志/状态展示）
	ProviderName string
	// SessionDir 是 Claude Code 配置根（缺省 $CLAUDE_CONFIG_DIR 或 ~/.claude）：
	// 文本记录落 <SessionDir>/projects/<SessionProject>/<session-id>.jsonl
	SessionDir string
	// SessionArchivePath 是本仓库会话记录（stream-json 原始行）的归档目录
	// （run.yaml 的 sessions-dir + 命名模板展开；空串表示不归档）。归档按会话
	// ID 命名，daemon 重启不丢、外部工具可直接内省。
	SessionArchiveDir string
	// SessionProject 是本仓库稳定的会话项目目录名（缺省由站点+仓库派生）：
	// worktree 在 /tmp 用完即删，会话记录的位置不受影响
	SessionProject string
	// Optimizations 是全局优化点层（Provider 之下的那一层；仅日志/状态展示，
	// 实际生效值见 Provider）
	Optimizations claudecfg.Overrides
	// DockerImage 非空时评审会话跑在 Docker 容器里（worktree 与 MCP 配置按
	// 相同绝对路径挂载；认证环境变量按白名单透传）
	DockerImage string
	// DockerNetwork 是 docker run 的 --network（如 host / 自定义网络）
	DockerNetwork string
}

// Flags 是命令行参数（未给出的项回退环境变量/默认值/自动检测）。SyncMirror
// 用指针区分「显式 false」与「未给出」，前者压过环境变量。
type Flags struct {
	Host          string
	Repository    string
	AccessToken   string
	// SessionDir 覆盖会话配置根（Claude Code 配置根）：runtime 装配时传
	// runtime.claude_dir 的解析值，使评审会话与对话会话共享同一配置根
	SessionDir    string
	Interval      string
	Timeout       string
	Model         string
	Reviewer      string
	ClaudeBin     string
	Debug         bool
	LogDir        string
	WorktreeRoot  string
	LockFile      string
	BaseBranch    string
	SyncMirror    *bool
	Concurrency   string
	DockerImage   string
	DockerNetwork string
}

var durationPattern = regexp.MustCompile(`^(\d+)(ms|s|m|h|d)?$`)

// ParseDuration 把 '30s' / '10m' / '1h' / '2d' / 裸毫秒数折成 time.Duration；
// 无法解析返回 false。
func ParseDuration(input string) (time.Duration, bool) {
	match := durationPattern.FindStringSubmatch(strings.TrimSpace(input))
	if match == nil {
		return 0, false
	}
	unit := strings.ToLower(match[2])
	if unit == "" {
		unit = "ms"
	}
	scales := map[string]time.Duration{
		"ms": time.Millisecond,
		"s":  time.Second,
		"m":  time.Minute,
		"h":  time.Hour,
		"d":  24 * time.Hour,
	}
	value, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(value) * scales[unit], true
}

func resolveDuration(label, input string, fallback time.Duration) (time.Duration, error) {
	if input == "" {
		return fallback, nil
	}
	duration, ok := ParseDuration(input)
	if !ok || duration <= 0 {
		return 0, fmt.Errorf("%s无法解析：「%s」（支持 30s / 10m / 1h / 2d 或毫秒数）", label, input)
	}
	return duration, nil
}

// resolveCount 解析正整数（并发数）；未给出回落 fallback，给了坏值则报错。
func resolveCount(label, input string, fallback int) (int, error) {
	if input == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(input))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s必须是正整数：「%s」", label, input)
	}
	return value, nil
}

// ParseRepository 解析 owner/name。与 gitea-assistant 的 --repo 口径一致。
func ParseRepository(raw string) (status.Repository, error) {
	owner, name, ok := strings.Cut(raw, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return status.Repository{}, fmt.Errorf("仓库必须使用 owner/name 格式：「%s」", raw)
	}
	return status.Repository{Owner: owner, Name: name}, nil
}

// ResolveConfig 完成配置装配。repoDir 是 dispatcher 宿主仓库根：日志与锁的
// 默认值锚定于此，与启动 cwd 无关（systemd / 仓库根 / 任意子目录均可启动）。
// detectRemote 仅在缺 host 或仓库时调用。
func ResolveConfig(
	flags Flags,
	repoDir string,
	getenv func(string) string,
	detectRemote func() (GitRemote, bool),
) (Config, error) {
	env := func(name string) string { return strings.TrimSpace(getenv(name)) }

	host := flags.Host
	if host == "" {
		host = env("GITEA_HOST")
	}
	repository := flags.Repository
	if repository == "" {
		repository = env("GITEA_REPOSITORY")
	}
	// 只有缺失时才去读 git remote（省一次子进程调用）
	var remote GitRemote
	remoteLoaded := false
	if host == "" || repository == "" {
		if detectRemote != nil {
			remote, remoteLoaded = detectRemote()
		}
	}
	if host == "" && remoteLoaded {
		host = remote.Host
	}
	if repository == "" && remoteLoaded {
		repository = remote.Repository
	}
	if host == "" {
		return Config{}, fmt.Errorf("缺少 Gitea 站点：--host / GITEA_HOST，或让检出带 http(s) origin remote 以便自动检测")
	}
	if repository == "" {
		return Config{}, fmt.Errorf("缺少仓库：--repo / GITEA_REPOSITORY，或让检出带 origin remote 以便自动检测")
	}
	parsedRepository, err := ParseRepository(repository)
	if err != nil {
		return Config{}, err
	}
	accessToken := flags.AccessToken
	if accessToken == "" {
		accessToken = env("GITEA_ACCESS_TOKEN")
	}
	if accessToken == "" {
		return Config{}, fmt.Errorf("缺少访问令牌：--token / GITEA_ACCESS_TOKEN（评审会以该账号身份提交 review）")
	}

	interval, err := resolveDuration("空闲轮询间隔", firstNonEmpty(flags.Interval, env("DISPATCH_INTERVAL_MS")), 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	sessionTimeout, err := resolveDuration("会话超时", firstNonEmpty(flags.Timeout, env("DISPATCH_TIMEOUT_MS")), 30*time.Minute)
	if err != nil {
		return Config{}, err
	}
	concurrency, err := resolveCount("会话并发", firstNonEmpty(flags.Concurrency, env("DISPATCH_CONCURRENCY")), DefaultConcurrency)
	if err != nil {
		return Config{}, err
	}

	syncMirror := false
	if flags.SyncMirror != nil {
		syncMirror = *flags.SyncMirror
	} else {
		switch strings.ToLower(env("DISPATCH_SYNC_MIRROR")) {
		case "1", "true":
			syncMirror = true
		}
	}

	sessionDir := strings.TrimSpace(flags.SessionDir)
	if sessionDir == "" {
		resolved, err := instances.ClaudeDir()
		if err != nil {
			return Config{}, fmt.Errorf("解析会话配置根: %w", err)
		}
		sessionDir = resolved
	}

	return Config{
		Host:           strings.TrimRight(host, "/"),
		AccessToken:    accessToken,
		Repository:     parsedRepository,
		SessionDir:     sessionDir,
		SessionProject: claudecfg.ProjectDirName("assistant-" + host + "-" + repository),
		Interval:       interval,
		SessionTimeout: sessionTimeout,
		LogDir:         firstNonEmpty(flags.LogDir, env("DISPATCH_LOG_DIR"), filepath.Join(repoDir, "logs")),
		WorktreeRoot: firstNonEmpty(
			flags.WorktreeRoot,
			env("DISPATCH_WORKTREE_ROOT"),
			filepath.Join(os.TempDir(), "agent-dispatcher", "worktrees"),
		),
		LockFile:      firstNonEmpty(flags.LockFile, env("DISPATCH_LOCK_FILE"), filepath.Join(repoDir, "dispatcher.lock")),
		BaseBranch:    firstNonEmpty(flags.BaseBranch, env("DISPATCH_BASE_BRANCH"), "main"),
		SyncMirror:    syncMirror,
		Concurrency:   concurrency,
		Model:         firstNonEmpty(flags.Model, env("DISPATCH_MODEL")),
		Reviewer:      firstNonEmpty(flags.Reviewer, env("DISPATCH_REVIEWER"), "ai"),
		ClaudeBin:     firstNonEmpty(flags.ClaudeBin, env("DISPATCH_CLAUDE_BIN"), "claude"),
		Debug:         flags.Debug || strings.EqualFold(env("DISPATCH_DEBUG"), "1") || strings.EqualFold(env("DISPATCH_DEBUG"), "true"),
		DockerImage:   firstNonEmpty(flags.DockerImage, env("DISPATCH_DOCKER_IMAGE")),
		DockerNetwork: firstNonEmpty(flags.DockerNetwork, env("DISPATCH_DOCKER_NETWORK")),
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
