// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// bound opens a memory store and the controller's view of it.
func bound(t *testing.T) (*store.Controlled, store.Store) {
	t.Helper()
	s, err := memory.Open(memory.Options{Key: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	})
	return store.ForController(s, "default", store.Delivered), s
}

// sandbox is one desired sandbox as the controller holds it.
func sandbox(id, name, phase string) v1.Sandbox {
	return v1.Sandbox{
		APIVersion: v1.APIVersion, Kind: "Sandbox",
		Metadata: v1.Metadata{Name: name, Labels: map[string]string{"team": "a"}, Annotations: map[string]string{"note": "kept"}},
		Spec:     v1.SandboxSpec{Environment: "default", Workdir: "/workspace"},
		Status: v1.SandboxStatus{
			ID: id, Owner: "alice", Environment: "default", Phase: phase,
			CreatedAt: time.Now().UTC().Truncate(time.Second),
		},
	}
}

// TestBridgeRoundTrip: what the controller writes is what it reads back, with
// the labels and annotations a recovery recreates the sandbox from.
func TestBridgeRoundTrip(t *testing.T) {
	c, _ := bound(t)
	obj := sandbox("sbx_a", "work", driver.Running)
	if err := c.Write(t.Context(), obj, controller.MutationCreated); err != nil {
		t.Fatalf("writing the object: %v", err)
	}
	objects, err := c.Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	got, held := objects["sbx_a"]
	if !held {
		t.Fatalf("the object did not come back: %v", objects)
	}
	if got.Metadata.Name != "work" || got.Metadata.Labels["team"] != "a" ||
		got.Metadata.Annotations["note"] != "kept" || got.Spec.Workdir != "/workspace" {
		t.Errorf("the manifest reads back %+v", got)
	}
	if got.Status.ID != "sbx_a" || got.Status.Owner != "alice" || got.Status.Phase != driver.Running ||
		!got.Status.CreatedAt.Equal(obj.Status.CreatedAt) {
		t.Errorf("the status reads back %+v", got.Status)
	}
	events, err := c.Events(t.Context(), "sbx_a", 10)
	if err != nil {
		t.Fatalf("reading the journal: %v", err)
	}
	if len(events) != 1 || events[0].Type != controller.MutationCreated {
		t.Errorf("the journal reads %+v", events)
	}
}

// TestBridgeWritesAreConditional: the second replica of a control plane cannot
// overwrite a row it never read, which is the version of design 010 doing its
// work through the seam the controller sees.
func TestBridgeWritesAreConditional(t *testing.T) {
	first, s := bound(t)
	second := store.ForController(s, "default", store.Delivered)
	obj := sandbox("sbx_a", "work", driver.Pending)
	if err := first.Write(t.Context(), obj, controller.MutationCreated); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	obj.Status.Phase = driver.Running
	if err := first.Write(t.Context(), obj, controller.MutationUpdated); err != nil {
		t.Fatalf("the second write of the same holder: %v", err)
	}
	if err := second.Write(t.Context(), obj, controller.MutationUpdated); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("a replica that never read the row wrote it: %v", err)
	}
	// Reading is what gives the second replica the version, and then it
	// writes like any other holder.
	if _, err := second.Load(); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if err := second.Write(t.Context(), obj, controller.MutationUpdated); err != nil {
		t.Fatalf("a replica that read the row could not write it: %v", err)
	}
}

// TestBridgeRemoves: a deleted object leaves the map and its removal is in the
// journal; removing it twice is not an error, because another replica may have
// ended it first.
func TestBridgeRemoves(t *testing.T) {
	c, _ := bound(t)
	if err := c.Write(t.Context(), sandbox("sbx_a", "work", driver.Running), controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := c.Remove(t.Context(), "sbx_a", controller.MutationDeleted); err != nil {
			t.Fatalf("removing the object: %v", err)
		}
	}
	objects, err := c.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Errorf("the removed object is still there: %v", objects)
	}
	events, err := c.Events(t.Context(), "sbx_a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Type != controller.MutationDeleted {
		t.Errorf("the journal reads %+v", events)
	}
}

// TestBridgeLoadsEveryPage: desired state is read a page at a time, so an
// environment with more sandboxes than one page comes back whole.
func TestBridgeLoadsEveryPage(t *testing.T) {
	c, _ := bound(t)
	const many = store.DefaultPageLimit + 5
	for i := range many {
		obj := sandbox(fmt.Sprintf("sbx_%04d", i), fmt.Sprintf("work-%04d", i), driver.Running)
		if err := c.Write(t.Context(), obj, controller.MutationCreated); err != nil {
			t.Fatalf("writing %d: %v", i, err)
		}
	}
	objects, err := c.Load()
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if len(objects) != many {
		t.Fatalf("loaded %d of %d objects", len(objects), many)
	}
}

