package main

import (
	"fmt"

	"assistant/internal/repoinstall"

	"github.com/spf13/cobra"
)

type repoToolOptions struct {
	Dir          string
	Tools        []string
	CodexPath    string
	Image        string
	SkillsSource string
	DryRun       bool
}

func (o *repoToolOptions) repoOptions(command *cobra.Command) repoinstall.Options {
	logf := func(format string, arguments ...any) {
		fmt.Fprintf(command.OutOrStdout(), "%s\n", fmt.Sprintf(format, arguments...))
	}
	return repoinstall.Options{
		Dir:             o.Dir,
		Tools:           o.Tools,
		CodexConfigPath: o.CodexPath,
		Image:           o.Image,
		SkillsSource:    o.SkillsSource,
		DryRun:          o.DryRun,
		Log:             logf,
	}
}

// newInstallCommand 在仓库检出内配置 assistant 工作流：评审 skill、AGENTS.md、
// 仓库 Actions workflow 与各 AI CLI 的 MCP 配置。
func newInstallCommand() *cobra.Command {
	options := &repoToolOptions{}
	command := &cobra.Command{
		Use:   "install",
		Short: "在当前仓库配置 assistant 工作流（skills/AGENTS.md/workflow/MCP）",
		Long: "把仓库配置成 assistant 工作流：\n" +
			"  - review 技能：bunx skills add <skills-source> --skill review（写 .claude/skills、\n" +
			"    .agents/skills，由 skills CLI 管理安装/更新/卸载）\n" +
			"  - AGENTS.md 的 assistant 段落（标签与流程约定）\n" +
			"  - .gitea/workflows/assistant.yml（sync + automerge 两个 job）\n" +
			"  - MCP 配置：claude（.mcp.json + .claude/settings.json 放行 gitea 工具）、\n" +
			"    opencode（opencode.json）、codex（全局 ~/.codex/config.toml）\n\n" +
			"模板按绑定数据动态渲染：身份是约定（内容评审 ai、状态评审/合并 merge），\n" +
			"镜像默认官方 latest，可用 --image 覆盖。其余生成内容带 marker，重复\n" +
			"install 幂等，uninstall 可精确移除。zcode 暂不支持。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return repoinstall.Install(command.Context(), options.repoOptions(command))
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.Dir, "dir", ".", "仓库检出目录")
	flags.StringSliceVar(&options.Tools, "tools", nil,
		"要配置的 AI CLI（缺省全部："+joinTools()+"; zcode 暂不支持）")
	flags.StringVar(&options.CodexPath, "codex-config", "", "覆盖 ~/.codex/config.toml 路径")
	flags.StringVar(&options.Image, "image", "",
		"workflow 容器镜像（缺省 "+repoinstall.DefaultImage+"）")
	flags.StringVar(&options.SkillsSource, "skills-source", "",
		"skills CLI 安装源（缺省 "+repoinstall.DefaultSkillsSource+"；none 表示不管理技能）")
	flags.BoolVar(&options.DryRun, "dry-run", false, "只输出将要写入的内容，不做任何修改")
	return command
}

// newUninstallCommand 移除 install 写入的内容。
func newUninstallCommand() *cobra.Command {
	options := &repoToolOptions{}
	command := &cobra.Command{
		Use:   "uninstall",
		Short: "移除 install 写入的仓库配置（只触碰带 marker 的内容）",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return repoinstall.Uninstall(command.Context(), options.repoOptions(command))
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.Dir, "dir", ".", "仓库检出目录")
	flags.StringSliceVar(&options.Tools, "tools", nil,
		"要清理的 AI CLI（缺省全部："+joinTools()+"）")
	flags.StringVar(&options.CodexPath, "codex-config", "", "覆盖 ~/.codex/config.toml 路径")
	flags.BoolVar(&options.DryRun, "dry-run", false, "只输出将要删除的内容，不做任何修改")
	return command
}

