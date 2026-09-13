package main

import (
	"fmt"
	"os"
	"strings"

	"assistant/internal/instances"
	"assistant/internal/setup"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// promptPassword 在 TTY 上读取密码（不回显）。
func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(password)), nil
}

type setupOptions struct {
	Host               string
	AdminToken         string
	AdminTokenFile     string
	AdminUser          string
	AdminPassword      string
	OAuth              bool
	Relogin            bool
	OAuthClientID      string
	OAuthClientSecret  string
	OAuthPort          int
	Repos              []string
	ReviewerName       string
	MergerName         string
	EmailDomain        string
	RequiredApprovals  int64
	CreateRepos        bool
	DryRun             bool
	AllowAdminOverride bool
}

func newSetupCommand(configFlag *string) *cobra.Command {
	options := &setupOptions{}
	command := &cobra.Command{
		Use:   "setup",
		Short: "初始化实例与仓库：建机器人账号/令牌、配协作者与分支保护、补齐标签",
		Long: "初始化 Gitea 实例与仓库，使「评审 → 批准 → 会签 → 自动合并」闭环成立：\n" +
			"  1. 复用/创建 reviewer（默认 ai）与 merger（默认 merge）账号；\n" +
			"  2. 生成访问令牌（已配置且有效的令牌直接复用）；\n" +
			"  3. 把两个账号加为仓库协作者（write），并补齐与 sync 相同口径的标签体系；\n" +
			"  4. 在默认分支配置分支保护（required approvals、驳回阻塞、过期批准作废、落后分支阻塞）；\n" +
			"  5. 把结果写回 config.json（0600）。\n\n" +
			"仓库清单可省略（--repos 与配置文件都为空时只初始化实例：建号与令牌，\n" +
			"不触碰任何仓库），之后再次运行 setup 补齐仓库即可。\n" +
			"管理员凭据支持多种方式：--admin-token、--admin-token-file、\n" +
			"--oauth（浏览器 OAuth2 登录，令牌不落盘）或 --admin-user/--admin-password。\n" +
			"全流程幂等，可重复执行。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runSetup(command, *configFlag, options)
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.Host, "host", "", "Gitea 站点根地址（缺省取配置文件中的 instance）")
	flags.StringVar(&options.AdminToken, "admin-token", "", "管理员访问令牌（可选，见 --oauth / --admin-user）")
	flags.StringVar(&options.AdminTokenFile, "admin-token-file", "", "从文件读取管理员令牌（避免 shell 历史/进程参数泄露）")
	flags.StringVar(&options.AdminUser, "admin-user", "", "管理员账号（换取长期令牌；缺密码时交互式输入）")
	flags.StringVar(&options.AdminPassword, "admin-password", "", "管理员密码（仅用于换取令牌，不落盘）")
	flags.BoolVar(&options.OAuth, "oauth", false, "用浏览器 OAuth2 登录（优先复用配置中的管理员凭据；--relogin 强制登录）")
	flags.BoolVar(&options.Relogin, "relogin", false, "忽略配置中的管理员凭据，强制 OAuth 重新登录（隐含 --oauth）")
	flags.StringVar(
		&options.OAuthClientID,
		"oauth-client-id",
		"",
		"OAuth2 Client ID（缺省用 Gitea 内置 tea 公共客户端；旧版 Gitea 需自建公共应用）",
	)
	flags.StringVar(&options.OAuthClientSecret, "oauth-client-secret", "", "OAuth2 Client Secret（公共客户端留空）")
	flags.IntVar(&options.OAuthPort, "oauth-port", 0, "OAuth 本地回调端口（缺省随机空闲端口；confidential 客户端需与注册的重定向 URI 端口一致）")
	flags.StringSliceVar(&options.Repos, "repos", nil, "仓库 owner/name（逗号分隔可多个；缺省取配置文件；两者皆空时只初始化账号与令牌）")
	flags.StringVar(&options.ReviewerName, "reviewer", "", "内容评审账号名（缺省 ai）")
	flags.StringVar(&options.MergerName, "merger", "", "状态评审/会签账号名（缺省 merge）")
	flags.StringVar(&options.EmailDomain, "email-domain", "", "机器人邮箱域名（缺省从 host 推导）")
	flags.Int64Var(&options.RequiredApprovals, "required-approvals", 0, "分支保护 required approvals（缺省 2）")
	flags.BoolVar(&options.AllowAdminOverride, "allow-admin-override", false,
		"允许管理员绕过分支保护（缺省关闭：勾选「管理员须遵守分支保护规则」）")
	flags.BoolVar(&options.CreateRepos, "create-repos", false, "仓库不存在时自动创建（私有，auto_init）")
	flags.BoolVar(&options.DryRun, "dry-run", false, "只输出将要执行的动作，不做任何写操作")
	return command
}

