package main

import (
	"os"
	"strings"

	"github.com/Cosmic-Developers-Union/assistant/internal/instances"
	"github.com/Cosmic-Developers-Union/assistant/internal/sessionindex"
	"github.com/Cosmic-Developers-Union/assistant/internal/sessionstore"

	"github.com/spf13/cobra"
)

// newSessionsMCPCommand 是会话记录查询 MCP：agent 在上下文被压缩后用它回查完整
// 聊天/评审历史（session_search / session_list / session_read / conversation_list）。
//
// 数据来自本地 SQLite 索引，与 daemon 归档时写的是同一份文件（落点由 runtime 派生，
// 两边必然一致）。索引未启用或配置读不到时，工具返回可读错误，不影响会话其它能力。
func newSessionsMCPCommand(configFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "sessions",
		Short: "会话记录查询 MCP（按 host/project/session 检索完整历史，供压缩后回查）",
		Long: "以 stdio 提供只读查询工具：\n" +
			"  session_search / session_list / session_read / conversation_list\n" +
			"数据来自本地检索索引（<state-dir>/sessions-index.sqlite3），由 assistant run\n" +
			"归档会话时维护；索引落点按同一份 config.json 解析，不锚定当前目录。\n" +
			"未启用索引时工具会返回可读错误（不影响会话其它能力）。",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			index, note := openSessionsIndex(*configFlag)
			if index != nil {
				defer index.Close()
			}
			// 提示写到 stderr，不污染 stdio 的 JSON-RPC 通道
			if note != "" {
				command.PrintErrf("提示：%s\n", note)
			}
			options := sessionstore.MCPOptions{Index: index, Version: version}
			return sessionstore.RunMCP(command.Context(), os.Stdin, os.Stdout, options)
		},
	}
}

// openSessionsIndex 解析并打开会话记录索引。(nil, 说明) 表示索引未启用——说明是
// 给操作者看的、不污染 JSON-RPC 通道的提示；(store, "") 表示可用。
//
// 存储未配置时就**不开**索引文件：只读查询命令不该因为没配归档而在磁盘上留一个
// 空 sqlite 文件（配置显式化——没有明确配置就不该有副作用）。
func openSessionsIndex(configFlag string) (*sessionindex.Store, string) {
	configPath, err := configTargetPath(configFlag)
	if err != nil {
		return nil, "无法确定配置落点（" + err.Error() + "），工具将返回未配置错误"
	}
	file, err := instances.Load(configPath)
	if err != nil {
		return nil, "读取配置失败（" + err.Error() + "），工具将返回未配置错误"
	}
	if file.Sessions == nil || file.Sessions.Storage == nil ||
		strings.TrimSpace(file.Sessions.Storage.Endpoint) == "" ||
		strings.TrimSpace(file.Sessions.Storage.Bucket) == "" {
		return nil, "未启用会话记录索引（" + configPath + " 的 sessions.storage 未配置 endpoint/bucket）"
	}
	runtime, err := file.ResolveRuntime("")
	if err != nil {
		return nil, "解析 runtime 失败（" + err.Error() + "），工具将返回未配置错误"
	}
	indexPath := runtime.SessionsIndexPath()
	if indexPath == "" {
		return nil, "状态库已关闭（state-file = off），会话记录索引随之停用"
	}
	index, err := sessionindex.Open(indexPath)
	if err != nil {
		return nil, "打开会话记录索引失败（" + indexPath + "）：" + err.Error()
	}
	return index, ""
}
