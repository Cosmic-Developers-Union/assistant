// Package repoinstall 为最终开发者把仓库配置成 assistant 工作流：评审 skill、
// AGENTS.md、仓库 Actions workflow，以及各 AI CLI（claude/opencode/codex）的
// MCP 配置；Claude 另有项目级 .claude/settings.json（env/权限放行，见
// internal/claudecfg）按白名单键增量托管。生成内容由 templates/ 下的模板绑定
// 渲染（身份约定 ai/merge），全部带 marker，uninstall 可精确移除；重复 install
// 幂等。
package repoinstall

// Marker 是自动生成内容的标识：卸载与覆盖只触碰带 marker 的内容。
const Marker = "managed-by: assistant"

// 身份是约定而不是配置：内容评审者恒为 ai，状态评审者/合并者恒为 merge。
// 仓库内 workflow 与标签流程按这两个名字工作；宿主机侧 config.json 仍可覆盖，
// 但仓库级自动化只承诺约定部署。
const (
	ConventionReviewer = "ai"
	ConventionMerger   = "merge"
)

// DefaultImage 是仓库 workflow 默认运行的 assistant 容器镜像。
const DefaultImage = "ghcr.io/cosmic-developers-union/assistant:latest"

// DefaultSkillsSource 是 review 技能通过 skills CLI 安装时的默认源（本仓库）。
const DefaultSkillsSource = "Cosmic-Developers-Union/assistant"

// 模板文件名（与 templates/ 目录内文件对应）。AGENTS.md 段落来自仓库根的
// content/agents.md（维护者编写），不走这里。
const workflowTemplateName = "workflow.yml.tmpl"

// ManagedSkillPath 返回 claude-code 的技能安装路径（skills CLI 的 agent 目录
// 约定；opencode/codex 见 skillPathForTool）。
func ManagedSkillPath() string { return ".claude/skills/review/SKILL.md" }

// skillPathForTool 返回各 AI CLI 的技能安装路径。
func skillPathForTool(tool string) (string, bool) {
	switch tool {
	case "claude":
		return ".claude/skills/review/SKILL.md", true
	case "opencode", "codex":
		return ".agents/skills/review/SKILL.md", true
	}
	return "", false
}

// ManagedAgentPath 是 AGENTS.md。
func ManagedAgentPath() string { return "AGENTS.md" }

// ManagedClaudePath 是 Claude Code 的项目说明文件：内容稳定为 @AGENTS.md 导入。
func ManagedClaudePath() string { return "CLAUDE.md" }

// ClaudeTemplate 是 CLAUDE.md 的规范内容（Claude Code 的 @ 导入语法）。
const ClaudeTemplate = "@AGENTS.md\n"

// ManagedWorkflowPaths 返回仓库级 workflow 相对路径（单一 workflow，内含
// sync 与 automerge 两个 job）。
func ManagedWorkflowPaths() []string {
	return []string{".gitea/workflows/assistant.yml"}
}

// LegacyWorkflowPaths 是历史版本 install 写入的 workflow：新版本已合并为
// 单一文件，install/uninstall 时会清理带 marker 的旧文件。
func LegacyWorkflowPaths() []string {
	return []string{".gitea/workflows/automerge.yml"}
}
