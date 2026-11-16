// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/statemachine"
	"latere.ai/x/pkg/statemachine/statemachinetest"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/store"
	v1 "latere.ai/x/cella/manifest/v1"
)

// Lifecycles drives the real writers of every stored lifecycle through its
// table on one adapter: each state is entered by the writers that reach it,
// every other state is asked of the writer, and two writers race on every
// pair of successors. A step the table has must land, any other must be
// refused with statemachine.ErrIllegalTransition and leave the record where
// it was.
func Lifecycles(t *testing.T, open Opener) {
	t.Helper()
	t.Run("Sandbox", func(t *testing.T) { objectLifecycle(t, open, sandboxes{}) })
	t.Run("Volume", func(t *testing.T) { objectLifecycle(t, open, volumes{}) })
	t.Run("Environment", func(t *testing.T) { objectLifecycle(t, open, environments{}) })
	t.Run("Operation", func(t *testing.T) { operationLifecycle(t, open) })
	t.Run("Delivery", func(t *testing.T) { deliveryLifecycle(t, open) })
	t.Run("Key", func(t *testing.T) { keyLifecycle(t, open) })
	t.Run("Row", func(t *testing.T) { rowLifecycle(t, open) })
}

// path is the states a record walks to reach to, from an initial state, along
// steps of the table: the first is the state it is created in.
func path[S ~string](table *statemachine.Table[S], to S) []S {
	type hop struct {
		state S
		via   []S
	}
	queue := []hop{}
	seen := map[S]bool{}
	for _, s := range table.Initial() {
		queue = append(queue, hop{s, []S{s}})
		seen[s] = true
	}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if h.state == to {
			return h.via
		}
		for _, next := range table.Next(h.state) {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, hop{next, append(append([]S{}, h.via...), next)})
			}
		}
	}
	return nil
}

// phased is the writer of one kind of object with a phase: it creates a row,
// moves it to a phase and reads the phase back.
type phased[P ~string] interface {
	prefix() string
	table() *statemachine.Table[P]
	// create writes the first phase of a new record, and move writes the next.
	create(ctx context.Context, c *store.Controlled, id string, phase P) error
	move(ctx context.Context, c *store.Controlled, id string, phase P) error
	// refresh reads the versions a bridge holds back from the store.
	refresh(c *store.Controlled) error
	phase(t *testing.T, s store.Store, id string) P
	kind() string
}

var objects atomic.Int64

func objectLifecycle[P ~string](t *testing.T, open Opener, kind phased[P]) {
	s := opened(t, open, Key)
	ctx := context.Background()
	table := kind.table()
	bridge := store.ForController(s, "env_one", store.Delivered)
	d := statemachinetest.Driver[P]{
		Enter: func(t *testing.T, state P) string {
			id := fmt.Sprintf("%s_%d", kind.prefix(), objects.Add(1))
			for i, step := range path(table, state) {
				var err error
				if i == 0 {
					err = kind.create(ctx, bridge, id, step)
				} else {
					err = kind.move(ctx, bridge, id, step)
				}
				if err != nil {
					t.Fatalf("entering %s: %s: %v", state, step, err)
				}
			}
			return id
		},
		Move: func(id string, to P) error {
			// A writer that read a row another writer moved is refused as
			// stale and reads again, as a writer on the lifecycle's
			// single-writer lease does after its own conflict.
			// Each writer is a bridge of its own, as each replica is, so what
			// it last read is what it read and not what a racing writer wrote.
			writer := store.ForController(s, "env_one", store.Delivered)
			var err error
			for range 8 {
				// The writer that finds the record in the state it is asked
				// for has nothing to do: a step the table does not have.
				if rerr := kind.refresh(writer); rerr != nil {
					return rerr
				}
				held, rerr := rowPhaseOf(ctx, s, kind.kind(), id)
				if rerr != nil {
					return rerr
				}
				if held == string(to) {
					return fmt.Errorf("%s %s is %s already: %w", kind.kind(), id, held, statemachine.ErrIllegalTransition)
				}
				err = kind.move(ctx, writer, id, to)
				if !errors.Is(err, controller.ErrVersionConflict) && !errors.Is(err, store.ErrVersionConflict) {
					return err
				}
			}
			return err
		},
		State: func(t *testing.T, id string) P {
			return kind.phase(t, s, id)
		},
	}
	statemachinetest.Exhaustive(t, table, d)
	statemachinetest.Race(t, table, d)
}

// sandboxes writes sandboxes through Controlled.Write.
type sandboxes struct{}

func (sandboxes) prefix() string { return "sbx" }
func (sandboxes) table() *statemachine.Table[v1.SandboxPhase] {
	return controller.SandboxLifecycle
}

