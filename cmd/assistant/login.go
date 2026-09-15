package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"assistant/internal/credentials"
	"assistant/internal/dispatcher"
	"assistant/internal/instances"
	"assistant/internal/repoinstall"
	"assistant/internal/setup"
	"assistant/internal/status"

	"github.com/spf13/cobra"
)

type loginOptions struct {
	// MCP 是旧版的显式开关：等价于「只处理身份与 mcp 用途令牌」，保留兼容。
	MCP           bool
	User          string
	Password      string
	PasswordStdin bool
	TokenFile     string
	TOTP          string
	TokenName     string
	Host          string
	Token         string
	// Rotate 为真时忽略已存令牌，强制按当前账号重建 mcp 令牌（assistant auth rotate）。
	Rotate        bool
	OAuthClientID string
	OAuthSecret   string
	OAuthScope    string
	OAuthPort     int
}

// newLoginCommand 管理平台登记：`--user`（配合密码或令牌）走身份登录并派生 MCP
// 长期令牌；不带身份参数时保持 tea / OAuth 的平台登记流程。
func newLoginCommand(configFlag *string) *cobra.Command {
	options := &loginOptions{}
	command := &cobra.Command{
		Use:   "login [host]",
		Short: "登录并登记平台（--user 身份登录并派生 MCP 令牌；否则复用 tea / OAuth）",
		Long: "平台登记与管理：\n" +
			"  assistant login <host> --user alice          身份登录：校验账号（含是否管理员）并自动\n" +
			"                                               创建/复用该身份在 (host, user, mcp) 上的长期令牌\n" +
			"  assistant login <host> --user alice --token-file <f>  录入已有专用令牌（--user 为身份断言）\n" +
			"  assistant login <host>                       不带身份参数：优先复用 tea CLI 登录令牌，\n" +
			"                                               否则 OAuth 登录并把 refresh 凭据写入 config.json\n" +
			"  assistant login list                         列出已登记平台（凭据类型/账号/仓库数）\n" +
			"  assistant login remove <host>                移除平台（不触碰 Gitea 侧账号/仓库）\n" +
			"  assistant auth status                        查看本地身份与各用途令牌（凭据库）\n\n" +
			"配置落点：--config / ASSISTANT_CONFIG，否则平台标准配置目录；不写当前目录。\n" +
			"凭据落点：与 config.json 同目录的 credentials.json（0600）。\n" +
			"host 缺省取配置中唯一实例，其次在带 Gitea remote 的检出内探测。",
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
	flags.BoolVar(&options.MCP, "mcp", false, "（兼容）等价于 --user：只处理身份与 mcp 用途令牌")
	flags.StringVar(&options.User, "user", "", "登录账号（缺省取同站点 tea 登录 user；录入令牌时作为身份断言）")
	flags.StringVar(&options.Password, "password", "", "账号密码（仅用于换取/派生令牌，不落盘；建议改用 --password-stdin）")
	flags.BoolVar(&options.PasswordStdin, "password-stdin", false, "从标准输入读取密码；否则在终端中隐藏输入")
	flags.StringVar(&options.TokenFile, "token-file", "", "录入已有专用令牌（与密码参数互斥）")
	flags.StringVar(&options.TOTP, "totp", "", "密码认证需要的双因素验证码")
	flags.StringVar(&options.TokenName, "token-name", "", "创建的令牌名（缺省按 host+账号+用途确定性派生）")
	flags.StringVar(&options.Host, "host", "", "平台地址（与位置参数二选一）")
	flags.StringVar(&options.Token, "token", "", "访问令牌：配 --user/--mcp 时录入 MCP 专用令牌，否则登记为平台令牌")
	flags.StringVar(&options.OAuthClientID, "oauth-client-id", "",
		"OAuth2 Client ID（缺省用 Gitea 内置 tea 公共客户端）")
	flags.StringVar(&options.OAuthSecret, "oauth-client-secret", "", "OAuth2 客户端密钥（confidential 客户端才需要）")
	flags.StringVar(&options.OAuthScope, "oauth-scope", "",
		"授权 scope（如 all；缺省不带 scope。已有授权记录换 scope 会被 Gitea 拒绝，需撤销旧授权或用本参数对齐）")
	flags.IntVar(&options.OAuthPort, "oauth-port", 0, "本地回调端口（0 = 随机空闲端口；tea 公共客户端允许任意 loopback 端口）")
	command.AddCommand(newLoginListCommand(configFlag), newLoginRemoveCommand(configFlag))
	return command
}

func runLogin(command *cobra.Command, configPath, argHost string, options *loginOptions) error {
	ctx := command.Context()
	logf := func(format string, arguments ...any) {
		fmt.Fprintf(command.OutOrStdout(), format+"\n", arguments...)
	}
	host, err := resolveLoginHost(ctx, configPath, argHost, options.Host)
	if err != nil {
		return err
	}
	writePath, file, err := loadInstanceFileForSetup(configPath)
	if err != nil {
		return err
	}
	if file == nil {
		file = &instances.File{}
	}

	if options.MCP {
		return runIdentityLogin(command, host, writePath, file, options)
	}
	// 身份登录：显式 --user，或任何只对身份/用途令牌有意义的参数
	if options.User != "" || options.Password != "" || options.PasswordStdin ||
		options.TokenFile != "" || options.TOTP != "" || options.TokenName != "" {
		return runIdentityLogin(command, host, writePath, file, options)
	}

	// 令牌登录：显式 --token，或复用 tea CLI 在本站点的登录（本地配置，只读）
	token, source, teaHint := resolveLoginToken(host, options.Token, os.Getenv)
	if token == "" && teaHint != "" {
		logf("%s", teaHint)
	}
	if token != "" {
		client, err := status.NewClient(host, token)
		if err != nil {
			return err
		}
		login, isAdmin, authErr := client.AuthenticatedIdentity(ctx)
		if authErr != nil && strings.TrimSpace(options.Token) != "" {
			return fmt.Errorf("--token 不可用（%s）: %w", source, authErr)
		}
		if authErr == nil {
			upsertLoginInstance(file, host, func(instance *instances.Instance) {
				instance.AdminToken = token
			})
			if err := saveLoginFile(file, writePath); err != nil {
				return err
			}
			recordIdentity(host, writePath, login, isAdmin, logf)
			logf("平台 %s 已登录 @%s（令牌来源：%s），写入 %s", host, login, source, writePath)
			return nil
		}
		logf("检测到 tea CLI 登录但令牌不可用（%v），改用 OAuth 登录", authErr)
	}

	result, err := setup.OAuthLogin(ctx, setup.OAuthOptions{
		Host:         host,
		ClientID:     options.OAuthClientID,
		ClientSecret: options.OAuthSecret,
		Scope:        options.OAuthScope,
		Port:         options.OAuthPort,
		Log:          logf,
	})
	if err != nil {
		return err
	}
	credential := &instances.OAuthCredential{
		ClientID:     firstNonEmpty(options.OAuthClientID, setup.DefaultOAuthClientID),
		ClientSecret: strings.TrimSpace(options.OAuthSecret),
		RefreshToken: result.RefreshToken,
	}
	upsertLoginInstance(file, host, func(instance *instances.Instance) {
		instance.AdminOAuth = credential
	})
	if err := saveLoginFile(file, writePath); err != nil {
		return err
	}
	recordIdentity(host, writePath, result.Login, result.IsAdmin, logf)
	logf("平台 %s 已登录 @%s（凭据写入 %s）", host, result.Login, writePath)
	return nil
}

// recordIdentity 把「以谁的身份、是不是实例管理员」写入凭据库。它不写令牌：
// admin 令牌仍由 config.json 承载（迁移到凭据库是下一步），这里只落身份事实，
// 供 setup/init/actions 做管理员门禁。凭据库不可写时只告警，不推翻已完成的平台登记。
func recordIdentity(host, configPath, user string, isAdmin bool, logf func(string, ...any)) {
	if strings.TrimSpace(user) == "" {
		return
	}
	path, err := credentials.PathFor(configPath)
	if err != nil {
		logf("警告：无法定位凭据库（%v），本次未记录身份", err)
		return
	}
	file, err := credentials.Load(path)
	if err != nil {
		logf("警告：读取凭据库失败（%v），本次未记录身份", err)
		return
	}
	file.SetIdentity(credentials.Identity{Host: host, User: user, IsAdmin: isAdmin})
	if err := credentials.Save(path, file); err != nil {
		logf("警告：写入凭据库失败（%v），本次未记录身份", err)
		return
	}
	if !isAdmin {
		logf("账号 @%s 不是实例管理员：setup/init/actions 与机器人凭据不可用（仅 MCP 工具面）", user)
	}
	// 这条登记路径（tea/OAuth）只能记录身份，无法派生 mcp 令牌：Gitea 的建令牌端点
	// 只接受账号密码的 Basic Auth。不说明的话，MCP 会在下一次使用时以「没有匹配的
	// 登录凭据」失败，看起来像工具坏了。
	if _, ok := file.CredentialForUser(host, user, credentials.PurposeMCP); !ok {
		logf("提示：MCP 工具面还没有凭据（本次只登记了身份）。运行 "+
			"assistant login %s --user %s 派生 mcp 长期令牌，或用 --user %s --token-file <文件> 录入已有令牌",
			host, user, user)
	}
}

// resolveLoginToken 解析登录令牌：显式 --token 优先，否则复用 tea CLI 配置中
// 本站点的登录；返回来源描述（空表示没有可用令牌）与诊断提示（tea 有登录条目
// 但没有令牌时给出可行动说明）。
func resolveLoginToken(host, explicitToken string, getenv func(string) string) (token, source, hint string) {
	if value := strings.TrimSpace(explicitToken); value != "" {
		return value, "--token", ""
	}
	if value, path, ok := repoinstall.DetectTeaToken(host, getenv); ok {
		return value, "tea config（" + path + "）", ""
	}
	if status := repoinstall.InspectTeaLogin(host, getenv); status.Found && !status.HasToken {
		method := status.AuthMethod
		if method == "" {
			method = "未知认证方式"
		}
		hint = fmt.Sprintf(
			"检测到 tea CLI 有本站点登录 %q（%s，%s）但没有保存令牌：请重新 `tea login` 保存令牌，"+
				"或改用 --token 提供访问令牌",
			status.Name, status.Path, method)
	}
	return "", "", hint
}

// upsertLoginInstance 按 host 更新/新增实例条目；保留未触碰的字段。
func upsertLoginInstance(file *instances.File, host string, mutate func(*instances.Instance)) {
	for index := range file.Instances {
		if sameHost(file.Instances[index].Host, host) {
			file.Instances[index].Host = host
			mutate(&file.Instances[index])
			return
		}
	}
	instance := instances.Instance{Host: host}
	mutate(&instance)
	file.Instances = append(file.Instances, instance)
}

func saveLoginFile(file *instances.File, path string) error {
	file.Normalize()
	if err := file.Validate(); err != nil {
		return err
	}
	return instances.Save(path, file)
}

// resolveLoginHost 决定登录哪个平台：显式地址 > 配置中唯一实例 > Gitea remote 探测。
func resolveLoginHost(ctx context.Context, configPath, argHost, flagHost string) (string, error) {
	if host := firstNonEmpty(argHost, flagHost); host != "" {
		return strings.TrimRight(host, "/"), nil
	}
	if _, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath}); err == nil &&
		file != nil && len(file.Instances) == 1 {
		return strings.TrimRight(file.Instances[0].Host, "/"), nil
	}
	remote, ok := dispatcher.SelectGiteaRemote(".", func(candidate string) bool {
		return status.ProbeGitea(ctx, candidate)
	})
	if ok {
		return remote.Host, nil
	}
	return "", fmt.Errorf("请指定平台地址：assistant login <host>（或 --host；也可在带 Gitea remote 的检出内运行）")
}