// newMCPCommand 是 MCP 包装层：`assistant mcp gitea` 自动检测当前开发者在本
// 项目的 Gitea 实例与访问令牌，然后拉起 gitea-mcp（stdio）。
func newMCPCommand() *cobra.Command {
	mcpCommand := &cobra.Command{
		Use:   "mcp",
		Short: "MCP 包装层（供各 AI CLI 的 MCP 配置调用）",
		Args:  cobra.NoArgs,
	}
	giteaOptions := struct {
		Host  string
		Token string
		Dir   string
	}{}
	giteaCommand := &cobra.Command{
		Use:   "gitea",
		Short: "拉起 gitea MCP：host 从 origin remote 推导，token 取当前开发者凭据",
		Long: "自动检测当前项目的 Gitea 实例与当前开发者的访问令牌，再以 stdio 拉起\n" +
			"gitea-mcp（默认 `go run gitea.com/gitea/gitea-mcp@latest`；可用 GITEA_MCP_BIN\n" +
			"指已安装的二进制）。检测顺序：\n" +
			"  host：--host > GITEA_HOST > origin remote 推导；\n" +
			"  token：--token > GITEA_ACCESS_TOKEN > GITEA_ACCESS_TOKEN_FILE >\n" +
			"         ~/.config/Cosmic-Developers-Union/assistant/token >\n" +
			"         ~/.config/mmc/gitea-token。\n" +
			"与当前开发者绑定，与管理员/实例配置无关。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return repoinstall.RunMCPGitea(command.Context(), repoinstall.MCPOptions{
				Dir:   giteaOptions.Dir,
				Host:  giteaOptions.Host,
				Token: giteaOptions.Token,
				Log: func(format string, arguments ...any) {
					fmt.Fprintf(command.ErrOrStderr(), format+"\n", arguments...)
				},
			})
		},
	}
	flags := giteaCommand.Flags()
	flags.StringVar(&giteaOptions.Host, "host", "", "覆盖 Gitea 站点（缺省 origin remote 推导）")
	flags.StringVar(&giteaOptions.Token, "token", "", "覆盖访问令牌（缺省多候选检测）")
	flags.StringVar(&giteaOptions.Dir, "dir", ".", "项目目录（缺省 cwd）")
	mcpCommand.AddCommand(giteaCommand, newDaemonMCPCommand())
	return mcpCommand
}

// newDoctorCommand 检测当前仓库的 assistant 配置状态：本地 install 产物
// （文件/段落/MCP）与仓库服务端（分支保护/标签/协作者/merge 令牌）。
func newDoctorCommand() *cobra.Command {
	options := &repoToolOptions{}
	var requiredApprovals int64
	var allowAdminOverride bool
	command := &cobra.Command{
		Use:   "doctor",
		Short: "检测当前仓库的 assistant 配置（本地 + 服务端，缺失/过期/非托管/遗留）",
		Long: "两级体检，只读：\n" +
			"  - 本地：对照当前模板与镜像检查 .claude/skills/review/SKILL.md、AGENTS.md、\n" +
			"    .gitea/workflows/assistant.yml 与 MCP 配置；\n" +
			"  - 服务端：按 origin remote/--repo 定位实例，检查分支保护策略、标签体系、\n" +
			"    协作者权限（ai 写 / merge 管理员）与 MERGE_TOKEN secret。\n" +
			"发现问题时以退出码 1 结束；本地问题重新 install、服务端问题重新 setup/actions。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runDoctor(command, options, requiredApprovals, allowAdminOverride)
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.Dir, "dir", ".", "仓库检出目录")
	flags.StringSliceVar(&options.Tools, "tools", nil,
		"要检测的 AI CLI（缺省全部："+joinTools()+"; zcode 暂不支持）")
	flags.StringVar(&options.CodexPath, "codex-config", "", "覆盖 ~/.codex/config.toml 路径")
	flags.StringVar(&options.Image, "image", "",
		"workflow 容器镜像（缺省 "+repoinstall.DefaultImage+"）")
	flags.Int64Var(&requiredApprovals, "required-approvals", 2, "服务端分支保护要求的批准数")
	flags.BoolVar(&allowAdminOverride, "allow-admin-override", false, "允许管理员绕过分支保护（不检查该项）")
	return command
}

func joinTools() string {
	tools := repoinstall.SupportedTools()
	joined := ""
	for index, tool := range tools {
		if index > 0 {
			joined += ","
		}
		joined += tool
	}
	return joined
}
