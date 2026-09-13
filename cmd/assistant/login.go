package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"assistant/internal/dispatcher"
	"assistant/internal/instances"
	"assistant/internal/setup"
	"assistant/internal/status"

	"github.com/spf13/cobra"
)

type loginOptions struct {
	Host          string
	OAuthClientID string
	OAuthSecret   string
	OAuthPort     int
}

// newLoginCommand 管理平台登记：默认走 OAuth 登录并把 admin_oauth 凭据写入
// config.json（可只加平台，不必立刻 setup）；list/remove 管理已登记平台。
func newLoginCommand(configFlag *string) *cobra.Command {
	options := &loginOptions{}
	command := &cobra.Command{
		Use:   "login [host]",
		Short: "OAuth 登录并登记平台（写入 admin_oauth）；login list/remove 管理平台",
		Long: "平台登记与管理：\n" +
			"  assistant login <host>       OAuth 登录，把 refresh 凭据写入 config.json\n" +
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
	flags.StringVar(&options.Host, "host", "", "平台地址（与位置参数二选一）")
	flags.StringVar(&options.OAuthClientID, "oauth-client-id", "",
		"OAuth2 公共客户端 ID（缺省 Gitea 内置 tea 客户端）")
	flags.StringVar(&options.OAuthSecret, "oauth-client-secret", "", "OAuth2 客户端密钥（confidential 客户端才需要）")
	flags.IntVar(&options.OAuthPort, "oauth-port", 0, "本地回调端口（0 = 随机空闲端口）")
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
	result, err := setup.OAuthLogin(ctx, setup.OAuthOptions{
		Host:         host,
		ClientID:     options.OAuthClientID,
		ClientSecret: options.OAuthSecret,
		Port:         options.OAuthPort,
		Log:          logf,
	})
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
	credential := &instances.OAuthCredential{
		ClientID:     firstNonEmpty(options.OAuthClientID, setup.DefaultOAuthClientID),
		ClientSecret: strings.TrimSpace(options.OAuthSecret),
		RefreshToken: result.RefreshToken,
	}
	replaced := false
	for index := range file.Instances {
		if sameHost(file.Instances[index].Host, host) {
			file.Instances[index].Host = host
			file.Instances[index].AdminOAuth = credential
			replaced = true
			break
		}
	}
	if !replaced {
		file.Instances = append(file.Instances, instances.Instance{Host: host, AdminOAuth: credential})
	}
	file.Normalize()
	if err := file.Validate(); err != nil {
		return err
	}
	if err := instances.Save(writePath, file); err != nil {
		return err
	}
	logf("平台 %s 已登录 @%s，凭据写入 %s", host, result.Login, writePath)
	return nil
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
				fmt.Fprintf(stdout, "%s\trepos=%d\tadmin=%s\treviewer=%s\tmerger=%s\n",
					instance.Host, len(instance.Repos), admin, instance.Reviewer.Name, instance.Merger.Name)
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
			if len(file.Instances) == 0 {
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
