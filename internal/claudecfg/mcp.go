package claudecfg

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// assistant 自己提供、与仓库内容无关的 MCP server 名：gitea（读写 Issue/PR，
// 凭据由会话环境或凭据库提供）与 daemon（自举的只读状态 MCP）。
const (
	MCPServerGitea  = "gitea"
	MCPServerDaemon = "daemon"
)

// MCPServerDef 返回 stdio MCP server 定义（Claude Code 的 mcpServers 条目形态）。
func MCPServerDef(command string, args ...string) map[string]any {
	values := make([]any, 0, len(args))
	for _, arg := range args {
		values = append(values, arg)
	}
	return map[string]any{"command": command, "args": values}
}

// GiteaMCPServer 是 assistant 指定的 gitea MCP：server 由 assistant 二进制自带
// （`assistant mcp gitea`），凭据来自会话环境（调度器钉定的 reviewer 令牌）或
// 凭据库——所以它跟「仓库里有没有 .mcp.json、写的是什么」无关，会话总是用它。
func GiteaMCPServer(command string) map[string]any {
	return MCPServerDef(command, "mcp", "gitea")
}

// DaemonMCPServer 是自举的 daemon 状态 MCP（靠端点文件发现运行中的 daemon）。
func DaemonMCPServer(command string) map[string]any {
	return MCPServerDef(command, "mcp", "daemon")
}

var (
	assistantCommandOnce sync.Once
	assistantCommand     string
)

// AssistantCommand 返回当前 assistant 可执行文件的绝对路径（取不到时回退 PATH
// 名字 assistant）：会话里的 assistant MCP 用它启动，于是既不看会话的工作目录与
// PATH，也不要求会话镜像里预装了 assistant。
func AssistantCommand() string {
	assistantCommandOnce.Do(func() {
		assistantCommand = "assistant"
		path, err := os.Executable()
		if err != nil {
			return
		}
		absolute, err := filepath.Abs(strings.TrimSpace(path))
		if err != nil || absolute == "" {
			return
		}
		assistantCommand = absolute
	})
	return assistantCommand
}
