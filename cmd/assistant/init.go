package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"assistant/internal/dispatcher"
	"assistant/internal/instances"
	"assistant/internal/setup"
	"assistant/internal/status"

	"github.com/spf13/cobra"
)

type initOptions struct {
	RequiredApprovals  int64
	AllowAdminOverride bool
	CreateRepos        bool
	DryRun             bool
}

// repoSetupTarget 是当前仓库初始化/撤离的解析结果。
type repoSetupTarget struct {
	Path     string
	FullName string
	Host     string
	Instance instances.Instance
	File     *instances.File
}

// newInitCommand 只初始化当前仓库：复用平台（login/setup）的管理员凭据与 ai/
// merge 账号，配好协作者/分支保护/标签，把仓库自动登记进 config.json，并写入
// 仓库级 Actions secret。
func newInitCommand(configFlag *string) *cobra.Command {
	options := &initOptions{}
	command := &cobra.Command{
		Use:   "init [owner/name]",
		Short: "初始化当前仓库并自动登记进 config.json（复用平台凭据与 ai/merge 账号）",
		Long: "只处理当前仓库，不重建平台：\n" +
			"  1. 按 remote/--repo/位置参数确定仓库，定位 config.json 中的平台；\n" +
			"  2. 复用或补齐 ai/merge 账号与令牌，配协作者、分支保护（同 setup 口径）、标签；\n" +
			"  3. 写入仓库级 Actions secret（MERGE_TOKEN）；\n" +
			"  4. 把仓库条目（含 merger_token）自动加入 config.json。\n\n" +
			"平台不在配置中时先运行 assistant login <host>。",
		Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			return runInit(command, *configFlag, args, options)
		},
	}
	flags := command.Flags()
	flags.Int64Var(&options.RequiredApprovals, "required-approvals", 2, "分支保护要求的批准数")
	flags.BoolVar(&options.AllowAdminOverride, "allow-admin-override", false, "允许管理员绕过分支保护")
	flags.BoolVar(&options.CreateRepos, "create-repos", false, "仓库不存在时自动创建（私有，auto_init）")
	flags.BoolVar(&options.DryRun, "dry-run", false, "只输出将要做的操作，不做任何修改")
	return command
}

// newDeinitCommand 是 init 的反命令：把当前仓库从 config.json 移除；--purge
// 同时清理服务端该仓库的助手配置（分支保护、ai/merge 协作者、MERGE_TOKEN）。
func newDeinitCommand(configFlag *string) *cobra.Command {
	var purge bool
	var dryRun bool
	command := &cobra.Command{
		Use:     "deinit [owner/name]",
		Aliases: []string{"uninit"},
		Short:   "撤离当前仓库：从 config.json 移除；--purge 同时清理服务端配置",
		Long: "只处理当前仓库：\n" +
			"  默认仅从 config.json 的 repos[] 移除（不触碰服务端）；\n" +
			"  --purge 额外删除分支保护、移除 ai/merge 协作者、删除 MERGE_TOKEN secret。\n" +
			"本地安装产物（skills/AGENTS.md/workflow/MCP）用 assistant uninstall 清理。",
		Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			return runDeinit(command, *configFlag, args, purge, dryRun)
		},
	}
	flags := command.Flags()
	flags.BoolVar(&purge, "purge", false, "同时清理服务端：分支保护、ai/merge 协作者、MERGE_TOKEN secret")
	flags.BoolVar(&dryRun, "dry-run", false, "只输出将要做的操作，不做任何修改")
	return command
}

