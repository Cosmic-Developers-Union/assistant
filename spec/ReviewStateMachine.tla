---- MODULE ReviewStateMachine ----
(***************************************************************************)
(* gitea-assistant 评审状态机形式化规约（双批准模型）                       *)
(*                                                                         *)
(* 对应实现: internal/status/manager.go 的 reconcilePullRequest 与          *)
(* internal/status/merge.go 的 countersignState + AutoMerge。               *)
(* 建模单个 PR（sync 按 PR 独立 reconcile）。验证目标：                     *)
(*   - 安全性：任何可达状态下，sync 收敛后（无未同步的环境变更）的标签       *)
(*     满足行动方归属语义——球永远明确地属于作者、内容评审者或合并人之一      *)
(*   - 活性：环境静默后系统收敛到正确标签（不存在标签层面的死锁/空等）       *)
(*                                                                         *)
(* 关键建模决策：                                                            *)
(*   - 双批准模型：review 按作者角色分两条通道——内容结论（contentState，    *)
(*     人员 reviewer "ai" 的批准/驳回/COMMENT）驱动状态标签；状态结论        *)
(*     (stateState，状态评审者 "merge" 的门禁驳回与会签) 只表达门禁健康度。  *)
(*     两条通道互不可替代：内容批准不是状态批准，状态批准不是内容批准        *)
(*   - 状态批准是惰性会签：Merge 动作在同一机械动作内完成「会签 + 合并」，   *)
(*     不存在「盖了章等着」的中间态；会签使 required approvals 计满，        *)
(*     同时以官方批准覆盖状态评审者自己的门禁驳回                           *)
(*   - 「reviewer 提交 review 后 Gitea 是否消费 requested_reviewers」实测    *)
(*     与源码结论矛盾，建模为非确定性选择——两种行为下规约都必须成立          *)
(*   - requested_reviewers 无时间戳，无法区分「旧请求残留」与「重新请求」；  *)
(*     原生「请求评审」按钮会生成晚于最新内容结论的请求记录（recordFresh，  *)
(*     实现可观测），与 requested_reviewers 的有无共同构成意图通道；记录    *)
(*     缺失的版本/场景退化为其余通道（recordFresh 不被置位亦须成立）         *)
(*   - 团队评审请求无法按成员身份吸收（实现不知道团队里谁回应了），一律      *)
(*     视为有效意图；且未实测 Gitea 是否会消费团队请求，保守建模为永不消费  *)
(*   - dismiss_stale_approvals 使推送作废旧批准（内容批准与会签一并作废）；  *)
(*     旧批准之下可能露出更早的 review，该情形归约为 none（那些状态可由     *)
(*     其他动作序列直接到达）。responded 是单调历史（含被 dismiss 的        *)
(*     review），不随作废回退；状态驳回（requestChanges）推送后仅标记       *)
(*     stale 不被 dismiss，建模为保持不变                                    *)
(*   - 必要检查门禁（分支保护 required status checks）建模为 checks；        *)
(*     Merge 动作本身也受门禁约束，对应 Gitea 的合并按钮硬性限制             *)
(***************************************************************************)
EXTENDS TLC

