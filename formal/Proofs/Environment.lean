import Machines.Environment

/-!
Claims about the environment phase table, proved over the table that
`controller.EnvironmentLifecycle` renders into `Machines/Environment.lean`.
-/

namespace Proofs.Environment
open Machines.Environment Statemachine

/-- An environment never returns to `Pending`: nothing has a step to it. -/
theorem never_pending_again :
    ∀ s ∈ machine.states, State.«Pending» ∉ machine.next s := by
  decide

/-- Once an environment has been `Ready` it is only ever `Ready` or
`Offline`. -/
theorem ready_stays_placed (s : State) (r : Reaches machine .«Ready» s) :
    s ∈ [State.«Ready», .«Offline»] :=
  machine.reaches_closed (by decide) r (by decide)

/-- Concurrent writers that check the phase they land on keep the table. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Environment
