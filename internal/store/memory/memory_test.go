// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
	"latere.ai/x/cella/internal/store/storetest"
	driver "latere.ai/x/cella/runtime"
)

// TestMemoryStore runs the whole contract of design 010 against the in-process
// adapter.
func TestMemoryStore(t *testing.T) {
	storetest.Run(t, open)
}

func open(t storetest.TB, key []byte) store.Store {
	t.Helper()
	s, err := memory.Open(memory.Options{Key: key})
	if err != nil {
		t.Fatalf("opening the memory store: %v", err)
	}
	return s
}

// TestMemoryIsNotDurable pins the answer the lost rule of design 005 reads:
// what the memory store holds dies with the process, so a lost sandbox is
// reaped after the grace rather than recreated.
func TestMemoryIsNotDurable(t *testing.T) {
	s := open(t, storetest.Key)
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	}()
	if s.Durable() {
		t.Error("the memory store reports itself durable")
	}
}

// TestMemoryRefusesAKeyOfTheWrongLength keeps a short key from reaching AES,
// where it would be a runtime failure on the first secret rather than a
// start-up failure.
func TestMemoryRefusesAKeyOfTheWrongLength(t *testing.T) {
	if _, err := memory.Open(memory.Options{Key: []byte("too short")}); err == nil {
		t.Fatal("a nine byte key was accepted")
	}
}

// TestMemoryRefusesAWriteAfterClose: a closed store runs no transaction, so a
// caller that outlived the shutdown fails rather than writing into a map
// nobody reads.
func TestMemoryRefusesAWriteAfterClose(t *testing.T) {
	s := open(t, nil)
	if err := s.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}
	if err := s.Tx(t.Context(), func(store.Tx) error { return nil }); !errors.Is(err, memory.ErrClosed) {
		t.Fatalf("a transaction after Close: %v, want the closed store", err)
	}
}

// TestMemoryHonorsACancelledContext: a cancelled caller does not write, in
// the transaction and in every statement inside it. The memory adapter holds
// the same rule Postgres holds for free, so a caller that gave up reads the
// same answer whichever store it opened.
func TestMemoryHonorsACancelledContext(t *testing.T) {
	s := open(t, storetest.Key)
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	}()
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Tx(gone, func(store.Tx) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("a transaction on a cancelled context: %v", err)
	}
	obj := store.Object{Kind: store.KindSandbox, ID: "sbx_a", Owner: "alice", Name: "one"}
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		for _, tc := range []struct {
			name string
			call func() error
		}{
			{"a write", func() error { _, err := tx.Desired().Put(gone, obj, 0); return err }},
			{"a read", func() error { _, err := tx.Desired().Get(gone, store.KindSandbox, "sbx_a"); return err }},
			{"a read by name", func() error { _, err := tx.Desired().ByName(gone, store.KindSandbox, "alice", "one"); return err }},
			{"a list", func() error {
				_, _, err := tx.Desired().List(gone, store.KindSandbox, store.Filter{}, store.Page{})
				return err
			}},
			{"a delete", func() error { return tx.Desired().Delete(gone, store.KindSandbox, "sbx_a") }},
			{"a count", func() error { _, err := tx.Desired().Count(gone, store.KindSandbox, "alice"); return err }},
			{"a status", func() error { return tx.Desired().PutStatus(gone, store.KindSandbox, "sbx_a", nil) }},
			{"the last applied state", func() error { _, err := tx.Desired().LastApplied(gone, "sbx_a"); return err }},
			{"a last applied write", func() error { return tx.Desired().SetLastApplied(gone, "sbx_a", nil) }},
			{"an observed write", func() error { return tx.Observed().Put(gone, "env_one", driver.State{ID: "sbx_a"}) }},
			{"an observed read", func() error { _, _, err := tx.Observed().Get(gone, "sbx_a"); return err }},
			{"an observed list", func() error { _, _, err := tx.Observed().List(gone, store.Filter{}, store.Page{}); return err }},
			{"a rebuild", func() error { return tx.Observed().Rebuild(gone, "env_one", nil) }},
			{"a journal append", func() error {
				_, err := tx.Journal().Append(gone, store.Event{ObjectID: "sbx_a", Type: "sandbox.created"})
				return err
			}},
			{"a journal read", func() error { _, _, err := tx.Journal().ByObject(gone, "sbx_a", store.Page{}); return err }},
			{"a journal prune", func() error { _, err := tx.Journal().Prune(gone, time.Now()); return err }},
			{"a value write", func() error { _, err := tx.Values().Put(gone, "sec_a", []byte("value")); return err }},
			{"a value read", func() error { _, _, err := tx.Values().Open(gone, "sec_a"); return err }},
			{"a value delete", func() error { return tx.Values().Delete(gone, "sec_a") }},
			{"a lease", func() error { _, err := tx.Leases().Acquire(gone, "reaper", "replica-one", time.Second); return err }},
			{"a lease release", func() error { return tx.Leases().Release(gone, "reaper", "replica-one") }},
		} {
			if err := tc.call(); !errors.Is(err, context.Canceled) {
				t.Errorf("%s on a cancelled context: %v", tc.name, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("the transaction did not commit: %v", err)
	}
}

// TestTheMemoryJournalKeepsARing: an object's records past the cap drop the
// oldest, delivered or not, the sequence keeps counting past what was
// dropped, and a cap of zero keeps everything.
func TestTheMemoryJournalKeepsARing(t *testing.T) {
	appendN := func(t *testing.T, s *memory.Store, object string, n int) []store.Event {
		t.Helper()
		var out []store.Event
		err := s.Tx(t.Context(), func(tx store.Tx) error {
			for range n {
				if _, err := tx.Journal().Append(t.Context(), store.Event{ObjectID: object, Type: "sandbox.exec"}); err != nil {
					return err
				}
			}
			var err error
			out, _, err = tx.Journal().ByObject(t.Context(), object, store.Page{Limit: 100})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	ring, err := memory.Open(memory.Options{JournalCap: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ring.Close() })
	kept := appendN(t, ring, "sbx_a", 5)
	if len(kept) != 3 || kept[0].Seq != 5 || kept[2].Seq != 3 {
		t.Fatalf("a ring of three over five appends keeps %+v, want sequences 5, 4, 3", kept)
	}
	if other := appendN(t, ring, "sbx_b", 1); len(other) != 1 || other[0].Seq != 1 {
		t.Fatalf("the cap is per object, and another object's journal reads %+v", other)
	}
	unbounded, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unbounded.Close() })
	if all := appendN(t, unbounded, "sbx_a", 5); len(all) != 5 {
		t.Fatalf("a cap of zero kept %d of five", len(all))
	}
}
