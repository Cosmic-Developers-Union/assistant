package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"assistant/internal/claudecfg"
	"assistant/internal/credentials"
	"assistant/internal/daemon"
	"assistant/internal/dispatcher"
	"assistant/internal/envref"
	"assistant/internal/instances"
	"assistant/internal/provider"
	"assistant/internal/statestore"
	"assistant/internal/status"

	"github.com/spf13/cobra"
)

// dispatcherOptions 是调度命令共享的命令行参数（未给出的项回退环境变量/默认值）。
type dispatcherOptions struct {
	Host          string
	RepoDir       string
	Token         string
	APIListen     string
	Verbose       bool
	Reviewer      string
	Model         string
	ClaudeBin     string
	Debug         bool
	LogDir        string
	WorktreeRoot  string
	LockFile      string
	BaseBranch    string
	SyncMirror    bool
	Interval      string
	Timeout       string
	Concurrency   string
	DockerImage   string
	DockerNetwork string
	Run           string
	Runtime       string
	DryRun        bool
}

// dispatchTarget 是一个 (instance, 仓库) 的完整运行上下文。
type dispatchTarget struct {
	instance instances.Instance
	repo     instances.Repo
	config   dispatcher.Config
	client   *status.Client
	repoDir  string
	// managed 表示 repoDir 是默认受管克隆：缺失时自动 clone，运行期间每轮强制
	// 对齐 origin 基线（config.SyncMirror 对受管克隆恒为开）
	managed bool
	// skipReason 非空表示该目标未就绪（缺少 install 产物等），daemon 跳过它但
	// 保持其余仓库与状态 API/对话可用
	skipReason string
}

// targetToken 返回操作该仓库的令牌：目标解析时已从凭据库（purpose=review）写入
// config.AccessToken；环境变量单实例模式则来自 GITEA_ACCESS_TOKEN / --token。
func targetToken(target dispatchTarget) string {
	return target.config.AccessToken
}

// countRepos 统计 runtime 引用的 gitea 通道仓库总数（--repo-dir 单目标校验用；
// referenced 为空表示引用全部）。
func countRepos(file *instances.File, referenced map[string]bool) int {
	total := 0
	for _, channel := range giteaChannels(file) {
		if len(referenced) > 0 && !referenced[channel.Key()] {
			continue
		}
		total += len(channel.Repos)
	}
	return total
}

// newDispatcherCommands 构造调度引擎子命令：run（常驻）/ list（只读列出）/
// review（立即评审单个 PR）/ triage（立即分诊单个 Issue）。
func newDispatcherCommands(repoFlag, configFlag *string) []*cobra.Command {
	runOptions := &dispatcherOptions{}
	runCommand := &cobra.Command{
		Use:   "run",
		Short: "长驻主循环：检测待办 → 每待办一个会话 → 验证 → 清理（部署形态）",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runDispatchLoop(command, *repoFlag, *configFlag, runOptions)
		},
	}
	addDispatcherConnectionFlags(runCommand, runOptions)
	runCommand.Flags().StringVar(&runOptions.Interval, "interval", "", "空闲轮询间隔（缺省 30s）")
	runCommand.Flags().StringVar(&runOptions.Timeout, "timeout", "", "单会话超时（缺省 30m）")
	runCommand.Flags().StringVar(
		&runOptions.Concurrency,
		"concurrency",
		"",
		"每轮待办处理的会话并发数（缺省 8；轮间 barrier 保证同一待办同时至多一个会话）",
	)
	runCommand.Flags().BoolVar(
		&runOptions.Verbose,
		"verbose",
		false,
		"展示系统正在做什么（步骤/目标/进度/外部调用）；--debug 已包含",
	)
	runCommand.Flags().BoolVar(
		&runOptions.DryRun,
		"dry-run",
		false,
		"只读演练：列出将执行的待办并逐步说明动作（日志/worktree/会话/完成判定/重试），零副作用",
	)
	runCommand.Flags().StringVar(
		&runOptions.Run,
		"run",
		"",
		"（已退役）run.yaml 已并入 config.json：`assistant config migrate` 导入旧文件后按 --config / 当前目录运行",
	)
	runCommand.Flags().StringVar(
		&runOptions.APIListen,
		"api-listen",
		"",
		"daemon 状态 API 监听地址（Bearer 鉴权，端点写入配置目录 daemon.json；缺省取 runtime 的 api_listen=127.0.0.1:8770；off 关闭）",
	)

	listOptions := &dispatcherOptions{}
	listCommand := &cobra.Command{
		Use:   "list",
		Short: "只读列出当前待办（快速验证 host/仓库/令牌/标签链路）",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runDispatchList(command, *repoFlag, *configFlag, listOptions)
		},
	}
	addDispatcherConnectionFlags(listCommand, listOptions)

	reviewOptions := &dispatcherOptions{}
	reviewCommand := &cobra.Command{
		Use:   "review <number>",
		Short: "立即评审单个 PR（跳过检测，端到端调试用）",
		Args:  cobra.ExactArgs(1),
		// 编号是数字参数：不触发文件补全
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) error {
			number, err := parseItemNumber(args[0])
			if err != nil {
				return err
			}
			return runDispatchOneShot(command, *repoFlag, *configFlag, reviewOptions, dispatcher.KindPull, number)
		},
	}
	addDispatcherConnectionFlags(reviewCommand, reviewOptions)
	reviewCommand.Flags().StringVar(&reviewOptions.Timeout, "timeout", "", "单会话超时（缺省 30m）")

	triageOptions := &dispatcherOptions{}
	triageCommand := &cobra.Command{
		Use:   "triage <number>",
		Short: "立即分诊单个 Issue（跳过检测）",
		Args:  cobra.ExactArgs(1),
		// 编号是数字参数：不触发文件补全
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) error {
			number, err := parseItemNumber(args[0])
			if err != nil {
				return err
			}
			return runDispatchOneShot(command, *repoFlag, *configFlag, triageOptions, dispatcher.KindIssue, number)
		},
	}
	addDispatcherConnectionFlags(triageCommand, triageOptions)
	triageCommand.Flags().StringVar(&triageOptions.Timeout, "timeout", "", "单会话超时（缺省 30m）")

	return []*cobra.Command{runCommand, listCommand, reviewCommand, triageCommand}
}

