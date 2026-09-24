package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"assistant/internal/credentials"
	"assistant/internal/dispatcher"
	"assistant/internal/instances"
	"assistant/internal/status"

	"github.com/spf13/cobra"
)

// loginOptions 是唯一登录入口的输入：host + username + password（无参数时交互式
// 询问）。没有第二种登录方式，凭据只有一份落点（credentials.json）。
type loginOptions struct {
	Host          string
	User          string
	Password      string
	PasswordStdin bool
	TOTP          string
	TokenName     string
	// Rotate 为真时忽略已存令牌，重建当前账号的用途令牌（令牌名保持不变）。
	Rotate bool
}

// newLoginCommand 是唯一登录入口：校验账号身份（含是否实例管理员），按身份派生
// 用途令牌并写入凭据库。
func newLoginCommand(configFlag *string) *cobra.Command {
	options := &loginOptions{}
	command := &cobra.Command{
		Use:   "login [host]",
		Short: "登录 Gitea（host + username + password），按身份派生用途令牌",
		Long: "登录并登记平台。不带参数时进入交互式询问（host → username → password）。\n\n" +
			"登录做三件事：\n" +
			"  1. 用账号密码向站点认证，确认账号名与是否实例管理员；\n" +
			"  2. 派生 purpose=mcp 的长期令牌（仓库/Issue 读写），供编辑器/CLI 的 MCP 使用；\n" +
			"  3. 账号是实例管理员时再派生 purpose=admin 的长期令牌，供\n" +
			"     setup/init/actions 等管理操作使用。\n\n" +
			"令牌写在平台标准配置目录的 credentials.json（Linux: ~/.config/Cosmic-Developers-Union/assistant/；\n" +
			"0600），按 (host, user, purpose) 唯一；重复登录时仍然有效的令牌原样复用，不重复创建\n" +
			"（ASSISTANT_CREDENTIALS 可显式指定凭据库位置）。\n\n" +
			"  assistant login list            列出平台、身份与该站点的各用途令牌\n" +
			"  assistant login remove <host>   移除平台条目与该站点的本地凭据",
		Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			host := ""
			if len(args) == 1 {
				host = args[0]
			}
			return runLogin(command, *configFlag, host, options)
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.Host, "host", "", "平台地址（与位置参数二选一；都缺省时交互式询问）")
	flags.StringVar(&options.User, "user", "", "登录账号（缺省交互式询问）")
	flags.StringVar(&options.Password, "password", "", "账号密码（仅用于本次派生令牌，不落盘；建议用 --password-stdin）")
	flags.BoolVar(&options.PasswordStdin, "password-stdin", false, "从标准输入读取密码（非交互环境）")
	flags.StringVar(&options.TOTP, "totp", "", "双因素验证码")
	flags.StringVar(&options.TokenName, "token-name", "", "令牌名（缺省按 host+账号+用途确定性派生）")
	flags.BoolVar(&options.Rotate, "rotate", false, "忽略已存令牌，重新创建该账号的用途令牌（旧同名令牌在站点上删除）")
	command.AddCommand(newLoginListCommand(configFlag), newLoginRemoveCommand(configFlag))
	return command
}

func runLogin(command *cobra.Command, configPath, argHost string, options *loginOptions) error {
	prompts := newPromptSession(command)
	host, err := resolveLoginHost(command.Context(), configPath, argHost, options.Host)
	if err != nil {
		// 交互式：把「从配置/remote 推断不出来」变成一次询问，而不是报错
		host, err = prompts.line("Gitea 站点地址（如 https://gitea.example.com）：")
		if err != nil {
			return err
		}
	}
	if host == "" {
		return fmt.Errorf("缺少站点地址（--host / 位置参数，或交互式输入）")
	}
	return runIdentityLogin(command, prompts, host, configPath, options)
}

// promptSession 承载一次登录的交互式询问。整个流程共用一个 bufio.Reader：
// 每次新建 reader 会预读并丢掉后续行，导致 host/账号/密码三次询问只拿到第一行。
type promptSession struct {
	command *cobra.Command
	reader  *bufio.Reader
}

func newPromptSession(command *cobra.Command) *promptSession {
	return &promptSession{command: command, reader: bufio.NewReader(command.InOrStdin())}
}

// interactive 判断能否向用户提问：stdin 是终端，或调用方显式注入了输入源（测试/管道）。
func (p *promptSession) interactive() bool {
	if p.command.InOrStdin() != os.Stdin {
		return true
	}
	return isTerminal(os.Stdin)
}

// secret 读取一行密码输入：只去掉行尾换行，不去首尾空格（密码里的空格是有效字符）。
func (p *promptSession) secret(prompt string) (string, error) {
	if !p.interactive() {
		return "", fmt.Errorf("非交互环境请用 --password-stdin 提供密码")
	}
	fmt.Fprint(p.command.ErrOrStderr(), prompt)
	line, err := p.reader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("读取输入失败: %w", err)
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

// line 读取一行交互输入；非交互环境绝不等待（stdio MCP / CI 里不能挂起）。
func (p *promptSession) line(prompt string) (string, error) {
	if !p.interactive() {
		return "", fmt.Errorf("非交互环境请用参数提供")
	}
	fmt.Fprint(p.command.ErrOrStderr(), prompt)
	line, err := p.reader.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", fmt.Errorf("读取输入失败: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// resolveLoginHost 决定登录哪个平台：显式地址 > 配置中唯一实例 > Gitea remote 探测。
func resolveLoginHost(ctx context.Context, configPath, argHost, flagHost string) (string, error) {
	if host := firstNonEmpty(argHost, flagHost); host != "" {
		return strings.TrimRight(host, "/"), nil
	}
	if _, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath}); err == nil &&
		file != nil && giteaHostCount(file) == 1 {
		return strings.TrimRight(giteaChannels(file)[0].Host, "/"), nil
	}
	remote, ok := dispatcher.SelectGiteaRemote(".", func(candidate string) bool {
		return status.ProbeGitea(ctx, candidate)
	})
	if ok {
		return remote.Host, nil
	}
	return "", fmt.Errorf("请指定平台地址")
}

