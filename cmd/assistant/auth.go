package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"assistant/internal/credentials"
	"assistant/internal/instances"

	"github.com/spf13/cobra"
)

// newAuthCommand 管理本地凭据库（credentials.json）：身份与按用途派生的令牌。
// 只操作本地记录，不触碰 Gitea 侧账号与令牌。
func newAuthCommand(configFlag *string) *cobra.Command {
	command := &cobra.Command{
		Use:   "auth",
		Short: "本地凭据库管理：status 查看身份与用途令牌、rotate 轮换 mcp 令牌、remove 清除记录",
		Args:  cobra.NoArgs,
	}
	command.AddCommand(
		newAuthStatusCommand(configFlag),
		newAuthRotateCommand(configFlag),
		newAuthRemoveCommand(configFlag),
	)
	return command
}

func newAuthStatusCommand(configFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status [host]",
		Short: "显示本地身份（是否管理员）与各用途令牌",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			configPath, err := setupConfigWritePath(*configFlag)
			if err != nil {
				return err
			}
			path, err := credentials.PathFor(configPath)
			if err != nil {
				return err
			}
			store, err := credentials.Load(path)
			if err != nil {
				return err
			}
			stdout := command.OutOrStdout()
			filter := ""
			if len(args) == 1 {
				filter = credentials.NormalizeHost(args[0])
			}
			printed := 0
			for _, identity := range store.Identity {
				if filter != "" && credentials.NormalizeHost(identity.Host) != filter {
					continue
				}
				printed++
				fmt.Fprintf(stdout, "%s\t@%s\tadmin=%t\tverified=%s\n",
					identity.Host, identity.User, identity.IsAdmin, identity.VerifiedAt)
				printCredentialLines(stdout, store, identity.Host, identity.User)
			}
			// 有令牌但没身份记录（例如手工编辑过凭据库）也要显示出来
			for _, host := range hostsOf(store) {
				if filter != "" && host != filter {
					continue
				}
				if _, ok := store.IdentityFor(host); ok {
					continue
				}
				printed++
				fmt.Fprintf(stdout, "%s\tidentity=none\n", host)
				printCredentialLines(stdout, store, host, "")
			}
			if printed == 0 {
				if filter != "" {
					fmt.Fprintf(stdout, "凭据库中没有 %s 的记录（assistant login %s --user <账号>）\n", filter, filter)
				} else {
					fmt.Fprintf(stdout, "凭据库为空：%s（assistant login <host> --user <账号>）\n", path)
				}
				return nil
			}
			fmt.Fprintf(stdout, "凭据库：%s\n", path)
			return nil
		},
	}
}

// printCredentialLines 打印某站点（可限定账号）的全部用途令牌；只显示令牌末 8 位。
func printCredentialLines(stdout io.Writer, store *credentials.File, host, user string) {
	for _, credential := range store.Credentials {
		if credentials.NormalizeHost(credential.Host) != credentials.NormalizeHost(host) {
			continue
		}
		if user != "" && credential.User != user {
			continue
		}
		scopes := strings.Join(credential.Scopes, ",")
		fmt.Fprintf(stdout, "  purpose=%s\tuser=@%s\tname=%s\tlast8=%s\tsource=%s\tscopes=%s\n",
			credential.Purpose, credential.User, credential.TokenName, credential.LastEight,
			credential.Source, scopes)
	}
}

// hostsOf 返回凭据库里出现过的站点（排序、去重）。
func hostsOf(store *credentials.File) []string {
	seen := map[string]bool{}
	var hosts []string
	add := func(host string) {
		host = credentials.NormalizeHost(host)
		if host != "" && !seen[host] {
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	for _, identity := range store.Identity {
		add(identity.Host)
	}
	for _, credential := range store.Credentials {
		add(credential.Host)
	}
	return hosts
}

func newAuthRotateCommand(configFlag *string) *cobra.Command {
	options := &loginOptions{MCP: true, Rotate: true}
	command := &cobra.Command{
		Use:   "rotate <host>",
		Short: "重建当前账号的 mcp 令牌（旧同名令牌在站点上删除，令牌名保持不变）",
		Long: "按 --user 指定的账号重建 mcp 用途令牌：先删除站点上同名旧令牌，再用最小\n" +
			"scope 新建，并更新凭据库。需要密码（--password / --password-stdin / 终端输入）。\n" +
			"轮换后旧令牌立即失效；引用了旧令牌的编辑器会话需要重启。",
		Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			host := ""
			if len(args) == 1 {
				host = args[0]
			}
			resolved, err := resolveLoginHost(command.Context(), *configFlag, host, options.Host)
			if err != nil {
				return err
			}
			writePath, file, err := loadInstanceFileForSetup(*configFlag)
			if err != nil {
				return err
			}
			if file == nil {
				file = &instances.File{}
			}
			fmt.Fprintf(command.OutOrStdout(), "轮换 %s 的 mcp 令牌\n", resolved)
			return runIdentityLogin(command, resolved, writePath, file, options)
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.Host, "host", "", "平台地址（与位置参数二选一）")
	flags.StringVar(&options.User, "user", "", "账号（缺省取同站点 tea 登录 user）")
	flags.StringVar(&options.Password, "password", "", "账号密码（建议改用 --password-stdin）")
	flags.BoolVar(&options.PasswordStdin, "password-stdin", false, "从标准输入读取密码")
	flags.StringVar(&options.TOTP, "totp", "", "双因素验证码")
	flags.StringVar(&options.TokenName, "token-name", "", "令牌名（缺省沿用派生名）")
	return command
}

func newAuthRemoveCommand(configFlag *string) *cobra.Command {
	var host string
	var user string
	command := &cobra.Command{
		Use:   "remove [host]",
		Short: "清除本地凭据记录（不动 Gitea 侧令牌，必要时到 Applications 页面撤销）",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			argHost := ""
			if len(args) == 1 {
				argHost = args[0]
			}
			target, err := resolveLoginHost(command.Context(), *configFlag, argHost, host)
			if err != nil {
				return err
			}
			configPath, err := setupConfigWritePath(*configFlag)
			if err != nil {
				return err
			}
			path, err := credentials.PathFor(configPath)
			if err != nil {
				return err
			}
			store, err := credentials.Load(path)
			if err != nil {
				return err
			}
			users := []string{strings.TrimSpace(user)}
			if users[0] == "" {
				users = store.Users(target)
			}
			removed := 0
			for _, name := range users {
				removed += store.RemoveUser(target, name)
			}
			if removed == 0 {
				return fmt.Errorf("凭据库中没有 %s 的记录", target)
			}
			stdout := command.OutOrStdout()
			if store.Empty() {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return err
				}
				fmt.Fprintf(stdout, "已清除 %s 的 %d 条凭据，凭据库已空并删除 %s\n", target, removed, path)
				return nil
			}
			if err := credentials.Save(path, store); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "已清除 %s 的 %d 条凭据（%s）\n", target, removed, path)
			return nil
		},
	}
	flags := command.Flags()
	flags.StringVar(&host, "host", "", "平台地址（与位置参数二选一）")
	flags.StringVar(&user, "user", "", "只清除该账号（缺省清除该站点全部账号）")
	return command
}