func addDispatcherConnectionFlags(command *cobra.Command, options *dispatcherOptions) {
	flags := command.Flags()
	flags.StringVar(&options.Runtime, "runtime", "", "运行时名（缺省 default_runtime；只有一个 runtime 时可省）")
	flags.StringVar(&options.Host, "host", "", "Gitea API 根地址（缺省：GITEA_HOST 或 origin remote 推导）")
	flags.StringVar(&options.RepoDir, "repo-dir", "",
		"仓库检出目录（覆盖 repo.dir；config.json 多目标时需配合 --repo 指定唯一仓库）")
	flags.StringVar(&options.Token, "token", "", "访问令牌（缺省：GITEA_ACCESS_TOKEN；评审以该账号身份提交）")
	flags.StringVar(&options.Reviewer, "reviewer", "", "完成判定匹配的 reviewer 账号（缺省 ai）")
	flags.StringVar(&options.Model, "model", "", "会话模型（缺省用账号默认）")
	flags.StringVar(&options.ClaudeBin, "claude-bin", "", "claude 可执行文件（缺省 PATH 上的 claude）")
	flags.BoolVar(&options.Debug, "debug", false,
		"详细日志：会话命令行、claude 原始 stream 事件、stderr 尾部、对话消息与回复（DISPATCH_DEBUG=1 等效）")
	flags.StringVar(&options.DockerImage, "docker-image", "",
		"评审会话镜像：非空时会话跑在 docker 容器里（如 ghcr.io/cosmic-developers-union/assistant-review:latest）")
	flags.StringVar(&options.DockerNetwork, "docker-network", "",
		"会话容器的 docker 网络（如 host；缺省 bridge）")
	flags.StringVar(&options.LogDir, "log-dir", "", "会话日志目录（缺省 <仓库检出>/logs）")
	flags.StringVar(
		&options.WorktreeRoot,
		"worktree-root",
		"",
		"PR worktree 根目录（缺省系统临时目录下 agent-dispatcher/<owner>-<repo>/worktrees）",
	)
	flags.StringVar(&options.LockFile, "lock-file", "", "单飞锁文件（缺省 <仓库检出>/dispatcher.lock）")
	flags.StringVar(
		&options.BaseBranch,
		"base-branch",
		"",
		"镜像同步基线分支（缺省 main；--sync-mirror 开启时检出强制对齐 origin/<分支>）",
	)
	flags.BoolVar(
		&options.SyncMirror,
		"sync-mirror",
		false,
		"每轮检测前把宿主检出强制对齐 origin 基线（fetch --prune + checkout -f -B + clean -fd；部署形态开启，共享开发检出勿开）",
	)
}

func parseItemNumber(raw string) (int64, error) {
	number, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("无效的编号：%s", raw)
	}
	return number, nil
}

// resolveEnvDispatcher 是环境变量单实例模式的配置装配：host/仓库缺省时从 cwd
// 的 origin remote 推导，日志/锁默认值锚定宿主检出根。
func resolveEnvDispatcher(
	command *cobra.Command,
	repoFlag string,
	options *dispatcherOptions,
) (dispatcher.Config, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return dispatcher.Config{}, "", fmt.Errorf("获取当前目录: %w", err)
	}
	repoDir := strings.TrimSpace(options.RepoDir)
	if repoDir != "" {
		if repoDir, err = filepath.Abs(repoDir); err != nil {
			return dispatcher.Config{}, "", fmt.Errorf("解析 --repo-dir: %w", err)
		}
	} else {
		repoDir = cwd
		if root, ok := dispatcher.RepoRoot(cwd); ok {
			repoDir = root
		}
	}
	config, err := dispatcher.ResolveConfig(
		dispatcherFlags(command, repoFlag, options),
		repoDir, os.Getenv,
		func() (dispatcher.GitRemote, bool) {
			remote, gitea := dispatcher.SelectGiteaRemote(repoDir, func(host string) bool {
				return status.ProbeGitea(context.Background(), host)
			})
			if remote.Host == "" {
				return dispatcher.GitRemote{}, false
			}
			// 非 Gitea remote 仅在显式指定 host 时作为仓库路径来源，避免拿
			// GitHub 等地址去连 Gitea API
			if !gitea && options.Host == "" && strings.TrimSpace(os.Getenv("GITEA_HOST")) == "" {
				return dispatcher.GitRemote{}, false
			}
			return remote, true
		},
	)
	if err != nil {
		return dispatcher.Config{}, "", err
	}
	return config, repoDir, nil
}

func dispatcherFlags(command *cobra.Command, repoFlag string, options *dispatcherOptions) dispatcher.Flags {
	flags := dispatcher.Flags{
		Host:          options.Host,
		Repository:    repoFlag,
		AccessToken:   options.Token,
		Interval:      options.Interval,
		Timeout:       options.Timeout,
		Model:         options.Model,
		Reviewer:      options.Reviewer,
		ClaudeBin:     options.ClaudeBin,
		Debug:         options.Debug,
		LogDir:        options.LogDir,
		WorktreeRoot:  options.WorktreeRoot,
		LockFile:      options.LockFile,
		BaseBranch:    options.BaseBranch,
		Concurrency:   options.Concurrency,
		DockerImage:   options.DockerImage,
		DockerNetwork: options.DockerNetwork,
	}
	// 显式 --sync-mirror=false 压过 DISPATCH_SYNC_MIRROR=1；未给出则交给环境变量
	if command.Flags().Changed("sync-mirror") {
		value := options.SyncMirror
		flags.SyncMirror = &value
	}
	return flags
}

