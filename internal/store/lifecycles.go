// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"latere.ai/x/pkg/statemachine"
)

// ErrIllegalTransition is a write that would take a record along a step its
// lifecycle lacks. It changes nothing. Every adapter reports it for the same
// refusal, and the error wraps statemachine.ErrIllegalTransition.
var ErrIllegalTransition = statemachine.ErrIllegalTransition

// The lifecycles the store keeps, each a statemachine table. A write of the
// state is one function per column that binds the table's sources into the
// statement that writes it, and counts the rows: a statement that changed no
// row is ErrIllegalTransition (or ErrNotFound where no row has the id).
// The tests drive those writers through each table on Postgres and on the
// memory adapter, and formal/ renders the tables into the Lean and TLA+
// models the lean and tla gates check.

// OperationState is the state of a worker operation, as operations.state
// holds it.
type OperationState string

// The states one operation passes through. A queued row waits for a worker, a
// claimed row is one worker's until its heartbeat lapses, and a done row
// carries the result and is kept until the sweep forgets it.
const (
	OperationQueued  OperationState = "queued"
	OperationClaimed OperationState = "claimed"
	OperationDone    OperationState = "done"
)

// OperationTable is the lifecycle of a worker operation, held in
// operations.state. Enqueue writes the first state; Claim takes queued rows,
// and takes claimed rows too where their worker stopped heartbeating, which
// leaves the state as it is and changes the holder (the claim protocol, not a
// step); Acknowledge ends a queued or claimed row with its result.
var OperationTable = statemachine.Must(statemachine.New("Operation", []OperationState{OperationQueued}, map[OperationState][]OperationState{
	OperationQueued:  {OperationClaimed, OperationDone},
	OperationClaimed: {OperationDone},
	OperationDone:    nil,
}))

// DeliveryState is where an event of the journal is in its delivery. It is
// derived from two timestamps: pending while neither acked_at nor dropped_at
// is set, delivered once the sink took it, dropped once delivery gave it up.
type DeliveryState string

// The delivery states of one journal event.
const (
	DeliveryPending   DeliveryState = "pending"
	DeliveryDelivered DeliveryState = "delivered"
	DeliveryDropped   DeliveryState = "dropped"
)

// DeliveryTable is the delivery lifecycle of a journal event. Append writes
// pending, or delivered for an event design 009 names no type for, which is
// stored finished. Acknowledge delivers a pending event and Drop ends one.
// Defer records a failed attempt of a pending event and is no step.
var DeliveryTable = statemachine.Must(statemachine.New("Delivery", []DeliveryState{DeliveryPending, DeliveryDelivered}, map[DeliveryState][]DeliveryState{
	DeliveryPending:   {DeliveryDelivered, DeliveryDropped},
	DeliveryDelivered: nil,
	DeliveryDropped:   nil,
}))

// KeyState is whether an environment key is in force, derived from
// environment_keys.revoked_at: active while it is null, revoked once set.
type KeyState string

// The states of an environment key.
const (
	KeyActive  KeyState = "active"
	KeyRevoked KeyState = "revoked"
)

// KeyTable is the lifecycle of an environment key. Record writes a key active,
// or revoked where the mint recorded a revocation with it; Revoke ends an
// active key. A key revoked again keeps the earlier instant, which is no
// step.
var KeyTable = statemachine.Must(statemachine.New("Key", []KeyState{KeyActive, KeyRevoked}, map[KeyState][]KeyState{
	KeyActive:  {KeyRevoked},
	KeyRevoked: nil,
}))

// RowState is whether an object's row is live, derived from
// objects.deleted_at: live while it is null, deleted once set.
type RowState string

// The states of an object's row.
const (
	RowLive    RowState = "live"
	RowDeleted RowState = "deleted"
)

// RowTable is the lifecycle of the row of any object kind. Put creates it
// live and Delete ends it: a deleted row keeps its id and frees its name, and
// no write brings it back.
var RowTable = statemachine.Must(statemachine.New("Row", []RowState{RowLive}, map[RowState][]RowState{
	RowLive:    {RowDeleted},
	RowDeleted: nil,
}))

// sources is the states a write of to may find the record in, as text for the
// `= ANY($n)` of a guarded statement.
func sources[S ~string](t *statemachine.Table[S], to S) []string {
	from := t.Sources(to)
	out := make([]string, 0, len(from))
	for _, s := range from {
		out = append(out, string(s))
	}
	return out
}

// The sources of each table, exported because the adapters are other packages
// and bind the same condition.

// OperationSources is the states from which an operation takes a step to to.
func OperationSources(to OperationState) []string { return sources(OperationTable, to) }

// DeliverySources is the states from which a journal event takes a step to to.
func DeliverySources(to DeliveryState) []string { return sources(DeliveryTable, to) }

// KeySources is the states from which an environment key takes a step to to.
func KeySources(to KeyState) []string { return sources(KeyTable, to) }

// RowSources is the states from which an object's row takes a step to to.
func RowSources(to RowState) []string { return sources(RowTable, to) }

// DeliveryOf is the delivery state an event is stored in: delivered where it
// carries the instant the sink took it, dropped where delivery gave it up,
// pending otherwise.
func DeliveryOf(e Event) DeliveryState {
	switch {
	case !e.AckedAt.IsZero():
		return DeliveryDelivered
	case !e.DroppedAt.IsZero():
		return DeliveryDropped
	}
	return DeliveryPending
}

// guard puts the lifecycle's condition on a write of row, which takes the
// object to the phase `to` from the version it last read, in the phase last
// (empty where it was never read): a write of a row that does not exist yet
// must start the lifecycle; a write that keeps the phase it last read lands
// only while the row is still in it; and a write that changes the phase is a
// step, which lands only from a phase with a step to `to`.
func guard[P ~string](t *statemachine.Table[P], row *Object, to P, version int64, last string) error {
	if version == 0 {
		return t.CheckStart(to)
	}
	if last == string(to) {
		row.Sources = []string{string(to)}
		return nil
	}
	row.Sources = sources(t, to)
	return nil
}

// ParseDeliveryState reads a delivery state a query derived from the
// timestamps of an event, and names it as a state of DeliveryTable.
func ParseDeliveryState(text string) DeliveryState { return DeliveryState(text) }
