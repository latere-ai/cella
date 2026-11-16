import Machines.Engine

/-!
Claims about the device engine state table, proved over the table that
`device.EngineTable` renders into `Machines/Engine.lean`.
-/

namespace Proofs.Engine
open Machines.Engine Statemachine

/-- A computer with no device connected only starts: from `not_connected`
the one step is `starting`. -/
theorem not_connected_only_starts (s : State) (h : s ∈ machine.next .«not_connected») :
    s = .«starting» := by
  revert s; decide

/-- A paused engine is never `online` or `offline` directly: a resume starts
it, or its device is removed. -/
theorem paused_leaves_by_start_or_removal (s : State) (h : s ∈ machine.next .«paused») :
    s = .«starting» ∨ s = .«not_connected» := by
  revert s; decide

/-- The engine is `starting` only again from `online`, `offline` or `paused`
or `not_connected`, never from `starting` itself. -/
theorem starting_never_loops :
    ∀ s ∈ machine.states, State.«starting» ∈ machine.next s → s ≠ .«starting» := by
  decide

end Proofs.Engine
