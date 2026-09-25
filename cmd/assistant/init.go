// init 是 dev 专用的仓库级初始化：以开发者自己的令牌（purpose=mcp，要求目标
// 仓库的管理员权限）工作，与站点管理员的 assistant setup（admin 令牌）严格
// 区分。Actions 密钥（MERGE_TOKEN）完全由 setup 扫描分发，init 不写服务端密钥。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Cosmic-Developers-Union/assistant/internal/credentials"
	"github.com/Cosmic-Developers-Union/assistant/internal/dispatcher"
	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/repoinstall"
	"github.com/Cosmic-Developers-Union/assistant/internal/setup"
	"github.com/Cosmic-Developers-Union/assistant/internal/status"

	"github.com/spf13/cobra"
)

type initOptions struct {
	Reviewer            string
	Merger              string
	Image               string
	ExtraApprovals      int64
	StatusCheckContexts []string
	DryRun              bool
}

// repoSetupTarget 是当前仓库初始化的解析结果。
type repoSetupTarget struct {
	Path     string
	FullName string
	Host     string
	Instance instances.Instance
	File     *instances.File
}

// newInitCommand 初始化当前仓库的 assistant 集成，子命令按职责分工：
//
//   - init actions：已弃用，兼容旧用法；新入口是 install gitea-actions；
//   - init merge：把 merge 账号加为仓库协作者（admin 权限）；
//   - init branch-protection：按当前协作者自动生成分支保护规则。
func newInitCommand(configFlag *string) *cobra.Command {
	options := &initOptions{}
	command := &cobra.Command{
		Use:   "init",
		Short: "初始化当前仓库（dev 专用）：merge/ai 协作者 / 标签 / branch-protection",
		Long: "只处理当前仓库的 dev 侧初始化，需要**目标仓库的管理员权限**（用开发者\n" +
			"自己的 purpose=mcp 令牌，先 assistant login add）。与站点管理员的 assistant\n" +
			"setup（admin 令牌：建号、令牌、MERGE_TOKEN 密钥分发）严格区分。\n\n" +
			"子命令：\n" +
			"  merge               把 merge 账号加为仓库协作者（admin 权限）；\n" +
			"  ai                  邀请 ai 账号加入协作者（write 权限，内容评审）；\n" +
			"  labels              把标签收敛为规范体系（与 setup/action label-sync 同一口径）；\n" +
			"  branch-protection   读取当前协作者，自动生成分支保护规则。\n\n" +
			"MERGE_TOKEN secret 由 setup 扫描「merge 为管理员协作者」的仓库自动分发。",
		Args: cobra.NoArgs,
	}
	command.PersistentFlags().String("repo", "", "仓库 owner/name（缺省从 origin remote 识别）")
	command.PersistentFlags().BoolVar(&options.DryRun, "dry-run", false, "只输出将要做的操作，不做任何修改")
	command.AddCommand(
		newInitActionsCommand(options),
		newInitMergeCommand(configFlag, options),
		newInitReviewerCommand(configFlag, options),
		newInitLabelsCommand(configFlag, options),
		newInitBranchProtectionCommand(configFlag, options),
	)
	return command
}

// newInitActionsCommand 写本地 Actions workflow：不需要任何凭据与服务端访问。
func newInitActionsCommand(options *initOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "actions",
		Short: "写本地 Actions workflow（.gitea/workflows/assistant.yml）",
		Long: "兼容旧用法。在当前目录写/更新 assistant 托管的 Actions workflow（action label-sync 与\n" +
			"action automerge 两个 job）；请改用 assistant install gitea-actions。\n" +
			"带 marker 防覆盖用户手写文件；MERGE_TOKEN secret 不在这里配——由站点管理员\n" +
			"运行 assistant setup 自动分发到 merge 为管理员协作者的仓库。",
		Args:       cobra.NoArgs,
		Deprecated: "请改用 assistant install gitea-actions",
		RunE: func(command *cobra.Command, _ []string) error {
			logf := commandLogger(command, "init actions")
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			installOptions := repoinstall.Options{Dir: dir, Image: options.Image, DryRun: options.DryRun, Log: logf}
			if err := repoinstall.InstallWorkflows(installOptions); err != nil {
				return err
			}
			logf("本地 workflow 就绪；服务端 secret 由 assistant setup 分发")
			return nil
		},
	}
	command.Flags().StringVar(&options.Image, "image", "", "workflow 运行的 assistant 镜像（缺省官方镜像）")
	return command
}