// resolveDispatchTargets 解析运行目标：config.json 的 gitea 通道 × repos 按
// runtime 展开（运行树与缺省参数来自 runtime），否则退回环境变量单实例模式。
// 配置只带聊天通道时返回空目标（纯聊天 daemon：状态 API 与对话照常常驻）。
func resolveDispatchTargets(
	command *cobra.Command,
	repoFlag, configPath string,
	options *dispatcherOptions,
) ([]dispatchTarget, error) {
	// run.yaml 已并入 config.json（runtime 节 + gitea 通道）：--run / --config
	// 指向 YAML 时点名纠正，而不是让它深入到凭据库报一个不相关的错
	if strings.TrimSpace(options.Run) != "" {
		return nil, fmt.Errorf(
			"--run 已退役：run.yaml 已并入 config.json——`assistant config migrate` 导入旧文件，之后按 --config / 标准配置目录运行")
	}
	if trimmed := strings.TrimSpace(configPath); trimmed != "" &&
		(strings.HasSuffix(trimmed, ".yaml") || strings.HasSuffix(trimmed, ".yml")) {
		if _, statErr := os.Stat(trimmed); statErr == nil {
			return nil, fmt.Errorf(
				"%s 是 YAML（旧 run.yaml），不是 config.json：run.yaml 已并入 config.json——`assistant config migrate %s` 导入",
				trimmed, trimmed)
		}
	}
	resolvedPath, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return nil, err
	}
	if file == nil {
		config, repoDir, err := resolveEnvDispatcher(command, repoFlag, options)
		if err != nil {
			return nil, err
		}
		client, err := newDispatchClient(config, options.Debug)
		if err != nil {
			return nil, err
		}
		return []dispatchTarget{{config: config, client: client, repoDir: repoDir}}, nil
	}
	runtime, err := file.ResolveRuntime(options.Runtime)
	if err != nil {
		return nil, err
	}
	// 调度只吃本 runtime 引用的 gitea 通道：未引用/已停用的不产生评审目标
	// （runtime.Channels 为空表示引用全部，合成 main 的形态）
	referenced := map[string]bool{}
	for _, key := range runtime.Channels {
		referenced[key] = true
	}

	// --repo-dir 是单目标覆盖：配置里出现多个仓库时要求 --repo 收敛到唯一仓库，
	// 避免把同一个检出强加到多个循环（锁/worktree 会互相踩）
	if override := strings.TrimSpace(options.RepoDir); override != "" && repoFlag == "" && countRepos(file, referenced) > 1 {
		return nil, fmt.Errorf("--repo-dir 只适用于单个仓库：请加 --repo owner/name 指定（当前配置 %d 个仓库）", countRepos(file, referenced))
	}

	var targets []dispatchTarget
	matchedFilter := repoFlag == ""
	for _, channel := range giteaChannels(file) {
		if !channel.IsEnabled() {
			continue
		}
		if len(referenced) > 0 && !referenced[channel.Key()] {
			continue
		}
		if len(channel.Repos) == 0 {
			// 已登记但未 setup 的站点（如仅 login）：跳过，不阻断其他站点
			fmt.Fprintf(command.ErrOrStderr(), "跳过 gitea 通道 %s：未配置仓库（assistant setup --host %s 后写入 repos）\n",
				channel.Key(), channel.Host)
			continue
		}
		for _, repo := range channel.Repos {
			if repoFlag != "" {
				if repo.Name != repoFlag {
					continue
				}
				matchedFilter = true
			}
			target, err := giteaTarget(command, file, resolvedPath, runtime, channel, repo, options)
			if err != nil {
				return nil, err
			}
			// 会话 MCP 以 ASSISTANT_CONFIG 继承同一份配置：daemon 用 --config 指向
			// 非标准路径时，会话内 `assistant mcp gitea/daemon` 也必须解析到它。
			target.config.ConfigPath = resolvedPath
			targets = append(targets, target)
		}
	}
	if !matchedFilter {
		return nil, fmt.Errorf("仓库 %s 不在 gitea 通道的 repos 中", repoFlag)
	}
	if len(targets) == 0 && !hasChatService(file) {
		return nil, fmt.Errorf("配置文件没有可运行的仓库（gitea 通道未配置 repos，先 assistant setup）")
	}
	return targets, nil
}

// hasChatService 报告配置是否带聊天通道（决定零 gitea 目标时是否纯聊天常驻）。
func hasChatService(file *instances.File) bool {
	for _, channel := range file.Channels {
		if channel.Type != instances.ChannelGitea && channel.IsEnabled() {
			return true
		}
	}
	return false
}

// maskTokenForLog 令牌打码（日志展示用）：只保留前后各 4 位。
func maskTokenForLog(token string) string {
	if token == "" {
		return "(空)"
	}
	if len(token) <= 8 {
		return "***"
	}
	return token[:4] + "***" + token[len(token)-4:]
}

