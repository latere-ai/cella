// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"
	"time"

	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
)

// TestRevocationListOutsideATransaction is the shape the verifier and the
// mint hold the list by: one call, one transaction, no state change of theirs
// to commit with.
func TestRevocationListOutsideATransaction(t *testing.T) {
	s, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	list := store.NewRevocations(s)
	now := time.Now().UTC()

	if revoked, err := list.Revoked(t.Context(), "01JLIVE"); err != nil || revoked {
		t.Fatalf("Revoked before any revocation = %v %v", revoked, err)
	}
	if err := list.Revoke(t.Context(), "01JLIVE", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if revoked, err := list.Revoked(t.Context(), "01JLIVE"); err != nil || !revoked {
		t.Fatalf("Revoked after the revocation = %v %v", revoked, err)
	}
	if err := list.Revoke(t.Context(), "01JPAST", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	n, err := list.Forget(t.Context(), now)
	if err != nil || n != 1 {
		t.Fatalf("Forget = %d %v, want the one row whose exp had passed", n, err)
	}
	if revoked, err := list.Revoked(t.Context(), "01JLIVE"); err != nil || !revoked {
		t.Fatalf("the sweep dropped a live revocation: %v %v", revoked, err)
	}
}

// TestRevocationListReportsTheStoresFailure keeps a list that cannot answer
// from answering "not revoked", which would be an accepted token.
func TestRevocationListReportsTheStoresFailure(t *testing.T) {
	s, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	list := store.NewRevocations(s)
	if _, err := list.Revoked(t.Context(), "01JLIVE"); err == nil {
		t.Error("a closed store reported that the jti is not revoked")
	}
	if err := list.Revoke(t.Context(), "01JLIVE", time.Now()); err == nil {
		t.Error("a closed store accepted a revocation")
	}
	if _, err := list.Forget(t.Context(), time.Now()); err == nil {
		t.Error("a closed store reported a sweep")
	}
}
