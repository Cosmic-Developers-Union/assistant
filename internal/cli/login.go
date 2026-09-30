package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/integration/gitea"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// passwordSource 是「本次派生/轮换令牌用的密码来源」。独立成类型是因为
// readIdentityPassword 只消费这三个字段：账号与站点都由调用方显式传入，
// loginOptions 的其余旗标（--host/--token-name/--rotate）不允许被密码流程
// 静默卷入。
type passwordSource struct {
	Password      string
	PasswordStdin bool
	TOTP          string
}

// loginOptions 是唯一登录入口的输入：host + username + password（无参数时交互式
// 询问）。没有第二种登录方式，凭据只有一份落点（credentials.json）。
type loginOptions struct {
	passwordSource
	Host      string
	User      string
	TokenName string
	// Rotate 为真时忽略已存令牌，重建当前账号的用途令牌（令牌名保持不变）。
	Rotate bool
	// Channel 是对话通道分支的输入（--name/--base-url/--api-base-url），
	// 只在 --type 不是 gitea 时有意义
	Channel loginChannelOptions
}

// loginTypes 是 login 认识的全部类型。取值分两处来源：gitea 是唯一有站点地址的
// 类型，其余三个与 channels[].type 同一套——写成常量拼接而不是另列一份，是为了让
// 「加一个平台」只需要改 channels 的类型定义，login 这边自动跟上。
func loginTypes() []string {
	return append([]string{instances.ChannelGitea}, credentials.ChannelPurposes()...)
}

// resolveLoginType 校验 --type。非法值必须列出全部合法取值：用户是从文档或
// 旧命令名猜着敲的，报「未知类型」等于没告诉他正确写法。
func resolveLoginType(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if slices.Contains(loginTypes(), value) {
		return value, nil
	}
	return "", fmt.Errorf("--type 必须是 %s 之一：%q", strings.Join(loginTypes(), "、"), raw)
}

// rejectForeignFlags 在旗标与 --type 不匹配时显式报错。静默忽略是这里最糟的
// 失败方式：敲了 --password-stdin 却什么也没发生，排查要一路翻到实现里才知道
// 那个旗标只对 gitea 有效。
func rejectForeignFlags(command *cobra.Command, loginType string, allowed []string) error {
	var rejected []string
	// LocalFlags 而不是 Flags：后者含从父命令继承的持久旗标（--type 自己），会被
	// 判成「这个类型不接受的旗标」——那样整条通道登录在任何真实命令树下都起不来。
	// 遍历用 VisitAll + Changed：LocalFlags 是临时拼出的集合，它的 Visit 只走自己
	// 的 actual 列表（永远为空），不按 Changed 走。
	command.LocalFlags().VisitAll(func(flag *pflag.Flag) {
		if flag.Changed && !slices.Contains(allowed, flag.Name) {
			rejected = append(rejected, "--"+flag.Name)
		}
	})
	if len(rejected) == 0 {
		return nil
	}
	sort.Strings(rejected)
	return fmt.Errorf("--type %s 下不接受 %s（该类型用 %s）",
		loginType, strings.Join(rejected, "、"), flagHintFor(loginType))
}

// flagHintFor 说清某个类型该用哪些旗标。
func flagHintFor(loginType string) string {
	switch loginType {
	case instances.ChannelWeixin:
		return "--name / --base-url"
	case instances.ChannelQQ, instances.ChannelTelegram:
		return "--name / --api-base-url（凭据与白名单交互式询问）"
	default:
		return "--host / --user / --password / --rotate"
	}
}

// giteaLoginFlags 是 Gitea 登录的旗标名（newLoginAddCommand 注册的那一组）。
var giteaLoginFlags = []string{"host", "user", "password", "password-stdin", "totp", "token-name", "rotate"}