// giteaTarget 装配 gitea 通道 × 仓库的运行上下文（原 instances 与 run.yaml
// 双轨的统一形态）：repo.dir 有则用共享检出，否则用 runtime 树下的受管克隆；
// 令牌 = 通道 token（缺省回退凭据库 purpose=review）；日志/锁/评审工作区与
// 轮询、超时、并发缺省值全部按 runtime。provider 按 repo > 通道 > runtime >
// 全局默认解析（仅运行时注入）。
func giteaTarget(
	command *cobra.Command,
	file *instances.File,
	configPath string,
	runtime instances.Runtime,
	channel *instances.Channel,
	repo instances.Repo,
	options *dispatcherOptions,
) (dispatchTarget, error) {
	repoDir := strings.TrimSpace(options.RepoDir)
	managed := false
	switch {
	case repoDir != "":
		// --repo-dir 显式覆盖：按共享检出处理（不克隆、不强制镜像）
		absolute, err := filepath.Abs(repoDir)
		if err != nil {
			return dispatchTarget{}, fmt.Errorf("解析 --repo-dir: %w", err)
		}
		repoDir = absolute
	case strings.TrimSpace(repo.Dir) != "":
		repoDir = repo.Dir
	default:
		managed = true
		dir, err := runtime.TargetPath(channel.Host, repo.Name)
		if err != nil {
			return dispatchTarget{}, fmt.Errorf("定位 %s 的受管克隆落点: %w", repo.Name, err)
		}
		repoDir = dir
	}
	flags := dispatcherFlags(command, repo.Name, options)
	flags.Host = channel.Host
	flags.Repository = repo.Name
	// 会话配置根与对话会话共享（runtime 的 claude 目录）
	flags.SessionDir = runtime.ClaudeConfigDir()
	// 评审身份：通道显式 token 优先（$VAR/${VAR} 引用在消费点展开），否则
	// 凭据库 purpose=review（会话提交的 review 以该令牌账号落库）
	channelToken, err := envref.Expand(channel.Token, envref.Options{
		Field: fmt.Sprintf("channels[%s].token", channel.Key()),
	})
	if err != nil {
		return dispatchTarget{}, err
	}
	if token := strings.TrimSpace(channelToken); token != "" {
		flags.AccessToken = token
	} else {
		reviewCredential, err := tokenForPurpose(configPath, channel.Host, credentials.PurposeReview)
		if err != nil {
			return dispatchTarget{}, err
		}
		flags.AccessToken = reviewCredential.Token
	}
	if flags.Reviewer == "" {
		flags.Reviewer = channel.Reviewer
	}
	if managed {
		// 受管克隆必须随时与 origin/<基线> 一致：恒开镜像同步（配置预览也如实
		// 反映）；显式 dir 的共享检出仍由 --sync-mirror 控制
		value := true
		flags.SyncMirror = &value
		stateDir, err := runtime.RepoStatePath(channel.Host, repo.Name)
		if err != nil {
			return dispatchTarget{}, fmt.Errorf("定位 %s 的状态目录: %w", repo.Name, err)
		}
		if flags.LogDir == "" {
			flags.LogDir = filepath.Join(stateDir, "logs")
		}
		if flags.LockFile == "" {
			flags.LockFile = filepath.Join(stateDir, "dispatcher.lock")
		}
	}
	if flags.WorktreeRoot == "" && strings.TrimSpace(os.Getenv("DISPATCH_WORKTREE_ROOT")) == "" {
		// 评审工作区：runtime 的 review_root + 命名模板去掉逐待办叶子后的仓库
		// 级基名（模板已含站点与仓库占位符，同名仓库跨站点不会相撞）
		base, err := runtime.ReviewDirBase(channel.Host, repo.Name)
		if err != nil {
			return dispatchTarget{}, fmt.Errorf("展开 %s 的评审工作区基名: %w", repo.Name, err)
		}
		flags.WorktreeRoot = filepath.Join(runtime.ReviewRootDir(), base, "worktrees")
	}
	// 轮询/超时/并发缺省取 runtime（旗标优先；毫秒数字面量解析无歧义）
	if flags.Interval == "" {
		flags.Interval = strconv.FormatInt(runtime.Interval(), 10)
	}
	if flags.Timeout == "" {
		flags.Timeout = strconv.FormatInt(runtime.Timeout(), 10)
	}
	if flags.Concurrency == "" {
		flags.Concurrency = strconv.Itoa(runtime.Workers())
	}
	config, err := dispatcher.ResolveConfig(flags, repoDir, os.Getenv, func() (dispatcher.GitRemote, bool) {
		return dispatcher.GitRemote{Host: channel.Host, Repository: repo.Name}, true
	})
	if err != nil {
		return dispatchTarget{}, err
	}
	// review agent 定义（agents 池，用户同名覆盖内置）：独立执行时它的
	// model/claude_bin 生效；provider 仍走 repo > 通道 > runtime > 全局链
	if reviewAgent, ok := file.Agents["review"]; ok {
		if flags.Model == "" {
			flags.Model = reviewAgent.Model
		}
		if flags.ClaudeBin == "" {
			flags.ClaudeBin = reviewAgent.ClaudeBin
		}
	}
	providerName := file.GiteaProviderName(runtime, channel, &repo)
	effective, err := file.EffectiveOverrides(providerName)
	if err != nil {
		return dispatchTarget{}, fmt.Errorf("%s（provider %s）: %w", repo.Name, providerName, err)
	}
	config.Provider = effective
	config.ProviderName = providerName
	optimizations, err := file.EffectiveOptimizations()
	if err != nil {
		return dispatchTarget{}, err
	}
	config.Optimizations = optimizations
	client, err := newDispatchClient(config, options.Debug)
	if err != nil {
		return dispatchTarget{}, err
	}
	return dispatchTarget{
		instance: channelInstance(channel),
		repo:     repo,
		config:   config,
		client:   client,
		repoDir:  repoDir,
		managed:  managed,
	}, nil
}

// checkTargetsHealth 在 run 启动前逐 instance 检查服务可用性（版本端点 + 各
// 角色令牌认证）；任一不可用即拒绝启动，不带病运行。
func checkTargetsHealth(ctx context.Context, targets []dispatchTarget, log func(string)) error {
	checked := make(map[string]bool)
	for _, target := range targets {
		host := target.instance.Host
		if host == "" {
			host = target.config.Host
		}
		if checked[host] {
			continue
		}
		checked[host] = true
		version, err := target.client.CheckHealth(ctx)
		if err != nil {
			return fmt.Errorf("instance %s 不可用: %w", host, err)
		}
		login, err := target.client.AuthenticatedUser(ctx)
		if err != nil {
			return fmt.Errorf("instance %s reviewer 令牌校验失败: %w", host, err)
		}
		log(fmt.Sprintf("instance %s 可用（Gitea %s，reviewer @%s）", host, version, login))
		// merge / admin 令牌同样来自凭据库；缺失只提示，不阻断启动
		for _, purpose := range []string{credentials.PurposeMerge, credentials.PurposeAdmin} {
			credential, ok, credentialErr := credentialFor(target.config.ConfigPath, host, purpose)
			if credentialErr != nil {
				return credentialErr
			}
			if !ok {
				log(fmt.Sprintf("提示：instance %s 缺少 %s 用途令牌——%s", host, purpose, missingTokenHint(host, purpose)))
				continue
			}
			roleClient, clientErr := status.NewClient(host, credential.Token)
			if clientErr != nil {
				return clientErr
			}
			roleLogin, loginErr := roleClient.AuthenticatedUser(ctx)
			if loginErr != nil {
				if purpose == credentials.PurposeAdmin {
					log(fmt.Sprintf("警告：instance %s admin 令牌校验失败（%v），分支保护读取将回退严格模式", host, loginErr))
					continue
				}
				return fmt.Errorf("instance %s %s 令牌校验失败: %w", host, purpose, loginErr)
			}
			log(fmt.Sprintf("instance %s %s @%s 可用", host, purpose, roleLogin))
		}
	}
	return nil
}

// shortGiteaPath 把 Gitea 请求 URL 折成站点相对路径（日志可读性）：
// https://host/api/v1/repos/a/b/issues?limit=50 → GET /repos/a/b/issues?limit=50
func shortGiteaPath(host, rawURL string) string {
	prefix := strings.TrimRight(host, "/") + "/api/v1"
	if rest, ok := strings.CutPrefix(rawURL, prefix); ok {
		return rest
	}
	if parsed, err := url.Parse(rawURL); err == nil {
		path := strings.TrimPrefix(parsed.Path, "/api/v1")
		if parsed.RawQuery != "" {
			return path + "?" + parsed.RawQuery
		}
		return path
	}
	return rawURL
}

