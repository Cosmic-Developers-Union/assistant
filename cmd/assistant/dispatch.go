package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"assistant/internal/dispatcher"
	"assistant/internal/status"

	"github.com/spf13/cobra"
)

// dispatcherOptions 是调度命令共享的命令行参数（未给出的项回退环境变量/默认值）。
type dispatcherOptions struct {
	Host         string
	Token        string
	Reviewer     string
	Model        string
	ClaudeBin    string
	LogDir       string
	WorktreeRoot string
	LockFile     string
	BaseBranch   string
	SyncMirror   bool
	Interval     string
	Timeout      string
	Concurrency  string
	DryRun       bool
}

// newDispatcherCommands 构造调度引擎子命令：run（常驻）/ list（只读列出）/
// review（立即评审单个 PR）/ triage（立即分诊单个 Issue）。
func newDispatcherCommands(repoFlag *string) []*cobra.Command {
	runOptions := &dispatcherOptions{}
	runCommand := &cobra.Command{
		Use:   "run",
		Short: "长驻主循环：检测待办 → 每待办一个会话 → 验证 → 清理（部署形态）",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runDispatchLoop(command, *repoFlag, runOptions)
		},
	}
	addDispatcherConnectionFlags(runCommand, runOptions)
	runCommand.Flags().StringVar(&runOptions.Interval, "interval", "", "空闲轮询间隔（缺省 30s）")
	runCommand.Flags().StringVar(&runOptions.Timeout, "timeout", "", "单会话超时（缺省 30m）")
	runCommand.Flags().StringVar(
		&runOptions.Concurrency,
		"concurrency",
		"",
		"每轮待办处理的会话并发数（缺省 1；轮间 barrier 保证同一待办同时至多一个会话）",
	)
	runCommand.Flags().BoolVar(
		&runOptions.DryRun,
		"dry-run",
		false,
		"只读演练：列出将执行的待办并逐步说明动作（日志/worktree/会话/完成判定/重试），零副作用",
	)

	listOptions := &dispatcherOptions{}
	listCommand := &cobra.Command{
		Use:   "list",
		Short: "只读列出当前待办（快速验证 host/仓库/令牌/标签链路）",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runDispatchList(command, *repoFlag, listOptions)
		},
	}
	addDispatcherConnectionFlags(listCommand, listOptions)

	reviewOptions := &dispatcherOptions{}
	reviewCommand := &cobra.Command{
		Use:   "review <number>",
		Short: "立即评审单个 PR（跳过检测，端到端调试用）",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			number, err := parseItemNumber(args[0])
			if err != nil {
				return err
			}
			return runDispatchOneShot(command, *repoFlag, reviewOptions, dispatcher.KindPull, number)
		},
	}
	addDispatcherConnectionFlags(reviewCommand, reviewOptions)
	reviewCommand.Flags().StringVar(&reviewOptions.Timeout, "timeout", "", "单会话超时（缺省 30m）")

	triageOptions := &dispatcherOptions{}
	triageCommand := &cobra.Command{
		Use:   "triage <number>",
		Short: "立即分诊单个 Issue（跳过检测）",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			number, err := parseItemNumber(args[0])
			if err != nil {
				return err
			}
			return runDispatchOneShot(command, *repoFlag, triageOptions, dispatcher.KindIssue, number)
		},
	}
	addDispatcherConnectionFlags(triageCommand, triageOptions)
	triageCommand.Flags().StringVar(&triageOptions.Timeout, "timeout", "", "单会话超时（缺省 30m）")

	return []*cobra.Command{runCommand, listCommand, reviewCommand, triageCommand}
}

func addDispatcherConnectionFlags(command *cobra.Command, options *dispatcherOptions) {
	flags := command.Flags()
	flags.StringVar(&options.Host, "host", "", "Gitea API 根地址（缺省：GITEA_HOST 或 origin remote 推导）")
	flags.StringVar(&options.Token, "token", "", "访问令牌（缺省：GITEA_ACCESS_TOKEN；评审以该账号身份提交）")
	flags.StringVar(&options.Reviewer, "reviewer", "", "完成判定匹配的 reviewer 账号（缺省 ai）")
	flags.StringVar(&options.Model, "model", "", "会话模型（缺省用账号默认）")
	flags.StringVar(&options.ClaudeBin, "claude-bin", "", "claude 可执行文件（缺省 PATH 上的 claude）")
	flags.StringVar(&options.LogDir, "log-dir", "", "会话日志目录（缺省 <仓库根>/logs）")
	flags.StringVar(
		&options.WorktreeRoot,
		"worktree-root",
		"",
		"PR worktree 根目录（缺省系统临时目录下 agent-dispatcher/worktrees）",
	)
	flags.StringVar(&options.LockFile, "lock-file", "", "单飞锁文件（缺省 <仓库根>/dispatcher.lock）")
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

// resolveDispatcher 完成调度命令的配置装配：host/仓库缺省时从 cwd 的 origin
// remote 推导，日志/锁默认值锚定宿主检出根。
func resolveDispatcher(
	command *cobra.Command,
	repoFlag string,
	options *dispatcherOptions,
) (dispatcher.Config, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return dispatcher.Config{}, "", fmt.Errorf("获取当前目录: %w", err)
	}
	repoDir := cwd
	if root, ok := dispatcher.RepoRoot(cwd); ok {
		repoDir = root
	}
	flags := dispatcher.Flags{
		Host:         options.Host,
		Repository:   repoFlag,
		AccessToken:  options.Token,
		Interval:     options.Interval,
		Timeout:      options.Timeout,
		Model:        options.Model,
		Reviewer:     options.Reviewer,
		ClaudeBin:    options.ClaudeBin,
		LogDir:       options.LogDir,
		WorktreeRoot: options.WorktreeRoot,
		LockFile:     options.LockFile,
		BaseBranch:   options.BaseBranch,
		Concurrency:  options.Concurrency,
	}
	// 显式 --sync-mirror=false 压过 DISPATCH_SYNC_MIRROR=1；未给出则交给环境变量
	if command.Flags().Changed("sync-mirror") {
		value := options.SyncMirror
		flags.SyncMirror = &value
	}
	config, err := dispatcher.ResolveConfig(flags, repoDir, os.Getenv, func() (dispatcher.GitRemote, bool) {
		url, ok := dispatcher.OriginRemote(cwd)
		if !ok {
			return dispatcher.GitRemote{}, false
		}
		return dispatcher.ParseGitRemoteURL(url)
	})
	if err != nil {
		return dispatcher.Config{}, "", err
	}
	return config, repoDir, nil
}