// newInitMergeCommand 把 merge 账号加为当前仓库的协作者（admin 权限）：merge
// 需要仓库管理员权限做分支保护读取、会签与合并。
func newInitMergeCommand(configFlag *string, options *initOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "merge",
		Short: "把 merge 账号加为当前仓库协作者（admin 权限）",
		Long: "把 merge 账号加为当前仓库协作者并授予 admin 权限（分支保护读取、会签、\n" +
			"合并都需要）。需要你是目标仓库的管理员（或 owner）；平台必须已在\n" +
			"config.json 中（先 assistant login add <host>）。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runInitMerge(command, *configFlag, options)
		},
	}
	command.Flags().StringVar(&options.Merger, "merger", instances.DefaultMergerName, "merge 账号名")
	return command
}

// newInitBranchProtectionCommand 按当前协作者自动生成分支保护规则：required
// approvals = 协作者中可评审人数（不含 merge），合并白名单只含 merge。
func newInitBranchProtectionCommand(configFlag *string, options *initOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "branch-protection",
		Short: "按当前协作者自动生成分支保护规则",
		Long: "生成分支保护规则（可重复执行，幂等）。票数不随协作者数量增长：\n" +
			"  - merge 恒有一票（检查通过后自动批准并合并）——只有 merge 时为 1；\n" +
			"  - ai 协作者存在时 +1（内容批准）；\n" +
			"  - --extra-approvals 显式附加额外票数（默认 0）；\n" +
			"  - 合并白名单只含 merge 账号（开发者 @merge 触发批准，自动合并）；\n" +
			"  - 驳回阻塞、未回应评审请求阻塞、过期批准作废、落后分支阻塞；\n" +
			"  - 管理员须遵守分支保护规则。\n\n" +
			"需要你是目标仓库的管理员（或 owner）；先运行 assistant init merge。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runInitBranchProtection(command, *configFlag, options)
		},
	}
	command.Flags().StringVar(&options.Merger, "merger", instances.DefaultMergerName, "merge 账号名（合并白名单）")
	command.Flags().StringVar(&options.Reviewer, "reviewer", instances.DefaultReviewerName, "ai 账号名（存在时票数 +1）")
	command.Flags().Int64Var(&options.ExtraApprovals, "extra-approvals", 0, "在 merge/ai 之上附加的批准数")
	command.Flags().StringSliceVar(&options.StatusCheckContexts, "status-check-contexts", nil,
		"合并前必须全绿的检查 context（逗号分隔，支持 glob）；配置后 automerge 在检查运行期间武装 Gitea 原生\nauto-merge，检查变绿即由服务端即时合并。缺省不改动服务端现状")
	return command
}

// newInitReviewerCommand 把 ai 账号加为当前仓库的协作者（write 权限）：ai 做
// 内容评审，提交 APPROVED / REQUEST_CHANGES。
func newInitReviewerCommand(configFlag *string, options *initOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "ai",
		Short: "邀请 ai 账号加入协作者（write 权限，内容评审）",
		Long: "把 ai 账号加为当前仓库协作者并授予 write 权限：ai 是内容评审者，可以\n" +
			"读代码、提交 review（APPROVED / REQUEST_CHANGES），不做合并。加入后重跑\n" +
			"assistant init branch-protection，required approvals 会从 1 升为 2。\n" +
			"需要你是目标仓库的管理员（或 owner）。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runInitReviewer(command, *configFlag, options)
		},
	}
	command.Flags().StringVar(&options.Reviewer, "reviewer", instances.DefaultReviewerName, "ai 账号名")
	return command
}