func newDispatchClient(config dispatcher.Config, debug bool) (*status.Client, error) {
	client, err := status.NewClient(config.Host, config.AccessToken)
	if err != nil {
		return nil, err
	}
	if debug {
		// --debug：观测系统对 Gitea 的全部外部调用（Why/What 的外部调用面）。
		// 只显示 method + 站点相对路径（不含主机名与令牌），一眼可读
		client.EnableRequestLog(func(method, url string, statusCode int, duration time.Duration) {
			fmt.Fprintf(os.Stderr, "[dispatch:d %s] Gitea %s %s → %d %dms\n",
				time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
				method, shortGiteaPath(config.Host, url), statusCode, duration.Milliseconds())
		})
	}
	return client, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// previewProviders 在 dry-run 中说明每个目标生效的 provider（值掩码，环境
// 变量只列键名）：provider 覆盖只作用于运行时会话配置，零副作用。
func previewProviders(command *cobra.Command, targets []dispatchTarget) {
	for _, target := range targets {
		global := ""
		if env, settings, mcp := target.config.Optimizations.Counts(); env+settings+mcp > 0 {
			global = fmt.Sprintf(" + 全局优化（env %d 项：%s；settings %d 项；mcp %d 个）",
				env, strings.Join(maskedEnvKeys(target.config.Optimizations.Env), "、"), settings, mcp)
		}
		if target.config.ProviderName == "" && global == "" {
			continue
		}
		name := target.config.ProviderName
		preset := ""
		if name == "" {
			name = "内置缺省"
		} else if provider.HasPreset(name) {
			preset = " 内置预设"
		}
		env, settings, mcp := target.config.Provider.Counts()
		fmt.Fprintf(command.ErrOrStderr(),
			"dry-run：%s 使用 provider %s%s（env %d 项：%s；settings %d 项；mcp %d 个）%s——注入会话 --settings/--mcp-config，不写仓库文件\n",
			target.repo.Name, name, preset, env, strings.Join(maskedEnvKeys(target.config.Provider.Env), "、"), settings, mcp, global)
	}
}

// maskedEnvKeys 返回排序后的 env 键名（值不落日志）。
func maskedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// previewManagedTargets 是 ensureManagedTargets 的 dry-run 版本：只说明将克隆/
// 对齐基线的动作，不触碰文件系统与网络。
func previewManagedTargets(command *cobra.Command, targets []dispatchTarget) {
	for _, target := range targets {
		if !target.managed {
			continue
		}
		if _, err := os.Stat(filepath.Join(target.repoDir, ".git")); err != nil {
			fmt.Fprintf(command.ErrOrStderr(), "dry-run：将克隆 %s → %s\n", target.repo.Name, target.repoDir)
			continue
		}
		fmt.Fprintf(command.ErrOrStderr(), "dry-run：检测轮前将把 %s 对齐 origin/%s\n",
			target.repo.Name, target.config.BaseBranch)
	}
}

// prepareManagedTargets 就地准备受管克隆（缺失时按平台/仓库 clone）；非受管检出
// （显式 dir）不触碰。克隆失败（网络/权限）才算未就绪——**仓库里有没有
// assistant install 的产物不构成门槛**：会话的 MCP 与评审协议都由 assistant 注入
// （见 claudecfg.GiteaMCPServer / dispatcher.ReviewProtocolPrompt），仓库长什么样
// 都照常评审。脚手架缺失只作为提示记一行，方便顺手补 install。
func prepareManagedTargets(command *cobra.Command, targets []dispatchTarget) []dispatchTarget {
	for index := range targets {
		target := &targets[index]
		if !target.managed {
			continue
		}
		cloned, err := dispatcher.EnsureRepo(
			target.repoDir, target.instance.Host, target.repo.Name, targetToken(*target))
		if err != nil {
			target.skipReason = err.Error()
			fmt.Fprintf(command.ErrOrStderr(), "跳过 %s：%v\n", target.repo.Name, err)
			continue
		}
		if cloned {
			fmt.Fprintf(command.ErrOrStderr(), "已克隆 %s → %s\n", target.repo.Name, target.repoDir)
		}
		if missing := missingScaffolding(target.repoDir); len(missing) > 0 {
			fmt.Fprintf(command.ErrOrStderr(),
				"提示：%s 没有 %s（评审照常进行，MCP 与会话配置由 assistant 注入）；"+
					"需要仓库内的编辑器/CI 脚手架时运行 `assistant install` 并提交\n",
				target.repo.Name, strings.Join(missing, "、"))
		}
	}
	return targets
}

// missingScaffolding 返回仓库缺失的 assistant install 产物（仅用于提示，不影响
// 就绪判定）。
func missingScaffolding(repoDir string) []string {
	candidates := []string{
		".mcp.json",
		filepath.Join(".claude", "settings.json"),
		filepath.Join(".claude", "skills", "review", "SKILL.md"),
		"AGENTS.md",
	}
	missing := make([]string, 0, len(candidates))
	for _, relative := range candidates {
		if _, err := os.Stat(filepath.Join(repoDir, relative)); err != nil {
			missing = append(missing, relative)
		}
	}
	return missing
}

// readyTargets 过滤掉未就绪目标（prepareManagedTargets 的结果）。
func readyTargets(targets []dispatchTarget) []dispatchTarget {
	ready := make([]dispatchTarget, 0, len(targets))
	for _, target := range targets {
		if target.skipReason == "" {
			ready = append(ready, target)
		}
	}
	return ready
}

// skipSummary 汇总未就绪原因（日志/错误信息用）。
func skipSummary(targets []dispatchTarget) string {
	var parts []string
	for _, target := range targets {
		if target.skipReason != "" {
			parts = append(parts, fmt.Sprintf("%s（%s）", target.repo.Name, target.skipReason))
		}
	}
	return strings.Join(parts, "、")
}

// checkSessionRuntime 在调度开跑前把「会话能不能跑起来」的前提一次讲清：claude
// 可执行文件与版本、assistant 托管的会话配置根、以及每个 provider 的 AI 凭据来源。
// 这些前提**与仓库内容无关**，所以不管有没有就绪仓库都要检查——所有仓库都被跳过
// 时恰恰最需要这几行日志（否则只剩一句「没有可运行的仓库」）。
func checkSessionRuntime(command *cobra.Command, targets []dispatchTarget, log func(string)) {
	warnf := func(format string, arguments ...any) {
		fmt.Fprintf(command.ErrOrStderr(), "警告："+format+"\n", arguments...)
	}
	if len(targets) == 0 {
		return
	}
	config := targets[0].config
	if resolved, err := exec.LookPath(config.ClaudeBin); err != nil {
		warnf("找不到 claude 可执行文件（%s）：评审与对话会话都会失败（装 claude，或用 --claude-bin 指绝对路径）",
			config.ClaudeBin)
	} else if version := claudecfg.ClaudeVersion(config.ClaudeBin); version != "" {
		log(fmt.Sprintf("claude：%s（%s）", version, resolved))
	} else {
		log(fmt.Sprintf("claude：%s（读不出版本，可能不是 Claude Code CLI）", resolved))
	}
	if config.SessionDir != "" {
		// 一行一个事实：日志要能扫，不堆成长句
		log("会话配置根：" + config.SessionDir)
		log("  （assistant 托管：会话文本记录与 claude 全局配置都落这里；不读 ~/.claude）")
	}
	// 凭据按 provider 去重报告：会话不读 ~/.claude 登录态，没有 provider 凭据就必然失败
	reported := map[string]bool{}
	for _, target := range targets {
		name := target.config.ProviderName
		if name == "" {
			name = "内置缺省"
		} else if provider.HasPreset(name) {
			name += " 内置预设"
		}
		if reported[name] {
			continue
		}
		reported[name] = true
		if source := claudecfg.CredentialSource(target.config.Provider); source != "" {
			// 最小请求实测：凭据/端点不对时在这里就报出来，而不是等每条会话
			// 重试几分钟（401 在日志里是看不见的）
			check := provider.CheckCredential(command.Context(), target.config.Provider)
			log("AI 凭据：" + name)
			log("  来源 = " + source)
			if check.OK() {
				log("  自检 = " + check.Describe())
			} else {
				warnf("provider %s 的凭据自检失败：%s", name, check.Describe())
				fmt.Fprintf(command.ErrOrStderr(), "  %s\n", provider.CredentialHint)
			}
			continue
		}
		warnf("provider %s 没有可用的 AI 凭据：会话会认证失败；%s", name, claudecfg.MissingCredentialHint)
	}
}

// dispatchLogger 按三级日志约定构造 dispatcher 的日志回调组：
//
//	default = What happened?     → [dispatch]（恒输出）
//	verbose = What is happening? → [dispatch:v]（--verbose/--debug 输出）
//	debug   = Why is it happening? → [dispatch:d]（仅 --debug 输出）
type dispatchLoggers struct {
	info    func(string)
	verbose func(string)
	debug   func(string)
}

func newDispatchLoggers(w io.Writer, verbose, debug bool) dispatchLoggers {
	if debug {
		verbose = true
	}
	newLogger := func(prefix string, enabled bool) func(string) {
		return func(line string) {
			if !enabled {
				return
			}
			fmt.Fprintf(w, "[%s %s] %s\n", prefix,
				time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), line)
		}
	}
	return dispatchLoggers{
		info:    newLogger("dispatch", true),
		verbose: newLogger("dispatch:v", verbose),
		debug:   newLogger("dispatch:d", debug),
	}
}

