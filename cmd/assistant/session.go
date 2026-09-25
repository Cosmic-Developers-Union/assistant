package main

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Cosmic-Developers-Union/assistant/internal/envref"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/sessionstore"

	"github.com/spf13/cobra"
)

// sessionPushOptions 是 `assistant session push` 的参数。
type sessionPushOptions struct {
	URL    string
	Token  string
	Host   string
	Root   string
	All    bool
	DryRun bool
	Quiet  bool
}

// newSessionCommand 管理会话记录的远端留存：把本地文本记录（claude 的 projects/*.jsonl）
// 推给 `assistant serve`，按 (host, project, session) 归档，供 sessions MCP 回查。
func newSessionCommand(configFlag *string) *cobra.Command {
	options := &sessionPushOptions{}
	command := &cobra.Command{
		Use:   "session",
		Short: "会话记录远端留存（推给 assistant serve，按 host/project/session 归档）",
		Args:  cobra.NoArgs,
	}
	pushCommand := &cobra.Command{
		Use:   "push",
		Short: "把本地会话文本记录增量推送到记录库服务端",
		Long: "扫描 <CLAUDE_CONFIG_DIR>/projects/*/*.jsonl（缺省 <配置目录>/claude/projects），\n" +
			"按 (host 标签, claude 项目名, 会话 id) 推送，聊天会话附带会话实体（conversation）\n" +
			"与通道（transport）——记录本身就是原样 jsonl，服务端只做归档与检索。\n" +
			"服务端地址与令牌的解析顺序：--url/--token > ASSISTANT_SESSIONS_URL/TOKEN >\n" +
			"<配置目录>/sessions-remote.json（远端推荐）> <配置目录>/serve.json（同机 serve）。\n" +
			"增量状态写在 <配置目录>/session-push.json：大小与修改时间没变就跳过。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runSessionPush(command.Context(), command, *configFlag, options)
		},
	}
	flags := pushCommand.Flags()
	flags.StringVar(&options.URL, "url", "", "记录库服务端地址（缺省自动解析）")
	flags.StringVar(&options.Token, "token", "", "记录库访问令牌（缺省自动解析）")
	flags.StringVar(&options.Host, "host", "", "宿主机标签（缺省主机名）：区分不同机器上的同名项目")
	flags.StringVar(&options.Root, "root", "", "claude 配置根（缺省 CLAUDE_CONFIG_DIR 或 <配置目录>/claude）")
	flags.BoolVar(&options.All, "all", false, "忽略增量状态，全部重新推送")
	flags.BoolVar(&options.DryRun, "dry-run", false, "只列出将推送的会话，不发起请求")
	flags.BoolVar(&options.Quiet, "quiet", false, "只输出汇总")
	command.AddCommand(pushCommand)
	return command
}

func runSessionPush(ctx context.Context, command *cobra.Command, configPath string, options *sessionPushOptions) error {
	stdout := command.OutOrStdout()
	configDir, err := resolveConfigDir(configPath)
	if err != nil {
		return err
	}
	root := strings.TrimSpace(options.Root)
	if root == "" {
		root, err = instances.ClaudeDir()
		if err != nil {
			return err
		}
	}
	logf := func(format string, arguments ...any) {
		if options.Quiet {
			return
		}
		fmt.Fprintf(stdout, format+"\n", arguments...)
	}
	collectOptions := sessionstore.CollectOptions{
		Root:      root,
		Host:      strings.TrimSpace(options.Host),
		ChatDir:   filepath.Join(configDir, "chat"),
		All:       options.All,
		StatePath: filepath.Join(configDir, "session-push.json"),
	}
	if options.DryRun {
		batch, err := sessionstore.Collect(collectOptions)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "dry-run：%d 条待推送（记录根 %s）\n", len(batch.Sessions), root)
		for _, session := range batch.Sessions {
			detail := fmt.Sprintf("  %s 来源=%s 行=%d", session.Key.String(), session.Source, len(session.Lines))
			if session.Conversation != "" {
				detail += " 会话=" + session.Conversation
			}
			fmt.Fprintln(stdout, detail)
		}
		return nil
	}

	// 记录库地址解析：旗标 > config.json 的 sessions.remote > env/sidecar
	remote := sessionstore.RemoteConfig{URL: strings.TrimSpace(options.URL), Token: strings.TrimSpace(options.Token)}
	remoteSource := "flag"
	if remote.URL == "" {
		if _, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath}); err == nil && file != nil &&
			file.Sessions != nil && file.Sessions.Remote != nil && strings.TrimSpace(file.Sessions.Remote.URL) != "" {
			remote.URL = strings.TrimSpace(file.Sessions.Remote.URL)
			remote.Token = strings.TrimSpace(file.Sessions.Remote.Token)
			if expanded, expandErr := envref.Expand(remote.Token, envref.Options{Field: "sessions.remote.token"}); expandErr == nil {
				remote.Token = expanded
			}
			remoteSource = "config"
		}
	}
	if remote.URL == "" {
		resolved := sessionstore.ResolveRemote(configDir)
		remote.URL = resolved.URL
		remote.Token = cmp.Or(remote.Token, resolved.Token)
		remoteSource = "env/sidecar"
	}
	if remote.URL == "" {
		return fmt.Errorf("没有记录库服务端地址：config.json 加 sessions.remote，或先在同一台机器上 assistant serve，或设置 --url / ASSISTANT_SESSIONS_URL，或写 <配置目录>/%s", sessionstore.RemoteFile)
	}
	logf("记录库 = %s（来源 %s）", remote.URL, remoteSource)
	result, err := sessionstore.Push(ctx, collectOptions, sessionstore.NewClient(remote.URL, remote.Token), logf)
	if err != nil {
		return err
	}
	if result.Stored == 0 {
		fmt.Fprintln(stdout, "没有新记录需要推送")
		return nil
	}
	fmt.Fprintf(stdout, "已推送 %d 条会话记录 → %s\n", result.Stored, remote.URL)
	return nil
}
