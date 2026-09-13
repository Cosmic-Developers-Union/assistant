// Package repoinstall 为最终开发者把仓库配置成 assistant 工作流：评审 skill、
// AGENTS.md、仓库 Actions workflow，以及各 AI CLI（claude/opencode/codex）的
// MCP 配置。所有写入都带 marker，uninstall 可精确移除；重复 install 幂等。
package repoinstall

import "fmt"

// Marker 是自动生成内容的标识：卸载与覆盖只触碰带 marker 的内容。
const Marker = "managed-by: assistant"

// SkillTemplate 是评审/分诊协议（.claude/skills/review/SKILL.md）。
const SkillTemplate = `<!-- managed-by: assistant（assistant install 生成，可重复安装覆盖） -->
# review

本仓库的评审与分诊协议。执行 ` + "`review pr #N`" + ` / ` + "`triage issue #N`" + ` 时严格遵循。

## PR 审查协议

1. 只处理被点名的 PR；用 gitea MCP 读取 PR、diff、评论与检查状态。
2. 评审对象是当前目录检出的 head（提示词里会给出钉定的 head）：
   - 用 MCP 查到 PR 的 head 与提示词不一致时，说明作者已推送新代码：
     不要评审旧代码，提交 REQUEST_CHANGES，正文注明「head 已更新，需以新 head
     重新评审」，然后结束。
3. 检查（所有失败都要在结论中引用文件与行号）：
   - 正确性、并发/边界、错误处理；
   - 测试是否覆盖新行为与回归点；
   - 安全（注入、凭据泄露、越权）。
4. 需要验证时在检出内运行构建/测试（如 ` + "`go test ./...`" + `）；不要修改代码。
5. 以 Gitea 原生 Pull Request Review 提交结论：
   - 通过 → APPROVED；需要修改 → REQUEST_CHANGES（逐条列出）；仅讨论 → COMMENT。
   - 正文首行注明所评审的 head（短 sha）。
6. 不要合并、不要改标签——标签由 assistant sync 维护。

## Issue 分诊协议

1. 读取 Issue 正文与评论，判断类型与优先级，在下方标签体系中选最贴切的一个
   类型标签；必要时给 priority/high 或 priority/low。
2. 无法复现/信息不足 → 打 needs-info 并评论说明还需要什么信息；
   与已有 Issue 重复 → duplicate；确认不处理 → wontfix（两者会触发自动关闭）。
3. 有效且可行动 → 移除 status/triage，按结论打 status/confirmed 或
   status/in-process，并评论分诊结论（类型、优先级、下一步）。
4. 只处理被点名的 Issue。

## 标签体系（由 assistant sync 强制维护）

- 类型：type/bug、type/feature、type/refactor、type/task
- 优先级：priority/high、priority/low
- Issue 生命周期：status/triage → status/confirmed / status/in-process / status/blocked
- PR 状态：status/in-progress、status/review、status/changes-requested、status/approved
- 行动方：awaiting/author、awaiting/reviewer、awaiting/merge
- 关闭原因：duplicate、wontfix、needs-info
`

// AgentTemplate 是追加/新建的 AGENTS.md 内容（marker 包裹，可卸载）。
const AgentTemplate = `<!-- managed-by: assistant（assistant install 生成，可重复安装覆盖） -->
## assistant 自动化约定

本仓库由 assistant 维护标签与评审流程，改动代码时遵守以下约定：

- **标签是流程状态机**：不要手工设置 ` + "`status/*`" + `、` + "`awaiting/*`" + `、` + "`type/*`" + `、
  ` + "`priority/*`" + ` 标签（sync 会收敛）；需要评审时用下方触发方式。
- **触发评审**：评论 ` + "`/review`" + `、` + "`@ai`" + `，或使用 Gitea 的「请求评审」；
  三者等价，都会登记官方评审请求并进入评审队列。
- **PR 必须经评审**：分支保护要求 2 个批准（内容评审 + 状态会签），只有
  ` + "`merge`" + ` 账号可以合并；落后 main、有未解决讨论或被驳回都会阻塞合并。
- **作者推送会作废旧评审**：批准只背书被评审的那批提交，rebase/推送后需重新评审。
- **AI 评审会话**由宿主机 dispatcher 拉起；评审结论以 Gitea 原生 review 落库。
- 分诊与评审的详细协议见 ` + "`.claude/skills/review/SKILL.md`" + `。
<!-- /managed-by: assistant -->
`

// WorkflowSyncTemplate 是事件驱动的标签/状态同步工作流。
const WorkflowSyncTemplate = `# managed-by: assistant（assistant install 生成，可重复安装覆盖）
name: Gitea Assistant
on:
  issues: [opened, edited, closed, reopened, labeled, unlabeled]
  pull_request: [opened, edited, closed, reopened, labeled, unlabeled, synchronize]
  issue_comment: [created]
concurrency:
  group: assistant-sync
  cancel-in-progress: false
jobs:
  sync:
    runs-on: ubuntu-latest
    container:
      image: ghcr.io/cosmic-developers-union/assistant:latest
    steps:
      - run: assistant sync --verbose
        env:
          GITEA_HOST: ${{ github.server_url }}
          GITEA_ACCESS_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          GITEA_REPOSITORY: ${{ github.repository }}
          GITEA_STATE_REVIEWER: ${{ vars.STATE_REVIEWER }}
          GITEA_STATE_TOKEN: ${{ secrets.STATE_TOKEN }}
          GITEA_BRANCH_PROTECTION_TOKEN: ${{ secrets.BRANCH_PROTECTION_TOKEN }}
`

// WorkflowAutoMergeTemplate 是定时自动合并工作流（以 merge 身份执行）。
const WorkflowAutoMergeTemplate = `# managed-by: assistant（assistant install 生成，可重复安装覆盖）
name: Gitea Assistant AutoMerge
on:
  schedule:
    - cron: "*/5 * * * *"
  workflow_dispatch:
concurrency:
  group: assistant-automerge
  cancel-in-progress: false
jobs:
  automerge:
    runs-on: ubuntu-latest
    container:
      image: ghcr.io/cosmic-developers-union/assistant:latest
    steps:
      - run: assistant automerge --verbose
        env:
          GITEA_HOST: ${{ github.server_url }}
          # 合并白名单只允许 merge：自动合并必须以 merge 身份执行
          GITEA_ACCESS_TOKEN: ${{ secrets.STATE_TOKEN }}
          GITEA_REPOSITORY: ${{ github.repository }}
          GITEA_STATE_REVIEWER: ${{ vars.STATE_REVIEWER }}
          GITEA_STATE_TOKEN: ${{ secrets.STATE_TOKEN }}
          GITEA_BRANCH_PROTECTION_TOKEN: ${{ secrets.BRANCH_PROTECTION_TOKEN }}
`

// ManagedSkillPath 返回 skill 相对路径。
func ManagedSkillPath() string { return ".claude/skills/review/SKILL.md" }

// ManagedAgentPath 是 AGENTS.md。
func ManagedAgentPath() string { return "AGENTS.md" }

// ManagedWorkflowPaths 返回仓库级 workflow 相对路径。
func ManagedWorkflowPaths() []string {
	return []string{
		".gitea/workflows/assistant.yml",
		".gitea/workflows/automerge.yml",
	}
}

// MarkerLine 返回注释形态的 marker 行。
func MarkerLine() string { return fmt.Sprintf("<!-- %s -->", Marker) }