// TestBridgeLoadsOnlyItsEnvironment: a store shared by two environments hands
// each controller its own, because a controller drives one environment.
func TestBridgeLoadsOnlyItsEnvironment(t *testing.T) {
	c, s := bound(t)
	if err := c.Write(t.Context(), sandbox("sbx_a", "work", driver.Running), controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	elsewhere := store.ForController(s, "other", store.Delivered)
	// A name is unique per owner and kind across every environment, which is
	// design 010's index and not a per environment one.
	obj := sandbox("sbx_b", "work-elsewhere", driver.Running)
	obj.Status.Environment = "other"
	if err := elsewhere.Write(t.Context(), obj, controller.MutationCreated); err != nil {
		t.Fatal(err)
	}
	objects, err := c.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || objects["sbx_a"].Status.ID != "sbx_a" {
		t.Fatalf("this environment loaded %v", objects)
	}
}

// TestBridgeSavesTheWholeMap: a caller that holds the store as a plain Store
// still gets the snapshot contract, so the durable store is a drop-in for the
// file store rather than a second kind of thing.
func TestBridgeSavesTheWholeMap(t *testing.T) {
	c, _ := bound(t)
	objects := map[string]v1.Sandbox{
		"sbx_a": sandbox("sbx_a", "one", driver.Running),
		"sbx_b": sandbox("sbx_b", "two", driver.Running),
	}
	if err := c.Save(objects); err != nil {
		t.Fatalf("saving: %v", err)
	}
	delete(objects, "sbx_b")
	if err := c.Save(objects); err != nil {
		t.Fatalf("saving the smaller map: %v", err)
	}
	held, err := c.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held["sbx_a"].Metadata.Name != "one" {
		t.Fatalf("the store holds %v", held)
	}
}

// TestBridgeRebuilds: the observed index the reaper writes reaches the store's
// own contract, where the lost rule of design 005 reads it.
func TestBridgeRebuilds(t *testing.T) {
	c, s := bound(t)
	states := []driver.State{{ID: "sbx_a", Owner: "alice", Phase: driver.Running}}
	if err := c.Rebuild(t.Context(), "default", states); err != nil {
		t.Fatalf("rebuilding: %v", err)
	}
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		got, environment, err := tx.Observed().Get(t.Context(), "sbx_a")
		if err != nil {
			return err
		}
		if environment != "default" || got.Phase != driver.Running {
			t.Errorf("the index holds %+v on %q", got, environment)
		}
		return nil
	}); err != nil {
		t.Fatalf("reading the index: %v", err)
	}
}

// TestBridgeIsTheLeaseSeam: the controller's lease is the store's, held by one
// identity per process.
func TestBridgeIsTheLeaseSeam(t *testing.T) {
	c, s := bound(t)
	other := store.ForController(s, "default", store.Delivered)
	if c.Holder() == "" || c.Holder() == other.Holder() {
		t.Fatalf("two processes share the holder %q", c.Holder())
	}
	held, err := c.Acquire(t.Context(), controller.ReaperLease, controller.LeaseTTL)
	if err != nil || !held {
		t.Fatalf("the first holder took %v: %v", held, err)
	}
	held, err = other.Acquire(t.Context(), controller.ReaperLease, controller.LeaseTTL)
	if err != nil || held {
		t.Fatalf("a second holder took the lease: %v %v", held, err)
	}
}

// TestBridgeReportsTheStore: what the store answers about durability and
// readiness is what the controller and the probe read.
func TestBridgeReportsTheStore(t *testing.T) {
	c, s := bound(t)
	if c.Durable() {
		t.Error("the memory store is reported durable")
	}
	if c.Store() != s {
		t.Error("the bridge reports another store")
	}
	if err := c.Ready(t.Context()); err != nil {
		t.Errorf("an open store is not ready: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("closing: %v", err)
	}
	if err := c.Ready(t.Context()); err == nil {
		t.Error("a closed store reports itself ready")
	}
}

// TestBridgeReportsARowItCannotRead: a row this binary did not write is a
// read failure and not an empty sandbox, because an empty sandbox is desired
// state the controller would act on.
func TestBridgeReportsARowItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  store.Object
	}{
		{"an object that is not a sandbox", store.Object{
			Kind: store.KindSandbox, ID: "sbx_a", Owner: "alice", Name: "one",
			Environment: "default", Data: []byte(`"not a sandbox"`),
		}},
		{"a status that is not a status", store.Object{
			Kind: store.KindSandbox, ID: "sbx_b", Owner: "alice", Name: "two",
			Environment: "default", Data: []byte(`{}`), Status: []byte(`"not a status"`),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, s := bound(t)
			if err := s.Tx(t.Context(), func(tx store.Tx) error {
				_, err := tx.Desired().Put(t.Context(), tc.row, 0)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Load(); err == nil {
				t.Fatal("the row was read as a sandbox")
			}
		})
	}
}

// TestBridgeReportsAStoreFailure: a store that does not answer fails the
// controller's call rather than losing the write quietly.
func TestBridgeReportsAStoreFailure(t *testing.T) {
	c, s := bound(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	obj := sandbox("sbx_a", "work", driver.Running)
	if err := c.Write(t.Context(), obj, controller.MutationCreated); err == nil {
		t.Error("a write to a closed store was reported as done")
	}
	if err := c.Remove(t.Context(), "sbx_a", controller.MutationDeleted); err == nil {
		t.Error("a remove on a closed store was reported as done")
	}
	if err := c.Rebuild(t.Context(), "default", nil); err == nil {
		t.Error("a rebuild on a closed store was reported as done")
	}
	if _, err := c.Load(); err == nil {
		t.Error("a load from a closed store was reported as done")
	}
	if err := c.Save(map[string]v1.Sandbox{"sbx_a": obj}); err == nil {
		t.Error("a save to a closed store was reported as done")
	}
	if _, err := c.Events(t.Context(), "sbx_a", 10); err == nil {
		t.Error("a journal read from a closed store was reported as done")
	}
	if _, err := c.Acquire(t.Context(), controller.ReaperLease, controller.LeaseTTL); err == nil {
		t.Error("a lease on a closed store was reported as taken")
	}
}
