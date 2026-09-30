package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Cosmic-Developers-Union/assistant/internal/config"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"

	"github.com/spf13/cobra"
)

// version 由构建注入（-ldflags -X），assistant 与 assistantd 两个二进制共用。
var version = "dev"

type managerRunner func(context.Context, io.Writer, io.Writer, commandOptions) error

type commandOptions struct {
	Repository string
	Verbose    bool
	// ConfigPath 是配置文件路径（--config / ASSISTANT_CONFIG / 当前目录的 ./config.json）
	ConfigPath string
}

// NewRootCommand 构造 assistant 二进制的根命令，接上 dev 侧默认的自动化
// runner（label-sync / automerge）；测试直接用 newRootCommand 注入替身。
func NewRootCommand(stdout, stderr io.Writer) *cobra.Command {
	return newRootCommand(stdout, stderr, runLabelSync, runAutoMerge)
}

func newRootCommand(stdout, stderr io.Writer, labelSyncer, merger managerRunner) *cobra.Command {
	options := commandOptions{}
	command := &cobra.Command{
		Use:           "assistant",
		Short:         "仓库辅助机器人（Issue/PR 例行事务）与评审会话调度引擎（headless claude）",
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(command *cobra.Command, _ []string) error {
			// 无子命令时打印帮助；未知命令由 NoArgs 校验报错（与 yargs strict 同义）
			return command.Help()
		},
		Example: "  assistant action label-sync --verbose\n" +
			"  assistant action automerge --verbose\n" +
			"  assistant run --dry-run",
	}
	command.SetOut(stdout)
	command.SetErr(stderr)
	flags := command.PersistentFlags()
	flags.BoolVarP(&options.Verbose, "verbose", "v", false, "显示扫描和状态修改过程（不适用于调度命令）")
	flags.StringVar(&options.Repository, "repo", "", "只扫描 owner/name 指定的仓库")
	flags.StringVar(
		&options.ConfigPath,
		"config",
		"",
		"配置文件（缺省 ASSISTANT_CONFIG 或当前目录的 ./config.json；都没有时用环境变量单实例模式）",
	)
	// 保留 Cobra 默认的 `completion` 子命令（bash/zsh/fish/powershell），
	// 以及命令/旗标的动态补全。
	command.CompletionOptions.DisableDefaultCmd = false

	actionCommand := &cobra.Command{
		Use:   "action",
		Short: "执行仓库自动化动作（标签同步与自动合并）",
		Args:  cobra.NoArgs,
	}
	actionCommand.AddCommand(
		newAutomationCommand("label-sync",
			"规范 Issue 标签并把 PR 原生评审状态同步为状态标签（单次执行，供 CI 事件驱动）",
			"", labelSyncer, &options, stdout, stderr),
		newAutomationCommand("automerge",
			"合并门禁全绿且分支未过期的已批准 PR（一次运行至多一个，squash；供 CI schedule 驱动）",
			"", merger, &options, stdout, stderr),
	)
	command.AddCommand(actionCommand)
	// 旧的顶层入口：已 install 的目标仓库里 workflow 仍写着 `assistant sync` /
	// `assistant automerge`，容器镜像更新后不能直接变成 unknown command——
	// 那会让门禁在无人察觉的情况下停摆，直到重跑 install。
	command.AddCommand(
		newAutomationCommand("sync", "已弃用：请改用 assistant action label-sync",
			"请改用 assistant action label-sync", labelSyncer, &options, stdout, stderr),
		newAutomationCommand("automerge", "已弃用：请改用 assistant action automerge",
			"请改用 assistant action automerge", merger, &options, stdout, stderr),
	)
	command.AddCommand(newDispatcherCommand(&options.Repository, &options.ConfigPath))
	command.AddCommand(newLoginCommand(&options.ConfigPath))
	command.AddCommand(newInitCommand(&options.ConfigPath))
	command.AddCommand(newSetupCommand(&options.ConfigPath))
	command.AddCommand(newInstallCommand(), newUninstallCommand(), newMCPCommand(&options.ConfigPath), newDoctorCommand(&options.ConfigPath))
	command.AddCommand(newValidateCommand(&options.ConfigPath))
	command.AddCommand(newConfigCommand(&options.ConfigPath))
	return command
}

// newAutomationCommand 构造执行仓库自动化动作的叶子命令：--repo / --config /
// --verbose 等来自 root 的持久旗标，动作实现由 action 决定。deprecated 非空时
// 兼作旧入口的兼容层——cobra 会把带 Deprecated 的命令从帮助列表里隐藏，只在
// 实际使用到它时打印弃用提示。
func newAutomationCommand(
	use, short, deprecated string,
	action managerRunner,
	options *commandOptions,
	stdout, stderr io.Writer,
) *cobra.Command {
	return &cobra.Command{
		Use:        use,
		Short:      short,
		Args:       cobra.NoArgs,
		Deprecated: deprecated,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := options.validate(); err != nil {
				return err
			}
			return action(command.Context(), stdout, stderr, *options)
		},
	}
}