func runInitReviewer(command *cobra.Command, configPath string, options *initOptions) error {
	ctx := command.Context()
	logf := commandLogger(command, "init ai")
	client, target, err := newRepoClientForTarget(ctx, command, configPath, logf)
	if err != nil {
		return err
	}
	if options.DryRun {
		logf("dry-run：将把 %s 加为 %s 的协作者（write）", options.Reviewer, target.FullName)
		return nil
	}
	if err := client.AddCollaborator(ctx, target.FullName, options.Reviewer, "write"); err != nil {
		return err
	}
	logf("已把 %s 加为 %s 的协作者（write）", options.Reviewer, target.FullName)
	logf("重跑 assistant init branch-protection 可把 required approvals 升为 2")
	return nil
}

// newInitLabelsCommand 把当前仓库的标签收敛为规范体系：补齐缺失标签、scoped
// 组内互斥、删除不在体系内的标签（与 setup / action label-sync 同一口径）。
func newInitLabelsCommand(configFlag *string, options *initOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "labels",
		Short: "把当前仓库的标签收敛为规范体系（补齐/互斥/删除体系外）",
		Long: "把当前仓库的标签收敛为 assistant 规范体系：补齐缺失标签、scoped 组内\n" +
			"互斥、删除不在体系内的标签。口径与 assistant setup / action label-sync 一致，收敛后\n" +
			"标签完全合规，label-sync 无需再作修正。需要你是目标仓库的管理员（或 owner）。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runInitLabels(command, *configFlag, options)
		},
	}
	return command
}

func runInitLabels(command *cobra.Command, configPath string, options *initOptions) error {
	ctx := command.Context()
	logf := commandLogger(command, "init labels")
	client, target, err := newRepoClientForTarget(ctx, command, configPath, logf)
	if err != nil {
		return err
	}
	if options.DryRun {
		logf("dry-run：将收敛 %s 的标签体系（补齐缺失、scoped 互斥、删除体系外）", target.FullName)
		return nil
	}
	if err := client.ReconcileLabels(ctx, target.FullName, client.Token()); err != nil {
		return fmt.Errorf("收敛标签体系: %w", err)
	}
	logf("标签体系已收敛（%s）", target.FullName)
	return nil
}

func runInitMerge(command *cobra.Command, configPath string, options *initOptions) error {
	ctx := command.Context()
	logf := commandLogger(command, "init merge")
	client, target, err := newRepoClientForTarget(ctx, command, configPath, logf)
	if err != nil {
		return err
	}
	if options.DryRun {
		logf("dry-run：将把 %s 加为 %s 的协作者（admin）", options.Merger, target.FullName)
		return nil
	}
	if err := client.AddCollaborator(ctx, target.FullName, options.Merger, "admin"); err != nil {
		return err
	}
	logf("已把 %s 加为 %s 的协作者（admin）", options.Merger, target.FullName)
	logf("MERGE_TOKEN secret 由站点管理员运行 assistant setup 分发")
	return nil
}

func runInitBranchProtection(command *cobra.Command, configPath string, options *initOptions) error {
	ctx := command.Context()
	logf := commandLogger(command, "init branch-protection")
	client, target, err := newRepoClientForTarget(ctx, command, configPath, logf)
	if err != nil {
		return err
	}
	// 票数不随协作者数量增长：merge 恒有一票，ai 协作者存在时 +1，其余用
	// --extra-approvals 显式附加
	collaborators, err := client.ListCollaborators(ctx, target.FullName)
	if err != nil {
		return err
	}
	hasMerger, hasReviewer := false, false
	for _, collaborator := range collaborators {
		if collaborator.Permission != "admin" && collaborator.Permission != "write" {
			continue
		}
		switch collaborator.Name {
		case options.Merger:
			hasMerger = true
		case options.Reviewer:
			hasReviewer = true
		}
	}
	if !hasMerger {
		seen := make([]string, 0, len(collaborators))
		for _, collaborator := range collaborators {
			seen = append(seen, collaborator.Name+"("+collaborator.Permission+")")
		}
		return fmt.Errorf("协作者中没有 %s 账号：它持有一票批准并执行会签合并，先运行 assistant init merge（当前协作者: %s）",
			options.Merger, strings.Join(seen, ", "))
	}
	approvals := 1 + options.ExtraApprovals
	if hasReviewer {
		approvals++
	}
	info, exists, err := client.GetRepo(ctx, target.FullName)
	if err != nil {
		return err
	}
	if !exists || info.DefaultBranch == "" || info.Empty {
		return fmt.Errorf("仓库为空或无默认分支，无法配置分支保护")
	}
	logf("required approvals=%d（merge 恒 1 票，ai 协作者在否=%v，附加 %d），合并白名单=%s，分支=%s",
		approvals, hasReviewer, options.ExtraApprovals, options.Merger, info.DefaultBranch)
	if options.DryRun {
		logf("dry-run：未触碰服务端")
		return nil
	}
	if err := client.EnsureBranchProtection(ctx, target.FullName, setup.ProtectionOptions{
		Branch:              info.DefaultBranch,
		MergerName:          options.Merger,
		RequiredApprovals:   int64(approvals),
		StatusCheckContexts: options.StatusCheckContexts,
	}); err != nil {
		return err
	}
	logf("分支保护已写入 %s（%s）", target.FullName, info.DefaultBranch)
	return nil
}

