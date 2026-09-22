// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
)

// TestRetentionPrunesWhatIsDone: one prune forgets the finished records and
// the answered operations older than the window, and keeps a record still
// waiting for the sink and an operation not yet answered whatever their age.
func TestRetentionPrunesWhatIsDone(t *testing.T) {
	s, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	now := time.Now().UTC()
	old, recent := now.Add(-48*time.Hour), now.Add(-time.Hour)
	err = s.Tx(t.Context(), func(tx store.Tx) error {
		for _, e := range []store.Event{
			{ObjectID: "sbx_a", Type: "sandbox.created", At: old, AckedAt: old},
			{ObjectID: "sbx_a", Type: "sandbox.stopped", At: old, DroppedAt: old},
			{ObjectID: "sbx_a", Type: "sandbox.started", At: old},
			{ObjectID: "sbx_b", Type: "sandbox.created", At: recent, AckedAt: recent},
		} {
			if _, err := tx.Journal().Append(t.Context(), e); err != nil {
				return err
			}
		}
		for _, op := range []store.Operation{
			{ID: "op_done", Environment: "env_a", Type: "Exec", State: store.OperationDone, CreatedAt: old},
			{ID: "op_waiting", Environment: "env_a", Type: "Exec", CreatedAt: old},
			{ID: "op_fresh", Environment: "env_a", Type: "Exec", State: store.OperationDone, CreatedAt: recent},
		} {
			if err := tx.Operations().Enqueue(t.Context(), op); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	pruned, err := store.NewRetention(s, 24*time.Hour).Prune(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 3 {
		t.Fatalf("the prune forgot %d rows, want the acknowledged record, the dropped one and the answered operation", pruned)
	}
	err = s.Tx(t.Context(), func(tx store.Tx) error {
		left, _, err := tx.Journal().ByObject(t.Context(), "sbx_a", store.Page{})
		if err != nil {
			return err
		}
		if len(left) != 1 || left[0].Type != "sandbox.started" {
			t.Errorf("sbx_a keeps %+v, want the record still waiting for the sink", left)
		}
		kept, _, err := tx.Journal().ByObject(t.Context(), "sbx_b", store.Page{})
		if err != nil {
			return err
		}
		if len(kept) != 1 {
			t.Errorf("a record inside the window was pruned: %+v", kept)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRetentionReportsAFailedPrune: a store that cannot run the transaction
// is an error the tick reports, never a count of zero it could read as done.
func TestRetentionReportsAFailedPrune(t *testing.T) {
	s, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.NewRetention(s, time.Hour).Prune(t.Context(), time.Now()); err == nil {
		t.Fatal("a prune over a closed store reported nothing")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	open, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	if _, err := store.NewRetention(open, time.Hour).Prune(ctx, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled prune answered %v", err)
	}
}
