## Labels

当前标签体系服务于 Issue 和 PR。带 `/` 前缀的标签是 Gitea scoped label，同一前缀下互斥。体系由 `assistant action label-sync`
强制收敛（补齐缺失、scoped 设为互斥、删除不在体系内的标签），不要创建或手改体系外标签：

- 类型：`type/bug`、`type/feature`、`type/refactor`、`type/task`
- 优先级：`priority/high`、`priority/low`；默认 normal 不打标签
- Issue 状态：`status/triage`、`status/confirmed`、`status/in-process`、`status/blocked`
- 关闭原因：`duplicate`、`wontfix`
- 流程卡点：`needs-info`

PR 使用以下状态标签：

- `status/in-progress`：开发中
- `status/review`：等待 review（存在未回应的官方评审请求、`@ai`/`@reviewer` 提及或行首 `/review` 命令）
- `status/changes-requested`：要求修改（含 reviewer 留下的待回应 COMMENT、与目标分支冲突或落后待解决）
- `status/approved`：可以合并

PR 同时使用以下行动方标签，显式表达当前轮到谁处理：

- `awaiting/author`：等待 PR 作者（对应 `status/in-progress`、`status/changes-requested`）
- `awaiting/reviewer`：等待 reviewer（对应 `status/review`）
- `awaiting/merge`：等待机械合并（automerge 在事件驱动的 sync 后立即执行，每 5 分钟 schedule 兜底；
  合并前状态评审者 `merge` 账号对 head 会签——内容批准（`ai`）+ 状态会签双批准缺一不可）（对应
  `status/approved`）

`status/*` 与 `awaiting/*` 由系统根据原生 Pull Review 从同一判断确定性导出，开发者不手动设置。

`status/triage` 触发 Issue review。review 必须产生以下结果之一：

1. 有效：设置 `status/confirmed`，并设置 `priority/high`、`priority/low` 或不设置优先级标签（normal）。
2. 信息不足：说明需要补充的具体信息，添加 `needs-info`，并移除 `status/triage`。信息补齐后重新添加
   `status/triage`。
3. 重复 / 不处理：在评论中说明依据，添加 `duplicate` 或 `wontfix`，系统会自动关闭 Issue。

开发者完成修改后使用 Gitea 原生「请求评审」功能，或在 PR 评论中 `@ai` / `@reviewer`（等效的轻量复审请求）、发独占行首的
`/review` 命令（刻意的拉起指令，`/reviewer`、行内引用不算）。三者等价，都会登记为官方评审请求（分支保护
`block_on_official_review_requests` 按它阻塞合并；reviewer 回应后系统撤回遗留请求）。reviewer 回应过之后，重新点击
「请求评审」按钮（产生晚于最新内容结论的请求记录）与 `@ai` / `@reviewer` 提及评论、行首 `/review` 命令同为有效复审
信号（记录缺失的版本/场景退化为提及与命令）。reviewer 必须给出评论并提交 `REQUEST_CHANGES` 或 `APPROVED`——只要还有
未解决项就不得批准；批准前必须先发布评审结论评论，再提交 `APPROVED`。reviewer 提交的 `COMMENT` review 表示有待作者
回应的讨论，系统会设置 `status/changes-requested`，作者必须回复。PR 分支落后基础分支或无法合并时，系统在开发中状态下标记
`status/changes-requested`（已有批准等评审结论不覆盖；不自动提交驳回 review，门禁只在合并时校验，不阻断评审）；已批准的 PR 由
automerge 跳过，需 rebase 后重新评审。

注意：main 的分支保护开启 `dismiss_stale_approvals`，**向 PR 推送新提交会使已有的 `APPROVED` 失效**，PR 回到
`awaiting/author`（等待作者）。这是有意设计：批准只背书被评审过的那批提交。因此推送评审修复后，必须重新表达评审意图把 PR 交还
reviewer（reviewer 已回应过时用 `@ai` / `@reviewer` 提及评论或行首
`/review` 命令）；请求 review 前请把本次改动全部推完，避免批准被后续推送反复作废。

## PR and Issues

- 如果存在 gitea mcp 工具, 优先使用 mcp 工具, 否则使用 `tea` cli
- 创建对应的分支时, 应创建对应的 PR, 并标记为 `WIP` 状态, 避免意外合并（WIP/draft 不进评审队列，也不参与 automerge）
- 相关的功能请求, bugs 修复应尽可能在同一个 PR 中完成, 避免在多个 PR 中处理, 除非有足够的理由
- 一个 PR 最终要么 fix all bugs 合并, 要么创建相关 Iss 并合并
- PR 合并只能由 `merge` 账号执行（合并白名单只含 merge，管理员也须遵守分支保护），即 automerge CI：PR 获得
  `status/approved` + `awaiting/merge` 且门禁全绿（分支未落后 main、无冲突、必要检查通过、无未回应的官方评审请求）时自动
  squash 合并，一次合并使其余 open PR 全部过期（需 rebase 后重新评审）。合并采用**双批准**：内容批准（reviewer `ai`）+ 状态会签
  （状态评审者 `merge` 账号，合并前对 head 盖章，纯机械核验检查/冲突/落后）；需要立即合并时可 `workflow_dispatch` 召唤
  automerge 流程（事件驱动之外，5 分钟 schedule 兜底）
- 当上游发生变更时, 优先考虑 `rebase` 合并（作者 rebase 到 origin/main 后重新走评审/合并门禁）
- **reviewer 人员账号名称为 `ai`, 别名 `reviewer`；状态评审者（机械核验与会签）账号为 `merge`**
- 所有 PR 提交变更前或更新前, 要求检查并 rebase origin/main
- PR 中允许包含目标代码和相关的脚手架 (包括 AGENTS.md CLAUDE.md skills 等相关变动), review 不应阻止或要求单独 PR
- 本地脚手架由 `assistant install` 统一管理（CLAUDE.md 的 `@AGENTS.md`、AGENTS.md 段落、skills、workflow、MCP 配置、
  `.claude/settings.json` 的 env/权限托管项、`.assistant/review.md` 的评审约定段落），
  生成内容不要手改；内容更新走 assistant 仓库的 `content/`、`skills/` 源文件
- 项目评审约定写在 `.assistant/review.md`（托管段落之外）：评审会话会把它作为附加 system 提示词注入，
  适合放目录边界、必须通过的检查、兼容性要求等项目特有规则

## Commit 检查清单

- 本次变更的提交是否属于一个主题, 如果否, 则分开提交
- 是否移除`dead code`(有特殊要求需要补充注释), 如果否, 则移除后提交
- 相关文档是否已经更新, 过时内容, 不存在的引用是否移除, 如果否, 则更新文档, 清理过时内容, 移除不存在的引用
- 是否移除失效, 过时的测试, 如果没有, 移除后提交
- 是否对测试代码进行测试, 如果没有, 补充测试后提交
- rebase/解决冲突后是否残留合并冲突标记：pre-commit 的 merge-marker-guard 会拦截，但提交前仍应通读完整 diff（`git add -A`
  会把冲突标记一起暂存，dprint 还会把标记行重排进正文，事后极难发现）

Note: 提交前只运行覆盖本次变更及其直接受影响模块的 fmt、lint、test 和 typecheck。只有发布流程或用户明确要求时才运行全仓检查。
