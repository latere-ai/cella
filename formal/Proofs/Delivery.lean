import Machines.Delivery

/-!
Claims about the journal delivery table, proved over the table that
`internal/store.DeliveryTable` renders into `Machines/Delivery.lean`.
-/

namespace Proofs.Delivery
open Machines.Delivery Statemachine

/-- A delivered event is never dropped: nothing follows `delivered`. -/
theorem delivered_never_dropped (s : State) (r : Reaches machine .«delivered» s) :
    s ≠ .«dropped» := by
  have := machine.terminal_stays terminal_delivered r
  subst this; decide

/-- A dropped event is never delivered: nothing follows `dropped`. -/
theorem dropped_never_delivered (s : State) (r : Reaches machine .«dropped» s) :
    s ≠ .«delivered» := by
  have := machine.terminal_stays terminal_dropped r
  subst this; decide

/-- Only a pending event is finished: every predecessor of `delivered` and
of `dropped` is `pending`. -/
theorem finished_only_from_pending :
    ∀ s ∈ machine.states,
      (State.«delivered» ∈ machine.next s ∨ State.«dropped» ∈ machine.next s) → s = .«pending» := by
  decide

end Proofs.Delivery