VARIABLES
    contentState,   \* 最新内容结论（"ai" 提交的正式 review）: "none" | "approved" | "requestChanges" | "comment"
    requestedUsers, \* requested_reviewers 中的用户（⊆ {"ai"}；请求面向内容评审者）
    requestedTeam,  \* 是否存在团队评审请求（无法按成员吸收，一律视为意图）
    responded,      \* 提交过正式 review 的账号集合（单调历史，含被 dismiss 的）
    requestFresh,   \* ground truth: ai 的请求创建于其最新回应之后（实现无法观测，仅文档化用）
    recordFresh,    \* 最新内容结论之后存在原生按钮生成的评审请求记录（#86 实测信号）
    freshMention,   \* 最新内容结论之后的评论中存在评审意图信号：实现中是两条独立
                    \* 谓词的并集（hasReviewerMention ‖ hasReviewCommand，即 @ai/@reviewer
                    \* 提及或行首 /review 命令）；同一扫描窗口、同一效果，建模为同一信号
    stateState,     \* 状态通道最新结论: "none" | "approved"（会签） | "requestChanges"（门禁驳回）
    behind,         \* merge_base != base.sha（落后于基础分支）
    mergeable,      \* Gitea 判定可合并（无冲突）
    checks,         \* 必要检查聚合: "passed" | "pending" | "failed"
    labels,         \* sync 写出的 status/* 标签: "inProgress" | "review" | "changesRequested" | "approved"
    dirty,          \* 上次 sync 之后环境发生过变化（标签可能滞后）
    quiet,          \* 环境静默（活性性质的触发条件）
    merged          \* 已合并（终态）

vars == <<contentState, requestedUsers, requestedTeam, responded, requestFresh,
          recordFresh, freshMention, stateState, behind, mergeable, checks,
          labels, dirty, quiet, merged>>

ContentStates == {"none", "approved", "requestChanges", "comment"}
States == {"none", "approved", "requestChanges", "comment"}
StateStates == {"none", "approved", "requestChanges"}
CheckStates == {"passed", "pending", "failed"}
Labels == {"inProgress", "review", "changesRequested", "approved"}

Init ==
    /\ contentState = "none"
    /\ requestedUsers = {}
    /\ requestedTeam = FALSE
    /\ responded = {}
    /\ requestFresh = FALSE
    /\ recordFresh = FALSE
    /\ freshMention = FALSE
    /\ stateState = "none"
    /\ behind = FALSE
    /\ mergeable = TRUE
    /\ checks = "passed"
    /\ labels = "inProgress"
    /\ dirty = TRUE             \* 新 PR 尚未同步
    /\ quiet = FALSE
    /\ merged = FALSE

(***************************************************************************)
(* 与 manager.go 对应的派生量                                               *)
(***************************************************************************)

ContentFound == contentState # "none"

\* hasUnansweredReviewRequest：提交过正式 review 的 reviewer 名下请求视为
\* 已回应；从未回应者的请求与团队请求仍构成意图。状态评审者的结论不吸收
\* 对人员 reviewer 的请求。
HasUnansweredRequest ==
    requestedTeam \/ \E user \in requestedUsers : user \notin responded

Intent == HasUnansweredRequest \/ recordFresh \/ freshMention

\* sync 进入门禁求值时使用的等效状态（有意图时按 REQUEST_REVIEW 处理）
EffectiveState == IF Intent THEN "requestReview" ELSE contentState

\* sync 在非跳过情形下会设置的标签。与 Sync 动作分头编码，由 MirrorCorrect
\* 交叉验证两处实现一致。
ExpectedLabels ==
    IF ~ContentFound /\ ~Intent
    THEN IF ~mergeable \/ behind THEN "changesRequested" ELSE "inProgress"
    ELSE IF EffectiveState \in {"requestReview", "approved"}
         THEN IF ~mergeable \/ behind \/ checks = "failed"
              THEN "changesRequested"
              ELSE IF EffectiveState = "requestReview" THEN "review" ELSE "approved"
         ELSE "changesRequested"

(***************************************************************************)
(* sync（assistant reconcile）                                              *)
(***************************************************************************)

Sync ==
    /\ ~merged
    /\ IF ~ContentFound /\ ~Intent
       THEN \* 无内容结论且无意图：开发中；冲突/落后提升为「要求修改」
            \* （状态通道的门禁驳回只表达门禁健康度，门禁恢复即回到开发中）
            /\ labels' = IF ~mergeable \/ behind THEN "changesRequested" ELSE "inProgress"
            /\ UNCHANGED <<contentState, requestedUsers, responded, stateState>>
       ELSE IF EffectiveState \in {"requestReview", "approved"}
            THEN IF checks = "pending"
                 THEN UNCHANGED <<labels, contentState, requestedUsers, responded, stateState>>  \* 检查运行中，本轮跳过
                 ELSE IF ~mergeable \/ behind \/ checks = "failed"
                      THEN \* 门禁未过：状态评审者驳回（状态通道已有驳回时不重复提交）
                           \* 提交的 review 进入时间线，responded 随之吸收其作者
                           /\ labels' = "changesRequested"
                           /\ IF stateState # "requestChanges"
                              THEN /\ stateState' = "requestChanges"
                                   /\ responded' = responded \cup {"merge"}
                              ELSE UNCHANGED <<stateState, responded>>
                      ELSE /\ labels' = IF EffectiveState = "requestReview" THEN "review" ELSE "approved"
                           /\ UNCHANGED <<contentState, requestedUsers, responded, stateState>>
            ELSE \* 驳回或 COMMENT 讨论：等待作者
                 /\ labels' = "changesRequested"
                 /\ UNCHANGED <<contentState, requestedUsers, responded, stateState>>
    /\ dirty' = FALSE
    /\ UNCHANGED <<requestedTeam, requestFresh, recordFresh, freshMention, behind,
                  mergeable, checks, quiet, merged>>

(***************************************************************************)
(* 环境动作（作者 / 内容评审者 / CI / 基础分支，非确定发生）                 *)
(***************************************************************************)

\* 原生「请求评审」。仅当请求不存在时产生新信号——重复请求同一 reviewer
\* 是 no-op（该 reviewer 已在 requestedUsers 中时本动作无效果，故设前提）。
AuthorRequest ==
    /\ ~merged /\ ~quiet
    /\ "ai" \notin requestedUsers
    /\ requestedUsers' = requestedUsers \cup {"ai"}
    /\ requestFresh' = TRUE
    /\ dirty' = TRUE
    /\ UNCHANGED <<contentState, requestedTeam, responded, recordFresh, freshMention,
                  stateState, behind, mergeable, checks, labels, quiet, merged>>

\* 团队评审请求：无法按成员身份吸收，永不消费（保守建模，见模块头注释）。
AuthorRequestTeam ==
    /\ ~merged /\ ~quiet /\ ~requestedTeam
    /\ requestedTeam' = TRUE
    /\ dirty' = TRUE
    /\ UNCHANGED <<contentState, requestedUsers, responded, requestFresh, recordFresh,
                  freshMention, stateState, behind, mergeable, checks, labels, quiet, merged>>

\* 原生按钮复审：reviewer 已回应后重新点击「请求评审」，生成晚于最新内容
\* 结论的请求记录（Gitea 1.27 实测稳定生成；记录缺失的版本无此动作，
\* recordFresh 保持 FALSE，退化为提及/命令通道）。
AuthorRequestButton ==
    /\ ~merged /\ ~quiet /\ ~recordFresh
    /\ recordFresh' = TRUE
    /\ dirty' = TRUE
    /\ UNCHANGED <<contentState, requestedUsers, requestedTeam, responded, requestFresh,
                  freshMention, stateState, behind, mergeable, checks, labels, quiet, merged>>

\* 评论中 @ai/@reviewer 提及（最新内容结论之后才构成新意图；freshMention
\* 已为 TRUE 时再提及无新效果）。
AuthorMention ==
    /\ ~merged /\ ~quiet /\ ~freshMention
    /\ freshMention' = TRUE
    /\ dirty' = TRUE
    /\ UNCHANGED <<contentState, requestedUsers, requestedTeam, responded, requestFresh,
                  recordFresh, stateState, behind, mergeable, checks, labels, quiet, merged>>

\* 内容评审者提交正式 review。Gitea 是否消费其名下请求是非确定的：
\* 源码层面会删除（世界 A），本实例实测残留（世界 B）——规约对两者都须成立。
ReviewerSubmit(state) ==
    /\ ~merged /\ ~quiet
    /\ state \in {"approved", "requestChanges", "comment"}
    /\ contentState' = state
    /\ responded' = responded \cup {"ai"}
    /\ freshMention' = FALSE        \* 提及扫描窗口移到本次 review 之后
    /\ recordFresh' = FALSE         \* 记录扫描窗口同样移到本次 review 之后
    /\ requestFresh' = FALSE        \* 即便请求残留，也已先于本次回应（成为历史）
    /\ \/ requestedUsers' = requestedUsers \ {"ai"}   \* 世界 A：请求被消费
       \/ UNCHANGED requestedUsers                    \* 世界 B：请求残留
    /\ dirty' = TRUE
    /\ UNCHANGED <<requestedTeam, stateState, behind, mergeable, checks, labels, quiet, merged>>

\* 作者推送新提交：检查重跑（pending），可能引入或解决冲突；
\* dismiss_stale_approvals 作废旧批准——内容批准与会签一并作废（归约为
\* none，见模块头注释）；状态驳回（requestChanges）仅标记 stale，建模为
\* 保持不变；responded 是单调历史，不随作废回退。
AuthorPush ==
    /\ ~merged /\ ~quiet
    /\ contentState' = IF contentState = "approved" THEN "none" ELSE contentState
    /\ stateState' = IF stateState = "approved" THEN "none" ELSE stateState
    /\ checks' = "pending"
    /\ mergeable' \in {TRUE, FALSE}
    /\ dirty' = TRUE
    /\ UNCHANGED <<requestedUsers, requestedTeam, responded, requestFresh, recordFresh,
                  freshMention, behind, labels, quiet, merged>>

\* 作者 rebase 到最新基础分支：消除落后与冲突，检查重跑，旧批准同样作废。
AuthorRebase ==
    /\ ~merged /\ ~quiet
    /\ contentState' = IF contentState = "approved" THEN "none" ELSE contentState
    /\ stateState' = IF stateState = "approved" THEN "none" ELSE stateState
    /\ behind' = FALSE
    /\ mergeable' = TRUE
    /\ checks' = "pending"
    /\ dirty' = TRUE
    /\ UNCHANGED <<requestedUsers, requestedTeam, responded, requestFresh, recordFresh,
                  freshMention, labels, quiet, merged>>

\* 基础分支前移（他人合并了别的 PR）。
BaseAdvances ==
    /\ ~merged /\ ~quiet /\ ~behind
    /\ behind' = TRUE
    /\ dirty' = TRUE
    /\ UNCHANGED <<contentState, requestedUsers, requestedTeam, responded,
                  requestFresh, recordFresh, freshMention, stateState, mergeable,
                  checks, labels, quiet, merged>>

\* 基础分支变更导致冲突（无需 PR 侧推送）。
ConflictAppears ==
    /\ ~merged /\ ~quiet /\ mergeable
    /\ mergeable' = FALSE
    /\ dirty' = TRUE
    /\ UNCHANGED <<contentState, requestedUsers, requestedTeam, responded, requestFresh,
                  recordFresh, freshMention, stateState, behind, checks, labels, quiet, merged>>

\* 检查状态迁移：pending 出结果、重跑翻转、抖动，统一建模为任意迁移。
ChecksTransition ==
    /\ ~merged /\ ~quiet
    /\ checks' \in CheckStates \ {checks}
    /\ dirty' = TRUE
    /\ UNCHANGED <<contentState, requestedUsers, requestedTeam, responded, requestFresh,
                  recordFresh, freshMention, stateState, behind, mergeable, labels, quiet, merged>>

\* 环境静默：此后只有 sync（与终态）可发生，用于活性收敛性质。
EnvQuiet ==
    /\ ~quiet
    /\ quiet' = TRUE
    /\ UNCHANGED <<contentState, requestedUsers, requestedTeam, responded, requestFresh,
                  recordFresh, freshMention, stateState, behind, mergeable, checks,
                  labels, dirty, merged>>

\* 会签 + 合并（同一机械动作）：门禁全绿时状态评审者对 head 盖状态批准——
\* required approvals 的第二票，同时以官方批准覆盖状态通道此前的驳回——
\* 随即合并。人类手动合并同样受此约束：没有会签在场（或召唤 automerge）
\* 就无法满足 required approvals。
Merge ==
    /\ ~merged /\ ~quiet
    /\ labels = "approved"
    /\ checks = "passed"
    /\ mergeable
    /\ ~behind
    /\ stateState' = "approved"
    /\ responded' = responded \cup {"merge"}   \* 会签进入时间线
    /\ merged' = TRUE
    /\ dirty' = FALSE
    /\ UNCHANGED <<contentState, requestedUsers, requestedTeam, requestFresh,
                  recordFresh, freshMention, behind, mergeable, checks, labels, quiet>>

MergedStutter ==
    /\ merged
    /\ UNCHANGED vars

Next ==
    \/ Sync
    \/ AuthorRequest
    \/ AuthorRequestTeam
    \/ AuthorRequestButton
    \/ AuthorMention
    \/ \E state \in {"approved", "requestChanges", "comment"} : ReviewerSubmit(state)
    \/ AuthorPush
    \/ AuthorRebase
    \/ BaseAdvances
    \/ ConflictAppears
    \/ ChecksTransition
    \/ EnvQuiet
    \/ Merge
    \/ MergedStutter

\* sync 由事件与定时 schedule 驱动，假定它持续运行（弱公平）。
Spec == Init /\ [][Next]_vars /\ WF_vars(Sync)

(***************************************************************************)
(* 安全性不变量                                                             *)
(***************************************************************************)

TypeOK ==
    /\ contentState \in ContentStates
    /\ requestedUsers \subseteq {"ai"}
    /\ requestedTeam \in BOOLEAN
    /\ responded \subseteq {"ai", "merge"}
    \* 内容结论只能来自内容评审者
    /\ contentState # "none" => "ai" \in responded
    /\ requestFresh \in BOOLEAN
    /\ requestFresh => "ai" \in requestedUsers
    /\ recordFresh \in BOOLEAN
    /\ freshMention \in BOOLEAN
    /\ stateState \in StateStates
    /\ behind \in BOOLEAN
    /\ mergeable \in BOOLEAN
    /\ checks \in CheckStates
    /\ labels \in Labels
    /\ dirty \in BOOLEAN
    /\ quiet \in BOOLEAN
    /\ merged \in BOOLEAN

\* sync 收敛后（无未同步变更、检查不在运行中——pending 时 sync 按设计跳过），
\* 标签与算法输出一致。Sync 与 ExpectedLabels 是同一算法的两份独立编码，
\* 相互印证。
MirrorCorrect ==
    (~dirty /\ ~merged /\ checks # "pending") => labels = ExpectedLabels

\* S1 球权归属：内容评审者本人回应过（驳回或讨论）、所有请求都已吸收、无新
\* 提及且无新的按钮请求记录 → 球确定性在作者。这是「COMMENT 后 PR 不得停留
\* 在 review 空等」的直接表达。意图可能来自其他未回应者或团队请求——那种情
\* 形下 PR 留在 review 是正确行为（确有 reviewer 未回应），不在本性质约束内。
BallWithAuthorAfterResponse ==
    (~dirty /\ ~merged /\ checks # "pending"
     /\ contentState \in {"requestChanges", "comment"}
     /\ ~HasUnansweredRequest /\ ~recordFresh /\ ~freshMention)
    => labels = "changesRequested"

\* S2 approved 标签的真实性：只有内容评审者本人批准且门禁全绿时才成立——
\* 状态评审者的会签（stateState = "approved"）绝不能单独驱动 approved。
ApprovedGenuine ==
    (~dirty /\ ~merged /\ checks # "pending" /\ labels = "approved")
    => (contentState = "approved" /\ checks = "passed" /\ mergeable /\ ~behind)

\* S3 review 标签的真实性：门禁全绿且存在有效评审意图。
ReviewGenuine ==
    (~dirty /\ ~merged /\ checks # "pending" /\ labels = "review")
    => (checks = "passed" /\ mergeable /\ ~behind /\ Intent)

\* S4 落后、冲突或必要检查失败时，绝不显示 review/approved。
BlockedNeverReviewable ==
    (~dirty /\ ~merged /\ checks # "pending" /\ (behind \/ ~mergeable \/ checks = "failed"))
    => labels \in {"inProgress", "changesRequested"}

\* S5 无内容结论、无意图且分支健康 → 开发中。状态会签在场也不能例外——
\* 它是状态通道的结论，不构成内容批准。
FreshPRIsInProgress ==
    (~dirty /\ ~merged /\ checks # "pending"
     /\ ~ContentFound /\ requestedUsers = {} /\ ~requestedTeam
     /\ ~recordFresh /\ ~freshMention /\ mergeable /\ ~behind)
    => labels = "inProgress"

\* S6 状态会签只出现在合并前一刻：会签在场蕴含门禁全绿且标签已到 approved
\* （会签与合并是同一机械动作，不存在「盖章等待」或「带病盖章」）。
CountersignOnlyWhenGreen ==
    (~dirty /\ ~merged /\ stateState = "approved")
    => (checks = "passed" /\ mergeable /\ ~behind /\ labels = "approved")

(***************************************************************************)
(* 活性：环境静默后系统收敛——标签与算法输出一致且不再变化。                 *)
(* 不存在「标签等待 reviewer 而 reviewer 在等作者」式的死锁。               *)
(***************************************************************************)

ConvergesWhenQuiet ==
    quiet ~> [](merged \/ (~dirty /\ (checks # "pending" => labels = ExpectedLabels)))

====
