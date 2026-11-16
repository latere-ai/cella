import Machines.Sandbox

/-!
Claims about the sandbox phase table, proved over the table that
`controller.SandboxLifecycle` renders into `Machines/Sandbox.lean`.
-/

namespace Proofs.Sandbox
open Machines.Sandbox Statemachine

/-- A deleting sandbox stays deleting: nothing follows `deleting`, so no
driver read and no recovery brings it back. -/
theorem deleting_stays (s : State) (r : Reaches machine .«deleting» s) :
    s = .«deleting» :=
  machine.terminal_stays terminal_deleting r

/-- Only the controller's recovery makes a sandbox `recovering`, and only
from `lost`. -/
theorem recovering_only_from_lost :
    ∀ s ∈ machine.states, State.«recovering» ∈ machine.next s → s = .«lost» := by
  decide

/-- A sandbox waits in `queued` only from `pending` at creation or from
`running` by preemption, and a queued one is never reported by a driver:
the states that lead to `queued` are `running` alone. -/
theorem queued_only_by_preemption :
    ∀ s ∈ machine.states, State.«queued» ∈ machine.next s → s = .«running» := by
  decide

/-- A sandbox the control plane failed itself or the driver lost is never
`pending` again from `lost`: from `lost` only recovery, failure or a delete
follow. -/
theorem lost_leaves_by_recovery (s : State) (h : s ∈ machine.next .«lost») :
    s = .«recovering» ∨ s = .«failed» ∨ s = .«deleting» := by
  revert s; decide

/-- Concurrent writers that check the phase they land on keep the table,
whatever they read and in whatever order they land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Sandbox
