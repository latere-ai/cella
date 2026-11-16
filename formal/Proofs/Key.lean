import Machines.Key

/-!
Claims about the environment key table, proved over the table that
`internal/store.KeyTable` renders into `Machines/Key.lean`.
-/

namespace Proofs.Key
open Machines.Key Statemachine

/-- A revoked key stays revoked: nothing follows `revoked`. -/
theorem revoked_stays (s : State) (r : Reaches machine .«revoked» s) :
    s = .«revoked» :=
  machine.terminal_stays terminal_revoked r

/-- Only an active key is revoked. -/
theorem revoked_only_from_active :
    ∀ s ∈ machine.states, State.«revoked» ∈ machine.next s → s = .«active» := by
  decide

end Proofs.Key
