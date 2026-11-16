import Machines.Operation

/-!
Claims about the worker operation table, proved over the table that
`internal/store.OperationTable` renders into `Machines/Operation.lean`.
-/

namespace Proofs.Operation
open Machines.Operation Statemachine

/-- A done operation keeps its result: nothing follows `done`. -/
theorem done_stays (s : State) (r : Reaches machine .«done» s) :
    s = .«done» :=
  machine.terminal_stays terminal_done r

/-- Only a queued operation is claimed: the one predecessor of `claimed` is
`queued`. A claim taken again from a worker that stopped heartbeating leaves
the state as it is. -/
theorem claimed_only_from_queued :
    ∀ s ∈ machine.states, State.«claimed» ∈ machine.next s → s = .«queued» := by
  decide

/-- Concurrent writers that check the state they land on keep the table. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Operation
