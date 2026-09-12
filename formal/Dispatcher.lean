/-!
# agent-dispatcher 形式化：时间线模型

对评审调度涉及的完整世界做时间线建模：**参与者**（作者 / 评审者 / 合并者 /
其他人）在时间线上施加**动作**（push 代码、更新说明、转 draft / mark ready、
提交 review、合并），PR 的 head 与生命周期状态是时间线上的聚合量，dispatcher
的完成判定定义为时间线片段上的谓词。在此之上机器检验五组安全性质：

A1. `stale_push_invalidates_completion`：会话起点之后作者 push 了不同代码
    （时间线尾部追加 push 事件）⇒ 完成判定不可能通过——评审锚定旧 head 即作废。
A2. `update_description_irrelevant`：作者更新说明（不改变 head 的事件）对完成
    判定无影响——「push 代码」与「改说明」在模型里被严格区分。
B1–B2. `push_keeps_pr_state` / `merge_sets_merged` / `convert_to_draft_blocks_queue`：
    代码推送不改变 PR 生命周期状态；合并使状态进入 merged；转回 draft（WIP）
    后队列门关闭——draft 不进评审队列。
C1–C3（会话内部状态机）：完成只锚定当前 head；head 漂移 ⇒ 本轮作废；
    同一待办至多两个会话。
D1–D2（全局并发守卫）：入队受 pending 与 cap 双重守卫——同一待办不可能
    自我并发，会话数不越过上限。

## 与实现的对应

- 时间线事件 ↔ Gitea 侧事实（`src/verify.ts` 的 head 漂移检测、review 列表）；
- `completeOk` ↔ `verifyPullReview`；
- 会话内部状态机 ↔ `src/loop.ts` 的 `processItem` attempt 循环；
- 入队守卫 ↔ 检测去重与 `runLoop` 的轮间 barrier / 并发上限。

## 检查方式

`lean apps/agent-dispatcher/formal/Dispatcher.lean`，退出码 0 即全部证明通过（纯 core Lean，无需
mathlib）。本文件是规格：改变上述任何一条语义的实现改动，必须同步修改模型
并让证明重新通过，否则不应合入。
-/

namespace Dispatcher

abbrev PrId := Nat
abbrev Sha := Nat

/-! ## 参与者、动作与时间线 -/

/-- 参与者角色。 -/
inductive Actor where
  | author -- 作者
  | reviewer -- 评审者（本仓库为 `ai` 账号）
  | merger -- 合并者（automerge 或人类）
  | other -- 其他无关者
deriving DecidableEq

/-- 评审结论（内容抽象：结论种类参与建模，正文文本不参与）。 -/
inductive Verdict where
  | approve
  | requestChanges
  | comment
deriving DecidableEq

/-- PR 生命周期状态。draft 即 WIP：不进评审队列。 -/
inductive PrState where
  | draft
  | ready
  | merged
  | closed
deriving DecidableEq

/-- 时间线上的动作。内容一律抽象为标识符：代码以 head sha 标识
    （同 sha 即同内容），说明以 `text` 标识（说明的具体内容与本文
    任何性质无关，参与建模的只有「发生了一次说明更新」这一事实）。 -/
inductive Action where
  | push (head : Sha) -- 推送代码：改变 PR head
  | updateDescription (text : Nat) -- 更新说明：不改变 head
  | markReady -- draft → ready
  | convertToDraft -- ready → draft
  | review (head : Sha) (v : Verdict) -- 提交 review（记录其阅读的 head）
  | merge (head : Sha) -- 合并（记录合并时的 head）
  | comment -- 普通评论：不参与任何判定
deriving DecidableEq

/-- 时间线事件：谁、做什么。时间戳记录在事件上，但下文的聚合与判定按
    时间线结构（List 顺序）表达——「某时刻之后」= 时间线片段
    （`pre ++ post` 的 `post`），时间戳本身不参与计算。 -/
structure Event where
  actor : Actor
  time : Nat
  action : Action

abbrev Timeline := List Event

def mkPush (h : Sha) : Event := { actor := .author, time := 0, action := .push h }
def mkDescription (txt : Nat) : Event := { actor := .author, time := 0, action := .updateDescription txt }
def mkConvert : Event := { actor := .author, time := 0, action := .convertToDraft }
def mkMerge (h : Sha) : Event := { actor := .merger, time := 0, action := .merge h }

/-! ## 时间线聚合量：head 与 PR 状态 -/

/-- 时间线终点的 PR head：从初始 head 出发，push 事件依次覆盖（最后一个
    push 生效）。说明、评审、状态切换都不改变 head。 -/