func (options commandOptions) validate() error {
	if options.Repository != "" {
		if _, err := parseRepository(options.Repository); err != nil {
			return err
		}
	}
	return nil
}

func runLabelSync(ctx context.Context, _, stderr io.Writer, options commandOptions) error {
	_, file, err := resolveInstanceFile(options)
	if err != nil {
		return err
	}
	if file == nil {
		return withManager(ctx, stderr, options, func(ctx context.Context, manager *status.Manager) error {
			return manager.Sync(ctx)
		})
	}
	return runManagerAction(ctx, stderr, options, file, func(ctx context.Context, manager *status.Manager) error {
		return manager.Sync(ctx)
	})
}

func runAutoMerge(ctx context.Context, _, stderr io.Writer, options commandOptions) error {
	_, file, err := resolveInstanceFile(options)
	if err != nil {
		return err
	}
	if file == nil {
		return withManager(ctx, stderr, options, func(ctx context.Context, manager *status.Manager) error {
			return manager.AutoMerge(ctx)
		})
	}
	return runManagerAction(ctx, stderr, options, file, func(ctx context.Context, manager *status.Manager) error {
		return manager.AutoMerge(ctx)
	})
}

// withManager 完成两个子命令共用的准备：载入 .env 与环境变量、建立客户端、
// 认证预检（令牌被拒或地址错误时立即以退出码 78 结束，不重试）、构造 Manager。
func withManager(
	ctx context.Context,
	stderr io.Writer,
	options commandOptions,
	run func(context.Context, *status.Manager) error,
) error {
	verbosef := func(format string, arguments ...any) {
		if options.Verbose {
			fmt.Fprintf(stderr, format+"\n", arguments...)
		}
	}

	workingDirectory, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("获取当前目录: %w", err)
	}
	dotEnvPath, err := config.LoadDotEnv(workingDirectory)
	if err != nil {
		return err
	}
	if dotEnvPath == "" {
		verbosef("未找到 .env，使用进程环境变量")
	} else {
		verbosef("已载入配置: %s", dotEnvPath)
	}

	configuration, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	verbosef("连接 Gitea: %s", configuration.Host)

	client, err := status.NewClient(configuration.Host, configuration.AccessToken)
	if err != nil {
		return err
	}
	if err := client.VerifyAuthentication(ctx); err != nil {
		return err
	}
	verbosef("认证通过")
	// 分支保护端点要求 repo admin（Actions 内置令牌无法授予）；配置了独立
	// 令牌时仅这一处读取改用它，其余调用仍用基础令牌。
	if configuration.BranchProtectionToken != "" {
		if err := client.UseBranchProtectionToken(configuration.BranchProtectionToken); err != nil {
			return err
		}
		verbosef("分支保护读取使用 GITEA_BRANCH_PROTECTION_TOKEN 独立令牌")
	}
	// 状态评审（门禁驳回）配置了独立令牌时以状态评审者账号提交，使驳回成为
	// official review；未配置时以基础令牌身份提交（不计数，仅时间线记录）。
	if configuration.StateToken != "" {
		if err := client.UseStateReviewerToken(configuration.StateToken); err != nil {
			return err
		}
		verbosef("状态评审提交使用 GITEA_STATE_TOKEN 独立令牌")
	}
	managerOptions := []status.ManagerOption{status.WithProgress(verbosef)}
	// 身份是约定：内容评审者 ai（NewManager 默认），状态评审者/合并者 merge。
	// env 模式没有账号配置，按约定补默认；GITEA_STATE_REVIEWER 仍可显式覆盖。
	stateReviewer := firstNonEmpty(configuration.StateReviewer, "merge")
	managerOptions = append(managerOptions, status.WithStateReviewer(stateReviewer))
	verbosef("状态评审者: %s", stateReviewer)

	// 优先使用 --repo 参数，其次使用环境变量 GITEA_REPOSITORY
	repoToUse := options.Repository
	if repoToUse == "" && configuration.Repository != "" {
		repoToUse = configuration.Repository
		verbosef("使用环境变量指定的仓库: %s", repoToUse)
	}

	if repoToUse != "" {
		repository, err := parseRepository(repoToUse)
		if err != nil {
			return err
		}
		managerOptions = append(managerOptions, status.WithRepository(repository))
	}
	return run(ctx, status.NewManager(client, managerOptions...))
}

func parseRepository(raw string) (status.Repository, error) {
	owner, name, ok := strings.Cut(raw, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return status.Repository{}, fmt.Errorf("--repo 必须使用 owner/name 格式")
	}
	return status.Repository{Owner: owner, Name: name}, nil
}
