import Machines.Row

/-!
Claims about the object row table, proved over the table that
`internal/store.RowTable` renders into `Machines/Row.lean`.
-/

namespace Proofs.Row
open Machines.Row Statemachine

/-- A deleted row stays deleted: no write brings it back, so its name is
free for good. -/
theorem deleted_stays (s : State) (r : Reaches machine .«deleted» s) :
    s = .«deleted» :=
  machine.terminal_stays terminal_deleted r

/-- Only a live row is deleted. -/
theorem deleted_only_from_live :
    ∀ s ∈ machine.states, State.«deleted» ∈ machine.next s → s = .«live» := by
  decide

end Proofs.Row
