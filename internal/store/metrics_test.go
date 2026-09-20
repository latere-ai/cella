// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"slices"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// storeRecorder is design 017's seam under test.
type storeRecorder struct {
	mu  sync.Mutex
	ops []string
}

func (r *storeRecorder) StoreQuery(op string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, op)
}

func (r *storeRecorder) read() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ops)
}

// TestStoreOperationsAreMeasured is design 017's store histogram: one
// observation per method the controller called, under the method's own name
// and never a statement's.
func TestStoreOperationsAreMeasured(t *testing.T) {
	s, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	rec := &storeRecorder{}
	c := store.ForController(s, "default", store.Journaled).Measure(rec)

	obj := v1.Sandbox{}
	obj.Metadata.Name = "work"
	obj.Spec.Environment = "default"
	obj.Status = v1.SandboxStatus{ID: "sbx_measured", Owner: "alice", Environment: "default", Phase: driver.Running}

	if _, err := c.Load(); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(t.Context(), obj, "created"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rebuild(t.Context(), "default", []driver.State{{ID: obj.Status.ID, Phase: driver.Running}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Events(t.Context(), obj.Status.ID, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire(t.Context(), "reaper", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(t.Context(), obj.Status.ID, "deleted"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(map[string]v1.Sandbox{}); err != nil {
		t.Fatal(err)
	}

	got := rec.read()
	// Save reads the environment back before it writes, so load is observed
	// twice. The vocabulary is what the label is held to, not the count.
	want := []string{store.OpAcquire, store.OpEvents, store.OpLoad, store.OpRebuild, store.OpRemove, store.OpSave, store.OpWrite}
	for _, op := range want {
		if !slices.Contains(got, op) {
			t.Errorf("%s was not measured: %v", op, got)
		}
	}
	for _, op := range got {
		if !slices.Contains(want, op) {
			t.Errorf("the op label carries %q, which is outside the vocabulary %v", op, want)
		}
	}
}

// TestNoRecorderMeasuresNothing is the default: a store built without one
// behaves as it did before design 017, and Measure with nil keeps it.
func TestNoRecorderMeasuresNothing(t *testing.T) {
	s, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := store.ForController(s, "default", store.Journaled).Measure(nil)
	if _, err := c.Load(); err != nil {
		t.Fatal(err)
	}
}
