package cli

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	gitremote "github.com/Cosmic-Developers-Union/assistant/internal/integration/gitea"
	"github.com/Cosmic-Developers-Union/assistant/internal/project"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"
	"github.com/spf13/cobra"
)

func giteaInstance(name string) (string, *credentials.File, credentials.Gitea, error) {
	path, err := credentials.Path()
	if err != nil {
		return "", nil, credentials.Gitea{}, err
	}
	file, err := credentials.Load(path)
	if err != nil {
		return "", nil, credentials.Gitea{}, err
	}
	for _, entry := range file.Instances.Gitea {
		if entry.Name == name {
			return path, file, entry, nil
		}
	}
	return "", nil, credentials.Gitea{}, fmt.Errorf("Gitea 实例 %q 不存在；先 instance add gitea", name)
}
func newProvisionCommand() *cobra.Command {
	var admin, prefix, reviewer, merger, domain, reviewPassword, mergePassword string
	var dry bool
	root := &cobra.Command{Use: "provision", Short: "用管理员实例创建机器人账号与令牌", Args: cobra.NoArgs}
	cmd := &cobra.Command{Use: "gitea", Short: "创建 ai/merge 并立即保存各自凭据，不重置已有账号密码", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) (resultErr error) {
		path, file, entry, err := giteaInstance(admin)
		if err != nil {
			return err
		}
		defer func() { resultErr = credentials.RedactError(resultErr, entry.Token) }()
		if reviewer == merger {
			return fmt.Errorf("reviewer 与 merger 必须不同")
		}
		if prefix == "" {
			prefix = admin
		}
		client, err := project.NewClient(entry)
		if err != nil {
			return err
		}
		for _, role := range []struct{ user, passwordFile string }{{reviewer, reviewPassword}, {merger, mergePassword}} {
			name := prefix + "-" + role.user
			var old *credentials.Gitea
			for i := range file.Instances.Gitea {
				if file.Instances.Gitea[i].Name == name {
					old = &file.Instances.Gitea[i]
				}
			}
			for _, row := range rows(file) {
				if row.Name == name && row.Type != "gitea" {
					return fmt.Errorf("实例名 %s 被其他平台使用", name)
				}
			}
			password := ""
			if role.passwordFile != "" {
				password, err = newPrompts(cmd).secret(role.passwordFile, "机器人密码")
				if err != nil {
					return err
				}
			}
			account, err := client.ProvisionAccount(cmd.Context(), name, role.user, role.user+"@"+domain, password, old, dry)
			if err != nil {
				return err
			}
			if dry {
				cmd.Printf("演练：接入 @%s → %s（不写平台或本地凭据）\n", role.user, name)
				continue
			}
			if old == nil {
				file.Instances.Gitea = append(file.Instances.Gitea, account)
			} else {
				*old = account
			}
			if err := credentials.Save(path, file); err != nil {
				return fmt.Errorf("账号已接入，保存 %s 凭据失败: %w", name, err)
			}
			cmd.Printf("已接入 @%s，凭据保存为实例 %s\n", role.user, name)
		}
		return nil
	}}
	cmd.Flags().StringVar(&admin, "admin", "", "已登记的管理员实例名")
	cmd.MarkFlagRequired("admin")
	cmd.Flags().StringVar(&prefix, "name-prefix", "", "机器人实例名前缀（缺省管理员实例名）")
	cmd.Flags().StringVar(&reviewer, "reviewer", "ai", "内容评审账号")
	cmd.Flags().StringVar(&merger, "merger", "merge", "会签/合并账号")
	cmd.Flags().StringVar(&domain, "email-domain", "assistant.invalid", "新建账号邮箱域名")
	cmd.Flags().StringVar(&reviewPassword, "reviewer-password-file", "", "已有 reviewer 的密码文件，不回显")
	cmd.Flags().StringVar(&mergePassword, "merger-password-file", "", "已有 merger 的密码文件，不回显")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "只读检查，不创建账号/令牌或保存凭据")
	root.AddCommand(cmd)
	return root
}
func newTokenCommand() *cobra.Command {
	var passwordFile string
	cmd := &cobra.Command{Use: "token <实例名>", Short: "为已登记 Gitea 账号新建令牌并替换本地凭据，保留其他令牌", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		path, file, entry, err := giteaInstance(args[0])
		if err != nil {
			return err
		}
		password, err := newPrompts(cmd).secret(passwordFile, "密码")
		if err != nil {
			return err
		}
		token, err := project.IssueToken(cmd.Context(), entry.URL, entry.Username, password)
		if err != nil {
			return err
		}
		for i := range file.Instances.Gitea {
			if file.Instances.Gitea[i].Name == entry.Name {
				file.Instances.Gitea[i].Token = token
			}
		}
		if err := credentials.Save(path, file); err != nil {
			return fmt.Errorf("令牌已创建，保存凭据失败: %w", err)
		}
		cmd.Printf("已为 @%s 创建令牌并保存；旧平台令牌未撤销\n", entry.Username)
		return nil
	}}
	cmd.Flags().StringVar(&passwordFile, "password-file", "", "账号密码文件（非交互必需）")
	return cmd
}

