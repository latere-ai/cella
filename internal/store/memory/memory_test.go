// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory_test

import (
	"context"
	"errors"
	"testing"

	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
	"latere.ai/x/cella/internal/store/storetest"
)

// TestMemoryStore runs the whole contract of design 010 against the in-process
// adapter.
func TestMemoryStore(t *testing.T) {
	storetest.Run(t, open)
}

func open(t *testing.T, key []byte) store.Store {
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

// TestMemoryHonoursACancelledContext: a cancelled caller does not write, in
// the transaction and in the statements inside it.
func TestMemoryHonoursACancelledContext(t *testing.T) {
	s := open(t, storetest.Key)
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Tx(ctx, func(store.Tx) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("a transaction on a cancelled context: %v", err)
	}
	if err := s.Tx(t.Context(), func(tx store.Tx) error {
		if _, err := tx.Desired().Put(ctx, store.Object{Kind: store.KindSandbox, ID: "sbx_a"}, 0); !errors.Is(err, context.Canceled) {
			t.Errorf("a write on a cancelled context: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("the transaction did not commit: %v", err)
	}
}