func sandbox(id string, phase v1.SandboxPhase) v1.Sandbox {
	return v1.Sandbox{
		APIVersion: v1.APIVersion, Kind: v1.KindSandbox,
		Metadata: v1.Metadata{Name: id},
		Spec:     v1.SandboxSpec{Environment: "env_one"},
		Status:   v1.SandboxStatus{ID: id, Owner: "alice", Environment: "env_one", Phase: phase, CreatedAt: time.Now().UTC()},
	}
}

func (sandboxes) create(ctx context.Context, c *store.Controlled, id string, phase v1.SandboxPhase) error {
	return c.Write(ctx, sandbox(id, phase), controller.MutationCreated)
}
func (sandboxes) move(ctx context.Context, c *store.Controlled, id string, phase v1.SandboxPhase) error {
	return c.Write(ctx, sandbox(id, phase), controller.MutationStatus)
}
func (sandboxes) refresh(c *store.Controlled) error { _, err := c.Load(); return err }
func (sandboxes) phase(t *testing.T, s store.Store, id string) v1.SandboxPhase {
	return v1.ParseSandboxPhase(rowPhase(t, s, store.KindSandbox, id))
}

func (sandboxes) kind() string    { return store.KindSandbox }
func (volumes) kind() string      { return store.KindVolume }
func (environments) kind() string { return store.KindEnvironment }

func rowPhaseOf(ctx context.Context, s store.Store, kind, id string) (string, error) {
	var obj store.Object
	err := s.Tx(ctx, func(tx store.Tx) error {
		var err error
		obj, err = tx.Desired().Get(ctx, kind, id)
		return err
	})
	return obj.Phase, err
}

func rowPhase(t *testing.T, s store.Store, kind, id string) string {
	t.Helper()
	ctx := context.Background()
	var obj store.Object
	within(ctx, t, s, func(tx store.Tx) error {
		var err error
		obj, err = tx.Desired().Get(ctx, kind, id)
		return err
	})
	return obj.Phase
}

// volumes writes volumes through Controlled.WriteVolume.
type volumes struct{}

func (volumes) prefix() string { return "vol" }
func (volumes) table() *statemachine.Table[v1.VolumePhase] {
	return controller.VolumeLifecycle
}

func volume(id string, phase v1.VolumePhase) v1.Volume {
	return v1.Volume{
		APIVersion: v1.APIVersion, Kind: v1.KindVolume,
		Metadata: v1.Metadata{Name: id},
		Spec:     v1.VolumeSpec{Environment: "env_one", Size: "1Gi"},
		Status:   v1.VolumeStatus{ID: id, Owner: "alice", Phase: phase},
	}
}

func (volumes) create(ctx context.Context, c *store.Controlled, id string, phase v1.VolumePhase) error {
	return c.WriteVolume(ctx, controller.VolumeAct{Type: controller.MutationVolumeCreated, Object: volume(id, phase)})
}
func (volumes) move(ctx context.Context, c *store.Controlled, id string, phase v1.VolumePhase) error {
	return c.WriteVolume(ctx, controller.VolumeAct{Type: controller.MutationVolumeStatus, Object: volume(id, phase)})
}
func (volumes) refresh(c *store.Controlled) error { _, err := c.LoadVolumes(); return err }
func (volumes) phase(t *testing.T, s store.Store, id string) v1.VolumePhase {
	return v1.ParseVolumePhase(rowPhase(t, s, store.KindVolume, id))
}

// environments writes environments through Controlled.WriteEnvironmentStatus,
// which is the writer of the phase an environment moves through, after
// WriteEnvironment created the row.
type environments struct{}

func (environments) prefix() string { return "env" }
func (environments) table() *statemachine.Table[v1.EnvironmentPhase] {
	return controller.EnvironmentLifecycle
}

func environment(id string, phase v1.EnvironmentPhase) v1.Environment {
	return v1.Environment{
		APIVersion: v1.APIVersion, Kind: v1.KindEnvironment,
		Metadata: v1.Metadata{Name: id},
		Spec:     v1.EnvironmentSpec{Mode: v1.EnvironmentWorker},
		Status:   v1.EnvironmentStatus{ID: id, Owner: "alice", Phase: phase},
	}
}

