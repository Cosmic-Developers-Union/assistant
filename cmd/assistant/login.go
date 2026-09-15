package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"assistant/internal/dispatcher"
	"assistant/internal/instances"
	"assistant/internal/repoinstall"
	"assistant/internal/setup"
	"assistant/internal/status"

	"github.com/spf13/cobra"
)

type loginOptions struct {
	MCP           bool
	User          string
	PasswordStdin bool
	TokenFile     string
	TOTP          string
	TokenName     string
	Host          string
	Token         string
	OAuthClientID string
	OAuthSecret   string
	OAuthScope    string
	OAuthPort     int
}

// newLoginCommand 管理平台登记：默认走 OAuth 登录并把 admin_oauth 凭据写入
// config.json（可只加平台，不必立刻 setup）；list/remove 管理已登记平台。
func newLoginCommand(configFlag *string) *cobra.Command {
	options := &loginOptions{}
	command := &cobra.Command{
		Use:   "login [host]",
		Short: "登录并登记平台（优先复用 tea CLI 登录，其次 OAuth）；list/remove 管理平台",
		Long: "平台登记与管理：\n" +
			"  assistant login <host> --mcp  独立 MCP 长期令牌（--user 密码认证，或 --token / --token-file 录入）\n" +
			"  assistant login <host>       优先复用 tea CLI 的登录令牌（--token 可显式指定），\n" +
			"                               否则 OAuth 登录并把 refresh 凭据写入 config.json\n" +
			"  assistant login list         列出已登记平台（凭据类型/仓库数/账号名）\n" +
			"  assistant login remove <host> 移除平台（不触碰 Gitea 侧账号/仓库）\n\n" +
			"配置落点：--config / ASSISTANT_CONFIG，否则平台标准配置目录；不写当前目录。\n" +
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
	flags.BoolVar(&options.MCP, "mcp", false, "独立 MCP 长期令牌：密码认证创建或手动录入，不复用 tea/管理令牌")
	flags.StringVar(&options.User, "user", "", "MCP 个人账号用户名（缺省取同站点 tea 登录的 user）")
	flags.BoolVar(&options.PasswordStdin, "password-stdin", false, "从标准输入读取 MCP 登录密码；否则在终端中隐藏输入")
	flags.StringVar(&options.TokenFile, "token-file", "", "从文件录入 MCP 专用长期令牌")
	flags.StringVar(&options.TOTP, "totp", "", "密码认证需要的双因素验证码")
	flags.StringVar(&options.TokenName, "token-name", "", "创建的 MCP 令牌名（缺省自动生成唯一名称）")
	flags.StringVar(&options.Host, "host", "", "平台地址（与位置参数二选一）")
	flags.StringVar(&options.Token, "token", "", "访问令牌（跳过 OAuth；缺省先尝试复用 tea CLI 在站点的登录）")
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
		return runMCPLogin(command, host, writePath, file, options)
	}
	if options.User != "" || options.PasswordStdin || options.TokenFile != "" || options.TOTP != "" || options.TokenName != "" {
		return fmt.Errorf("--user / --password-stdin / --token-file / --totp / --token-name 仅用于 --mcp")
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
		login, authErr := client.AuthenticatedUser(ctx)
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
	logf("平台 %s 已登录 @%s（凭据写入 %s）", host, result.Login, writePath)
	return nil
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
			_, file, err := resolveInstanceFile(commandOptions{ConfigPath: *configFlag})
			if err != nil {
				return err
			}
			stdout := command.OutOrStdout()
			if file == nil || len(file.Instances) == 0 {
				fmt.Fprintln(stdout, "未登记任何平台（assistant setup 初始化，或 assistant login <host> 添加）")
				return nil
			}
			for _, instance := range file.Instances {
				admin := "none"
				switch {
				case instance.AdminOAuth != nil:
					admin = "oauth"
				case instance.AdminToken != "":
					admin = "token"
				}
				mcp := "none"
				if instance.MCPToken != "" {
					mcp = "token"
				}
				fmt.Fprintf(stdout, "%s\trepos=%d\tadmin=%s\treviewer=%s\tmerger=%s\tmcp=%s\n",
					instance.Host, len(instance.Repos), admin, instance.Reviewer.Name, instance.Merger.Name, mcp)
			}
			return nil
		},
	}
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
