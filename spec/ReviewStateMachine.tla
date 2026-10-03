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
(*     (stateState，状态评审者 "merge" 的会签) 只在合并前一刻产生；门禁在    *)
(*     Merge 动作校验。两条通道互不可替代：内容批准不是状态批准，状态批准    *)
(*     不是内容批准                                                          *)
(*   - 状态批准是惰性会签：Merge 动作在同一机械动作内完成「会签 + 合并」，   *)
(*     不存在「盖了章等着」的中间态；会签使 required approvals 计满          *)
(*     （内容批准 + 状态会签）                                               *)
(*   - 「reviewer 提交 review 后 Gitea 是否消费 requested_reviewers」实测    *)
(*     与源码结论矛盾，建模为非确定性选择——两种行为下规约都必须成立          *)
(*   - 回应按 head 锚定：responded 只记录**当前 head** 上的正式回应（对应       *)
(*     ReviewerRespondedOnHead），author push/rebase 使其清空。旧版本把        *)
(*     responded 建为单调历史（含被 dismiss 的 review、不随作废回退），而       *)
(*     dismiss_stale_approvals 恰恰要使推送后的旧回应失效——两者矛盾会让        *)
(*     「推送后重新请求评审」被判为已回应而撤回请求：请求消失、分支保护门禁      *)
(*     解绑，PR 又因缺少当前 head 的批准被 automerge 跳过（线上 PR #199 的      *)
(*     缺陷）。标签口径与调度队列口径（ListPullRequestsRequestingReview）      *)
(*     必须共用这一定义                                                          *)
(*   - 请求新鲜度：requested_reviewers 本身不带时刻，但时间线的 review_request  *)
(*     事件带 created_at 与 assignee，实现据此区分「旧请求残留」与「重新请求」  *)
(*     （实现为 Client.ListReviewRequestEvents；读取失败时 fail-open 不撤回）；  *)
(*     原生「请求评审」按钮另会生成晚于最新内容结论的请求记录（recordFresh，    *)
(*     实现可观测）；记录缺失的版本/场景退化为其余通道（recordFresh 不被置位    *)
(*     亦须成立）                                                                *)
(*   - 团队评审请求无法按成员身份吸收（实现不知道团队里谁回应了），一律      *)
(*     视为有效意图；且未实测 Gitea 是否会消费团队请求，保守建模为永不消费  *)
(*   - dismiss_stale_approvals 使推送作废旧批准（内容批准与会签一并作废）；  *)
(*     旧批准之下可能露出更早的 review，该情形归约为 none（那些状态可由     *)
(*     其他动作序列直接到达）。responded 随之清空——回应锚定 head，旧 head   *)
(*     上的结论不回应新请求                                                  *)
(*   - 「status/review = 评审请求中」：评审意图（原生请求、按钮复审记录、    *)
(*     @ai//review 提及）只由 Intent 决定标签，不再被门禁（冲突/落后/检查   *)
(*     失败/pending）阻断——门禁只在 Merge 动作校验。此前「检查失败即自动   *)
(*     驳回」会把被取消的检查误判为失败而卡住评审，已废弃                    *)
(*   - 必要检查门禁（分支保护 required status checks）建模为 checks；        *)
(*     Merge 动作本身也受门禁约束，对应 Gitea 的合并按钮硬性限制             *)
(***************************************************************************)
EXTENDS TLC

VARIABLES
    approvalOnHead, \* 最新内容批准是否明确锚定当前 head（空提交号不计）
    contentState,   \* 最新内容结论（"ai" 提交的正式 review）: "none" | "approved" | "requestChanges" | "comment"
    requestedUsers, \* requested_reviewers 中的用户（⊆ {"ai","merge"}）
    requestedTeam,  \* 是否存在团队评审请求（无法按成员吸收，一律视为意图）
    responded,      \* 在**当前 head** 上提交过正式 review 的账号集合。
                    \* 实现对应 ReviewerRespondedOnHead：只有 CommitID 为空
                    \* （Gitea 未给提交号，保守取已回应）或等于 head 的正式
                    \* review 才算「回应了当前请求」；被 dismiss 的不计（推送
                    \* 使旧回应失效，正是 dismiss_stale_approvals 的语义）。
                    \* 见 AuthorPush：推送使该集合清空。
    requestFresh,   \* ground truth: ai 的请求创建于其最新回应之后（实现无法观测，仅文档化用）
    recordFresh,    \* 最新内容结论之后存在原生按钮生成的评审请求记录（#86 实测信号）
    freshMention,   \* 最新内容结论之后的评论中存在评审意图信号：实现中是两条独立
                    \* 谓词的并集（hasReviewerMention ‖ hasReviewCommand，即 @ai/@reviewer
                    \* 提及或行首 /review 命令）；同一扫描窗口、同一效果，建模为同一信号
    stateState,     \* 状态通道最新结论: "none" | "approved"（会签）
    behind,         \* merge_base != base.sha（落后于基础分支）
    mergeable,      \* Gitea 判定可合并（无冲突）
    checks,         \* 必要检查聚合: "passed" | "pending" | "failed"
    labels,         \* sync 写出的 status/* 标签: "inProgress" | "review" | "changesRequested" | "approved"
    dirty,          \* 上次 sync 之后环境发生过变化（标签可能滞后）
    quiet,          \* 环境静默（活性性质的触发条件）
    merged          \* 已合并（终态）

vars == <<approvalOnHead, contentState, requestedUsers, requestedTeam, responded, requestFresh,
          recordFresh, freshMention, stateState, behind, mergeable, checks,
          labels, dirty, quiet, merged>>

ContentStates == {"none", "approved", "requestChanges", "comment"}
States == {"none", "approved", "requestChanges", "comment"}
StateStates == {"none", "approved"}
CheckStates == {"passed", "pending", "failed"}
Labels == {"inProgress", "review", "changesRequested", "approved"}

Init ==
    /\ approvalOnHead = FALSE
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

\* hasUnansweredReviewRequest：**按 head 锚定**——responded 只记录当前 head 上
\* 的正式回应，因此「旧 head 的结论」不吸收请求。与调度队列的
\* ListPullRequestsRequestingReview（ReviewerRespondedOnHead）共用同一定义，
\* 两份判定必须给出同一答案，否则会出现：标签口径认为已回应而撤回请求，队列
\* 口径却认为待评审——请求消失、分支保护门禁解绑，PR 又因缺少当前 head 的批准
\* 被 automerge 跳过（线上 PR #199 的缺陷形状）。
\* 团队请求恒为未回应：无法按成员身份吸收。
HasUnansweredRequest ==
    requestedTeam \/ \E user \in requestedUsers : user \notin responded

Intent == HasUnansweredRequest \/ recordFresh \/ freshMention

\* sync 进入门禁求值时使用的等效状态（有意图时按 REQUEST_REVIEW 处理）
EffectiveState == IF Intent THEN "requestReview" ELSE contentState

\* sync 在非跳过情形下会设置的标签。与 Sync 动作分头编码，由 MirrorCorrect
\* 交叉验证两处实现一致。门禁不再参与标签推导——评审请求即排队，合并门禁
\* 由 Merge 动作校验。
ExpectedLabels ==
    IF ~ContentFound /\ ~Intent
    THEN IF ~mergeable \/ behind THEN "changesRequested" ELSE "inProgress"
    ELSE IF EffectiveState = "requestReview" THEN "review"
         ELSE IF EffectiveState = "approved" THEN "approved"
              ELSE "changesRequested"

(***************************************************************************)
(* sync（assistant reconcile）                                              *)
(***************************************************************************)

Sync ==
    /\ ~merged
    /\ IF ~ContentFound /\ ~Intent
       THEN \* 无内容结论且无意图：开发中；冲突/落后提升为「要求修改」
            /\ labels' = IF ~mergeable \/ behind THEN "changesRequested" ELSE "inProgress"
            /\ UNCHANGED <<approvalOnHead, contentState, requestedUsers, responded, stateState>>
       ELSE IF EffectiveState \in {"requestReview", "approved"}
            THEN \* 评审意图 / 批准直接落标签：不被 checks/conflict/behind 阻断
                 /\ labels' = IF EffectiveState = "requestReview" THEN "review" ELSE "approved"
                 /\ UNCHANGED <<approvalOnHead, contentState, requestedUsers, responded, stateState>>
            ELSE \* 驳回或 COMMENT 讨论：等待作者
                 /\ labels' = "changesRequested"
                 /\ UNCHANGED <<approvalOnHead, contentState, requestedUsers, responded, stateState>>
    /\ dirty' = FALSE
    /\ UNCHANGED <<approvalOnHead, requestedTeam, requestFresh, recordFresh, freshMention, behind,
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
    /\ UNCHANGED <<approvalOnHead, contentState, requestedTeam, responded, recordFresh, freshMention,
                  stateState, behind, mergeable, checks, labels, quiet, merged>>

\* 团队评审请求：无法按成员身份吸收，永不消费（保守建模，见模块头注释）。
AuthorRequestTeam ==
    /\ ~merged /\ ~quiet /\ ~requestedTeam
    /\ requestedTeam' = TRUE
    /\ dirty' = TRUE
    /\ UNCHANGED <<approvalOnHead, contentState, requestedUsers, responded, requestFresh, recordFresh,
                  freshMention, stateState, behind, mergeable, checks, labels, quiet, merged>>

\* 原生按钮复审：reviewer 已回应后重新点击「请求评审」，生成晚于最新内容
\* 结论的请求记录（Gitea 1.27 实测稳定生成；记录缺失的版本无此动作，
\* recordFresh 保持 FALSE，退化为提及/命令通道）。
AuthorRequestButton ==
    /\ ~merged /\ ~quiet /\ ~recordFresh
    /\ recordFresh' = TRUE
    /\ dirty' = TRUE
    /\ UNCHANGED <<approvalOnHead, contentState, requestedUsers, requestedTeam, responded, requestFresh,
                  freshMention, stateState, behind, mergeable, checks, labels, quiet, merged>>

\* 评论中 @ai/@reviewer 提及（最新内容结论之后才构成新意图；freshMention
\* 已为 TRUE 时再提及无新效果）。
AuthorMention ==
    /\ ~merged /\ ~quiet /\ ~freshMention
    /\ freshMention' = TRUE
    /\ dirty' = TRUE
    /\ UNCHANGED <<approvalOnHead, contentState, requestedUsers, requestedTeam, responded, requestFresh,
                  recordFresh, stateState, behind, mergeable, checks, labels, quiet, merged>>

\* 内容评审者提交正式 review。Gitea 是否消费其名下请求是非确定的：
\* 源码层面会删除（世界 A），本实例实测残留（世界 B）——规约对两者都须成立。
\* responded 记录的是「当前 head 上已回应」，本次 review 正落在当前 head，
\* 故加入 {"ai"}；后续 AuthorPush 会清空它（旧回应随 head 前移失效）。
ReviewerSubmit(state) ==
    /\ approvalOnHead' = (state = "approved")
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
\* none，见模块头注释）。**responded 随之清空**：回应锚定 head，head 前移后
\* 旧 head 上的结论不再回应新请求（这正是 #199 缺陷的核心——旧规则把
\* responded 当单调历史，于是重新请求评审被判为「已回应」而撤回）。
AuthorPush ==
    /\ approvalOnHead' = FALSE
    /\ ~merged /\ ~quiet
    /\ contentState' = IF contentState = "approved" THEN "none" ELSE contentState
    /\ stateState' = IF stateState = "approved" THEN "none" ELSE stateState
    /\ responded' = {}
    /\ checks' = "pending"
    /\ mergeable' \in {TRUE, FALSE}
    /\ dirty' = TRUE
    /\ UNCHANGED <<requestedUsers, requestedTeam, requestFresh, recordFresh,
                  freshMention, behind, labels, quiet, merged>>

\* 作者 rebase 到最新基础分支：消除落后与冲突，检查重跑，旧批准同样作废
\* （head 前移：responded 随之清空，理由同 AuthorPush）。
AuthorRebase ==
    /\ approvalOnHead' = FALSE
    /\ ~merged /\ ~quiet
    /\ contentState' = IF contentState = "approved" THEN "none" ELSE contentState
    /\ stateState' = IF stateState = "approved" THEN "none" ELSE stateState
    /\ responded' = {}
    /\ behind' = FALSE
    /\ mergeable' = TRUE
    /\ checks' = "pending"
    /\ dirty' = TRUE
    /\ UNCHANGED <<requestedUsers, requestedTeam, requestFresh, recordFresh,
                  freshMention, labels, quiet, merged>>

\* 基础分支前移（他人合并了别的 PR）。
BaseAdvances ==
    /\ ~merged /\ ~quiet /\ ~behind
    /\ behind' = TRUE
    /\ dirty' = TRUE
    /\ UNCHANGED <<approvalOnHead, contentState, requestedUsers, requestedTeam, responded,
                  requestFresh, recordFresh, freshMention, stateState, mergeable,
                  checks, labels, quiet, merged>>

\* 基础分支变更导致冲突（无需 PR 侧推送）。
ConflictAppears ==
    /\ ~merged /\ ~quiet /\ mergeable
    /\ mergeable' = FALSE
    /\ dirty' = TRUE
    /\ UNCHANGED <<approvalOnHead, contentState, requestedUsers, requestedTeam, responded, requestFresh,
                  recordFresh, freshMention, stateState, behind, checks, labels, quiet, merged>>

\* 检查状态迁移：pending 出结果、重跑翻转、抖动，统一建模为任意迁移。
ChecksTransition ==
    /\ ~merged /\ ~quiet
    /\ checks' \in CheckStates \ {checks}
    /\ dirty' = TRUE
    /\ UNCHANGED <<approvalOnHead, contentState, requestedUsers, requestedTeam, responded, requestFresh,
                  recordFresh, freshMention, stateState, behind, mergeable, labels, quiet, merged>>

\* 环境静默：此后只有 sync（与终态）可发生，用于活性收敛性质。
EnvQuiet ==
    /\ ~quiet
    /\ quiet' = TRUE
    /\ UNCHANGED <<approvalOnHead, contentState, requestedUsers, requestedTeam, responded, requestFresh,
                  recordFresh, freshMention, stateState, behind, mergeable, checks,
                  labels, dirty, merged>>

\* 会签 + 合并（同一机械动作）：门禁全绿时状态评审者对 head 盖状态批准——
\* required approvals 的第二票——随即合并。人类手动合并同样受此约束：
\* 没有会签在场（或召唤 automerge）就无法满足 required approvals。
Merge ==
    /\ ~merged /\ ~quiet
    /\ contentState = "approved"
    /\ approvalOnHead
    /\ checks = "passed"
    /\ mergeable
    /\ ~behind
    /\ stateState' = "approved"
    /\ responded' = responded \cup {"merge"}   \* 会签进入时间线
    /\ merged' = TRUE
    /\ dirty' = FALSE
    /\ UNCHANGED <<approvalOnHead, contentState, requestedUsers, requestedTeam, requestFresh,
                  recordFresh, freshMention, behind, mergeable, checks, labels, quiet>>

UnknownHeadApproval ==
    /\ ~merged /\ ~quiet
    /\ contentState' = "approved"
    /\ approvalOnHead' = FALSE
    /\ dirty' = TRUE
    /\ UNCHANGED <<requestedUsers, requestedTeam, responded, requestFresh,
                  recordFresh, freshMention, stateState, behind, mergeable, checks,
                  labels, quiet, merged>>

MergedStutter ==
    /\ merged
    /\ UNCHANGED vars

Next ==
    \/ UnknownHeadApproval
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
    /\ approvalOnHead \in BOOLEAN
    /\ contentState \in ContentStates
    /\ requestedUsers \subseteq {"ai"}
    /\ requestedTeam \in BOOLEAN
    /\ responded \subseteq {"ai", "merge"}
    \* contentState 是内容通道的最新结论（**历史**），responded 是「当前 head 上
    \* 已回应」。两者在推送后合法地分离：旧结论仍在（contentState ≠ "none"，
    \* 供 latestContentReview 取用），但它不回答新 head（responded 已清空）。
    \* 因此不再断言「有结论 ⇒ 有人回应当前 head」。这里只保留仍成立的方向：
    \* 内容评审者回应了当前 head ⇒ 必然存在内容结论（回应即写结论，见
    \* ReviewerSubmit）。注意不能反过来对 responded 整体断言——状态评审者的
    \* 会签（Merge）也会写入 responded，而它不产生内容结论。
    /\ "ai" \in responded => contentState # "none"
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

\* sync 收敛后（无未同步变更），标签与算法输出一致。Sync 与 ExpectedLabels 是
\* 同一算法的两份独立编码，相互印证。
MirrorCorrect ==
    (~dirty /\ ~merged) => labels = ExpectedLabels

\* S1 球权归属：内容评审者本人回应过（驳回或讨论）、所有请求都已吸收、无新
\* 提及且无新的按钮请求记录 → 球确定性在作者。这是「COMMENT 后 PR 不得停留
\* 在 review 空等」的直接表达。意图可能来自其他未回应者或团队请求——那种情
\* 形下 PR 留在 review 是正确行为（确有 reviewer 未回应），不在本性质约束内。
BallWithAuthorAfterResponse ==
    (~dirty /\ ~merged
     /\ contentState \in {"requestChanges", "comment"}
     /\ ~HasUnansweredRequest /\ ~recordFresh /\ ~freshMention)
    => labels = "changesRequested"

\* S2 approved 标签的真实性：只有内容评审者本人批准时才成立——状态评审者的
\* 会签（stateState = "approved"）绝不能单独驱动 approved。合并门禁（检查/
\* 冲突/落后）由 Merge 动作校验，不参与标签推导。
ApprovedGenuine ==
    (~dirty /\ ~merged /\ labels = "approved")
    => contentState = "approved"

\* S3 review 标签的真实性：存在有效评审意图（评审请求不被门禁阻断）。
ReviewGenuine ==
    (~dirty /\ ~merged /\ labels = "review")
    => Intent

\* S4 无内容结论、无意图且分支健康 → 开发中。状态会签在场也不能例外——
\* 它是状态通道的结论，不构成内容批准。
FreshPRIsInProgress ==
    (~dirty /\ ~merged
     /\ ~ContentFound /\ requestedUsers = {} /\ ~requestedTeam
     /\ ~recordFresh /\ ~freshMention /\ mergeable /\ ~behind)
    => labels = "inProgress"

\* S5 状态会签只出现在合并前一刻：会签在场蕴含门禁全绿且标签已到 approved
\* （会签与合并是同一机械动作，不存在「盖章等待」或「带病盖章」）。
CountersignOnlyWhenGreen ==
    (~dirty /\ ~merged /\ stateState = "approved")
    => (checks = "passed" /\ mergeable /\ ~behind /\ labels = "approved")

(***************************************************************************)
(* 活性：环境静默后系统收敛——标签与算法输出一致且不再变化。                 *)
(* 不存在「标签等待 reviewer 而 reviewer 在等作者」式的死锁。               *)
(***************************************************************************)

ConvergesWhenQuiet ==
    quiet ~> [](merged \/ (~dirty /\ labels = ExpectedLabels))


MergedRequiresCurrentApproval ==
    merged => (approvalOnHead /\ contentState = "approved" /\ stateState = "approved"
               /\ checks = "passed" /\ mergeable /\ ~behind)

====
