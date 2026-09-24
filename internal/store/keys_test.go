// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"
	"time"

	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
)

// TestKeyRegistry is the registry as the key half holds it: a record read
// back as it was written, a revocation that marks the record and refuses the
// jti in one transaction, a revocation of a jti with no record that is
// refused all the same, and the sweep of the revocation list that forgets the
// records of expired keys with the expired revocations.
func TestKeyRegistry(t *testing.T) {
	s, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	keys, revocations := store.NewKeyRegistry(s), store.NewRevocations(s)
	now := time.Now().UTC().Truncate(time.Second)

	live := auth.KeyRecord{JTI: "01JLIVE", Environment: "eu-gpu", Subject: "https://issuer.example|ops",
		MintedAt: now, ExpiresAt: now.Add(time.Hour)}
	expired := auth.KeyRecord{JTI: "01JGONE", Environment: "eu-gpu", MintedAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Minute)}
	for _, k := range []auth.KeyRecord{live, expired} {
		if err := keys.Record(t.Context(), k); err != nil {
			t.Fatal(err)
		}
	}
	listed, next, err := keys.List(t.Context(), "eu-gpu", "", 1)
	if err != nil || len(listed) != 1 || listed[0] != expired || next != expired.JTI {
		t.Fatalf("the first page is %+v with the cursor %q, %v; want the older key", listed, next, err)
	}

	if err = keys.Revoke(t.Context(), live.JTI, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if revoked, err := revocations.Revoked(t.Context(), live.JTI); err != nil || !revoked {
		t.Errorf("the revocation did not refuse the jti: %v %v", revoked, err)
	}
	listed, _, err = keys.List(t.Context(), "eu-gpu", expired.JTI, 50)
	if err != nil || len(listed) != 1 || !listed[0].RevokedAt.Equal(now) {
		t.Errorf("the revoked key reads back as %+v, %v", listed, err)
	}
	if err = keys.Revoke(t.Context(), "01JNORECORD", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if revoked, err := revocations.Revoked(t.Context(), "01JNORECORD"); err != nil || !revoked {
		t.Errorf("a jti with no record was not refused: %v %v", revoked, err)
	}

	// The reaper's sweep of the revocation list forgets the expired key's
	// record; the live key's record and both revocations outlive it.
	if _, err = revocations.Forget(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	listed, _, err = keys.List(t.Context(), "eu-gpu", "", 50)
	if err != nil || len(listed) != 1 || listed[0].JTI != live.JTI {
		t.Errorf("after the sweep the environment holds %+v, %v; want the live key alone", listed, err)
	}
}

// TestKeyRegistryReportsTheStoresFailure: a registry over a store that
// cannot answer reports it, so a mint is not handed out unrecorded and a
// revocation is not reported done.
func TestKeyRegistryReportsTheStoresFailure(t *testing.T) {
	s, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	keys := store.NewKeyRegistry(s)
	if err := keys.Record(t.Context(), auth.KeyRecord{JTI: "01J", Environment: "eu-gpu"}); err == nil {
		t.Error("a closed store recorded a key")
	}
	if err := keys.Revoke(t.Context(), "01J", time.Now(), time.Now()); err == nil {
		t.Error("a closed store accepted a revocation")
	}
	if _, _, err := keys.List(t.Context(), "eu-gpu", "", 50); err == nil {
		t.Error("a closed store listed keys")
	}
}
