package main

import (
	"errors"
	"fmt"

	"assistant/internal/credentials"
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
			"  - secret MERGE_TOKEN = 该项目专属 merge 令牌（唯一必需项）\n\n" +
			"身份是约定（内容评审 ai、状态评审/合并 merge），sync 用 Actions 内置\n" +
			"令牌，分支保护读取/评审请求维护/会签/合并都用 merge 令牌，因此无需\n" +
			"variable 或额外的管理员令牌。名称不带 GITEA_ 前缀（Gitea 保留前缀），\n" +
			"workflow 里作为 automerge job 的 GITEA_ACCESS_TOKEN。Gitea 的 secret 值只写不可读、\n" +
			"无法比对，因此每次运行都会覆盖写入；令牌轮换（如 setup 重建令牌）后\n" +
			"必须重新运行本命令，否则仓库 workflow 会因陈旧令牌静默失败。",
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
		return fmt.Errorf("需要多实例配置：--config / ASSISTANT_CONFIG（assistant setup / login 写入）")
	}

	var runErrors []error
	for _, instance := range file.Instances {
		if len(instance.Repos) == 0 {
			logf("%s: 没有仓库，跳过", instance.Host)
			continue
		}
		mergeCredential, err := tokenForPurpose(configPath, instance.Host, credentials.PurposeMerge)
		if err != nil {
			runErrors = append(runErrors, err)
			continue
		}
		if err := requireAdminIdentity(configPath, instance.Host); err != nil {
			runErrors = append(runErrors, err)
			continue
		}
		if options.DryRun {
			if err := setup.ConfigureActions(command.Context(), nil, instance, mergeCredential.Token, true, logf); err != nil {
				runErrors = append(runErrors, err)
			}
			continue
		}
		adminCredential, err := tokenForPurpose(configPath, instance.Host, credentials.PurposeAdmin)
		if err != nil {
			runErrors = append(runErrors, err)
			continue
		}
		admin, err := setup.NewAdmin(command.Context(), setup.Options{
			Host:       instance.Host,
			AdminToken: adminCredential.Token,
			Log:        logf,
		})
		if err != nil {
			runErrors = append(runErrors, err)
			continue
		}
		if err := setup.ConfigureActions(command.Context(), admin, instance, mergeCredential.Token, false, logf); err != nil {
			runErrors = append(runErrors, err)
			continue
		}
		logf("%s: 完成 %d 个仓库的 Actions 配置", instance.Host, len(instance.Repos))
	}
	return errors.Join(runErrors...)
}
