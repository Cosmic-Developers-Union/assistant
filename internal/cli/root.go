package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/mcps"
	run "github.com/Cosmic-Developers-Union/assistant/internal/runtime"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
	"github.com/spf13/cobra"
)

var version = "dev"

// NewRootCommand 将四个操作面接到同一二进制，运行与个人实例管理互不读取配置。
func NewRootCommand(stdout, stderr io.Writer) *cobra.Command {
	var configPath string
	var verbose, debug bool
	root := &cobra.Command{Use: "assistant", Short: "平台实例、AI 工具、仓库动作与 bot 运行", Version: version, Args: cobra.NoArgs, SilenceErrors: true, SilenceUsage: true, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }, Example: "  assistant instance add gitea --name work --url https://gitea.example --username developer\n  assistant action label-sync --dry-run\n  assistant run --config config.yaml"}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "展示正在进行的步骤与目标")
	root.PersistentFlags().BoolVar(&debug, "debug", false, "展示内部诊断信息")
	log := func(format string, args ...any) { fmt.Fprintf(stderr, format+"\n", args...) }
	root.AddCommand(newInstanceCommand(), newProjectCommand())
	action := &cobra.Command{Use: "action", Short: "对环境变量指定的一个仓库执行幂等动作", Args: cobra.NoArgs}
	for _, kind := range []string{"label-sync", "automerge"} {
		var dryRun bool
		cmd := &cobra.Command{Use: kind, Args: cobra.NoArgs, Short: map[string]string{"label-sync": "收敛标签与评审状态", "automerge": "实时校验门禁并 squash 至多一个 PR"}[kind], RunE: func(cmd *cobra.Command, _ []string) error {
			return runAction(cmd.Context(), kind, dryRun, verbose || debug, log)
		}}
		cmd.Flags().BoolVar(&dryRun, "dry-run", false, "只报告动作，不修改平台")
		action.AddCommand(cmd)
	}
	root.AddCommand(action)
	mcp := &cobra.Command{Use: "mcp", Short: "提供 stdio AI 会话工具", Args: cobra.NoArgs}
	var host, token, dir string
	gitea := &cobra.Command{Use: "gitea", Short: "检测站点与个人令牌后启动官方 gitea-mcp", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return mcps.RunGitea(cmd.Context(), mcps.GiteaOptions{Dir: dir, Host: host, Token: token, Stdin: cmd.InOrStdin(), Stdout: stdout, Stderr: stderr, Log: func(f string, a ...any) {
			if verbose || debug {
				log(f, a...)
			}
		}})
	}}
	gitea.Flags().StringVar(&host, "host", "", "显式指定站点")
	gitea.Flags().StringVar(&token, "token", "", "显式指定令牌")
	gitea.Flags().StringVar(&dir, "dir", ".", "探测 Git remote 的目录")
	mcp.AddCommand(gitea)
	sessions := &cobra.Command{Use: "sessions", Short: "只读查询 run 登记的会话元数据", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path := configPath
		if path == "" {
			path = "config.yaml"
		}
		return run.ServeSessions(cmd.Context(), path, cmd.InOrStdin(), stdout, version)
	}}
	sessions.Flags().StringVar(&configPath, "config", "", "运行配置文件（缺省 config.yaml）")
	mcp.AddCommand(sessions)
	root.AddCommand(mcp)
	var dryRun bool
	running := &cobra.Command{Use: "run", Short: "按 config.yaml 运行评审、分诊和消息 bot", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := run.Load(configPath)
		if err != nil {
			return err
		}
		cfg.Runtime.Debug = debug
		return run.Start(cmd.Context(), cfg, dryRun, verbose || debug, log)
	}}
	running.Flags().StringVar(&configPath, "config", "", "运行配置文件（缺省当前目录 config.yaml）")
	running.Flags().BoolVar(&dryRun, "dry-run", false, "只读取并列出待办，单次退出")
	root.AddCommand(running)
	return root
}

func runAction(ctx context.Context, kind string, dryRun, verbose bool, log func(string, ...any)) (resultErr error) {
	defer func() { resultErr = credentials.RedactError(resultErr, os.Getenv("GITEA_ACCESS_TOKEN")) }()
	host, token, repo := os.Getenv("GITEA_HOST"), os.Getenv("GITEA_ACCESS_TOKEN"), os.Getenv("GITEA_REPOSITORY")
	if host == "" || token == "" || repo == "" {
		return fmt.Errorf("action 必须设置 GITEA_HOST、GITEA_ACCESS_TOKEN、GITEA_REPOSITORY（owner/name）")
	}
	if err := credentials.ValidateHost(host); err != nil {
		return err
	}
	repository, err := parseRepository(repo)
	if err != nil {
		return err
	}
	client, err := status.NewClient(host, token)
	if err != nil {
		return err
	}
	if err := client.VerifyAuthentication(ctx); err != nil {
		return err
	}
	var api status.API = client
	if dryRun {
		api = status.NewDryRunAPI(client, log)
	}
	options := []status.ManagerOption{status.WithRepository(repository), status.WithStateReviewer("merge")}
	if verbose || dryRun {
		options = append(options, status.WithProgress(log))
	}
	manager := status.NewManager(api, options...)
	if kind == "automerge" {
		return manager.AutoMerge(ctx)
	}
	return manager.Sync(ctx)
}

func parseRepository(raw string) (status.Repository, error) {
	owner, name, err := parseRepo(raw)
	return status.Repository{Owner: owner, Name: name}, err
}
