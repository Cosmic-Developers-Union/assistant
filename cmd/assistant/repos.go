package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"assistant/internal/dispatcher"
	"assistant/internal/instances"

	"github.com/spf13/cobra"
)

// reposOptions 是 repos 子命令共享的参数。
type reposOptions struct {
	Host string
}

// newReposCommand 管理 config.json 中登记的仓库（本地配置，不触碰服务端）：
// 服务端（协作者/分支保护/标签/MERGE_TOKEN）仍由 setup / init 完成。
func newReposCommand(configFlag *string) *cobra.Command {
	options := &reposOptions{}
	command := &cobra.Command{
		Use:   "repos",
		Short: "管理 config.json 中登记的仓库（list/add/remove）",
		Long: "管理配置文件 instances[].repos（只改本地登记，不触碰服务端）：\n" +
			"  assistant repos list [--host H]                     列出已登记仓库\n" +
			"  assistant repos add <owner/name> [--host H] [--dir 路径]  登记仓库（已存在则更新 dir）\n" +
			"  assistant repos remove <owner/name> [--host H]      移除登记\n\n" +
			"服务端配置（协作者、分支保护、标签、MERGE_TOKEN）用 `assistant setup --host <H>`\n" +
			"按配置补齐，或对当前检出用 `assistant init`。",
		Args: cobra.NoArgs,
	}
	command.PersistentFlags().StringVar(&options.Host, "host", "",
		"平台地址（多平台时必填；缺省取已登记该仓库的平台，其次唯一实例）")
	command.AddCommand(
		newReposListCommand(configFlag, options),
		newReposAddCommand(configFlag, options),
		newReposRemoveCommand(configFlag, options),
	)
	return command
}

func newReposListCommand(configFlag *string, options *reposOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出配置中登记的仓库（含 dir 与仓库级 merger 令牌状态）",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			_, file, err := resolveInstanceFile(commandOptions{ConfigPath: *configFlag})
			if err != nil {
				return err
			}
			stdout := command.OutOrStdout()
			if file == nil || len(file.Instances) == 0 {
				fmt.Fprintln(stdout, "未登记任何平台（先 assistant login <host> 或 assistant setup）")
				return nil
			}
			selected := file.Instances
			if host := strings.TrimRight(strings.TrimSpace(options.Host), "/"); host != "" {
				instance, ok := findInstanceByHost(file, host)
				if !ok {
					return fmt.Errorf("平台 %s 不在配置中", host)
				}
				selected = []instances.Instance{instance}
			}
			total := 0
			for _, instance := range selected {
				if len(instance.Repos) == 0 {
					fmt.Fprintf(stdout, "%s\t未登记仓库\n", instance.Host)
					continue
				}
				for _, repo := range instance.Repos {
					dir := repo.Dir
					if dir == "" {
						dir = "-"
					}
					merger := "no"
					if repo.MergerToken != "" {
						merger = "yes"
					}
					fmt.Fprintf(stdout, "%s\t%s\tdir=%s\tmerger_token=%s\n", instance.Host, repo.Name, dir, merger)
					total++
				}
			}
			fmt.Fprintf(stdout, "共 %d 个仓库\n", total)
			return nil
		},
	}
}

func newReposAddCommand(configFlag *string, options *reposOptions) *cobra.Command {
	var dir string
	command := &cobra.Command{
		Use:   "add <owner/name>",
		Short: "登记仓库到配置（已存在则更新 dir；不触碰服务端）",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			return runReposAdd(command, *configFlag, args[0], options.Host, dir)
		},
	}
	command.Flags().StringVar(&dir, "dir", "", "仓库检出目录（缺省：当前检出匹配该仓库时自动登记其根目录）")
	return command
}

func newReposRemoveCommand(configFlag *string, options *reposOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <owner/name>",
		Short: "移除配置中的仓库登记（不触碰服务端）",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			return runReposRemove(command, *configFlag, args[0], options.Host)
		},
	}
}

func runReposAdd(command *cobra.Command, configPath, repoArg, hostFlag, dirFlag string) error {
	stdout := command.OutOrStdout()
	repoName := strings.TrimSpace(repoArg)
	if _, _, err := instances.ParseRepoName(repoName); err != nil {
		return err
	}
	path, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return err
	}
	if file == nil {
		return fmt.Errorf(
			"没有 config.json（--config / ASSISTANT_CONFIG / 平台标准配置目录）：先 assistant login <host> 注册平台")
	}
	explicitDir := strings.TrimSpace(dirFlag) != ""
	hintHost, checkoutDir := "", ""
	if strings.TrimSpace(hostFlag) == "" || !explicitDir {
		hintHost, checkoutDir = detectCheckout(repoName)
	}
	instance, err := selectRepoInstance(file, hostFlag, repoName, hintHost)
	if err != nil {
		return err
	}

	dir := ""
	switch {
	case explicitDir:
		if dir, err = filepath.Abs(strings.TrimSpace(dirFlag)); err != nil {
			return fmt.Errorf("解析 --dir: %w", err)
		}
	case hintHost != "" && sameHost(hintHost, instance.Host):
		dir = checkoutDir
	}

	updated := instance
	existing, found := instance.FindRepo(repoName)
	switch {
	case found:
		target := existing.Dir
		if explicitDir || dir != "" {
			target = dir
		}
		if target == existing.Dir {
			fmt.Fprintf(stdout, "%s 已在配置中（%s，dir=%s）\n", repoName, instance.Host, repoDirDisplay(target))
			return nil
		}
		for index := range updated.Repos {
			if updated.Repos[index].Name == repoName {
				updated.Repos[index].Dir = target
			}
		}
		if err := saveInstance(file, path, instance.Host, updated); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "已更新 %s 的 dir：%s → %s\n", repoName, repoDirDisplay(existing.Dir), repoDirDisplay(target))
		return nil
	default:
		updated.Repos = append(slices.Clone(updated.Repos), instances.Repo{Name: repoName, Dir: dir})
		if err := saveInstance(file, path, instance.Host, updated); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "已登记 %s → %s（dir=%s）\n", repoName, instance.Host, repoDirDisplay(dir))
		fmt.Fprintf(stdout, "服务端配置用 `assistant setup --host %s` 补齐（或对当前检出运行 assistant init）\n", instance.Host)
		return nil
	}
}