func runInit(command *cobra.Command, configPath string, args []string, options *initOptions) error {
	ctx := command.Context()
	logf := func(format string, arguments ...any) {
		fmt.Fprintf(command.OutOrStdout(), format+"\n", arguments...)
	}
	target, err := resolveRepoSetupTarget(ctx, command, configPath, args)
	if err != nil {
		return err
	}
	adminToken, adminErr := adminTokenForInstance(ctx, target.Instance, logf)
	if adminErr != nil {
		return fmt.Errorf("缺少管理员凭据（先 assistant login %s）: %w", target.Host, adminErr)
	}
	admin, err := setup.NewAdmin(ctx, setup.Options{Host: target.Host, AdminToken: adminToken, Log: logf})
	if err != nil {
		return err
	}
	updated, err := setup.Run(ctx, setup.Options{
		Host:               target.Host,
		AdminToken:         adminToken,
		OAuthClientID:      target.Instance.OAuthClientID,
		Repos:              []string{target.FullName},
		RequiredApprovals:  options.RequiredApprovals,
		AllowAdminOverride: options.AllowAdminOverride,
		CreateRepos:        options.CreateRepos,
		Existing:           &target.Instance,
		DryRun:             options.DryRun,
		Log:                logf,
	}, admin)
	if err != nil {
		return err
	}
	merged := mergeRepoIntoInstance(target.Instance, updated)
	if options.DryRun {
		logf("dry-run：未写入 %s", target.Path)
		return nil
	}

	// 仓库级 Actions secret 只针对当前仓库
	actionInstance := merged
	if repo, ok := merged.FindRepo(target.FullName); ok {
		actionInstance.Repos = []instances.Repo{repo}
	}
	if err := setup.ConfigureActions(ctx, admin, actionInstance, false, logf); err != nil {
		return fmt.Errorf("写入仓库 Actions 配置: %w", err)
	}

	if err := saveInstance(target.File, target.Path, target.Host, merged); err != nil {
		return err
	}
	logf("仓库 %s 已初始化并登记到 %s（本地文件可用 assistant install 补齐）", target.FullName, target.Path)
	return nil
}

func runDeinit(command *cobra.Command, configPath string, args []string, purge, dryRun bool) error {
	ctx := command.Context()
	logf := func(format string, arguments ...any) {
		fmt.Fprintf(command.OutOrStdout(), format+"\n", arguments...)
	}
	target, err := resolveRepoSetupTarget(ctx, command, configPath, args)
	if err != nil {
		return err
	}
	index := slices.IndexFunc(target.Instance.Repos, func(repo instances.Repo) bool {
		return repo.Name == target.FullName
	})
	if index < 0 {
		return fmt.Errorf("仓库 %s 不在配置的 repos[] 中", target.FullName)
	}

	if purge {
		adminToken, adminErr := adminTokenForInstance(ctx, target.Instance, logf)
		if adminErr != nil {
			return fmt.Errorf("--purge 需要管理员凭据（先 assistant login %s）: %w", target.Host, adminErr)
		}
		admin, err := setup.NewAdmin(ctx, setup.Options{Host: target.Host, AdminToken: adminToken, Log: logf})
		if err != nil {
			return err
		}
		if dryRun {
			logf("dry-run：将清理 %s 的服务端配置（分支保护/ai、merge 协作者/%s secret）",
				target.FullName, setup.ActionsSecretMergeToken)
		} else if err := purgeRepoServer(ctx, admin, target.Instance, target.FullName, logf); err != nil {
			return err
		}
	}
	if dryRun {
		logf("dry-run：未写入 %s", target.Path)
		return nil
	}
	instance := target.Instance
	instance.Repos = slices.Delete(slices.Clone(instance.Repos), index, index+1)
	if err := saveInstance(target.File, target.Path, target.Host, instance); err != nil {
		return err
	}
	logf("仓库 %s 已从 %s 移除（本地文件可用 assistant uninstall 清理）", target.FullName, target.Path)
	return nil
}

// resolveRepoSetupTarget 定位当前仓库、对应平台与配置：仓库来自位置参数 /
// --repo / 多 remote 探测；平台必须已在 config.json 中（login/setup 写入）。
func resolveRepoSetupTarget(
	ctx context.Context,
	command *cobra.Command,
	configPath string,
	args []string,
) (repoSetupTarget, error) {
	return resolveRepoSetupTargetWithProbe(ctx, command, configPath, args, func(candidate string) bool {
		return status.ProbeGitea(ctx, candidate)
	})
}

