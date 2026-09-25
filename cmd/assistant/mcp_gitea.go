package main

import (
	"fmt"

	"github.com/Cosmic-Developers-Union/assistant/internal/mcps"

	"github.com/spf13/cobra"
)

// newMCPCommand 是 MCP 包装层：`assistant mcp gitea` 自动检测当前开发者在本
// 项目的 Gitea 实例与访问令牌，然后拉起 gitea-mcp（stdio）。daemon 与 sessions
// 两个子命令的实现在各自的 *_mcp.go 里。
func newMCPCommand(configFlag *string) *cobra.Command {
	mcpCommand := &cobra.Command{
		Use:   "mcp",
		Short: "MCP 包装层（供各 AI CLI 的 MCP 配置调用）",
		Args:  cobra.NoArgs,
	}
	mcpCommand.AddCommand(newMCPGiteaCommand(), newDaemonMCPCommand(), newSessionsMCPCommand(configFlag))
	return mcpCommand
}

// mcpGiteaHelp 是 `assistant mcp gitea --help` 的正文：host 与 token 的完整
// 检测顺序，以及凭据库落点。
const mcpGiteaHelp = "自动检测当前项目的 Gitea 实例与当前开发者的访问令牌，再以 stdio 拉起\n" +
	"gitea-mcp（默认 `go run gitea.com/gitea/gitea-mcp@latest`；可用 GITEA_MCP_BIN\n" +
	"指已安装的二进制）。检测顺序：\n" +
	"  host：--host > GITEA_HOST > 检出内 Gitea remote 探测（多上游时选命中的）>\n" +
	"        config.json 唯一 gitea 通道 > 凭据库唯一站点；无上游且登记多平台时\n" +
	"        显式报错不猜；\n" +
	"  token：--token > GITEA_ACCESS_TOKEN > GITEA_ACCESS_TOKEN_FILE >\n" +
	"         assistant login add <host> --user <账号> 的上站点凭据（凭据库）。\n" +
	"凭据库在平台标准配置目录（ASSISTANT_CREDENTIALS 可显式指定位置），与当前\n" +
	"目录无关——从任何项目启动都解析同一份登录状态。"

// newMCPGiteaCommand 拉起 gitea MCP：host 从 origin remote 推导，token 取当前
// 开发者凭据。实现委托给 mcps.RunGitea。
func newMCPGiteaCommand() *cobra.Command {
	options := struct {
		Host  string
		Token string
		Dir   string
	}{}
	command := &cobra.Command{
		Use:   "gitea",
		Short: "拉起 gitea MCP：host 从 origin remote 推导，token 取当前开发者凭据",
		Long:  mcpGiteaHelp,
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return mcps.RunGitea(command.Context(), mcps.GiteaOptions{
				Dir:   options.Dir,
				Host:  options.Host,
				Token: options.Token,
				Log: func(format string, arguments ...any) {
					fmt.Fprintf(command.ErrOrStderr(), format+"\n", arguments...)
				},
			})
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.Host, "host", "", "覆盖 Gitea 站点（缺省 origin remote 推导）")
	flags.StringVar(&options.Token, "token", "", "覆盖访问令牌（缺省多候选检测）")
	flags.StringVar(&options.Dir, "dir", ".", "项目目录（缺省 cwd）")
	return command
}