func newDispatchClient(config dispatcher.Config) (*status.Client, error) {
	return status.NewClient(config.Host, config.AccessToken)
}

func dispatchLogger(w io.Writer) func(string) {
	return func(line string) {
		fmt.Fprintf(w, "[dispatch %s] %s\n", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), line)
	}
}

func newDispatchDeps(config dispatcher.Config, repoDir string, client *status.Client, w io.Writer) dispatcher.Deps {
	deps := dispatcher.Deps{
		Config:  config,
		API:     client,
		RepoDir: repoDir,
		Log:     dispatchLogger(w),
		BuildPrompt: func(kind string, number int64, extra dispatcher.PromptContext) string {
			extra.Repository = config.Repository.FullName()
			return dispatcher.BuildPrompt(kind, number, extra)
		},
		CurrentLogin: func(ctx context.Context) (string, error) {
			return client.AuthenticatedUser(ctx)
		},
		PrepareWorktree: func(pullNumber int64) (string, error) {
			worktreeDir := filepath.Join(config.WorktreeRoot, fmt.Sprintf("pr-%d", pullNumber))
			return dispatcher.PrepareWorktree(repoDir, pullNumber, worktreeDir)
		},
		RemoveWorktree: func(dir string) error {
			return dispatcher.RemoveWorktree(repoDir, dir)
		},
		RunSession: func(prompt, cwd string, onProgress func(string)) dispatcher.SessionOutcome {
			return dispatcher.RunSession(dispatcher.SessionOptions{
				Config:        config,
				Prompt:        prompt,
				Cwd:           cwd,
				MCPConfigPath: filepath.Join(repoDir, ".mcp.json"),
				OnProgress:    onProgress,
			})
		},
	}
	if config.SyncMirror {
		deps.SyncMirror = func() (string, error) {
			return dispatcher.SyncMirror(repoDir, config.BaseBranch)
		}
	}
	return deps
}

func runDispatchLoop(command *cobra.Command, repoFlag string, options *dispatcherOptions) error {
	config, repoDir, err := resolveDispatcher(command, repoFlag, options)
	if err != nil {
		return err
	}
	client, err := newDispatchClient(config)
	if err != nil {
		return err
	}
	deps := newDispatchDeps(config, repoDir, client, command.OutOrStdout())
	if options.DryRun {
		return dispatcher.DryRunPass(command.Context(), deps)
	}
	return dispatcher.RunLoop(command.Context(), deps)
}

func runDispatchList(command *cobra.Command, repoFlag string, options *dispatcherOptions) error {
	config, _, err := resolveDispatcher(command, repoFlag, options)
	if err != nil {
		return err
	}
	client, err := newDispatchClient(config)
	if err != nil {
		return err
	}
	work, err := dispatcher.ListWork(command.Context(), client, config.Repository)
	if err != nil {
		return err
	}
	stdout := command.OutOrStdout()
	for _, item := range work {
		fmt.Fprintf(stdout, "%s#%d  %s\n", item.Kind, item.Number, item.Title)
	}
	if len(work) == 0 {
		fmt.Fprintln(stdout, "当前无待办")
	} else {
		fmt.Fprintf(stdout, "共 %d 个待办\n", len(work))
	}
	return nil
}

// runDispatchOneShot 处理 review/triage 一次性命令：跳过检测直接处理指定编号，
// 与常驻实例互斥（共单飞锁），且与常驻实例同口径先对齐基线（fail-closed）。
func runDispatchOneShot(
	command *cobra.Command,
	repoFlag string,
	options *dispatcherOptions,
	kind string,
	number int64,
) error {
	config, repoDir, err := resolveDispatcher(command, repoFlag, options)
	if err != nil {
		return err
	}
	client, err := newDispatchClient(config)
	if err != nil {
		return err
	}
	deps := newDispatchDeps(config, repoDir, client, command.OutOrStdout())
	// 一次性命令同样亮明身份：review 以该账号落库
	if login, err := client.AuthenticatedUser(command.Context()); err == nil {
		deps.Log(fmt.Sprintf("当前账户：@%s（reviewer=%s）", login, config.Reviewer))
	}
	if config.SyncMirror {
		if _, err := dispatcher.SyncMirror(repoDir, config.BaseBranch); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(config.LogDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(config.WorktreeRoot, 0o755); err != nil {
		return err
	}
	if err := dispatcher.AcquireLock(config.LockFile, deps.Log); err != nil {
		return err
	}
	defer dispatcher.ReleaseLock(config.LockFile)
	dispatcher.ProcessItem(command.Context(), deps, dispatcher.WorkItem{Kind: kind, Number: number})
	return nil
}