func (environments) create(ctx context.Context, c *store.Controlled, id string, phase v1.EnvironmentPhase) error {
	_, err := c.WriteEnvironment(ctx, environment(id, phase), 0, controller.MutationEnvironmentCreated)
	return err
}
func (environments) move(ctx context.Context, c *store.Controlled, id string, phase v1.EnvironmentPhase) error {
	return c.WriteEnvironmentStatus(ctx, environment(id, phase), "")
}
func (environments) refresh(c *store.Controlled) error { _, err := c.LoadEnvironments(); return err }
func (environments) phase(t *testing.T, s store.Store, id string) v1.EnvironmentPhase {
	return v1.ParseEnvironmentPhase(rowPhase(t, s, store.KindEnvironment, id))
}

var nonce atomic.Int64

func operationLifecycle(t *testing.T, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	d := statemachinetest.Driver[store.OperationState]{
		Enter: func(t *testing.T, state store.OperationState) string {
			// Each operation has an environment of its own, so a claim of
			// that environment takes that operation alone.
			id := fmt.Sprintf("op_%d", nonce.Add(1))
			within(ctx, t, s, func(tx store.Tx) error {
				return tx.Operations().Enqueue(ctx, store.Operation{ID: id, Environment: id, Type: "Exec"})
			})
			for _, step := range path(store.OperationTable, state)[1:] {
				if err := moveOperation(ctx, s, id, step); err != nil {
					t.Fatalf("entering %s: %v", state, err)
				}
			}
			return id
		},
		Move: func(id string, to store.OperationState) error { return moveOperation(ctx, s, id, to) },
		State: func(t *testing.T, id string) store.OperationState {
			var op store.Operation
			within(ctx, t, s, func(tx store.Tx) error {
				var err error
				op, err = tx.Operations().Get(ctx, id)
				return err
			})
			return op.State
		},
	}
	statemachinetest.Exhaustive(t, store.OperationTable, d)
	statemachinetest.Race(t, store.OperationTable, d)
}

// moveOperation is the writer of operations.state that takes an operation to
// a state: a claim by a worker that is heartbeating, or the result that ends
// it. A writer that answers nil and left the operation as it was is refused:
// a claim that took nothing, and a result that is not the one stored.
func moveOperation(ctx context.Context, s store.Store, id string, to store.OperationState) error {
	switch to {
	case store.OperationClaimed:
		worker := fmt.Sprintf("worker_%d", nonce.Add(1))
		var claimed []store.Operation
		err := s.Tx(ctx, func(tx store.Tx) error {
			if err := tx.Operations().Register(ctx, store.Worker{Environment: id, Worker: worker}); err != nil {
				return err
			}
			var err error
			claimed, err = tx.Operations().Claim(ctx, id, worker, 1, time.Now().Add(-time.Hour))
			return err
		})
		if err != nil {
			return err
		}
		if len(claimed) == 0 {
			return fmt.Errorf("claiming %s took nothing: %w", id, statemachine.ErrIllegalTransition)
		}
		return nil
	case store.OperationDone:
		result := fmt.Sprintf("%d", nonce.Add(1))
		var op store.Operation
		err := s.Tx(ctx, func(tx store.Tx) error {
			if err := tx.Operations().Acknowledge(ctx, id, []byte(result)); err != nil {
				return err
			}
			var err error
			op, err = tx.Operations().Get(ctx, id)
			return err
		})
		if err != nil {
			return err
		}
		if string(op.Result) != result {
			return fmt.Errorf("acknowledging %s kept the result it had: %w", id, statemachine.ErrIllegalTransition)
		}
		return nil
	case store.OperationQueued:
		// Nothing writes a queued operation but Enqueue, which creates it.
	}
	return fmt.Errorf("no writer takes an operation to %s: %w", to, statemachine.ErrIllegalTransition)
}

