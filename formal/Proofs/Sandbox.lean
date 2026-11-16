import Machines.Sandbox

/-!
Claims about the sandbox phase table, proved over the table that
`controller.SandboxLifecycle` renders into `Machines/Sandbox.lean`.
-/

namespace Proofs.Sandbox
open Machines.Sandbox Statemachine

/-- A deleting sandbox stays deleting: nothing follows `Deleting`, so no
driver read and no recovery brings it back. -/
theorem deleting_stays (s : State) (r : Reaches machine .«Deleting» s) :
    s = .«Deleting» :=
  machine.terminal_stays «terminal_Deleting» r

/-- Only the controller's recovery makes a sandbox `Recovering`, and only
from `Lost`. -/
theorem recovering_only_from_lost :
    ∀ s ∈ machine.states, State.«Recovering» ∈ machine.next s → s = .«Lost» := by
  decide

/-- A sandbox waits in `Queued` only from `Pending` at creation or from
`Running` by preemption, and a queued one is never reported by a driver:
the states that lead to `Queued` are `Running` alone. -/
theorem queued_only_by_preemption :
    ∀ s ∈ machine.states, State.«Queued» ∈ machine.next s → s = .«Running» := by
  decide

/-- A sandbox the control plane failed itself or the driver lost is never
`Pending` again from `Lost`: from `Lost` only recovery, failure or a delete
follow. -/
theorem lost_leaves_by_recovery (s : State) (h : s ∈ machine.next .«Lost») :
    s = .«Recovering» ∨ s = .«Failed» ∨ s = .«Deleting» := by
  revert s; decide

/-- Concurrent writers that check the phase they land on keep the table,
whatever they read and in whatever order they land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Sandbox