func runSetup(command *cobra.Command, configPath string, options *setupOptions) error {
	stdout := command.OutOrStdout()
	logf := func(format string, arguments ...any) {
		fmt.Fprintf(stdout, "[setup] %s\n", fmt.Sprintf(format, arguments...))
	}

	// 读取已有配置（--config 允许指向尚不存在的文件；否则依次找当前目录与
	// 平台标准配置目录），并确定写回路径。
	_, file, err := loadInstanceFileForSetup(configPath)
	if err != nil {
		return err
	}
	writePath, err := setupConfigWritePath(configPath)
	if err != nil {
		return err
	}
	// 定位目标 instance：--host 指定，否则要求配置文件恰好一个 instance
	host := strings.TrimRight(strings.TrimSpace(options.Host), "/")
	var existing *instances.Instance
	if file != nil {
		switch {
		case host == "" && len(file.Instances) == 1:
			existing = &file.Instances[0]
			host = existing.Host
		case host != "":
			for index := range file.Instances {
				if file.Instances[index].Host == host {
					existing = &file.Instances[index]
					break
				}
			}
		case len(file.Instances) != 1:
			return fmt.Errorf("配置文件有多个 instance，请用 --host 指定要初始化的站点")
		}
	}
	if host == "" {
		return fmt.Errorf("缺少站点：--host 或配置文件中的 instance.host")
	}

	repos := cleanStrings(options.Repos)
	if len(repos) == 0 && existing != nil {
		repos = existing.RepoNames()
	}
	// 允许空仓库清单：只初始化实例（账号/令牌/OAuth 凭据），仓库配置留给
	// 之后的 setup 运行补齐。

	// 管理员凭据优先级：显式令牌（--admin-token/-file）> 配置里的 admin_token
	// > GITEA_ACCESS_TOKEN > 配置里的 admin_oauth（用 refresh token 换取，不弹
	// 浏览器）> --admin-user/--admin-password > OAuth 登录（需要 --oauth）。
	// 也就是说：配置里已有可用管理凭据时，setup 重复运行不会要求重新登录；
	// --relogin 可强制忽略凭据重新走 OAuth。
	adminToken := strings.TrimSpace(options.AdminToken)
	if adminToken == "" && strings.TrimSpace(options.AdminTokenFile) != "" {
		data, readErr := os.ReadFile(options.AdminTokenFile)
		if readErr != nil {
			return fmt.Errorf("读取令牌文件: %w", readErr)
		}
		adminToken = strings.TrimSpace(string(data))
		if adminToken == "" {
			return fmt.Errorf("令牌文件为空：%s", options.AdminTokenFile)
		}
	}
	if adminToken == "" && existing != nil {
		adminToken = existing.AdminToken
	}
	if adminToken == "" {
		adminToken = strings.TrimSpace(os.Getenv("GITEA_ACCESS_TOKEN"))
	}
	if adminToken == "" && !options.Relogin && existing != nil && existing.AdminOAuth != nil {
		access, _, refreshErr := setup.RefreshOAuthToken(
			command.Context(), host,
			existing.AdminOAuth.ClientID, existing.AdminOAuth.ClientSecret, existing.AdminOAuth.RefreshToken,
			nil,
		)
		if refreshErr == nil {
			adminToken = access
			logf("复用配置中的 OAuth 凭据（已用 refresh token 换取短期管理员令牌）")
		} else {
			logf("配置中的 OAuth 凭据已失效（%v），需要重新登录", refreshErr)
		}
	}
	adminPassword := options.AdminPassword
	if adminToken == "" && options.AdminUser != "" && adminPassword == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("缺少管理员密码：--admin-password（非交互环境）")
		}
		adminPassword, err = promptPassword(fmt.Sprintf("请输入管理员 @%s 的密码：", options.AdminUser))
		if err != nil {
			return fmt.Errorf("读取密码: %w", err)
		}
	}
	useOAuth := adminToken == "" && options.AdminUser == "" && (options.OAuth || options.Relogin)
	if adminToken == "" && options.AdminUser == "" && !useOAuth {
		return fmt.Errorf(
			"缺少管理员凭据：--admin-token / --admin-token-file，或 --oauth 登录 / --admin-user 密码")
	}

	var oauth *setup.OAuthOptions
	if useOAuth {
		oauth = &setup.OAuthOptions{
			Host:         host,
			ClientID:     options.OAuthClientID,
			ClientSecret: options.OAuthClientSecret,
			Port:         options.OAuthPort,
			Log:          logf,
		}
	}

	setupOptions := setup.Options{
		Host:               host,
		AdminToken:         adminToken,
		AdminUser:          options.AdminUser,
		AdminPassword:      adminPassword,
		OAuth:              oauth,
		Repos:              repos,
		ReviewerName:       options.ReviewerName,
		MergerName:         options.MergerName,
		EmailDomain:        options.EmailDomain,
		RequiredApprovals:  options.RequiredApprovals,
		AllowAdminOverride: options.AllowAdminOverride,
		CreateRepos:        options.CreateRepos,
		DryRun:             options.DryRun,
		Existing:           existing,
		Log:                logf,
	}
	admin, err := setup.NewAdmin(command.Context(), setupOptions)
	if err != nil {
		return err
	}
	instance, err := setup.Run(command.Context(), setupOptions, admin)
	if err != nil {
		return err
	}
	if useOAuth && instance.AdminToken == "" {
		logf("提示：OAuth 令牌不写入配置（会过期）；automerge 的必要检查门禁将回退为严格模式")
	}

	if file == nil {
		file = &instances.File{}
	}
	replaced := false
	for index := range file.Instances {
		if file.Instances[index].Host == instance.Host {
			file.Instances[index] = instance
			replaced = true
			break
		}
	}
	if !replaced {
		file.Instances = append(file.Instances, instance)
	}
	file.Normalize()
	if err := file.Validate(); err != nil {
		return err
	}

	if options.DryRun {
		logf("dry-run：未写入 %s", writePath)
	} else {
		if err := instances.Save(writePath, file); err != nil {
			return err
		}
		logf("配置已写入 %s（0600）", writePath)
	}
	logf("完成：%s reviewer=%s merger=%s repos=%d", instance.Host,
		instance.Reviewer.Name, instance.Merger.Name, len(instance.Repos))
	return nil
}

// loadInstanceFileForSetup 读取已有配置供 setup 增量更新：显式 --config /
// ASSISTANT_CONFIG 指向不存在的文件是允许的（首次创建）；否则用平台标准配置
// 目录（不存在时返回 nil）。不读当前目录 config.json。
func loadInstanceFileForSetup(configPath string) (string, *instances.File, error) {
	path := strings.TrimSpace(configPath)
	if path == "" {
		path = strings.TrimSpace(os.Getenv("ASSISTANT_CONFIG"))
	}
	if path == "" {
		standard, err := instances.DefaultConfigPath()
		if err != nil {
			return "", nil, nil
		}
		path = standard
	}
	if _, err := os.Stat(path); err == nil {
		file, err := instances.Load(path)
		return path, file, err
	} else if !os.IsNotExist(err) {
		return "", nil, err
	}
	return path, nil, nil
}

func cleanStrings(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			cleaned = append(cleaned, value)
		}
	}
	return cleaned
}
