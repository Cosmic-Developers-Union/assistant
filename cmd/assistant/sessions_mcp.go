package main

import (
	"fmt"
	"os"
	"path/filepath"

	"assistant/internal/sessionstore"

	"github.com/spf13/cobra"
)

// newSessionsMCPCommand 是会话记录查询 MCP：agent 在上下文被压缩后用它回查完整
// 聊天/评审历史（session_search / session_list / session_read / conversation_list）。
// 服务端地址与令牌从 <配置目录>/serve.json（同机）或 sessions-remote.json（远端）
// 解析，也可以用 ASSISTANT_SESSIONS_URL / ASSISTANT_SESSIONS_TOKEN 覆盖。
func newSessionsMCPCommand(configFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "sessions",
		Short: "会话记录查询 MCP（按 host/project/session 检索完整历史，供压缩后回查）",
		Long: "以 stdio 提供只读查询工具：\n" +
			"  session_search / session_list / session_read / conversation_list\n" +
			"数据来自 assistant serve 的记录库；端点与令牌解析顺序：\n" +
			"  ASSISTANT_SESSIONS_URL/TOKEN > <配置目录>/sessions-remote.json > <配置目录>/serve.json。\n" +
			"未配置服务端时工具会返回可读错误（不影响会话其它能力）。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			configDir, err := resolveConfigDir(*configFlag)
			if err != nil {
				return err
			}
			options := sessionstore.MCPOptions{ConfigDir: configDir, Version: version}
			if os.Getenv("ASSISTANT_SESSIONS_URL") == "" {
				// 兼容：配置目录不可用时仍可用环境变量
				if _, statErr := os.Stat(filepath.Join(configDir, sessionstore.ServeFile)); statErr != nil {
					fmt.Fprintf(command.ErrOrStderr(), "提示：未发现记录库服务端配置（%s），工具将返回未配置错误\n",
						filepath.Join(configDir, sessionstore.RemoteFile))
				}
			}
			return sessionstore.RunMCP(command.Context(), os.Stdin, os.Stdout, options)
		},
	}
}
