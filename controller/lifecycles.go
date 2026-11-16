// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	"latere.ai/x/pkg/statemachine"

	v1 "latere.ai/x/cella/manifest/v1"
)

// The lifecycles the controller keeps for the objects it stores, each a
// statemachine table. The phase of an object is written at one place per
// store: Controller.persist (with the spawn debit and the write that carries a
// record of its own) for a sandbox, persistVolume for a volume, and the
// environment loop for an environment. Each checks the step against its table
// before the write, and the durable store checks it again in the statement
// that writes, so a step a table lacks changes nothing and is refused with an
// error that wraps statemachine.ErrIllegalTransition.

// SandboxLifecycle is the lifecycle of a sandbox, held in status.phase and in the
// phase column of its row.
//
// A driver reports Pending, Starting, Running, Stopping, Stopped, Failed,
// Lost and Deleting, and the controller writes what the driver reported over
// the phase it held, so the steps among those phases are the ones a driver
// can make. The controller owns the rest: a sandbox is Queued by the
// scheduler, from Running when it is preempted, and leaves Queued when it is
// placed (to Pending, or to what the driver reads when it started the object
// it kept), when its start deadline passes (Failed), when its driver lost
// the object (Lost) or when it is deleted. Lost is written by the reaper for
// a sandbox its driver no longer holds, Recovering by recovery from Lost,
// and Failed by recovery that gave up. Deleting is written by every delete
// and is terminal: the row is removed once the driver has let go.
//
// Pending and Queued are the phases a row starts in. A direct environment
// that cannot place a create writes it Pending and then Failed with reason
// NoCapacity.
var SandboxLifecycle = statemachine.Must(statemachine.New("Sandbox", []v1.SandboxPhase{v1.SandboxPending, v1.SandboxQueued}, map[v1.SandboxPhase][]v1.SandboxPhase{
	v1.SandboxPending:    {v1.SandboxStarting, v1.SandboxRunning, v1.SandboxStopped, v1.SandboxFailed, v1.SandboxDeleting},
	v1.SandboxQueued:     {v1.SandboxPending, v1.SandboxStarting, v1.SandboxRunning, v1.SandboxFailed, v1.SandboxLost, v1.SandboxDeleting},
	v1.SandboxStarting:   {v1.SandboxRunning, v1.SandboxStopping, v1.SandboxStopped, v1.SandboxFailed, v1.SandboxLost, v1.SandboxDeleting},
	v1.SandboxRunning:    {v1.SandboxStarting, v1.SandboxStopping, v1.SandboxStopped, v1.SandboxFailed, v1.SandboxLost, v1.SandboxQueued, v1.SandboxDeleting},
	v1.SandboxStopping:   {v1.SandboxStopped, v1.SandboxFailed, v1.SandboxLost, v1.SandboxDeleting},
	v1.SandboxStopped:    {v1.SandboxPending, v1.SandboxStarting, v1.SandboxRunning, v1.SandboxLost, v1.SandboxDeleting},
	v1.SandboxFailed:     {v1.SandboxPending, v1.SandboxStarting, v1.SandboxRunning, v1.SandboxDeleting},
	v1.SandboxLost:       {v1.SandboxRecovering, v1.SandboxFailed, v1.SandboxDeleting},
	v1.SandboxRecovering: {v1.SandboxStarting, v1.SandboxRunning, v1.SandboxFailed, v1.SandboxDeleting},
	v1.SandboxDeleting:   nil,
}))

// VolumeLifecycle is the lifecycle of a volume, held in status.phase and in the
// phase column of its row. A volume is Pending until its source is applied,
// then Available, or Failed with a reason where the source cannot be applied.
// An Available volume its driver no longer holds is Lost and is never
// recreated, since its content went with it. A delete writes Deleting from
// any phase and is terminal: the row is removed once the driver has let go.
var VolumeLifecycle = statemachine.Must(statemachine.New("Volume", []v1.VolumePhase{v1.VolumePending}, map[v1.VolumePhase][]v1.VolumePhase{
	v1.VolumePending:   {v1.VolumeAvailable, v1.VolumeFailed, v1.VolumeDeleting},
	v1.VolumeAvailable: {v1.VolumeLost, v1.VolumeDeleting},
	v1.VolumeFailed:    {v1.VolumeDeleting},
	v1.VolumeLost:      {v1.VolumeDeleting},
	v1.VolumeDeleting:  nil,
}))

// EnvironmentLifecycle is the lifecycle of an environment, held in status.phase
// and in the phase column of its row. The environment a control plane drives
// itself starts Ready; one served by workers starts Pending and is Ready once
// a worker registers. An environment that has answered none of its workers
// for the offline window is Offline and is Ready again when one answers.
// Degraded is a phase of the contract that nothing writes, so the table does
// not have it. An environment never returns to Pending.
var EnvironmentLifecycle = statemachine.Must(statemachine.New("Environment", []v1.EnvironmentPhase{v1.EnvironmentPending, v1.EnvironmentReady}, map[v1.EnvironmentPhase][]v1.EnvironmentPhase{
	v1.EnvironmentPending: {v1.EnvironmentReady},
	v1.EnvironmentReady:   {v1.EnvironmentOffline},
	v1.EnvironmentOffline: {v1.EnvironmentReady},
}))

// checkMove checks one write of an object against its lifecycle: a write of an
// object the writer does not hold yet starts the lifecycle, a write that keeps
// the phase is no step, and any other write takes a step the lifecycle has.
func checkMove[P ~string](t *statemachine.Table[P], id string, from P, held bool, to P) error {
	switch {
	case !held:
		if err := t.CheckStart(to); err != nil {
			return fmt.Errorf("%s %s: %w", t.Name(), id, err)
		}
	case from != to:
		if err := t.Check(from, to); err != nil {
			return fmt.Errorf("%s %s: %w", t.Name(), id, err)
		}
	}
	return nil
}