def finalHead : Timeline → Sha → Sha
  | [], h => h
  | e :: es, h =>
    match e.action with
    | .push h' => finalHead es h'
    | _ => finalHead es h

/-- 单个动作对 PR 状态的迁移。 -/
def stepState : Action → PrState → PrState
  | .markReady, .draft => .ready
  | .convertToDraft, .ready => .draft
  | .convertToDraft, .draft => .draft
  | .merge _, _ => .merged
  | _, s => s

/-- push 对任何状态都是恒等迁移（定义性：构造子可直接归约）。 -/
theorem stepState_push (h : Sha) (s : PrState) : stepState (.push h) s = s := rfl

/-- merge 使任何状态进入 merged（定义性）。 -/
theorem stepState_merge (h : Sha) (s : PrState) : stepState (.merge h) s = .merged := rfl

/-- 时间线终点的 PR 状态：从初始状态依次施加动作迁移。 -/
def stateAfter : Timeline → PrState → PrState
  | [], s => s
  | e :: es, s => stateAfter es (stepState e.action s)

/-- 评审队列门：只有 ready（非 WIP）的 PR 进评审队列。 -/
def queueable : PrState → Bool
  | .ready => true
  | _ => false

/-! ## dispatcher 的决策：完成判定 -/

/-- 一次评审会话：prep 时钉定的 PR head。 -/
structure Session where
  pinnedHead : Sha

/-- 完成判定的时间线语义（`verifyPullReview`）：起点之后的片段里存在
    评审者锚定会话 head 的新 review，且片段终点的 head 仍是会话钉定的
    head（作者未再推送不同代码）。 -/
def completeOk (pre post : Timeline) (h0 : Sha) (sn : Session) : Prop :=
  (∃ e ∈ post, ∃ rh v, e.action = .review rh v ∧ rh = sn.pinnedHead ∧ e.actor = .reviewer)
    ∧ finalHead post (finalHead pre h0) = sn.pinnedHead

/-! ## 性质 A：push 作废评审，更新说明无关 -/

