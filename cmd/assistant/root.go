package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"assistant/internal/config"
	"assistant/internal/status"

	"github.com/spf13/cobra"
)

type managerRunner func(context.Context, io.Writer, io.Writer, commandOptions) error

type commandOptions struct {
	Repository string
	Verbose    bool
	Wait       bool
	Timeout    time.Duration
	Interval   time.Duration
	// ConfigPath 是多实例配置文件路径（--config / ASSISTANT_CONFIG / ./config.json）
	ConfigPath string
}

func newRootCommand(stdout, stderr io.Writer, checker, syncer, merger managerRunner) *cobra.Command {
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
		Example: "  assistant check\n" +
			"  assistant sync --verbose\n" +
			"  assistant automerge --verbose\n" +
			"  assistant run --dry-run\n" +
			"  assistant review 58",
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
		"多实例配置文件（缺省 ASSISTANT_CONFIG 或当前目录 config.json；都缺省时用环境变量单实例模式）",
	)
	// 保留 Cobra 默认的 `completion` 子命令（bash/zsh/fish/powershell），
	// 以及命令/旗标的动态补全。
	command.CompletionOptions.DisableDefaultCmd = false

	checkCommand := &cobra.Command{
		Use:   "check",
		Short: "按标签检索待 triage 的 Issue 和待 review 的 PR（只读）",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := options.validate(); err != nil {
				return err
			}
			return checker(command.Context(), stdout, stderr, options)
		},
	}
	checkCommand.Flags().BoolVar(
		&options.Wait,
		"wait",
		false,
		"没有待办时持续等待直到出现待办（与 --timeout 互斥）",
	)
	checkCommand.Flags().Var(
		dayDuration{&options.Timeout},
		"timeout",
		"没有待办时持续等待的时长（如 10m、1d12h）；0 表示立即返回",
	)
	checkCommand.Flags().DurationVar(
		&options.Interval,
		"interval",
		15*time.Second,
		"等待模式下的轮询间隔（仅在 --wait 或 --timeout > 0 时生效）",
	)
	syncCommand := &cobra.Command{
		Use:   "sync",
		Short: "规范 Issue 标签并把 PR 原生评审状态同步为状态标签（单次执行，供 CI 事件驱动）",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := options.validate(); err != nil {
				return err
			}
			return syncer(command.Context(), stdout, stderr, options)
		},
	}
	autoMergeCommand := &cobra.Command{
		Use:   "automerge",
		Short: "合并门禁全绿且分支未过期的已批准 PR（一次运行至多一个，squash；供 CI schedule 驱动）",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := options.validate(); err != nil {
				return err
			}
			return merger(command.Context(), stdout, stderr, options)
		},
	}
	command.AddCommand(checkCommand, syncCommand, autoMergeCommand)
	command.AddCommand(newDispatcherCommands(&options.Repository, &options.ConfigPath)...)
	command.AddCommand(newSetupCommand(&options.ConfigPath))
	command.AddCommand(newActionsCommand(&options.ConfigPath))
	command.AddCommand(newInstallCommand(), newUninstallCommand(), newMCPCommand(), newDoctorCommand())
	return command
}

func (options commandOptions) validate() error {
	if options.Repository != "" {
		if _, err := parseRepository(options.Repository); err != nil {
			return err
		}
	}
	if options.Wait && options.Timeout > 0 {
		return fmt.Errorf("--wait 与 --timeout 互斥，只能指定其一")
	}
	if options.Timeout < 0 {
		return fmt.Errorf("--timeout 不能为负")
	}
	if (options.Wait || options.Timeout > 0) && options.Interval <= 0 {
		return fmt.Errorf("--interval 必须大于 0")
	}
	return nil
}

func runCheck(ctx context.Context, stdout, stderr io.Writer, options commandOptions) error {
	_, file, err := resolveInstanceFile(options)
	if err != nil {
		return err
	}
	if file == nil {
		return withManager(ctx, stderr, options, func(ctx context.Context, manager *status.Manager) error {
			return waitForReport(ctx, stdout, manager, options.Wait, options.Timeout, options.Interval)
		})
	}
	managers, err := instanceManagers(ctx, file, options.Repository, stderr)
	if err != nil {
		return err
	}
	return waitForReport(ctx, stdout, &instanceChecker{managers: managers}, options.Wait, options.Timeout, options.Interval)
}

// workChecker 抽象 *status.Manager 的检索能力，便于为等待逻辑写单元测试。
type workChecker interface {
	Check(context.Context) (status.Report, error)
}

// waitForReport 检索并输出待办报告。没有待办时按 interval 轮询等待：forever 为 true
// 时一直等到出现待办；否则等到超时（timeout <= 0 表示不等待，立即返回）。等待期间
// ctx 被取消（Ctrl+C / SIGTERM，由 main 的 signal.NotifyContext 触发）同样结束。
// 三种结束方式都以 nil 返回，退出码 0。
func waitForReport(
	ctx context.Context,
	stdout io.Writer,
	checker workChecker,
	forever bool,
	timeout, interval time.Duration,
) error {
	var deadline <-chan time.Time
	if !forever && timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		report, err := checker.Check(ctx)
		if err != nil {
			// Check 内部的网络往返也可能撞上 Ctrl+C（ctx 已取消），此时与等待
			// 期间被中断一样，按「已中断等待」以退出码 0 结束。
			if errors.Is(err, context.Canceled) {
				fmt.Fprintln(stdout, "已中断等待")
				return nil
			}
			return err
		}
		if report.HasWork() || (!forever && timeout <= 0) {
			if err := status.WriteReport(stdout, report); err != nil {
				return fmt.Errorf("输出报告: %w", err)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(stdout, "已中断等待")
			return nil
		case <-deadline:
			fmt.Fprintf(stdout, "没有 Issue 或 PR 需要处理（等待 %s 后超时）\n", timeout)
			return nil
		case <-time.After(interval):
		}
	}
}

func runSync(ctx context.Context, _, stderr io.Writer, options commandOptions) error {
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
