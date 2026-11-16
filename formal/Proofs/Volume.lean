import Machines.Volume

/-!
Claims about the volume phase table, proved over the table that
`controller.VolumeLifecycle` renders into `Machines/Volume.lean`.
-/

namespace Proofs.Volume
open Machines.Volume Statemachine

/-- A deleting volume stays deleting. -/
theorem deleting_stays (s : State) (r : Reaches machine .«Deleting» s) :
    s = .«Deleting» :=
  machine.terminal_stays «terminal_Deleting» r

/-- A lost volume is never recreated: from `Lost` only `Deleting` follows,
because its content went with the volume. -/
theorem lost_is_never_recreated (s : State) (r : Reaches machine .«Lost» s) :
    s ∈ [State.«Lost», .«Deleting»] :=
  machine.reaches_closed (by decide) r (by decide)

/-- A volume is `Available` only once its source was applied: the one
predecessor of `Available` is `Pending`. -/
theorem available_only_from_pending :
    ∀ s ∈ machine.states, State.«Available» ∈ machine.next s → s = .«Pending» := by
  decide

/-- Concurrent writers that check the phase they land on keep the table. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Volume