// upsertLoginInstance 按 host 更新/新增 gitea 通道条目；保留未触碰的字段。
func upsertLoginInstance(file *instances.File, host string) {
	upsertGiteaChannel(file, host)
}

func saveLoginFile(file *instances.File, path string) error {
	return saveConfig(file, path)
}

func newLoginListCommand(configFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出已登记的平台、身份与各用途令牌",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			path, file, err := resolveInstanceFile(commandOptions{ConfigPath: *configFlag})
			if err != nil {
				return err
			}
			stdout := command.OutOrStdout()
			store, credentialPath := loadCredentialStore(command, path)
			if file == nil || giteaHostCount(file) == 0 {
				if store.Empty() {
					fmt.Fprintln(stdout, "未登记任何平台（assistant login 添加）")
					return nil
				}
				for _, host := range credentialHosts(store) {
					fmt.Fprintf(stdout, "%s\trepos=0\tidentity=%s\ttokens=%s\n",
						host, describeIdentity(store, host), describePurposes(store, host))
				}
				fmt.Fprintf(stdout, "凭据库：%s\n", credentialPath)
				return nil
			}
			for _, channel := range giteaChannels(file) {
				fmt.Fprintf(stdout, "%s\trepos=%d\treviewer=%s\tmerger=%s\tidentity=%s\ttokens=%s\n",
					channel.Host, len(channel.Repos), channel.Reviewer, channel.Merger,
					describeIdentity(store, channel.Host), describePurposes(store, channel.Host))
			}
			if credentialPath != "" {
				fmt.Fprintf(stdout, "凭据库：%s\n", credentialPath)
			}
			return nil
		},
	}
}

// loadCredentialStore 读取凭据库；缺失/损坏时返回空库并告警（list 只做展示）。
func loadCredentialStore(command *cobra.Command, configPath string) (*credentials.File, string) {
	path, err := credentials.Path()
	if err != nil {
		return &credentials.File{}, ""
	}
	store, err := credentials.Load(path)
	if err != nil {
		fmt.Fprintf(command.ErrOrStderr(), "警告：读取凭据库失败（%v）\n", err)
		return &credentials.File{}, path
	}
	return store, path
}

// describeIdentity 描述站点当前登录身份（含管理员标记）。
func describeIdentity(store *credentials.File, host string) string {
	identity, ok := store.IdentityFor(host)
	if !ok {
		return "none"
	}
	if identity.IsAdmin {
		return "@" + identity.User + "(admin)"
	}
	return "@" + identity.User
}

// describePurposes 列出该站点已有的用途令牌（purpose@user）。
func describePurposes(store *credentials.File, host string) string {
	var parts []string
	for _, credential := range store.Credentials {
		if !sameHost(credential.Host, host) {
			continue
		}
		parts = append(parts, credential.Purpose+"@"+credential.User)
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

// credentialHosts 返回凭据库里出现过的站点（去重）。
func credentialHosts(store *credentials.File) []string {
	seen := map[string]bool{}
	var hosts []string
	add := func(host string) {
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

func newLoginRemoveCommand(configFlag *string) *cobra.Command {
	var user string
	command := &cobra.Command{
		Use:   "remove <host>",
		Short: "移除平台条目与该站点的本地凭据（不触碰 Gitea 侧账号/仓库/令牌）",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			path, file, err := resolveInstanceFile(commandOptions{ConfigPath: *configFlag})
			if err != nil {
				return err
			}
			host := strings.TrimRight(strings.TrimSpace(args[0]), "/")
			stdout := command.OutOrStdout()

			removedPlatform := false
			if file != nil {
				kept := file.Channels[:0]
				for _, channel := range file.Channels {
					if channel.Type == instances.ChannelGitea && sameHost(channel.Host, host) {
						removedPlatform = true
						continue
					}
					kept = append(kept, channel)
				}
				file.Channels = kept
			}

			credentialPath, err := credentials.Path()
			if err != nil {
				return err
			}
			store, err := credentials.Load(credentialPath)
			if err != nil {
				return err
			}
			users := []string{strings.TrimSpace(user)}
			if users[0] == "" {
				users = store.Users(host)
			}
			removedCredentials := 0
			for _, name := range users {
				removedCredentials += store.RemoveUser(host, name)
			}
			if !removedPlatform && removedCredentials == 0 {
				return fmt.Errorf("平台 %s 不在配置中，也没有本地凭据", host)
			}

			if store.Empty() {
				if err := os.Remove(credentialPath); err != nil && !os.IsNotExist(err) {
					return err
				}
			} else if err := credentials.Save(credentialPath, store); err != nil {
				return err
			}

			emptyFile := file == nil || configHasNoPlatform(file)
			if emptyFile {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return err
				}
				fmt.Fprintf(stdout, "已移除 %s（含 %d 条本地凭据）；配置无剩余平台，已删除 %s\n",
					host, removedCredentials, path)
				return nil
			}
			if !emptyFile {
				file.Normalize()
				if err := file.Validate(); err != nil {
					return err
				}
				if err := instances.Save(path, file); err != nil {
					return err
				}
			}
			fmt.Fprintf(stdout, "已移除 %s（含 %d 条本地凭据）\n", host, removedCredentials)
			return nil
		},
	}
	command.Flags().StringVar(&user, "user", "", "只清除该账号的本地凭据（缺省清除该站点全部账号）")
	return command
}
