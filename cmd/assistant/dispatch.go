package main

import (
	"context"
	"fmt"
	"io"
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
	"assistant/internal/instances"
	"assistant/internal/provider"
	"assistant/internal/status"

	"github.com/spf13/cobra"
)

// dispatcherOptions 是调度命令共享的命令行参数（未给出的项回退环境变量/默认值）。
type dispatcherOptions struct {
	Host          string
	RepoDir       string
	Token         string
	APIListen     string
	Weixin        bool
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

// countRepos 统计配置中的仓库总数（--repo-dir 单目标校验用）。
func countRepos(file *instances.File) int {
	total := 0
	for _, instance := range file.Instances {
		total += len(instance.Repos)
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
		&runOptions.DryRun,
		"dry-run",
		false,
		"只读演练：列出将执行的待办并逐步说明动作（日志/worktree/会话/完成判定/重试），零副作用",
	)
	runCommand.Flags().StringVar(
		&runOptions.APIListen,
		"api-listen",
		"127.0.0.1:8770",
		"daemon 状态 API 监听地址（Bearer 鉴权，端点写入配置目录 daemon.json；none 关闭）",
	)
	runCommand.Flags().BoolVar(
		&runOptions.Weixin,
		"weixin",
		false,
		"启动微信对话桥（也可在 config.json 设 weixin.enabled=true；凭证用 assistant weixin login 扫码获取）",
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

// resolveDispatchTargets 解析运行目标：有配置文件时按 instance × repo 展开，
// 否则退回环境变量单实例模式。
func resolveDispatchTargets(
	command *cobra.Command,
	repoFlag, configPath string,
	options *dispatcherOptions,
) ([]dispatchTarget, error) {
	resolvedPath, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return nil, err
	}
	if file == nil {
		config, repoDir, err := resolveEnvDispatcher(command, repoFlag, options)
		if err != nil {
			return nil, err
		}
		client, err := newDispatchClient(config)
		if err != nil {
			return nil, err
		}
		return []dispatchTarget{{config: config, client: client, repoDir: repoDir}}, nil
	}

	// --repo-dir 是单目标覆盖：配置里出现多个仓库时要求 --repo 收敛到唯一仓库，
	// 避免把同一个检出强加到多个循环（锁/worktree 会互相踩）
	if override := strings.TrimSpace(options.RepoDir); override != "" && repoFlag == "" && countRepos(file) > 1 {
		return nil, fmt.Errorf("--repo-dir 只适用于单个仓库：请加 --repo owner/name 指定（当前配置 %d 个仓库）", countRepos(file))
	}

	var targets []dispatchTarget
	var skipped []string
	matchedFilter := repoFlag == ""
	for _, instance := range file.Instances {
		repos := instance.Repos
		if repoFlag != "" {
			repos = nil
			for _, repo := range instance.Repos {
				if repo.Name == repoFlag {
					repos = append(repos, repo)
				}
			}
			if len(repos) == 0 {
				continue
			}
			matchedFilter = true
		}
		if len(repos) == 0 {
			// 已登记但未 setup 的实例（如仅 login）：跳过，不阻断其他实例
			fmt.Fprintf(command.ErrOrStderr(), "跳过 %s：未配置仓库（assistant setup 后写入 repos）\n", instance.Host)
			skipped = append(skipped, instance.Host)
			continue
		}
		for _, repo := range repos {
			target, err := resolveInstanceTarget(command, file, resolvedPath, instance, repo, options)
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
		return nil, fmt.Errorf("仓库 %s 不在配置文件的 instances[].repos 中", repoFlag)
	}
	if len(targets) == 0 {
		if len(skipped) > 0 {
			return nil, fmt.Errorf(
				"配置文件没有可运行的仓库（%s 未配置 repos，先 assistant setup）",
				strings.Join(skipped, "、"))
		}
		return nil, fmt.Errorf("配置文件没有可运行的仓库")
	}
	return targets, nil
}

// resolveInstanceTarget 解析 (instance, repo) 的运行上下文：repo.dir 缺省时用
// 受管克隆落点（<数据目录>/…/repos/<host>/<owner>/<name>），与当前目录解耦；
// 日志/锁同样落在检出之外的状态目录，避免被基线对齐的 clean -fd 波及。
// provider 按 repo > instance > 全局默认解析，写进运行配置（仅运行时注入）。
func resolveInstanceTarget(
	command *cobra.Command,
	file *instances.File,
	configPath string,
	instance instances.Instance,
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
	default:
		repoDir = strings.TrimSpace(repo.Dir)
	}
	if repoDir == "" {
		managed = true
		defaultDir, err := instances.DefaultRepoDir(instance.Host, repo.Name)
		if err != nil {
			return dispatchTarget{}, fmt.Errorf("定位 %s 的默认仓库目录: %w", repo.Name, err)
		}
		repoDir = defaultDir
	}
	flags := dispatcherFlags(command, repo.Name, options)
	flags.Host = instance.Host
	flags.Repository = repo.Name
	// 评审身份从凭据库取（purpose=review）：会话提交的 review 以该令牌账号落库
	reviewCredential, err := tokenForPurpose(configPath, instance.Host, credentials.PurposeReview)
	if err != nil {
		return dispatchTarget{}, err
	}
	flags.AccessToken = reviewCredential.Token
	if flags.Reviewer == "" {
		flags.Reviewer = instance.Reviewer.Name
	}
	if managed {
		// 受管克隆必须随时与 origin/<基线> 一致：恒开镜像同步（配置预览也如实
		// 反映）；显式 dir 的共享检出仍由 --sync-mirror 控制
		value := true
		flags.SyncMirror = &value
		stateDir, err := instances.DefaultRepoStateDir(instance.Host, repo.Name)
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
		// 多仓库共用同一 worktree 根会撞 `pr-<N>` 目录名：按仓库（含站点）隔离
		slug, err := instances.HostSlug(instance.Host)
		if err != nil {
			return dispatchTarget{}, err
		}
		flags.WorktreeRoot = filepath.Join(
			os.TempDir(), "agent-dispatcher",
			slug+"-"+strings.ReplaceAll(repo.Name, "/", "-"), "worktrees",
		)
	}
	config, err := dispatcher.ResolveConfig(flags, repoDir, os.Getenv, func() (dispatcher.GitRemote, bool) {
		return dispatcher.GitRemote{Host: instance.Host, Repository: repo.Name}, true
	})
	if err != nil {
		return dispatchTarget{}, err
	}
	providerName := file.ProviderName(&instance, &repo)
	effective, err := file.EffectiveOverrides(providerName)
	if err != nil {
		return dispatchTarget{}, fmt.Errorf("%s（provider %s）: %w", repo.Name, providerName, err)
	}
	config.Provider = effective
	config.ProviderName = providerName
	config.Optimizations = file.Optimizations.Overrides()
	client, err := newDispatchClient(config)
	if err != nil {
		return dispatchTarget{}, err
	}
	return dispatchTarget{
		instance: instance,
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

func newDispatchClient(config dispatcher.Config) (*status.Client, error) {
	return status.NewClient(config.Host, config.AccessToken)
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
		log(fmt.Sprintf("会话配置根：%s（assistant 托管：会话记录与 claude 全局配置都落这里，不读 ~/.claude）",
			config.SessionDir))
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
			// 重试几分钟（409/401 在日志里是看不见的）
			check := provider.CheckCredential(command.Context(), target.config.Provider)
			if check.OK() {
				log(fmt.Sprintf("AI 凭据：%s（来源 %s）%s", name, source, check.Describe()))
			} else {
				warnf("provider %s 的凭据自检失败：%s", name, check.Describe())
				fmt.Fprintf(command.ErrOrStderr(), "  %s\n", provider.CredentialHint)
			}
			continue
		}
		warnf("provider %s 没有可用的 AI 凭据：会话会认证失败；%s", name, claudecfg.MissingCredentialHint)
	}
}

func dispatchLogger(w io.Writer) func(string) {
	return func(line string) {
		fmt.Fprintf(w, "[dispatch %s] %s\n", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), line)
	}
}

func newDispatchDeps(target dispatchTarget, w io.Writer, store *daemon.Store) dispatcher.Deps {
	config := target.config
	repoDir := target.repoDir
	token := targetToken(target)
	deps := dispatcher.Deps{
		Config:  config,
		API:     target.client,
		RepoDir: repoDir,
		Log:     dispatchLogger(w),
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
	// --sync-mirror 显式开启时对齐
	if config.SyncMirror || target.managed {
		deps.SyncMirror = func() (string, error) {
			return dispatcher.SyncMirror(repoDir, config.BaseBranch, token)
		}
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
		deps.OnStart = func(item dispatcher.WorkItem) {
			store.Start(host, repository, convert(item), time.Now())
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
	return deps
}

func runDispatchLoop(command *cobra.Command, repoFlag, configPath string, options *dispatcherOptions) error {
	targets, err := resolveDispatchTargets(command, repoFlag, configPath, options)
	if err != nil {
		return err
	}
	// dry-run 零副作用：受管克隆只提示将发生的动作，不 clone/不 fetch
	if options.DryRun {
		previewManagedTargets(command, targets)
		previewProviders(command, targets)
	} else {
		targets = prepareManagedTargets(command, targets)
	}
	log := dispatchLogger(command.OutOrStdout())
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
	for _, target := range targets {
		store.AddTarget(daemon.Target{
			Host:       target.instance.Host,
			Repository: target.repo.Name,
			Dir:        target.repoDir,
			BaseBranch: target.config.BaseBranch,
			Managed:    target.managed,
			Ready:      target.skipReason == "",
			SkipReason: target.skipReason,
			Provider:   target.config.ProviderName,
		})
	}
	if !options.DryRun {
		if err := startDaemonServices(command, configPath, options, store); err != nil {
			return err
		}
	}

	depsList := make([]dispatcher.Deps, 0, len(ready))
	for _, target := range ready {
		depsList = append(depsList, newDispatchDeps(target, command.OutOrStdout(), store))
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
		items, err := dispatcher.ListWork(command.Context(), target.client, target.config.Repository)
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
	log := dispatchLogger(command.OutOrStdout())
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
	deps := newDispatchDeps(target, command.OutOrStdout(), nil)
	dispatcher.ProcessItem(command.Context(), deps, dispatcher.WorkItem{Kind: kind, Number: number})
	return nil
}