func newLoginListCommand(configFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出已登记的平台",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			path, file, err := resolveInstanceFile(commandOptions{ConfigPath: *configFlag})
			if err != nil {
				return err
			}
			stdout := command.OutOrStdout()
			if file == nil || len(file.Instances) == 0 {
				fmt.Fprintln(stdout, "未登记任何平台（assistant setup 初始化，或 assistant login <host> 添加）")
				return nil
			}
			store, credentialPath := loadCredentialStore(command, path)
			for _, instance := range file.Instances {
				admin := "none"
				switch {
				case instance.AdminOAuth != nil:
					admin = "oauth"
				case instance.AdminToken != "":
					admin = "token"
				}
				fmt.Fprintf(stdout, "%s\trepos=%d\tadmin=%s\treviewer=%s\tmerger=%s\tmcp=%s\tidentity=%s\n",
					instance.Host, len(instance.Repos), admin, instance.Reviewer.Name, instance.Merger.Name,
					describeMCPCredential(store, instance), describeIdentity(store, instance.Host))
			}
			if credentialPath != "" {
				fmt.Fprintf(stdout, "凭据库：%s（assistant auth status 查看详情）\n", credentialPath)
			}
			return nil
		},
	}
}

// loadCredentialStore 读取凭据库；不存在或不可读时返回空库（list 只需展示现状，
// 不因凭据库缺失而失败）。
func loadCredentialStore(command *cobra.Command, configPath string) (*credentials.File, string) {
	path, err := credentials.PathFor(configPath)
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

// describeMCPCredential 描述实例的 mcp 凭据：凭据库优先，兼容旧 config.json 字段。
func describeMCPCredential(store *credentials.File, instance instances.Instance) string {
	if store != nil {
		credential, ok, err := store.CredentialForIdentity(instance.Host, credentials.PurposeMCP)
		switch {
		case err != nil:
			return "multiple"
		case ok:
			return "token@" + credential.User
		}
	}
	if instance.MCPToken != "" {
		if instance.MCPUser != "" {
			return "token@" + instance.MCPUser + "(legacy)"
		}
		return "token(legacy)"
	}
	// 有身份、没 mcp 令牌：旧的 tea/OAuth 登记路径留下的状态，明确标出来
	if _, ok := store.IdentityFor(instance.Host); ok {
		return "none(仅身份)"
	}
	return "none"
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

func newLoginRemoveCommand(configFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <host>",
		Short: "移除已登记的平台（不触碰 Gitea 侧账号/仓库）",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			path, file, err := resolveInstanceFile(commandOptions{ConfigPath: *configFlag})
			if err != nil {
				return err
			}
			if file == nil {
				return fmt.Errorf("没有可用的 config.json（--config / ASSISTANT_CONFIG / 标准配置目录）")
			}
			host := strings.TrimRight(strings.TrimSpace(args[0]), "/")
			kept := file.Instances[:0]
			removed := false
			for _, instance := range file.Instances {
				if sameHost(instance.Host, host) {
					removed = true
					continue
				}
				kept = append(kept, instance)
			}
			if !removed {
				return fmt.Errorf("平台 %s 不在配置中", host)
			}
			file.Instances = kept
			if len(file.Instances) == 0 && file.Weixin == nil {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return err
				}
				fmt.Fprintf(command.OutOrStdout(), "已移除平台 %s；配置无剩余平台，已删除 %s\n", host, path)
				return nil
			}
			file.Normalize()
			if err := file.Validate(); err != nil {
				return err
			}
			if err := instances.Save(path, file); err != nil {
				return err
			}
			fmt.Fprintf(command.OutOrStdout(), "已移除平台 %s（%s）\n", host, path)
			return nil
		},
	}
}
