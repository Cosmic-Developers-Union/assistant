package main

import (
	"context"
	"errors"
	"fmt"

	"assistant/internal/instances"
	"assistant/internal/setup"

	"github.com/spf13/cobra"
)

type actionsOptions struct {
	DryRun bool
}

// newActionsCommand 构造 actions 命令：把 config.json 中每个仓库所需的
// Actions secrets 幂等写入（仓库内 workflow 消费）。
func newActionsCommand(configFlag *string) *cobra.Command {
	options := &actionsOptions{}
	command := &cobra.Command{
		Use:   "actions",
		Short: "为配置中的仓库写入 Actions secrets（merge 令牌）",
		Long: "为 config.json 中的每个仓库幂等写入仓库级 Actions 配置：\n" +
			"  - secret STATE_TOKEN              = 该项目专属 merge 令牌（必需）\n" +
			"  - secret BRANCH_PROTECTION_TOKEN  = 静态 admin_token（有则写）\n\n" +
			"身份是约定（内容评审 ai、状态评审/合并 merge），sync 用 Actions 内置\n" +
			"令牌，因此无需 variable。名称不带 GITEA_ 前缀（Gitea 保留前缀），\n" +
			"workflow 里映射到 GITEA_STATE_TOKEN / GITEA_BRANCH_PROTECTION_TOKEN。\n" +
			"Gitea 的 secret 值只写不可读、无法比对，因此每次运行都会覆盖写入；\n" +
			"令牌轮换（如 setup 重建令牌）后必须重新运行本命令，否则仓库 workflow\n" +
			"会因陈旧令牌静默失败。管理员凭据取自配置（admin_token 或 admin_oauth）。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runActions(command, *configFlag, options)
		},
	}
	command.Flags().BoolVar(&options.DryRun, "dry-run", false, "只输出将要写入的配置，不做任何写操作")
	return command
}

func runActions(command *cobra.Command, configPath string, options *actionsOptions) error {
	stdout := command.OutOrStdout()
	logf := func(format string, arguments ...any) {
		fmt.Fprintf(stdout, "[actions] %s\n", fmt.Sprintf(format, arguments...))
	}
	_, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return err
	}
	if file == nil {
		return fmt.Errorf("需要多实例配置：--config / ASSISTANT_CONFIG / ./config.json")
	}

	var runErrors []error
	for _, instance := range file.Instances {
		if len(instance.Repos) == 0 {
			logf("%s: 没有仓库，跳过", instance.Host)
			continue
		}
		if len(instance.Merger.Token) == 0 {
			runErrors = append(runErrors, fmt.Errorf("%s: 缺少 merger 令牌（先运行 setup）", instance.Host))
			continue
		}
		if options.DryRun {
			if err := setup.ConfigureActions(command.Context(), nil, instance, true, logf); err != nil {
				runErrors = append(runErrors, err)
			}
			continue
		}
		adminToken, err := adminTokenForInstance(command.Context(), instance, logf)
		if err != nil {
			runErrors = append(runErrors, err)
			continue
		}
		admin, err := setup.NewAdmin(command.Context(), setup.Options{
			Host:       instance.Host,
			AdminToken: adminToken,
			Log:        logf,
		})
		if err != nil {
			runErrors = append(runErrors, err)
			continue
		}
		if err := setup.ConfigureActions(command.Context(), admin, instance, false, logf); err != nil {
			runErrors = append(runErrors, err)
			continue
		}
		logf("%s: 完成 %d 个仓库的 Actions 配置", instance.Host, len(instance.Repos))
	}
	return errors.Join(runErrors...)
}

// adminTokenForInstance 解析管理员令牌：静态 admin_token 优先；否则用
// admin_oauth 的 refresh token 现场换取短期 access token。
func adminTokenForInstance(
	ctx context.Context,
	instance instances.Instance,
	logf func(string, ...any),
) (string, error) {
	if instance.AdminToken != "" {
		return instance.AdminToken, nil
	}
	if instance.AdminOAuth == nil {
		return "", fmt.Errorf("%s: 缺少管理员凭据（admin_token 或 admin_oauth；先运行 setup）", instance.Host)
	}
	access, _, err := setup.RefreshOAuthToken(
		ctx, instance.Host,
		instance.AdminOAuth.ClientID, instance.AdminOAuth.ClientSecret, instance.AdminOAuth.RefreshToken,
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("%s: 刷新 OAuth 凭据失败: %w", instance.Host, err)
	}
	logf("%s: 已用 refresh token 换取短期管理员令牌", instance.Host)
	return access, nil
}