// dispatchLoggerStderr 是 stderr 上的 default 级日志（run.yaml 解析期提示）。
func dispatchLoggerStderr(command *cobra.Command) func(string) {
	return func(line string) {
		fmt.Fprintf(command.ErrOrStderr(), "[dispatch %s] %s\n",
			time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), line)
	}
}

func newDispatchDeps(target dispatchTarget, w io.Writer, store *daemon.Store, stateStore *statestore.Store, verbose, debug bool) dispatcher.Deps {
	config := target.config
	repoDir := target.repoDir
	token := targetToken(target)
	loggers := newDispatchLoggers(w, verbose, debug)
	deps := dispatcher.Deps{
		Config:     config,
		API:        target.client,
		RepoDir:    repoDir,
		Log:        loggers.info,
		LogVerbose: loggers.verbose,
		LogDebug:   loggers.debug,
		BuildPrompt: func(kind string, number int64, extra dispatcher.PromptContext) string {
			extra.Repository = config.Repository.FullName()
			return dispatcher.BuildPrompt(kind, number, extra)
		},
		CurrentLogin: func(ctx context.Context) (string, error) {
			return target.client.AuthenticatedUser(ctx)
		},
		PrepareWorktree: func(pullNumber int64) (string, error) {
			worktreeDir := filepath.Join(config.WorktreeRoot, fmt.Sprintf("pr-%d", pullNumber))
			return dispatcher.PrepareWorktree(repoDir, pullNumber, worktreeDir, token)
		},
		PrepareIssue: func(worktreeDir string) (string, error) {
			return dispatcher.PrepareBaselineWorktree(repoDir, config.BaseBranch, worktreeDir, token)
		},
		RemoveWorktree: func(dir string) error {
			return dispatcher.RemoveWorktree(repoDir, dir)
		},
		// 追问轮的输入：会话开始（since）之后他人（非 assistant 账号）的
		// 新评论，保证「ai 回复前必须读到消息」
		FollowUpMessages: func(ctx context.Context, item dispatcher.WorkItem, since time.Time) ([]string, error) {
			comments, err := target.client.ListIssueCommentsSince(ctx, config.Repository, item.Number, since)
			if err != nil {
				return nil, err
			}
			var messages []string
			for _, comment := range comments {
				if comment.User == config.Reviewer || comment.User == "" {
					continue
				}
				if strings.TrimSpace(comment.Body) == "" {
					continue
				}
				messages = append(messages, fmt.Sprintf("@%s：%s", comment.User, comment.Body))
			}
			return messages, nil
		},
		PostFollowUpNote: func(ctx context.Context, item dispatcher.WorkItem, count int) error {
			body := fmt.Sprintf("检测到 %d 条新消息，正在续接会话读取并回应（回复前先完整阅读全部消息）。", count)
			return target.client.CreateIssueComment(ctx, config.Repository, item.Number, body)
		},
		RunSession: func(request dispatcher.SessionRequest) dispatcher.SessionOutcome {
			item := request.Item
			// 稳定会话 ID：PR 以 head 为锚点（head 变化换新记录），Issue 以标题
			// 为锚点；同一待办重试复用同一会话记录（RunSession 自动 --resume）
			anchor := request.HeadSHA
			if item.Kind != dispatcher.KindPull {
				anchor = item.Title
			}
			return dispatcher.RunSession(dispatcher.SessionOptions{
				Config:        config,
				Prompt:        request.Prompt,
				Cwd:           request.Cwd,
				MCPConfigPath: filepath.Join(repoDir, ".mcp.json"),
				ProjectDir:    repoDir,
				SessionID: dispatcher.SessionID(
					config.Host, config.Repository.FullName(), item.Kind, item.Number, anchor),
				Title: dispatcher.SessionTitle(
					item.Kind, config.Repository.FullName(), item.Number, request.HeadSHA),
				OnProgress: request.OnProgress,
			})
		},
	}
	// 受管克隆每个检测轮前强制对齐 origin/<base>；显式 dir 的共享检出只在
	// --sync-mirror 显式开启时对齐。同一轮的多个待办并发起会话时共享一次
	// 同步（SingleFlightMirror）：受管克隆是同仓库唯一检出，并发 git 命令
	// 会互撞 .git/index.lock
	if config.SyncMirror || target.managed {
		deps.SyncMirror = dispatcher.SingleFlightMirror(config.Interval, func() (string, error) {
			return dispatcher.SyncMirror(repoDir, config.BaseBranch, token)
		})
	}
	if store != nil {
		host, repository := target.instance.Host, target.repo.Name
		convert := func(item dispatcher.WorkItem) daemon.Item {
			return daemon.Item{Kind: item.Kind, Number: item.Number, Title: item.Title}
		}
		deps.OnQueue = func(items []dispatcher.WorkItem) {
			converted := make([]daemon.Item, 0, len(items))
			for _, item := range items {
				converted = append(converted, convert(item))
			}
			store.SetQueue(host, repository, time.Now(), converted)
		}
		deps.OnStart = func(item dispatcher.WorkItem) bool {
			store.Start(host, repository, convert(item), time.Now())
			return true
		}
		deps.OnFinish = func(item dispatcher.WorkItem, outcome dispatcher.SessionOutcome) {
			store.Finish(host, repository, convert(item), daemon.Result{
				FinishedAt: time.Now(),
				Subtype:    outcome.Subtype,
				IsError:    outcome.IsError,
				NumTurns:   outcome.NumTurns,
				CostUSD:    outcome.CostUSD,
				DurationMS: outcome.DurationMS,
				SessionID:  outcome.SessionID,
				Errors:     outcome.Errors,
			})
		}
	}
	if stateStore != nil {
		host, repository := target.instance.Host, target.repo.Name
		convert := func(item dispatcher.WorkItem) statestore.Item {
			return statestore.Item{Kind: item.Kind, Number: item.Number, Title: item.Title}
		}
		deps.OnQueue = appendOnQueue(deps.OnQueue, func(items []dispatcher.WorkItem) {
			converted := make([]statestore.Item, 0, len(items))
			for _, item := range items {
				converted = append(converted, convert(item))
			}
			if err := stateStore.ReplaceQueue(host, repository, time.Now(), converted); err != nil {
				deps.Log(fmt.Sprintf("状态库队列写入失败：%v", err))
			}
		})
		// 同一待办同一时刻只一个 claude：sqlite 事务互斥（跨进程），残留 running
		// 超过两个会话超时视为崩溃残留并接管
		deps.OnStart = appendOnStartBool(deps.OnStart, func(item dispatcher.WorkItem) bool {
			started, err := stateStore.StartSession(
				host, repository, convert(item), time.Now(), "", 2*target.config.SessionTimeout)
			if err != nil {
				deps.Log(fmt.Sprintf("状态库会话登记失败：%v", err))
				return true // 状态库故障不阻断评审，内存守卫仍在
			}
			return started
		})
		deps.OnFinish = appendOnFinish(deps.OnFinish, func(item dispatcher.WorkItem, outcome dispatcher.SessionOutcome) {
			if err := stateStore.FinishSession(host, repository, convert(item), time.Time{}, statestore.Result{
				Subtype:    outcome.Subtype,
				IsError:    outcome.IsError,
				Turns:      outcome.NumTurns,
				CostUSD:    outcome.CostUSD,
				DurationMS: outcome.DurationMS,
				SessionID:  outcome.SessionID,
				Errors:     outcome.Errors,
			}); err != nil {
				deps.Log(fmt.Sprintf("状态库会话收尾失败：%v", err))
			}
		})
	}
	return deps
}

