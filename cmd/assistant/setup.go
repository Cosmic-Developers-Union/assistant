package main

import (
	"assistant/internal/config"
	"assistant/internal/credentials"
	"assistant/internal/instances"
	"assistant/internal/setup"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

type setupOptions struct {
	Host                string
	Repos               []string
	ReviewerName        string
	MergerName          string
	EmailDomain         string
	RequiredApprovals   int64
	StatusCheckContexts []string
	CreateRepos         bool
	DryRun              bool
	AllowAdminOverride  bool
}

func newSetupCommand(configFlag *string) *cobra.Command {
	options := &setupOptions{}
	command := &cobra.Command{
		Use:   "setup",
		Short: "初始化实例与仓库：建机器人账号/令牌、配协作者与分支保护、补齐标签",
		Long: "初始化 Gitea 实例与仓库，使「评审 → 批准 → 会签 → 自动合并」闭环成立：\n" +
			"  1. 复用/创建 reviewer（默认 ai）与 merger（默认 merge）账号；\n" +
			"  2. 为它们生成令牌（有效则复用），写入凭据库 purpose=review / merge；\n" +
			"  3. 把两个账号加为仓库协作者（write/admin），并补齐与 sync 相同口径的标签体系；\n" +
			"  4. 在默认分支配置分支保护（required approvals、驳回阻塞、过期批准作废、落后分支阻塞）；\n" +
			"  5. 扫描 merge 为管理员协作者的仓库，自动写入 MERGE_TOKEN secret；\n" +
			"  6. 把实例与仓库写回 config.json（0600）。\n\n" +
			"不使用任何独立的凭据参数：管理员令牌来自 assistant login 写入凭据库的\n" +
			"purpose=admin，因此运行前必须先用**管理员账号**登录：\n" +
			"  assistant login <host> --user <管理员账号>\n\n" +
			"仓库清单可省略（--repos 与配置文件都为空时只初始化实例：建号与令牌，\n" +
			"不触碰任何仓库），之后再次运行 setup 补齐仓库即可。全流程幂等。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runSetup(command, *configFlag, options)
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.Host, "host", "", "Gitea 站点根地址（缺省取配置文件中的 instance）")
	flags.StringSliceVar(&options.Repos, "repos", nil, "仓库 owner/name（逗号分隔可多个；缺省取配置文件；两者皆空时只初始化账号与令牌）")
	flags.StringVar(&options.ReviewerName, "reviewer", "", "内容评审账号名（缺省 ai）")
	flags.StringVar(&options.MergerName, "merger", "", "状态评审/会签账号名（缺省 merge）")
	flags.StringVar(&options.EmailDomain, "email-domain", "", "机器人邮箱域名（缺省从 host 推导）")
	flags.Int64Var(&options.RequiredApprovals, "required-approvals", 0, "分支保护 required approvals（缺省 2）")
	flags.StringSliceVar(&options.StatusCheckContexts, "status-check-contexts", nil,
		"合并前必须全绿的检查 context（逗号分隔，支持 glob）；配置后 automerge 在检查运行期间武装 Gitea 原生\nauto-merge，检查变绿即由服务端即时合并。缺省不改动服务端现状；勿填 assistant 自身工作流的 context")
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

	// 读取已有配置（--config 允许指向尚不存在的文件；否则用当前目录的 ./config.json），
	// 并确定写回路径。
	_, file, err := loadInstanceFileForSetup(configPath)
	if err != nil {
		return err
	}
	writePath, err := setupConfigWritePath(configPath)
	if err != nil {
		return err
	}
	// 定位目标 gitea 通道：--host 指定，否则要求配置文件恰好一个站点
	host := strings.TrimRight(strings.TrimSpace(options.Host), "/")
	var existing *instances.Channel
	var existingView *instances.Instance
	if file != nil {
		switch {
		case host == "" && giteaHostCount(file) == 1:
			existing = giteaChannels(file)[0]
			host = existing.Host
		case host != "":
			channel, ok := findGiteaChannel(file, host)
			if ok {
				existing = channel
			}
		case giteaHostCount(file) != 1 && len(file.Channels) > 0 && host == "":
			return fmt.Errorf("配置文件有多个站点，请用 --host 指定要初始化的 gitea 通道")
		}
	}
	if host == "" {
		return fmt.Errorf("缺少站点：--host 或配置文件中的 gitea 通道 host")
	}
	if existing != nil {
		view := channelInstance(existing)
		existingView = &view
	}
	// 能力门禁：本地身份记录显示不是管理员时立即拒绝（权威判定仍在服务端）
	if err := requireAdminIdentity(configPath, host); err != nil {
		return err
	}
	// 唯一的凭据来源：assistant login 写入的 admin 用途令牌
	adminCredential, err := tokenForPurpose(writePath, host, credentials.PurposeAdmin)
	if err != nil {
		return err
	}
	credentialPath, err := credentials.PathFor(writePath)
	if err != nil {
		return err
	}
	store, err := credentials.Load(credentialPath)
	if err != nil {
		return err
	}

	repos := cleanStrings(options.Repos)
	if len(repos) == 0 && existingView != nil {
		repos = existingView.RepoNames()
	}
	// 允许空仓库清单：只初始化实例（账号与令牌），仓库配置留给之后的 setup。

	setupOptions := setup.Options{
		Host:                host,
		AdminToken:          adminCredential.Token,
		Repos:               repos,
		ReviewerName:        options.ReviewerName,
		MergerName:          options.MergerName,
		EmailDomain:         options.EmailDomain,
		RequiredApprovals:   options.RequiredApprovals,
		StatusCheckContexts: options.StatusCheckContexts,
		AllowAdminOverride:  options.AllowAdminOverride,
		CreateRepos:         options.CreateRepos,
		DryRun:              options.DryRun,
		Existing:            existingView,
		ExistingCredentials: store.Credentials,
		Log:                 logf,
	}
	admin, err := setup.NewAdmin(command.Context(), setupOptions)
	if err != nil {
		return err
	}
	result, err := setup.Run(command.Context(), setupOptions, admin)
	if err != nil {
		return err
	}
	instance := result.Instance

	if file == nil {
		file = &instances.File{}
	}
	channel := upsertGiteaChannel(file, instance.Host)
	channel.Reviewer = instance.Reviewer.Name
	channel.Merger = instance.Merger.Name
	channel.Repos = instance.Repos

	if options.DryRun {
		logf("dry-run：未写入 %s 与 %s", writePath, credentialPath)
	} else {
		if err := saveConfig(file, writePath); err != nil {
			return err
		}
		for _, credential := range result.Credentials {
			store.SetCredential(credential)
		}
		if err := credentials.Save(credentialPath, store); err != nil {
			return err
		}
		logf("配置已写入 %s，机器人令牌写入 %s（0600）", writePath, credentialPath)
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
		if _, err := config.LoadBeside(path); err != nil {
			return "", nil, err
		}
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
