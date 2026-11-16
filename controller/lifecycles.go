// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strings"

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
// error that wraps statemachine.ErrIllegalTransition. A table's states are
// lowercase words, so each is the lowercase of the phase it names.

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
var SandboxLifecycle = newLifecycle("Sandbox", []v1.SandboxPhase{v1.SandboxPending, v1.SandboxQueued}, map[v1.SandboxPhase][]v1.SandboxPhase{
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
})

// VolumeLifecycle is the lifecycle of a volume, held in status.phase and in the
// phase column of its row. A volume is Pending until its source is applied,
// then Available, or Failed with a reason where the source cannot be applied.
// An Available volume its driver no longer holds is Lost and is never
// recreated, since its content went with it. A delete writes Deleting from
// any phase and is terminal: the row is removed once the driver has let go.
var VolumeLifecycle = newLifecycle("Volume", []v1.VolumePhase{v1.VolumePending}, map[v1.VolumePhase][]v1.VolumePhase{
	v1.VolumePending:   {v1.VolumeAvailable, v1.VolumeFailed, v1.VolumeDeleting},
	v1.VolumeAvailable: {v1.VolumeLost, v1.VolumeDeleting},
	v1.VolumeFailed:    {v1.VolumeDeleting},
	v1.VolumeLost:      {v1.VolumeDeleting},
	v1.VolumeDeleting:  nil,
})

// EnvironmentLifecycle is the lifecycle of an environment, held in status.phase
// and in the phase column of its row. The environment a control plane drives
// itself starts Ready; one served by workers starts Pending and is Ready once
// a worker registers. An environment that has answered none of its workers
// for the offline window is Offline and is Ready again when one answers.
// Degraded is a phase of the contract that nothing writes, so the table does
// not have it. An environment never returns to Pending.
var EnvironmentLifecycle = newLifecycle("Environment", []v1.EnvironmentPhase{v1.EnvironmentPending, v1.EnvironmentReady}, map[v1.EnvironmentPhase][]v1.EnvironmentPhase{
	v1.EnvironmentPending: {v1.EnvironmentReady},
	v1.EnvironmentReady:   {v1.EnvironmentOffline},
	v1.EnvironmentOffline: {v1.EnvironmentReady},
})

// State is a phase as a table names it: the lowercase of the phase the object
// carries on the wire, since a table's states are lowercase words. The phase
// itself is unchanged wherever it is stored and served.
type State string

// Lifecycle is a statemachine table over the phases of one kind of object,
// which speaks the phases themselves and not the table's lowercase states.
type Lifecycle[P ~string] struct {
	table *statemachine.Table[State]
	wire  map[State]P
}

func newLifecycle[P ~string](name string, initial []P, steps map[P][]P) *Lifecycle[P] {
	l := &Lifecycle[P]{wire: make(map[State]P, len(steps))}
	next := make(map[State][]State, len(steps))
	for from, to := range steps {
		l.wire[stateOf(from)] = from
		succ := make([]State, 0, len(to))
		for _, p := range to {
			succ = append(succ, stateOf(p))
		}
		next[stateOf(from)] = succ
	}
	start := make([]State, 0, len(initial))
	for _, p := range initial {
		start = append(start, stateOf(p))
	}
	l.table = statemachine.Must(statemachine.New(name, start, next))
	return l
}

func stateOf[P ~string](p P) State { return State(strings.ToLower(string(p))) }

// Table is the lifecycle as a statemachine table over lowercase states, which
// the model renderer and the writer tests take.
func (l *Lifecycle[P]) Table() *statemachine.Table[State] { return l.table }

// Phases is every phase the lifecycle has, in the table's order.
func (l *Lifecycle[P]) Phases() []P {
	var out []P
	for _, s := range l.table.States() {
		out = append(out, l.wire[s])
	}
	return out
}

// State is the table's name for a phase.
func (l *Lifecycle[P]) State(p P) State { return stateOf(p) }

// Phase is the phase a table state names.
func (l *Lifecycle[P]) Phase(s State) P { return l.wire[s] }

// Can reports whether the lifecycle has the step from one phase to another.
func (l *Lifecycle[P]) Can(from, to P) bool { return l.table.Can(stateOf(from), stateOf(to)) }

// Terminal reports whether nothing leaves a phase.
func (l *Lifecycle[P]) Terminal(p P) bool { return l.table.Terminal(stateOf(p)) }

// Check is nil where the lifecycle has the step, and otherwise an error that
// wraps statemachine.ErrIllegalTransition.
func (l *Lifecycle[P]) Check(from, to P) error { return l.table.Check(stateOf(from), stateOf(to)) }

// CheckStart is nil where an object may start in a phase.
func (l *Lifecycle[P]) CheckStart(p P) error { return l.table.CheckStart(stateOf(p)) }

// Steps is the phases with a step to a phase, as the wire names them: the
// condition of a write that changes the phase.
func (l *Lifecycle[P]) Steps(to P) []string {
	out := []string{}
	for _, s := range l.table.Sources(stateOf(to)) {
		out = append(out, string(l.wire[s]))
	}
	return out
}

// Sources is the phases a stored row may hold for a write of to to land: the
// phases with a step to it, and to itself, which a write that keeps the phase
// is. It is the condition of a guarded write, bound as `phase = ANY($n)`.
func (l *Lifecycle[P]) Sources(to P) []string {
	out := []string{string(to)}
	for _, s := range l.table.Sources(stateOf(to)) {
		out = append(out, string(l.wire[s]))
	}
	return out
}

// Move checks one write of an object against the lifecycle: a write of an
// object the writer does not hold yet starts the lifecycle, a write that keeps
// the phase is no step, and any other write takes a step the lifecycle has.
func (l *Lifecycle[P]) Move(id string, from P, held bool, to P) error {
	switch {
	case !held:
		if err := l.CheckStart(to); err != nil {
			return fmt.Errorf("%s %s: %w", l.table.Name(), id, err)
		}
	case from != to:
		if err := l.Check(from, to); err != nil {
			return fmt.Errorf("%s %s: %w", l.table.Name(), id, err)
		}
	}
	return nil
}