func newProjectCommand() *cobra.Command {
	var instance, dir, repo, reviewer, merger, mergeInstance string
	var dry bool
	root := &cobra.Command{Use: "project", Short: "当前项目的 Gitea 标签、协作者、Actions 与 MCP", Args: cobra.NoArgs}
	root.PersistentFlags().StringVar(&instance, "instance", "", "明确选择个人 Gitea 实例")
	root.PersistentFlags().StringVar(&dir, "dir", ".", "当前项目目录")
	root.PersistentFlags().StringVar(&repo, "repo", "", "owner/name（缺省从匹配实例的 Git remote 推导）")
	root.PersistentFlags().BoolVar(&dry, "dry-run", false, "只读检查并报告，不写平台或文件")
	var checks []string
	configure := &cobra.Command{Use: "configure", Short: "规范标签、协作者和默认分支双批准保护", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, _, entry, err := giteaInstance(instance)
		if err != nil {
			return err
		}
		owner, name, err := projectRepo(dir, repo, entry.URL)
		if err != nil {
			return err
		}
		client, err := project.NewClient(entry)
		if err != nil {
			return err
		}
		err = client.Configure(cmd.Context(), project.RepositoryOptions{Owner: owner, Name: name, Reviewer: reviewer, Merger: merger, Checks: checks, DryRun: dry, Log: func(f string, a ...any) { fmt.Fprintf(cmd.ErrOrStderr(), f+"\n", a...) }})
		return credentials.RedactError(err, entry.Token)
	}}
	configure.Flags().StringVar(&reviewer, "reviewer", "ai", "内容评审账号（write 权限）")
	configure.Flags().StringVar(&merger, "merger", "merge", "合并账号（admin 权限，仍受分支保护）")
	configure.Flags().StringSliceVar(&checks, "required-checks", nil, "必要检查 context，缺省保留已有设置")
	root.AddCommand(configure)
	labels := &cobra.Command{Use: "labels", Short: "只规范当前仓库标签，不修改协作者或分支保护", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, _, entry, err := giteaInstance(instance)
		if err != nil {
			return err
		}
		owner, name, err := projectRepo(dir, repo, entry.URL)
		if err != nil {
			return err
		}
		client, err := status.NewClient(entry.URL, entry.Token)
		if err != nil {
			return err
		}
		var api status.API = client
		if dry {
			api = status.NewDryRunAPI(client, func(f string, a ...any) { fmt.Fprintf(cmd.ErrOrStderr(), f+"\n", a...) })
		}
		if err := status.NewManager(api).ReconcileLabels(cmd.Context(), status.Repository{Owner: owner, Name: name}); err != nil {
			return credentials.RedactError(err, entry.Token)
		}
		cmd.Printf("标签配置已处理：%s/%s\n", owner, name)
		return nil
	}}
	root.AddCommand(labels)
	install := &cobra.Command{Use: "install", Short: "安装项目工具配置", Args: cobra.NoArgs}
	uninstall := &cobra.Command{Use: "uninstall", Short: "只卸载本工具配置，保留账号、权限、secret 与其他工具", Args: cobra.NoArgs}
	action := &cobra.Command{Use: "action", Short: "安装 workflow 并注入 MERGE_TOKEN，文件变更需提交推送", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := projectDir(dir)
		if err != nil {
			return err
		}
		if err := project.InstallWorkflow(path, false, true); err != nil {
			return err
		}
		_, file, entry, err := giteaInstance(instance)
		if err != nil {
			return err
		}
		owner, name, err := projectRepo(path, repo, entry.URL)
		if err != nil {
			return err
		}
		token := ""
		for _, candidate := range file.Instances.Gitea {
			selected := candidate.Name == mergeInstance
			if mergeInstance == "" {
				selected = candidate.Username == "merge" && credentials.NormalizeHost(candidate.URL) == credentials.NormalizeHost(entry.URL)
			}
			if selected {
				if credentials.NormalizeHost(candidate.URL) != credentials.NormalizeHost(entry.URL) {
					return fmt.Errorf("合并实例与项目站点不匹配")
				}
				if token != "" {
					return fmt.Errorf("多个 merge 实例，请指定 --merge-instance")
				}
				token = candidate.Token
			}
		}
		client, err := project.NewClient(entry)
		if err != nil {
			return err
		}
		if err := client.InstallActionsSettings(cmd.Context(), owner, name, token, dry); err != nil {
			return credentials.RedactError(err, token, entry.Token)
		}
		if err := project.InstallWorkflow(path, false, dry); err != nil {
			return err
		}
		cmd.Println("workflow 配置已处理；提交并推送文件变更后平台生效")
		return nil
	}}
	action.Flags().StringVar(&mergeInstance, "merge-instance", "", "用于 MERGE_TOKEN 的实例名（缺省该站点唯一 merge 账号）")
	install.AddCommand(action)
	install.AddCommand(&cobra.Command{Use: "mcp", Short: "安装 assistant-gitea server，无需凭据，可选绑定实例", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if instance != "" {
			if _, _, _, err := giteaInstance(instance); err != nil {
				return err
			}
		}
		path, err := projectDir(dir)
		if err != nil {
			return err
		}
		if err := project.ConfigureMCP(path, instance, false, dry); err != nil {
			return err
		}
		cmd.Println("MCP 配置已处理：assistant-gitea（不含密钥）")
		return nil
	}})
	for _, kind := range []string{"action", "mcp"} {
		uninstall.AddCommand(&cobra.Command{Use: kind, Short: "移除本工具托管配置，保留其他配置", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := projectDir(dir)
			if err != nil {
				return err
			}
			if kind == "action" {
				if err := project.InstallWorkflow(path, true, dry); err != nil {
					return err
				}
				cmd.Println("本地 workflow 已处理；提交推送后生效，MERGE_TOKEN 与其他 workflow 保留")
				return nil
			}
			if err := project.ConfigureMCP(path, "", true, dry); err != nil {
				return err
			}
			cmd.Println("MCP 托管 server 已处理，其他配置保留")
			return nil
		}})
	}
	root.AddCommand(install, uninstall)
	return root
}
func projectDir(dir string) (string, error) {
	command := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	data, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("定位当前 Git 项目: %w", err)
	}
	return filepath.Abs(strings.TrimSpace(string(data)))
}
func projectRepo(dir, explicit, host string) (string, string, error) {
	if explicit != "" {
		return credentials.ParseRepoName(explicit)
	}
	var found string
	for _, remote := range gitremote.ListRemotes(dir) {
		if credentials.NormalizeHost(remote.Host) == credentials.NormalizeHost(host) {
			if found != "" && found != remote.Repository {
				return "", "", fmt.Errorf("当前站点有多个仓库 remote，请指定 --repo")
			}
			found = remote.Repository
		}
	}
	if found == "" {
		return "", "", fmt.Errorf("没有匹配所选实例的 Git remote，请指定 --repo owner/name")
	}
	return credentials.ParseRepoName(found)
}