func runReposRemove(command *cobra.Command, configPath, repoArg, hostFlag string) error {
	stdout := command.OutOrStdout()
	repoName := strings.TrimSpace(repoArg)
	if _, _, err := instances.ParseRepoName(repoName); err != nil {
		return err
	}
	path, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return err
	}
	if file == nil {
		return fmt.Errorf(
			"没有 config.json（--config / ASSISTANT_CONFIG / 平台标准配置目录）：先 assistant login <host> 注册平台")
	}
	if !repoRegistered(file, repoName) {
		return fmt.Errorf("仓库 %s 不在任何平台的 repos[] 中（assistant repos list 查看）", repoName)
	}
	instance, err := selectRepoInstance(file, hostFlag, repoName)
	if err != nil {
		return err
	}
	index := slices.IndexFunc(instance.Repos, func(repo instances.Repo) bool { return repo.Name == repoName })
	if index < 0 {
		return fmt.Errorf("仓库 %s 不在 %s 的 repos[] 中（assistant repos list 查看）", repoName, instance.Host)
	}
	updated := instance
	updated.Repos = slices.Delete(slices.Clone(instance.Repos), index, index+1)
	if err := saveInstance(file, path, instance.Host, updated); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已移除 %s（%s，剩余 %d 个仓库）\n", repoName, instance.Host, len(updated.Repos))
	fmt.Fprintln(stdout, "服务端未触碰；需要清理分支保护/协作者/MERGE_TOKEN 时用 assistant deinit --purge")
	return nil
}

// selectRepoInstance 选择仓库登记所属的平台：--host 指定 > 检出 remote 对应的
// 平台（autoHosts，仅命中配置时生效）> 已登记该仓库的平台（repoName 非空时）>
// 配置中唯一实例。歧义或多平台时报可行动错误。
func selectRepoInstance(file *instances.File, hostFlag, repoName string, autoHosts ...string) (instances.Instance, error) {
	if host := strings.TrimRight(strings.TrimSpace(hostFlag), "/"); host != "" {
		instance, ok := findInstanceByHost(file, host)
		if !ok {
			return instances.Instance{}, fmt.Errorf("平台 %s 不在配置中：先 assistant login %s", host, host)
		}
		return instance, nil
	}
	for _, hint := range autoHosts {
		if hint = strings.TrimRight(strings.TrimSpace(hint), "/"); hint == "" {
			continue
		}
		if instance, ok := findInstanceByHost(file, hint); ok {
			return instance, nil
		}
	}
	if repoName != "" {
		var matches []instances.Instance
		for _, instance := range file.Instances {
			if _, ok := instance.FindRepo(repoName); ok {
				matches = append(matches, instance)
			}
		}
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
		default:
			return instances.Instance{}, fmt.Errorf("仓库 %s 登记在多个平台，请用 --host 指定", repoName)
		}
	}
	switch len(file.Instances) {
	case 0:
		return instances.Instance{}, fmt.Errorf("配置中没有平台：先 assistant login <host>")
	case 1:
		return file.Instances[0], nil
	default:
		hosts := make([]string, 0, len(file.Instances))
		for _, instance := range file.Instances {
			hosts = append(hosts, instance.Host)
		}
		return instances.Instance{}, fmt.Errorf("配置中有多个平台（%s），请用 --host 指定", strings.Join(hosts, "、"))
	}
}

// repoRegistered 判断仓库是否登记在任一平台的 repos[] 中。
func repoRegistered(file *instances.File, repoName string) bool {
	for _, instance := range file.Instances {
		if _, ok := instance.FindRepo(repoName); ok {
			return true
		}
	}
	return false
}

// detectCheckout 从当前检出的 remote 识别仓库：返回 remote 对应平台与检出根
// 目录（--dir 缺省与 --host 缺省时的自动提示）。只读本地 git 配置，不访问网络。
func detectCheckout(fullName string) (host, dir string) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", ""
	}
	for _, remote := range dispatcher.ListRemotes(cwd) {
		if remote.Repository != fullName {
			continue
		}
		if root, ok := dispatcher.RepoRoot(cwd); ok {
			return remote.Host, root
		}
		return remote.Host, cwd
	}
	return "", ""
}

func repoDirDisplay(dir string) string {
	if dir == "" {
		return "未设置"
	}
	return dir
}