// newLoginCommand 是 login 命令组：add（唯一登录入口）、list/remove（查看与
// 移除）、token（用途令牌的查看与轮换）。裸命令只打印帮助。
//
// --type 选择登录类型：gitea 登录站点并派生用途令牌；weixin 扫码、qq / telegram
// 登记通道凭据。三者都只往 credentials.json 写密钥，config.json 只留结构性的
// 通道条目。
func newLoginCommand(configFlag *string) *cobra.Command {
	loginType := instances.ChannelGitea
	command := &cobra.Command{
		Use:   "login",
		Short: "平台登录与本地凭据管理（add/list/remove/token）",
		Long: "登录的唯一入口，以及本地凭据库的管理命令组。凭据一律落在平台标准配置\n" +
			"目录的 credentials.json（0600，按 (host, user, purpose) 唯一），config.json\n" +
			"只描述结构（有哪些平台、通道、白名单、端点）。\n\n" +
			"--type 选择登录类型：\n" +
			"  gitea（缺省）host + username + password → 站点确认身份（账号名 + 是否实例\n" +
			"                管理员）→ 按身份派生用途令牌\n" +
			"  weixin          扫码登录，按通道键（weixin 或 weixin/<实例名>）写入\n" +
			"  qq              交互读入开放平台 AppID/AppSecret → 换取 access token 实测后写入\n" +
			"  telegram        交互读入 BotFather token → getMe 实测后写入\n\n" +
			"  assistant login add <host>                登录 Gitea 并按身份派生用途令牌\n" +
			"  assistant login add --type weixin         扫码登录微信 Bot\n" +
			"  assistant login add --type telegram       登记 Telegram 通道凭据\n" +
			"  assistant login list                      列出平台、身份与各用途令牌\n" +
			"  assistant login list --type telegram      列出该类型的通道实例与凭据状态\n" +
			"  assistant login remove <host>             移除平台条目与该站点的本地凭据\n" +
			"  assistant login token list|show|refresh   查看与轮换用途令牌",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	command.PersistentFlags().StringVar(&loginType, "type", instances.ChannelGitea,
		"登录类型："+strings.Join(loginTypes(), " | "))
	command.AddCommand(
		newLoginAddCommand(configFlag, &loginType),
		newLoginListCommand(configFlag, &loginType),
		newLoginRemoveCommand(configFlag, &loginType),
		newLoginTokenCommand(&loginType),
	)
	return command
}

// newLoginAddCommand 是唯一登录入口：按 --type 分派到 Gitea 登录（校验账号身份、
// 按身份派生用途令牌）或三个对话通道的凭据登记。
func newLoginAddCommand(configFlag *string, loginType *string) *cobra.Command {
	options := &loginOptions{}
	command := &cobra.Command{
		Use:   "add [host]",
		Short: "登录并登记凭据（Gitea 账号密码 / 微信扫码 / QQ 与 Telegram 通道凭据）",
		Long: "登录并登记平台或对话通道。Gitea 不带参数时进入交互式询问（host → username → password）。\n\n" +
			"--type gitea（缺省）登录做三件事：\n" +
			"  1. 用账号密码向站点认证，确认账号名与是否实例管理员；\n" +
			"  2. 派生 purpose=mcp 的长期令牌（仓库/Issue 读写），供编辑器/CLI 的 MCP 使用；\n" +
			"  3. 账号是实例管理员时再派生 purpose=admin 的长期令牌，供\n" +
			"     setup/init/actions 等管理操作使用。\n\n" +
			"--type weixin 走扫码（openclaw ilink）：终端展示二维码，手机扫码确认后把\n" +
			"  密钥写入凭据库，config.json 里留下通道条目。--name 决定写到哪个实例。\n" +
			"--type qq / telegram 交互读入凭据并先在线实测，通过才写盘。\n\n" +
			"令牌写在平台标准配置目录的 credentials.json（Linux: ~/.config/Cosmic-Developers-Union/assistant/；\n" +
			"0600），按 (host, user, purpose) 唯一；重复登录时仍然有效的令牌原样复用，不重复创建\n" +
			"（ASSISTANT_CREDENTIALS 可显式指定凭据库位置）。\n\n" +
			"  assistant login list            列出平台、身份与该站点的各用途令牌\n" +
			"  assistant login remove <host>   移除平台条目与该站点的本地凭据\n" +
			"  assistant login token refresh <host> [purpose]   轮换单条用途令牌",
		Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			resolvedType, err := resolveLoginType(derefLoginType(loginType))
			if err != nil {
				return err
			}
			host := ""
			if len(args) == 1 {
				host = args[0]
			}
			// 分派必须在解析 host 之前：weixin/qq/telegram 没有 Gitea 站点，
			// 让它们走 resolveLoginHost 会去探测当前 git 目录的 Gitea remote
			if resolvedType != instances.ChannelGitea {
				if host != "" {
					return fmt.Errorf("--type %s 不接受位置参数 <host>（它没有站点地址；"+
						"实例用 --name 命名）", resolvedType)
				}
				if err := rejectForeignFlags(command, resolvedType, channelLoginFlags); err != nil {
					return err
				}
				return runLoginAddChannel(command, *configFlag, resolvedType, &options.Channel)
			}
			if err := rejectForeignFlags(command, resolvedType, giteaLoginFlags); err != nil {
				return err
			}
			return runLogin(command, *configFlag, host, options)
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.Host, "host", "", "平台地址（与位置参数二选一；都缺省时交互式询问；仅 --type gitea）")
	flags.StringVar(&options.User, "user", "", "登录账号（缺省交互式询问；仅 --type gitea）")
	flags.StringVar(&options.Password, "password", "", "账号密码（仅用于本次派生令牌，不落盘；建议用 --password-stdin）")
	flags.BoolVar(&options.PasswordStdin, "password-stdin", false, "从标准输入读取密码（非交互环境）")
	flags.StringVar(&options.TOTP, "totp", "", "双因素验证码")
	flags.StringVar(&options.TokenName, "token-name", "", "令牌名（缺省按 host+账号+用途确定性派生）")
	flags.BoolVar(&options.Rotate, "rotate", false, "忽略已存令牌，重新创建该账号的用途令牌（旧同名令牌在站点上删除）")
	flags.StringVar(&options.Channel.Name, "name", "", "通道实例名（多开同一平台时区分用；缺省写匿名实例）")
	flags.StringVar(&options.Channel.BaseURL, "base-url", "",
		"weixin 专用：ilink API 根地址（缺省 "+instances.DefaultWeixinBaseURL+"）")
	flags.StringVar(&options.Channel.APIBaseURL, "api-base-url", "",
		"qq / telegram 专用：Bot API 根地址（可换自建反代）")
	return command
}

// channelLoginFlags 是对话通道分支认识的旗标（凭据与白名单交互式询问，不走旗标）。
var channelLoginFlags = []string{"name", "base-url", "api-base-url"}

// runLoginAddChannel 把 add 分派到三个对话平台的凭据登记。
func runLoginAddChannel(command *cobra.Command, configPath, loginType string, options *loginChannelOptions) error {
	switch loginType {
	case instances.ChannelWeixin:
		return runLoginAddWeixin(command, configPath, options)
	case instances.ChannelQQ:
		return runLoginAddQQ(command, configPath, options)
	case instances.ChannelTelegram:
		return runLoginAddTelegram(command, configPath, options)
	}
	return fmt.Errorf("--type %s 不支持登记凭据", loginType)
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

// secret 读取一行秘密输入：只去掉行尾换行，不去首尾空格（密码/token 里的空格是
// 有效字符）。nonInteractiveHint 是非交互环境下的出路提示——各类型的出路不同
// （Gitea 密码有 --password-stdin，通道密钥没有对应旗标），给错提示会把人引到
// 一个不存在的选项上。
func (p *promptSession) secret(prompt, nonInteractiveHint string) (string, error) {
	if !p.interactive() {
		return "", fmt.Errorf("非交互环境：%s", nonInteractiveHint)
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

// adminUsers 收集对话通道的白名单。
//
// qq / telegram 的空白名单是「全拒」——写完配置、通道起来了，却没有人能跟它
// 说话，而启动日志里看不出这一点。与其留一个死通道，不如在这里问清楚。
func (p *promptSession) adminUsers(channelType string) ([]string, error) {
	raw, err := p.line(fmt.Sprintf("%s 允许对话的用户 id（逗号分隔）：", channelType))
	if err != nil {
		return nil, err
	}
	users := splitUserList(raw)
	if len(users) == 0 {
		return nil, fmt.Errorf("%s 的 admin_users 空白名单时拒绝所有人；请至少填一个用户 id，"+
			"或登录后自己编辑 config.json 的该通道条目", channelType)
	}
	return users, nil
}

// splitUserList 解析逗号分隔的用户 id 列表（容忍中文逗号与空白）。
func splitUserList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，' || r == ';' || r == '；' || r == ' ' || r == '\t'
	})
	users := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		users = append(users, field)
	}
	return users
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
	remote, ok := gitea.SelectGiteaRemote(".", func(candidate string) bool {
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

func newLoginListCommand(configFlag *string, loginType *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出已登记的平台、身份与各用途令牌（--type 为通道时列出通道实例）",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			resolvedType, err := resolveLoginType(derefLoginType(loginType))
			if err != nil {
				return err
			}
			if resolvedType != instances.ChannelGitea {
				return runLoginListChannel(command, *configFlag, resolvedType)
			}
			return runLoginListGitea(command, *configFlag)
		},
	}
}

// runLoginListGitea 列出 Gitea 平台、身份与各用途令牌（只显示末 8 位，不出明文）。
func runLoginListGitea(command *cobra.Command, configPath string) error {
	path, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return err
	}
	stdout := command.OutOrStdout()
	store, credentialPath := loadCredentialStore(command, path)
	if file == nil || giteaHostCount(file) == 0 {
		if store.Empty() {
			fmt.Fprintln(stdout, "未登记任何平台（assistant login add 添加）")
			return nil
		}
		for _, host := range store.Hosts() {
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

func newLoginRemoveCommand(configFlag *string, loginType *string) *cobra.Command {
	var user string
	command := &cobra.Command{
		Use:   "remove <host>",
		Short: "移除平台条目与该站点的本地凭据（不触碰 Gitea 侧账号/仓库/令牌）",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			// remove 按 host 与账号操作凭据库，那是 Gitea 侧的概念。对话通道按
			// 通道键索引、身份是 bot 名，混进来要么删不掉、要么删出一条起不来的
			// 悬空通道条目——两种都很难事后看出来，所以在这里明确挡掉。
			resolvedType, err := resolveLoginType(derefLoginType(loginType))
			if err != nil {
				return err
			}
			if resolvedType != instances.ChannelGitea {
				return fmt.Errorf("--type %s 不适用于 remove：remove 只移除 Gitea 平台条目与该站点的"+
					"本地凭据；通道实例的增删改写 config.json 的 channels 列表", resolvedType)
			}
			return runLoginRemove(command, *configFlag, args[0], user)
		},
	}
	command.Flags().StringVar(&user, "user", "", "只清除该账号的本地凭据（缺省清除该站点全部账号）")
	return command
}

func runLoginRemove(command *cobra.Command, configPath, argHost, user string) error {
	path, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return err
	}
	host := strings.TrimRight(strings.TrimSpace(argHost), "/")
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
}
