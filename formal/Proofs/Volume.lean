import Machines.Volume

/-!
Claims about the volume phase table, proved over the table that
`controller.VolumeLifecycle` renders into `Machines/Volume.lean`.
-/

namespace Proofs.Volume
open Machines.Volume Statemachine

/-- A deleting volume stays deleting. -/
theorem deleting_stays (s : State) (r : Reaches machine .«deleting» s) :
    s = .«deleting» :=
  machine.terminal_stays terminal_deleting r

/-- A lost volume is never recreated: from `lost` only `deleting` follows,
because its content went with the volume. -/
theorem lost_is_never_recreated (s : State) (r : Reaches machine .«lost» s) :
    s ∈ [State.«lost», .«deleting»] :=
  machine.reaches_closed (by decide) r (by decide)

/-- A volume is `available` only once its source was applied: the one
predecessor of `available` is `pending`. -/
theorem available_only_from_pending :
    ∀ s ∈ machine.states, State.«available» ∈ machine.next s → s = .«pending» := by
  decide

/-- Concurrent writers that check the phase they land on keep the table. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Volume
