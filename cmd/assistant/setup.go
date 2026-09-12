package main

import (
	"fmt"
	"os"
	"strings"

	"assistant/internal/instances"
	"assistant/internal/setup"

	"github.com/spf13/cobra"
)

type setupOptions struct {
	Host              string
	AdminToken        string
	AdminUser         string
	AdminPassword     string
	Repos             []string
	ReviewerName      string
	MergerName        string
	EmailDomain       string
	RequiredApprovals int64
	CreateRepos       bool
	DryRun            bool
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
			"全流程幂等，可重复执行。管理员凭据用 --admin-token，或用 --admin-user/--admin-password 现场换取令牌。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runSetup(command, *configFlag, options)
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.Host, "host", "", "Gitea 站点根地址（缺省取配置文件中的 instance）")
	flags.StringVar(&options.AdminToken, "admin-token", "", "管理员访问令牌")
	flags.StringVar(&options.AdminUser, "admin-user", "", "管理员账号（与 --admin-password 搭配生成令牌）")
	flags.StringVar(&options.AdminPassword, "admin-password", "", "管理员密码（仅用于生成令牌，不落盘）")
	flags.StringSliceVar(&options.Repos, "repos", nil, "仓库 owner/name（逗号分隔可多个；缺省取配置文件中的 repos）")
	flags.StringVar(&options.ReviewerName, "reviewer", "", "内容评审账号名（缺省 ai）")
	flags.StringVar(&options.MergerName, "merger", "", "状态评审/会签账号名（缺省 merge）")
	flags.StringVar(&options.EmailDomain, "email-domain", "", "机器人邮箱域名（缺省从 host 推导）")
	flags.Int64Var(&options.RequiredApprovals, "required-approvals", 0, "分支保护 required approvals（缺省 2）")
	flags.BoolVar(&options.CreateRepos, "create-repos", false, "仓库不存在时自动创建（私有，auto_init）")
	flags.BoolVar(&options.DryRun, "dry-run", false, "只输出将要执行的动作，不做任何写操作")
	return command
}

func runSetup(command *cobra.Command, configPath string, options *setupOptions) error {
	stdout := command.OutOrStdout()
	logf := func(format string, arguments ...any) {
		fmt.Fprintf(stdout, "[setup] %s\n", fmt.Sprintf(format, arguments...))
	}

	// setup 允许 --config 指向尚不存在的文件（首次初始化即创建）
	path := strings.TrimSpace(configPath)
	if path == "" {
		path = strings.TrimSpace(os.Getenv("ASSISTANT_CONFIG"))
	}
	var file *instances.File
	var err error
	if path != "" {
		if _, statErr := os.Stat(path); statErr == nil {
			if file, err = instances.Load(path); err != nil {
				return err
			}
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
	} else if _, statErr := os.Stat("config.json"); statErr == nil {
		path = "config.json"
		if file, err = instances.Load(path); err != nil {
			return err
		}
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
	if len(repos) == 0 {
		return fmt.Errorf("缺少仓库：--repos owner/name[,...]")
	}

	// 管理员令牌优先级：--admin-token > 配置文件 admin_token > GITEA_ACCESS_TOKEN
	adminToken := strings.TrimSpace(options.AdminToken)
	if adminToken == "" && existing != nil {
		adminToken = existing.AdminToken
	}
	if adminToken == "" {
		adminToken = strings.TrimSpace(os.Getenv("GITEA_ACCESS_TOKEN"))
	}

	setupOptions := setup.Options{
		Host:              host,
		AdminToken:        adminToken,
		AdminUser:         options.AdminUser,
		AdminPassword:     options.AdminPassword,
		Repos:             repos,
		ReviewerName:      options.ReviewerName,
		MergerName:        options.MergerName,
		EmailDomain:       options.EmailDomain,
		RequiredApprovals: options.RequiredApprovals,
		CreateRepos:       options.CreateRepos,
		DryRun:            options.DryRun,
		Existing:          existing,
		Log:               logf,
	}
	admin, err := setup.NewAdmin(command.Context(), setupOptions)
	if err != nil {
		return err
	}
	instance, err := setup.Run(command.Context(), setupOptions, admin)
	if err != nil {
		return err
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

	writePath := path
	if writePath == "" {
		writePath = configPath
	}
	if writePath == "" {
		writePath = "config.json"
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

func cleanStrings(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			cleaned = append(cleaned, value)
		}
	}
	return cleaned
}
