// Package claudecfg 定义 assistant 对 Claude Code 的托管配置：项目级
// .claude/settings.json（assistant install 写入/核对）与 headless 评审会话的
// 独立 --settings（dispatcher 生成）共用同一份环境变量与权限放行，避免漂移。
//
// 只管理下面明确列出的键/条目；不读取也不写入任何用户级配置（~/.claude）。
package claudecfg

import "encoding/json"

// Env 是托管环境变量：把评审/开发会话的时长与输出上限钉死，键名与取值对齐
// Claude Code 官方环境变量文档。
var Env = map[string]string{
	// 长测试/构建：默认 2 分钟太低，默认上限与最大上限分别抬到 5/30 分钟
	"BASH_DEFAULT_TIMEOUT_MS": "300000",
	"BASH_MAX_TIMEOUT_MS":     "1800000",
	// 测试日志回读上限（官方允许的最大值 150000 字符）
	"BASH_MAX_OUTPUT_LENGTH": "150000",
	// gitea MCP：大 diff / 长 Issue 的执行超时与输出上限
	"MCP_TIMEOUT":           "30000",
	"MAX_MCP_OUTPUT_TOKENS": "50000",
}

// Allow 是托管权限放行：gitea MCP + 只读/低风险命令（写入类命令仍走权限判定）。
var Allow = []string{
	"mcp__gitea",
	"mcp__gitea__*",
	"Bash(assistant:*)",
	"Bash(git status:*)",
	"Bash(git diff:*)",
	"Bash(git log:*)",
	"Bash(git show:*)",
	"Bash(git branch:*)",
	"Bash(git rev-parse:*)",
	"Bash(git worktree list:*)",
}

// SettingSources 是 headless 会话加载的设置来源：只认项目级（随仓库提交的
// 评审基线），不读操作者的用户级与本地私有设置。MCP 工具面另由
// --strict-mcp-config + --mcp-config 钉死。
const SettingSources = "project"

// SessionSettings 生成 headless 会话的独立配置 JSON（--settings 的内容）。
func SessionSettings() ([]byte, error) {
	settings := map[string]any{
		"env": Env,
		"permissions": map[string]any{
			"allow": Allow,
		},
		// 项目级 .mcp.json 不自动启用：会话只用 --mcp-config 注入的服务器
		"enableAllProjectMcpServers": false,
	}
	encoded, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}