// resolveRepoSetupTargetWithProbe 是 resolveRepoSetupTarget 的可注入实现。
func resolveRepoSetupTargetWithProbe(
	ctx context.Context,
	command *cobra.Command,
	configPath string,
	args []string,
	probe func(string) bool,
) (repoSetupTarget, error) {
	repoArg := ""
	if len(args) == 1 {
		repoArg = args[0]
	}
	if repoArg == "" {
		repoArg, _ = command.Flags().GetString("repo")
	}
	path, file, err := resolveInstanceFile(commandOptions{ConfigPath: configPath})
	if err != nil {
		return repoSetupTarget{}, err
	}
	if file == nil {
		return repoSetupTarget{}, fmt.Errorf(
			"没有 config.json（--config / ASSISTANT_CONFIG / 平台标准配置目录）：先 assistant login <host> 注册平台")
	}

	dir, err := os.Getwd()
	if err != nil {
		return repoSetupTarget{}, fmt.Errorf("获取当前目录: %w", err)
	}
	fullName := strings.TrimSpace(repoArg)
	hostHint := ""
	if fullName == "" {
		remote, ok := dispatcher.SelectGiteaRemote(dir, probe)
		if !ok {
			return repoSetupTarget{}, fmt.Errorf("无法从 remote 识别 Gitea 仓库：用 --repo owner/name 显式指定")
		}
		fullName, hostHint = remote.Repository, remote.Host
	}
	if _, _, err := instances.ParseRepoName(fullName); err != nil {
		return repoSetupTarget{}, err
	}

	var instance instances.Instance
	found := false
	if hostHint != "" {
		instance, found = findInstanceByHost(file, hostHint)
	}
	if !found {
		for _, candidate := range file.Instances {
			if _, ok := candidate.FindRepo(fullName); ok {
				instance, found = candidate, true
				break
			}
		}
	}
	if !found && len(file.Instances) == 1 {
		instance, found = file.Instances[0], true
	}
	if !found {
		return repoSetupTarget{}, fmt.Errorf("平台不在配置中：先 assistant login <host>（或用 --config 指定其他配置）")
	}
	return repoSetupTarget{
		Path:     path,
		FullName: fullName,
		Host:     instance.Host,
		Instance: instance,
		File:     file,
	}, nil
}

// mergeRepoIntoInstance 把 setup.Run 返回的单仓库实例合并回原实例：保留原
// admin 凭据与其他仓库，更新账号令牌与目标仓库条目。
func mergeRepoIntoInstance(original, updated instances.Instance) instances.Instance {
	merged := original
	merged.Reviewer = updated.Reviewer
	merged.Merger = updated.Merger
	for _, repo := range updated.Repos {
		replaced := false
		for index := range merged.Repos {
			if merged.Repos[index].Name == repo.Name {
				merged.Repos[index] = repo
				replaced = true
				break
			}
		}
		if !replaced {
			merged.Repos = append(merged.Repos, repo)
		}
	}
	return merged
}

// purgeRepoServer 清理服务端该仓库的助手配置。
func purgeRepoServer(
	ctx context.Context,
	admin setup.Admin,
	instance instances.Instance,
	fullName string,
	logf func(string, ...any),
) error {
	info, exists, err := admin.GetRepo(ctx, fullName)
	if err != nil {
		return err
	}
	if !exists {
		logf("仓库 %s 在服务端不存在，跳过清理", fullName)
		return nil
	}
	if err := admin.DeleteBranchProtection(ctx, fullName, info.DefaultBranch); err != nil {
		return err
	}
	logf("已删除分支保护 %s", info.DefaultBranch)
	for _, account := range []string{instance.Reviewer.Name, instance.Merger.Name} {
		if account == "" {
			continue
		}
		if err := admin.RemoveCollaborator(ctx, fullName, account); err != nil {
			return err
		}
		logf("已移除协作者 %s", account)
	}
	if err := admin.DeleteRepoSecret(ctx, fullName, setup.ActionsSecretMergeToken); err != nil {
		return err
	}
	logf("已删除 Actions secret %s", setup.ActionsSecretMergeToken)
	return nil
}

// saveInstance 用 updated 替换文件中 host 对应的实例并落盘。
func saveInstance(file *instances.File, path, host string, updated instances.Instance) error {
	replaced := false
	for index := range file.Instances {
		if sameHost(file.Instances[index].Host, host) {
			file.Instances[index] = updated
			replaced = true
			break
		}
	}
	if !replaced {
		file.Instances = append(file.Instances, updated)
	}
	file.Normalize()
	if err := file.Validate(); err != nil {
		return err
	}
	return instances.Save(path, file)
}