// appendOnQueue 组合两个队列回调（内存 store 与 sqlite store 各一份）。
func appendOnQueue(first, second func([]dispatcher.WorkItem)) func([]dispatcher.WorkItem) {
	if first == nil {
		return second
	}
	return func(items []dispatcher.WorkItem) {
		first(items)
		second(items)
	}
}

// appendOnStart 组合会话开始回调；second 返回 false 表示互斥拒绝（跳过本待办）。
func appendOnStartBool(first func(dispatcher.WorkItem) bool, second func(dispatcher.WorkItem) bool) func(dispatcher.WorkItem) bool {
	if first == nil {
		return second
	}
	return func(item dispatcher.WorkItem) bool {
		first(item)
		return second(item)
	}
}

// appendOnFinish 组合会话结束回调。
func appendOnFinish(first func(dispatcher.WorkItem, dispatcher.SessionOutcome), second func(dispatcher.WorkItem, dispatcher.SessionOutcome)) func(dispatcher.WorkItem, dispatcher.SessionOutcome) {
	if first == nil {
		return second
	}
	return func(item dispatcher.WorkItem, outcome dispatcher.SessionOutcome) {
		first(item, outcome)
		second(item, outcome)
	}
}

// openStateStore 按 run.yaml 的 state-dir/state-file 打开 SQLite 状态库；未配置
// 返回 nil（内省与互斥退化为进程内守卫）。runPath 是已定位的 run.yaml 路径
// （空串表示 run.yaml 模式未激活，状态库随之下线）。
// openStateStore 打开 runtime 的 SQLite 状态库：默认开启（$root/state 下），
// state_file = "off" 显式关闭；环境变量单实例模式 runtime 为 nil（无状态库）。
func openStateStore(runtime *instances.Runtime, log func(string)) (*statestore.Store, error) {
	if runtime == nil {
		return nil, nil
	}
	path := runtime.StatePath()
	if path == "" {
		return nil, nil
	}
	store, err := statestore.Open(path)
	if err != nil {
		return nil, err
	}
	log("状态库：" + path + "（SQLite WAL，内省与跨进程互斥）")
	return store, nil
}

