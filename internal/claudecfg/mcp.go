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
	MCPServerGitea    = "gitea"
	MCPServerDaemon   = "daemon"
	MCPServerSessions = "sessions"
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

// SessionsMCPServer 是会话记录查询 MCP：上下文被压缩后回查完整聊天/评审历史
// （记录由 assistant run 归档到对象存储并建本地索引，本 MCP 只读索引）。
func SessionsMCPServer(command string) map[string]any {
	return MCPServerDef(command, "mcp", "sessions")
}

var (
	assistantCommandOnce sync.Once
	assistantCommand     string
)

// AssistantExecutable 取当前进程的可执行文件路径；测试替换它即可覆盖「取不到自身
// 路径时回退 assistant」这条分支。生产不修改。
var AssistantExecutable = os.Executable

// AssistantCommand 返回会话里 assistant MCP 的启动命令。assistantd 拉起的会话
// 要执行 `assistant mcp gitea`，而 mcp 子命令在 assistant 二进制里，解析顺序：
//  1. ASSISTANT_CLI 环境变量显式指定（两个二进制不在同一前缀时用）；
//  2. 自身就是 assistant（本体或 go run 开发流）：用自身绝对路径，与拆分前一致；
//  3. 同目录的 assistant：两个二进制装在同一个前缀下时直接命中；
//  4. 回退 PATH 名 assistant。
//
// 既不看会话的工作目录，也不要求会话镜像里预装了 assistant。
func AssistantCommand() string {
	assistantCommandOnce.Do(func() {
		assistantCommand = "assistant"
		// 显式覆盖优先于一切推断：部署形态两个二进制可能装在不同前缀下
		if override := strings.TrimSpace(os.Getenv("ASSISTANT_CLI")); override != "" {
			assistantCommand = override
			return
		}
		path, err := AssistantExecutable()
		if err != nil {
			return
		}
		// 先判空再 Abs：filepath.Abs("") 不报错，它返回**当前工作目录**——把
		// 一个空的自身路径当成 cwd 会让会话去启动一个碰巧叫 assistant 的目录。
		trimmed := strings.TrimSpace(path)
		if trimmed == "" {
			return
		}
		absolute, err := filepath.Abs(trimmed)
		if err != nil || absolute == "" {
			return
		}
		// 自身就是 assistant：沿用自身绝对路径（assistant 本体与 go run 开发流
		// 的临时产物都算），行为与拆分前完全一致。
		if filepath.Base(absolute) == "assistant" {
			assistantCommand = absolute
			return
		}
		// assistantd：优先同目录的 assistant——两个二进制装在同一个前缀下
		sibling := filepath.Join(filepath.Dir(absolute), "assistant")
		if info, statErr := os.Stat(sibling); statErr == nil && !info.IsDir() {
			assistantCommand = sibling
			return
		}
	})
	return assistantCommand
}