// newRepoClientForTarget 解析当前仓库与平台，并以开发者自己的令牌（purpose=mcp）
// 构造仓库级操作面；再校验调用者确实是仓库管理员——init 是 dev 专用，与 setup
// 的站点管理员严格区分。
func newRepoClientForTarget(
	ctx context.Context,
	command *cobra.Command,
	configPath string,
	logf func(string, ...any),
) (*setup.GiteaClient, repoSetupTarget, error) {
	repo, _ := command.Flags().GetString("repo")
	var args []string
	if repo != "" {
		args = []string{repo}
	}
	target, err := resolveRepoSetupTarget(ctx, command, configPath, args)
	if err != nil {
		return nil, repoSetupTarget{}, err
	}
	credential, err := tokenForPurpose(configPath, target.Host, credentials.PurposeMCP)
	if err != nil {
		return nil, repoSetupTarget{}, fmt.Errorf("init 使用开发者自己的令牌（purpose=mcp）: %w", err)
	}
	client, err := setup.NewRepoClient(ctx, target.Host, credential.Token, logf)
	if err != nil {
		return nil, repoSetupTarget{}, err
	}
	collaborators, collaboratorErr := client.ListCollaborators(ctx, target.FullName)
	if collaboratorErr != nil {
		return nil, repoSetupTarget{}, fmt.Errorf(
			"%s: 读取协作者失败（%v）：init 是 dev 专用，需要目标仓库的管理员权限；站点级配置用 assistant setup",
			target.FullName, collaboratorErr)
	}
	self := client.Login()
	for _, collaborator := range collaborators {
		if collaborator.Name == self && collaborator.Permission != "admin" {
			return nil, repoSetupTarget{}, fmt.Errorf(
				"%s: 你（@%s）只是 %s 协作者：init 需要仓库管理员权限；站点级配置用 assistant setup",
				target.FullName, self, collaborator.Permission)
		}
	}
	return client, target, nil
}

// commandLogger 返回带子命令前缀的 stdout 日志函数。
func commandLogger(command *cobra.Command, prefix string) func(string, ...any) {
	return func(format string, arguments ...any) {
		fmt.Fprintf(command.OutOrStdout(), "[%s] %s\n", prefix, fmt.Sprintf(format, arguments...))
	}
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
			"没有 config.json（--config / ASSISTANT_CONFIG / 当前目录）：先 assistant login add <host> 注册平台")
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

	channel, err := selectRepoGitea(file, hostHint, fullName)
	if err != nil {
		return repoSetupTarget{}, err
	}
	return repoSetupTarget{
		Path:     path,
		FullName: fullName,
		Host:     channel.Host,
		Instance: channelInstance(channel),
		File:     file,
	}, nil
}

// saveInstance 用 updated 替换文件中 host 对应的 gitea 通道并落盘（引擎回写共享）。
func saveInstance(file *instances.File, path, host string, updated instances.Instance) error {
	channel := upsertGiteaChannel(file, host)
	channel.Provider = updated.Provider
	channel.Reviewer = updated.Reviewer.Name
	channel.Merger = updated.Merger.Name
	channel.Repos = updated.Repos
	return saveConfig(file, path)
}