func runDispatchLoop(command *cobra.Command, repoFlag, configPath string, options *dispatcherOptions) error {
	targets, err := resolveDispatchTargets(command, repoFlag, configPath, options)
	if err != nil {
		return err
	}
	// runtime 选定运行树：状态库/状态 API/对话落点都按它；resolvedPath 是解析
	// 后的配置路径（daemon.json 端点与会话环境锚定它）
	var runtime *instances.Runtime
	resolvedPath, configFile, fileErr := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if fileErr != nil {
		return fileErr
	} else if configFile != nil {
		resolved, rtErr := configFile.ResolveRuntime(options.Runtime)
		if rtErr != nil {
			return rtErr
		}
		runtime = &resolved
	}
	// dry-run 零副作用：受管克隆只提示将发生的动作，不 clone/不 fetch
	if options.DryRun {
		previewManagedTargets(command, targets)
		previewProviders(command, targets)
	} else {
		targets = prepareManagedTargets(command, targets)
	}
	loggers := newDispatchLoggers(command.OutOrStdout(), options.Verbose, options.Debug)
	log := loggers.info
	ready := readyTargets(targets)
	if !options.DryRun {
		if err := checkTargetsHealth(command.Context(), ready, log); err != nil {
			return err
		}
		// 会话能不能跑起来与仓库内容无关：不管有没有就绪仓库都先把前提说清楚
		checkSessionRuntime(command, targets, log)
		for _, target := range ready {
			env, settings, mcp := target.config.Provider.Counts()
			globalEnv, globalSettings, globalMCP := target.config.Optimizations.Counts()
			if target.config.ProviderName == "" && env+settings+mcp == 0 && globalEnv+globalSettings+globalMCP == 0 {
				continue
			}
			name := target.config.ProviderName
			if name == "" {
				name = "内置缺省"
			} else if provider.HasPreset(name) {
				name += " 内置预设"
			}
			global := ""
			if globalEnv+globalSettings+globalMCP > 0 {
				global = fmt.Sprintf("（含全局优化 env %d 项，settings %d 项，mcp %d 个）",
					globalEnv, globalSettings, globalMCP)
			}
			log(fmt.Sprintf("%s 使用 provider %s（env %d 项，settings %d 项，mcp %d 个）%s",
				target.repo.Name, name, env, settings, mcp, global))
		}
	}

	// daemon 运行态：调度循环写入，状态 API 与对话会话的 MCP 读取；未就绪仓库也
	// 登记（带原因），对话/状态查询能解释「为什么这个仓库没在跑」
	store := daemon.NewStore(version)
	// SQLite WAL 共享状态（runtime 默认开启）：跨进程内省 + 同一待办的跨进程互斥
	stateStore, err := openStateStore(runtime, log)
	if err != nil {
		return err
	}
	defer stateStore.Close()
	for _, target := range targets {
		daemonTarget := daemon.Target{
			Host:       target.instance.Host,
			Repository: target.repo.Name,
			Dir:        target.repoDir,
			BaseBranch: target.config.BaseBranch,
			Managed:    target.managed,
			Ready:      target.skipReason == "",
			SkipReason: target.skipReason,
			Provider:   target.config.ProviderName,
		}
		store.AddTarget(daemonTarget)
		if err := stateStore.UpsertTarget(statestore.Target{
			Host:       daemonTarget.Host,
			Repository: daemonTarget.Repository,
			Dir:        daemonTarget.Dir,
			BaseBranch: daemonTarget.BaseBranch,
			Managed:    daemonTarget.Managed,
			Ready:      daemonTarget.Ready,
			SkipReason: daemonTarget.SkipReason,
			Provider:   daemonTarget.Provider,
		}); err != nil {
			return fmt.Errorf("登记目标到状态库: %w", err)
		}
	}
	if !options.DryRun {
		if err := startDaemonServices(command, resolvedPath, options, store, stateStore); err != nil {
			return err
		}
	}

	depsList := make([]dispatcher.Deps, 0, len(ready))
	for _, target := range ready {
		depsList = append(depsList, newDispatchDeps(target, command.OutOrStdout(), store, stateStore, options.Verbose, options.Debug))
	}
	if options.DryRun {
		for _, deps := range depsList {
			if err := dispatcher.DryRunPass(command.Context(), deps); err != nil {
				return err
			}
		}
		return nil
	}
	if len(depsList) == 0 {
		// 全部仓库未就绪：保持 daemon 常驻（状态 API/对话可用，日志给出修复指引），
		// 而不是崩溃重启
		log(fmt.Sprintf("没有可运行的仓库：%s；状态 API 与对话保持可用，修复后重启", skipSummary(targets)))
		<-command.Context().Done()
		return nil
	}
	return dispatcher.RunAll(command.Context(), depsList)
}

func runDispatchList(command *cobra.Command, repoFlag, configPath string, options *dispatcherOptions) error {
	targets, err := resolveDispatchTargets(command, repoFlag, configPath, options)
	if err != nil {
		return err
	}
	stdout := command.OutOrStdout()
	work := 0
	for _, target := range targets {
		items, err := dispatcher.ListWork(command.Context(), target.client, target.config.Repository, target.config.Reviewer)
		if err != nil {
			return fmt.Errorf("%s %s: %w", target.config.Host, target.config.Repository.FullName(), err)
		}
		if len(targets) > 1 {
			fmt.Fprintf(stdout, "%s %s\n", target.config.Host, target.config.Repository.FullName())
		}
		for _, item := range items {
			fmt.Fprintf(stdout, "%s#%d  %s\n", item.Kind, item.Number, item.Title)
		}
		work += len(items)
	}
	if work == 0 {
		fmt.Fprintln(stdout, "当前无待办")
	} else {
		fmt.Fprintf(stdout, "共 %d 个待办\n", work)
	}
	return nil
}

// runDispatchOneShot 处理 review/triage 一次性命令：跳过检测直接处理指定编号，
// 与常驻实例互斥（共单飞锁），且与常驻实例同口径先对齐基线（fail-closed）。
func runDispatchOneShot(
	command *cobra.Command,
	repoFlag, configPath string,
	options *dispatcherOptions,
	kind string,
	number int64,
) error {
	targets, err := resolveDispatchTargets(command, repoFlag, configPath, options)
	if err != nil {
		return err
	}
	if len(targets) != 1 {
		return fmt.Errorf("匹配到 %d 个仓库，请用 --repo owner/name 指定要处理的仓库", len(targets))
	}
	target := targets[0]
	if target.managed {
		targets = prepareManagedTargets(command, targets)
		target = targets[0]
		if target.skipReason != "" {
			return fmt.Errorf("仓库 %s 未就绪：%s", target.repo.Name, target.skipReason)
		}
	}
	config := target.config
	log := dispatchLoggerStderr(command)
	// 一次性命令同样亮明身份：review 以该账号落库
	if login, err := target.client.AuthenticatedUser(command.Context()); err == nil {
		log(fmt.Sprintf("当前账户：@%s（reviewer=%s）", login, config.Reviewer))
	}
	if config.SyncMirror || target.managed {
		if _, err := dispatcher.SyncMirror(target.repoDir, config.BaseBranch, targetToken(target)); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(config.LogDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(config.WorktreeRoot, 0o755); err != nil {
		return err
	}
	if err := dispatcher.AcquireLock(config.LockFile, log); err != nil {
		return err
	}
	defer dispatcher.ReleaseLock(config.LockFile)
	deps := newDispatchDeps(target, command.OutOrStdout(), nil, nil, options.Verbose, options.Debug)
	dispatcher.ProcessItem(command.Context(), deps, dispatcher.WorkItem{Kind: kind, Number: number})
	return nil
}