/-- 机制引理：时间线尾部追加 push，终点 head 就是推的 sha。 -/
theorem finalHead_append_push (es : Timeline) (h h' : Sha) :
    finalHead (es ++ [mkPush h']) h = h' := by
  induction es generalizing h with
  | nil => simp [finalHead, mkPush]
  | cons e tl ih =>
    cases e with
    | mk _ _ act => cases act <;> exact ih _

/-- **A1**：会话起点之后作者 push 了不同代码 ⇒ 完成判定不可能通过。
    评审锚定旧 head 即作废，待办放行、下一轮以新 head 重开。 -/
theorem stale_push_invalidates_completion (pre post : Timeline) (h0 : Sha) (sn : Session)
    (h' : Sha) (hne : h' ≠ sn.pinnedHead) :
    ¬ completeOk pre (post ++ [mkPush h']) h0 sn := by
  intro hc
  obtain ⟨_, hhead⟩ := hc
  rw [finalHead_append_push] at hhead
  exact hne hhead

/-- 机制引理：追加「更新说明」事件不改变终点 head。 -/
theorem finalHead_append_description (es : Timeline) (h : Sha) (txt : Nat) :
    finalHead (es ++ [mkDescription txt]) h = finalHead es h := by
  induction es generalizing h with
  | nil => simp [finalHead, mkDescription]
  | cons e tl ih =>
    cases e with
    | mk _ _ act => cases act <;> exact ih _

/-- 评审者锚定 head 的 review 出现在时间线片段中。 -/
def reviewedAnchored (es : Timeline) (h : Sha) : Prop :=
  ∃ e, e ∈ es ∧ ∃ rh v, e.action = .review rh v ∧ rh = h ∧ e.actor = .reviewer

/-- 片段追加「更新说明」事件后，锚定 review 的存在性保持（→ 方向）。 -/
theorem reviewedAnchored_append_description_left (es : Timeline) (h : Sha) (txt : Nat)
    (hrev : ∃ e, e ∈ es ∧ ∃ rh v, e.action = .review rh v ∧ rh = h ∧ e.actor = .reviewer) :
    ∃ e, e ∈ es ++ [mkDescription txt] ∧ ∃ rh v, e.action = .review rh v ∧ rh = h ∧ e.actor = .reviewer := by
  obtain ⟨e, he, rh, v, hact, hrh, hactor⟩ := hrev
  exact ⟨e, List.mem_append_left _ he, rh, v, hact, hrh, hactor⟩

/-- 片段追加「更新说明」事件后，锚定 review 的存在性无新增（← 方向：
    追加的不是 review 事件）。 -/
theorem reviewedAnchored_append_description_right (es : Timeline) (h : Sha) (txt : Nat)
    (hrev : ∃ e, e ∈ es ++ [mkDescription txt] ∧ ∃ rh v, e.action = .review rh v ∧ rh = h ∧ e.actor = .reviewer) :
    ∃ e, e ∈ es ∧ ∃ rh v, e.action = .review rh v ∧ rh = h ∧ e.actor = .reviewer := by
  obtain ⟨e, he, rh, v, hact, hrh, hactor⟩ := hrev
  simp only [List.mem_append] at he
  cases he with
  | inl he => exact ⟨e, he, rh, v, hact, hrh, hactor⟩
  | inr he =>
    cases he with
    | head => simp [mkDescription] at hact
    | tail _ hmem => cases hmem

/-- **A2**：作者更新说明（哪怕在会话期间）对完成判定无影响——
    「push 代码」与「改说明」被严格区分；说明内容不进任何判定。 -/
theorem update_description_irrelevant (pre post : Timeline) (h0 : Sha) (sn : Session) (txt : Nat) :
    (completeOk pre (post ++ [mkDescription txt]) h0 sn ↔ completeOk pre post h0 sn) := by
  constructor
  · intro hc
    obtain ⟨href, hhead⟩ := hc
    rw [finalHead_append_description] at hhead
    exact ⟨reviewedAnchored_append_description_right post sn.pinnedHead txt href, hhead⟩
  · intro hc
    obtain ⟨href, hhead⟩ := hc
    refine ⟨reviewedAnchored_append_description_left post sn.pinnedHead txt href, ?_⟩
    rw [finalHead_append_description]
    exact hhead

/-! ## 性质 B：PR 状态机与队列门 -/

/-- 机制引理：状态按时间线顺序迁移，尾部追加事件即施加其迁移。 -/
theorem stateAfter_append (es : Timeline) (e : Event) (s : PrState) :
    stateAfter (es ++ [e]) s = stepState e.action (stateAfter es s) := by
  induction es generalizing s with
  | nil => simp [stateAfter]
  | cons x tl ih =>
    simp only [List.cons_append, stateAfter]
    exact ih _

/-- **B1a**：代码推送不改变 PR 生命周期状态（状态只被 ready/draft/merge 迁移）。 -/
theorem push_keeps_pr_state (es : Timeline) (s : PrState) (h : Sha) :
    stateAfter (es ++ [mkPush h]) s = stateAfter es s := by
  rw [stateAfter_append]
  simp only [mkPush, stepState_push]

/-- **B1b**：合并使 PR 进入 merged（automerge 或人类，角色只记录在事件上）。 -/
theorem merge_sets_merged (es : Timeline) (s : PrState) (h : Sha) :
    stateAfter (es ++ [mkMerge h]) s = .merged := by
  rw [stateAfter_append]
  simp only [mkMerge, stepState_merge]

/-- 转回 draft 后，无论原状态如何，队列门都关闭。 -/
theorem stepState_convert_queue_closed (st : PrState) :
    queueable (stepState .convertToDraft st) = false := by
  cases st <;> simp [stepState, queueable]

/-- **B2**：作者把 PR 转回 draft（WIP）后，队列门关闭——draft 不进评审队列，
    已排队的待办在下一轮检测里自然消失。 -/
theorem convert_to_draft_blocks_queue (es : Timeline) (s : PrState) :
    queueable (stateAfter (es ++ [mkConvert]) s) = false := by
  rw [stateAfter_append]
  simp only [mkConvert]
  exact stepState_convert_queue_closed _

/-! ## 会话内部状态机（对应 processItem 的 attempt 循环） -/

/-- 单个待办的生命周期。`inflight head attempt` 的 `attempt` 从 1 计。 -/
inductive Todo where
  | pending
  | inflight (head : Sha) (attempt : Nat)
  | released
  | done (head : Sha)
deriving DecidableEq, Repr

/-- 一次完成判定时刻的外部事实。 -/
structure Facts where
  /-- 验证时刻的外部 PR head（作者随时可推，这是环境量而非 dispatcher 可控量） -/
  headNow : Sha
  /-- 会话起点之后 reviewer 是否已提交新 review -/
  reviewed : Bool

/-- 单待办一步迁移：对应 `processItem` 一次会话结束后的完成判定与走向决策。 -/
def step (todo : Todo) (f : Facts) : Option Todo :=
  match todo with
  | .pending => some (.inflight f.headNow 1)
  | .inflight h k =>
      if f.headNow = h then
        if f.reviewed then
          some (.done h) -- head 未动 + 新 review ⇒ 完成，锚定被评审的 head
        else if k < 2 then
          some (.inflight h (k + 1)) -- 同 head 重试一次
        else
          some .released -- 两个会话耗尽，放行
      else
        some .released -- head 漂移 ⇒ 本轮作废放行（不烧重试会话）
  | .released => some .pending -- 下一轮检测重新入队
  | .done _ => none -- 终态

/-- **C1**：完成只可能锚定验证时刻的外部 head——「完成」永远指对当前代码的完成。 -/
theorem done_anchors_current_head (todo : Todo) (f : Facts) (h : Sha) :
    step todo f = some (.done h) → h = f.headNow := by
  cases todo with
  | pending => simp [step]
  | released => simp [step]
  | done u => simp [step]
  | inflight u k =>
      intro heq
      simp only [step] at heq
      by_cases hhead : f.headNow = u
      · rw [if_pos hhead] at heq
        by_cases hrev : f.reviewed = true
        · rw [if_pos hrev] at heq
          injection heq with h1
          injection h1 with h2
          -- h2 : u = h，而 f.headNow = u ⇒ h = f.headNow
          exact (hhead.trans h2).symm
        · rw [if_neg hrev] at heq
          split at heq
          · simp at heq
          · simp at heq
      · rw [if_neg hhead] at heq
        simp at heq

/-- **C2**：作者在会话期间推送（外部 head 离开会话钉定的 head）后，
本轮无论 reviewer 是否已提交 review 都不可能判完成。 -/
theorem stale_head_never_done (h k : Sha) (f : Facts) (hne : f.headNow ≠ h) :
    step (.inflight h k) f ≠ some (.done h) := by
  intro heq
  have anchored := done_anchors_current_head (.inflight h k) f h heq
  exact hne anchored.symm

/-- **C3**：第 2 个会话结束后只会走向完成或放行——同一待办至多两个会话
（`released → pending → 入队` 是回到会话的唯一路径，且要重新过检测）。 -/
theorem no_third_session (h : Sha) (f : Facts) (t : Todo) :
    step (.inflight h 2) f = some t → t = .done h ∨ t = .released := by
  intro heq
  simp only [step] at heq
  by_cases hhead : f.headNow = h
  · rw [if_pos hhead] at heq
    by_cases hrev : f.reviewed = true
    · rw [if_pos hrev] at heq
      injection heq with h1
      exact Or.inl h1.symm
    · rw [if_neg hrev] at heq
      split at heq
      · omega
      · injection heq with h1
        exact Or.inr h1.symm
  · rw [if_neg hhead] at heq
    injection heq with h1
    exact Or.inr h1.symm

/-! ## 全局并发守卫 -/

/-- 全局状态：每个待办的状态、占用中的会话数、并发上限。
`todo` 是 `PrId → Todo` 的函数——同一待办在表示上只有一格，
并发会话不可能共用一格；占用数的一致性由入队守卫维持。 -/
structure Sys where
  todo : PrId → Todo
  nInFlight : Nat
  cap : Nat

/-- 待办是否正占用一个会话槽。 -/
def busy : Todo → Nat
  | .inflight _ _ => 1
  | _ => 0

/-- 每个待办至多占用一个会话槽。 -/
theorem busy_at_most_one (t : Todo) : busy t ≤ 1 := by
  cases t <;> simp [busy]

/-- 入队（`pending → inflight h 1`）：唯一增加占用数的迁移，
受「待办确为 pending」与「占用未达 cap」双重守卫。 -/
def enqueue (s : Sys) (p : PrId) (h : Sha) : Option Sys :=
  if s.todo p = .pending ∧ s.nInFlight < s.cap then
    some
      { todo := fun q => if q = p then .inflight h 1 else s.todo q
        nInFlight := s.nInFlight + 1
        cap := s.cap }
  else
    none

/-- **D1**：待办不在 pending 态（如已在会话中）则拒绝入队——
同一待办同一时刻至多一个会话，不可能自我并发。 -/
theorem enqueue_requires_pending (s : Sys) (p : PrId) (h : Sha) (hne : s.todo p ≠ .pending) :
    enqueue s p h = none := by
  unfold enqueue
  simp [hne]

/-- **D2**：占用已满则拒绝入队——并发会话数不越过 cap。 -/
theorem enqueue_requires_cap (s : Sys) (p : PrId) (h : Sha) (hcap : s.nInFlight ≥ s.cap) :
    enqueue s p h = none := by
  unfold enqueue
  rw [if_neg (by omega)]

end Dispatcher
