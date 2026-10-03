package cli

import (
	"fmt"
	"net/url"
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
	return &cobra.Command{Use: "token <实例名>", Short: "只读取并输出已保存的 Gitea 令牌，不修改任何状态", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		_, _, entry, err := giteaInstance(args[0])
		if err != nil {
			return err
		}
		cmd.Println(entry.Token)
		return nil
	}}
}

func newCreateTokenCommand() *cobra.Command {
	var passwordFile, user, name, otp string
	cmd := &cobra.Command{Use: "create-token <登录实例名>", Short: "创建并保存新令牌；管理员可指定目标用户", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) (resultErr error) {
		path, file, entry, err := giteaInstance(args[0])
		if err != nil {
			return err
		}
		if user == "" || strings.EqualFold(user, entry.Username) {
			user = entry.Username
		}
		if name == "" {
			name = entry.Name
			if user != entry.Username {
				name += "-" + user
			}
		}
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("目标实例名不能为空")
		}
		account := credentials.Gitea{Name: name, URL: entry.URL, Username: user}
		var previous *credentials.Gitea
		for i := range file.Instances.Gitea {
			old := &file.Instances.Gitea[i]
			if old.Name != name {
				continue
			}
			if credentials.NormalizeHost(old.URL) != credentials.NormalizeHost(entry.URL) || !strings.EqualFold(old.Username, user) {
				return fmt.Errorf("实例名 %s 已绑定不同站点或账号", name)
			}
			previous = old
			account = *old
		}
		for _, row := range rows(file) {
			if row.Name == name && row.Type != "gitea" {
				return fmt.Errorf("实例名 %s 被其他平台使用", name)
			}
		}
		password, err := entry.PasswordValue()
		if passwordFile != "" {
			password, err = newPrompts(cmd).secret(passwordFile, "登录账号密码")
		}
		if err != nil {
			return err
		}
		defer func() { resultErr = credentials.RedactError(resultErr, password, entry.Token) }()
		token, err := project.IssueTokenForUser(cmd.Context(), entry, user, password, otp)
		if err != nil {
			return err
		}
		account.Token = token
		if user == entry.Username {
			if err := account.SetPassword(password); err != nil {
				return err
			}
		}
		if previous != nil {
			*previous = account
		} else {
			file.Instances.Gitea = append(file.Instances.Gitea, account)
		}
		if err := credentials.Save(path, file); err != nil {
			return fmt.Errorf("令牌已创建，保存凭据失败: %w", err)
		}
		cmd.Printf("已为 @%s 创建令牌并保存为实例 %s；旧平台令牌未撤销\n", user, name)
		return nil
	}}
	cmd.Flags().StringVar(&passwordFile, "password-file", "", "可选覆盖登录账号的已保存密码")
	cmd.Flags().StringVar(&user, "user", "", "目标用户名（缺省登录账号，其他用户需管理员）")
	cmd.Flags().StringVar(&name, "name", "", "保存目标凭据的实例名（缺省登录实例名或其加目标用户名）")
	cmd.Flags().StringVar(&otp, "otp", "", "登录账号两步验证的当前验证码")
	return cmd
}

func newProjectCommand() *cobra.Command {
	var instance, dir, repo, reviewer, merger string
	var dry, force bool
	root := &cobra.Command{Use: "project", Short: "当前项目的 Gitea 标签、协作者、Actions 与 MCP", Args: cobra.NoArgs}
	root.PersistentFlags().StringVar(&dir, "dir", ".", "当前项目目录")
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
	configure.Flags().StringVar(&instance, "instance", "", "标签和权限配置使用的 Gitea 实例名")
	configure.Flags().StringVar(&repo, "repo", "", "owner/name（缺省从匹配实例的 Git remote 推导）")
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
	labels.Flags().StringVar(&instance, "instance", "", "标签配置使用的 Gitea 实例名")
	labels.Flags().StringVar(&repo, "repo", "", "owner/name（缺省从匹配实例的 Git remote 推导）")
	root.AddCommand(labels)
	install := &cobra.Command{Use: "install", Short: "安装项目工具配置", Args: cobra.NoArgs}
	uninstall := &cobra.Command{Use: "uninstall", Short: "只卸载本工具配置，保留账号、权限、secret 与其他工具", Args: cobra.NoArgs}
	action := &cobra.Command{Use: "action", Short: "本地生成与当前构建版本绑定的 workflow，无需站点或凭据", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := projectDir(dir)
		if err != nil {
			return err
		}
		if err := project.InstallWorkflow(path, project.WorkflowOptions{Version: binaryVersion(), DryRun: dry}); err != nil {
			return err
		}
		cmd.Printf("workflow 配置已处理：版本 %s；提交并推送文件变更后平台生效\n", binaryVersion())
		return nil
	}}
	install.AddCommand(action)
	mcp := &cobra.Command{Use: "mcp", Short: "安装 gitea MCP server，无需凭据或实例参数", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := projectDir(dir)
		if err != nil {
			return err
		}
		if err := project.ConfigureMCP(path, project.MCPOptions{DryRun: dry, Force: force, Log: func(message string) { fmt.Fprintln(cmd.ErrOrStderr(), message) }}); err != nil {
			return err
		}
		cmd.Println("MCP 配置已处理：gitea（不含密钥）")
		return nil
	}}
	mcp.Flags().BoolVarP(&force, "force", "f", false, "备份原文件后覆盖冲突或修复无效 JSON，保留可解析的其他配置")
	install.AddCommand(mcp)
	for _, kind := range []string{"action", "mcp"} {
		uninstall.AddCommand(&cobra.Command{Use: kind, Short: "移除本工具托管配置，保留其他配置", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := projectDir(dir)
			if err != nil {
				return err
			}
			if kind == "action" {
				if err := project.InstallWorkflow(path, project.WorkflowOptions{Remove: true, DryRun: dry}); err != nil {
					return err
				}
				cmd.Println("本地 workflow 已处理；提交推送后生效，MERGE_TOKEN 与其他 workflow 保留")
				return nil
			}
			if err := project.ConfigureMCP(path, project.MCPOptions{Remove: true, DryRun: dry}); err != nil {
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
		if remoteMatchesHost(remote, host) {
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

func remoteMatchesHost(remote gitremote.GitRemote, host string) bool {
	if credentials.NormalizeHost(remote.Host) == credentials.NormalizeHost(host) {
		return true
	}
	// HTTP remote 的协议与端口是确定的；SSH 的 Web 端口只能从已登记实例取得。
	if strings.HasPrefix(strings.ToLower(remote.URL), "http://") || strings.HasPrefix(strings.ToLower(remote.URL), "https://") {
		return false
	}
	a, err := url.Parse(remote.Host)
	if err != nil {
		return false
	}
	b, err := url.Parse(credentials.NormalizeHost(host))
	return err == nil && a.Hostname() != "" && strings.EqualFold(a.Hostname(), b.Hostname()) && b.Path == ""
}
