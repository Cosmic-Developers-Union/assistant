/-!
# 声明式 bot 调度核心

对应 internal/runtime/engine.go：平台快照发现待办；pending 与容量双守卫；
恢复 → 准备 → 执行 → 固化 → 观测 → 清理。执行结局只进入观测，不决定完成。
每轮 barrier 后释放全部槽位，下一轮只看平台快照。记忆锚点由 kind 决定。

检查：lean formal/Dispatcher.lean（纯 Lean，无 mathlib）。
-/
namespace Dispatcher

abbrev Item := Nat

/-- 平台待办与执行记录分离，成功退出不会移除平台待办。 -/
structure Sys where
  pending : Item → Bool
  inFlight : Nat
  cap : Nat
  todo : Item → Bool
  observedSuccess : Item → Bool

/-- 入队只能消费平台仍然挂起且未占槽的事件。 -/
def enqueue (s : Sys) (item : Item) : Option Sys :=
  if s.todo item = true ∧ s.pending item = false ∧ s.inFlight < s.cap then
    some { s with
      pending := fun q => if q = item then true else s.pending q
      inFlight := s.inFlight + 1 }
  else none

theorem same_item_never_concurrent (s : Sys) (p : Item) (h : s.pending p = true) :
    enqueue s p = none := by
  simp [enqueue, h]

theorem capacity_guard (s : Sys) (p : Item) (h : s.inFlight ≥ s.cap) :
    enqueue s p = none := by
  unfold enqueue
  rw [ite_eq_right (by omega)]

theorem absent_platform_todo_not_queued (s : Sys) (p : Item) (h : s.todo p = false) :
    enqueue s p = none := by
  simp [enqueue, h]

/-- 执行结局写观测，同时释放 pending；平台待办不由执行器修改。 -/
def finish (s : Sys) (p : Item) (success : Bool) : Sys :=
  { s with pending := fun q => if q = p then false else s.pending q
           inFlight := s.inFlight - 1
           observedSuccess := fun q => if q = p then success else s.observedSuccess q }

theorem agent_exit_cannot_complete_platform_todo (s : Sys) (p : Item) (ok : Bool) :
    (finish s p ok).todo = s.todo := rfl

theorem failure_releases_pending (s : Sys) (p : Item) :
    (finish s p false).pending p = false := by
  simp [finish]

/-- 轮间 barrier 保证没有遗留会话；平台快照完全替换待办。 -/
def poll (s : Sys) (snapshot : Item → Bool) : Sys :=
  { s with pending := fun _ => false, inFlight := 0, todo := snapshot }

theorem next_round_uses_platform_only (s : Sys) (snapshot : Item → Bool) :
    (poll s snapshot).todo = snapshot := rfl

inductive Kind where
  | review | triage | chat
  deriving DecidableEq

/-- 锚点：PR 按 head、Issue 按标题、聊天按用户及 reset 世代。 -/
def anchor : Kind → Nat → Nat → Nat → Nat
  | .review, head, _, _ => head
  | .triage, _, title, _ => title
  | .chat, _, _, generation => generation

theorem head_change_reanchors_review (h h' title generation : Nat) (different : h ≠ h') :
    anchor .review h title generation ≠ anchor .review h' title generation := different

theorem issue_retry_keeps_memory (h h' title generation : Nat) :
    anchor .triage h title generation = anchor .triage h' title generation := rfl

end Dispatcher

namespace Dispatcher

/-- 账号召唤来自平台的可见性与新请求，不需要仓库安装或协作者标记。 -/
def accountSummons (readable fresh : Bool) (_installed _collaborator : Bool) : Bool :=
  readable && fresh

theorem public_uninstalled_repo_can_summon (readable fresh : Bool) :
    accountSummons readable fresh false false = (readable && fresh) := rfl

theorem historical_mention_does_not_summon (readable installed collaborator : Bool) :
    accountSummons readable false installed collaborator = false := by
  simp [accountSummons]

end Dispatcher
