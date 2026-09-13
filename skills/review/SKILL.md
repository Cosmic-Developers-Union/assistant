---
name: review
description: 按本仓库的 assistant 流程评审 Pull Request（Gitea 原生 review 落库）与分诊 Issue。收到 review pr #N / triage issue #N 指令，或需要按 assistant 标签体系处理 PR/Issue 时使用。
---

# review

本仓库的评审与分诊协议。执行 `review pr #N` / `triage issue #N` 时严格遵循。

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
4. 需要验证时在检出内运行构建/测试（如 `go test ./...`）；不要修改代码。
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