func deliveryLifecycle(t *testing.T, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	d := statemachinetest.Driver[store.DeliveryState]{
		Enter: func(t *testing.T, state store.DeliveryState) string {
			e := store.Event{ObjectID: fmt.Sprintf("sbx_%d", nonce.Add(1)), Type: "sandbox.created", ID: fmt.Sprintf("evt_%d", nonce.Add(1))}
			if state == store.DeliveryDelivered {
				// An event design 009 names no type for is stored finished.
				e.AckedAt = time.Now().UTC()
			}
			within(ctx, t, s, func(tx store.Tx) error {
				_, err := tx.Journal().Append(ctx, e)
				return err
			})
			if state == store.DeliveryDropped {
				within(ctx, t, s, func(tx store.Tx) error { return tx.Journal().Drop(ctx, e.ID, time.Now()) })
			}
			return e.ID
		},
		Move: func(id string, to store.DeliveryState) error {
			return s.Tx(ctx, func(tx store.Tx) error {
				switch to {
				case store.DeliveryDelivered:
					return tx.Journal().Acknowledge(ctx, id, time.Now())
				case store.DeliveryDropped:
					return tx.Journal().Drop(ctx, id, time.Now())
				case store.DeliveryPending:
					// Nothing writes a pending event but Append, which
					// creates it.
				}
				return fmt.Errorf("no writer takes an event to %s: %w", to, statemachine.ErrIllegalTransition)
			})
		},
		State: func(t *testing.T, id string) store.DeliveryState {
			var state store.DeliveryState
			within(ctx, t, s, func(tx store.Tx) error {
				var err error
				state, err = tx.Journal().Delivery(ctx, id)
				return err
			})
			return state
		},
	}
	statemachinetest.Exhaustive(t, store.DeliveryTable, d)
	statemachinetest.Race(t, store.DeliveryTable, d)
	// Defer records an attempt of a pending event and is no step: a finished
	// event is left as it is.
	for _, state := range store.DeliveryTable.States() {
		id := d.Enter(t, state)
		err := s.Tx(ctx, func(tx store.Tx) error { return tx.Journal().Defer(ctx, id, time.Now().Add(time.Minute)) })
		if (state == store.DeliveryPending) != (err == nil) || (err != nil && !errors.Is(err, statemachine.ErrIllegalTransition)) {
			t.Errorf("Defer of a %s event = %v", state, err)
		}
		if got := d.State(t, id); got != state {
			t.Errorf("Defer of a %s event left it %s", state, got)
		}
	}
}

func keyLifecycle(t *testing.T, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	d := statemachinetest.Driver[store.KeyState]{
		Enter: func(t *testing.T, state store.KeyState) string {
			k := store.Key{JTI: fmt.Sprintf("jti_%d", nonce.Add(1)), Environment: "env_one",
				MintedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
			if state == store.KeyRevoked {
				k.RevokedAt = time.Now().Add(time.Minute)
			}
			within(ctx, t, s, func(tx store.Tx) error { return tx.Keys().Record(ctx, k) })
			return k.JTI
		},
		Move: func(jti string, to store.KeyState) error {
			if to != store.KeyRevoked {
				return fmt.Errorf("no writer takes a key to %s: %w", to, statemachine.ErrIllegalTransition)
			}
			return s.Tx(ctx, func(tx store.Tx) error {
				return tx.Keys().Revoke(ctx, jti, time.Now().Add(time.Minute))
			})
		},
		State: func(t *testing.T, jti string) store.KeyState {
			var key store.Key
			within(ctx, t, s, func(tx store.Tx) error {
				var err error
				key, _, err = tx.Keys().Get(ctx, jti)
				return err
			})
			if key.RevokedAt.IsZero() {
				return store.KeyActive
			}
			return store.KeyRevoked
		},
	}
	statemachinetest.Exhaustive(t, store.KeyTable, d)
	statemachinetest.Race(t, store.KeyTable, d)
}

func rowLifecycle(t *testing.T, open Opener) {
	s := opened(t, open, Key)
	ctx := context.Background()
	d := statemachinetest.Driver[store.RowState]{
		Enter: func(t *testing.T, state store.RowState) string {
			id := fmt.Sprintf("sbx_%d", nonce.Add(1))
			within(ctx, t, s, func(tx store.Tx) error {
				_, err := tx.Desired().Put(ctx, object(id, "alice", id), 0)
				return err
			})
			if state == store.RowDeleted {
				within(ctx, t, s, func(tx store.Tx) error { return tx.Desired().Delete(ctx, store.KindSandbox, id) })
			}
			return id
		},
		Move: func(id string, to store.RowState) error {
			if to != store.RowDeleted {
				return fmt.Errorf("no writer takes a row to %s: %w", to, statemachine.ErrIllegalTransition)
			}
			return s.Tx(ctx, func(tx store.Tx) error { return tx.Desired().Delete(ctx, store.KindSandbox, id) })
		},
		State: func(t *testing.T, id string) store.RowState {
			err := s.Tx(ctx, func(tx store.Tx) error {
				_, err := tx.Desired().Get(ctx, store.KindSandbox, id)
				return err
			})
			if errors.Is(err, store.ErrNotFound) {
				return store.RowDeleted
			}
			if err != nil {
				t.Fatal(err)
			}
			return store.RowLive
		},
	}
	statemachinetest.Exhaustive(t, store.RowTable, d)
	statemachinetest.Race(t, store.RowTable, d)
}

// within runs one transaction and fails the test when it does not commit.
func within(ctx context.Context, t *testing.T, s store.Store, fn func(store.Tx) error) {
	t.Helper()
	if err := s.Tx(ctx, fn); err != nil {
		t.Fatalf("the transaction did not commit: %v", err)
	}
}
