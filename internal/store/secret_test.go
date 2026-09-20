// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
	v1 "latere.ai/x/cella/manifest/v1"
)

// secret is one Secret as the controller hands it to the store: resolved,
// with the value in the field the row never keeps.
func secret(id, name string) v1.Secret {
	return v1.Secret{
		APIVersion: v1.APIVersion, Kind: v1.KindSecret,
		Metadata: v1.Metadata{Name: name, Labels: map[string]string{"team": "a"}},
		Spec: v1.SecretSpec{
			Kind:   v1.SecretStatic,
			Scope:  v1.SecretScope{Hosts: []string{"api.example.com"}, Ports: []int{443}},
			Inject: v1.SecretInject{Header: "Authorization", Scheme: v1.SchemeBearer},
		},
		Status: v1.SecretStatus{ID: id, Owner: "alice"},
	}
}

// TestSecretCollectionRoundTrips is the Secret half of the bridge: the object
// goes in without its value, the value goes in sealed, and both come back the
// way the controller wrote them.
func TestSecretCollectionRoundTrips(t *testing.T) {
	bridge, s := bound(t)
	ctx := t.Context()

	version, err := bridge.WriteSecret(ctx, secret("sec_a", "github"), []byte("ghp_canary"), controller.MutationSecretCreated)
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("the first value took version %d, want 1", version)
	}
	held, err := bridge.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := held["sec_a"]
	if !ok {
		t.Fatalf("the collection is %+v", held)
	}
	if got.Spec.Value != "" {
		t.Fatalf("the stored object carries the value: %+v", got.Spec)
	}
	if got.Status.Version != 1 || got.Status.Owner != "alice" || got.Metadata.Name != "github" {
		t.Fatalf("the stored status is %+v", got.Status)
	}
	if len(got.Spec.Scope.Hosts) != 1 || got.Spec.Inject.Scheme != v1.SchemeBearer {
		t.Fatalf("the stored spec is %+v", got.Spec)
	}

	plaintext, version, err := bridge.OpenValue(ctx, "sec_a")
	if err != nil || string(plaintext) != "ghp_canary" || version != 1 {
		t.Fatalf("the value reads back %q at %d: %v", plaintext, version, err)
	}

	// A write with no plaintext changes the object and leaves the value and
	// its version where they were.
	changed := secret("sec_a", "github")
	changed.Status.Version = version
	changed.Spec.Scope.Hosts = []string{"api.example.com", "*.example.com"}
	if version, err = bridge.WriteSecret(ctx, changed, nil, controller.MutationSecretUpdated); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("an update with no value moved the version to %d", version)
	}
	plaintext, _, err = bridge.OpenValue(ctx, "sec_a")
	if err != nil || string(plaintext) != "ghp_canary" {
		t.Fatalf("the value after an update with none: %q %v", plaintext, err)
	}

	// A write with one bumps the version the status reports.
	if version, err = bridge.WriteSecret(ctx, changed, []byte("ghp_rotated"), controller.MutationSecretUpdated); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("the rotated value took version %d, want 2", version)
	}
	held, err = bridge.LoadSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if held["sec_a"].Status.Version != 2 {
		t.Fatalf("the stored status is %+v", held["sec_a"].Status)
	}

	if err = bridge.RemoveSecret(ctx, "sec_a", controller.MutationSecretDeleted); err != nil {
		t.Fatal(err)
	}
	if _, _, err = bridge.OpenValue(ctx, "sec_a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the value after the delete: %v", err)
	}
	if held, err = bridge.LoadSecrets(); err != nil || len(held) != 0 {
		t.Fatalf("the collection after the delete is %+v: %v", held, err)
	}
	// Removing what is gone is not an error: the object is gone either way.
	if err = bridge.RemoveSecret(ctx, "sec_a", controller.MutationSecretDeleted); err != nil {
		t.Fatal(err)
	}
	_ = s
}

// TestSecretEvents is design 009's Secret row: three records with the version
// and the hosts, and never the value.
func TestSecretEvents(t *testing.T) {
	bridge, s := bound(t)
	ctx := t.Context()
	if _, err := bridge.WriteSecret(ctx, secret("sec_a", "github"), []byte("ghp_canary"), controller.MutationSecretCreated); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.WriteSecret(ctx, secret("sec_a", "github"), []byte("ghp_rotated"), controller.MutationSecretUpdated); err != nil {
		t.Fatal(err)
	}
	if err := bridge.RemoveSecret(ctx, "sec_a", controller.MutationSecretDeleted); err != nil {
		t.Fatal(err)
	}
	records := recordsOf(t, s, "sec_a")
	if len(records) != 3 {
		t.Fatalf("the journal holds %d rows for the secret", len(records))
	}
	want := []events.Type{events.TypeSecretDeleted, events.TypeSecretUpdated, events.TypeSecretCreated}
	for i, record := range records {
		if record.Type != want[i] {
			t.Fatalf("row %d is %s, want %s", i, record.Type, want[i])
		}
		if record.Object.Kind != events.KindSecret || record.Object.ID != "sec_a" {
			t.Fatalf("row %d is about %+v", i, record.Object)
		}
		if record.Object.Labels["team"] != "a" {
			t.Fatalf("row %d carries the labels %v", i, record.Object.Labels)
		}
		if strings.Contains(string(record.Data), "ghp_") {
			t.Fatalf("row %d carries a value: %s", i, record.Data)
		}
		if !strings.Contains(string(record.Data), "api.example.com") {
			t.Fatalf("row %d does not carry the hosts: %s", i, record.Data)
		}
	}
	// The create is at version 1 and the update at 2, which is what a reader
	// of the feed folds a rotation from.
	if !strings.Contains(string(records[2].Data), `"version":1`) ||
		!strings.Contains(string(records[1].Data), `"version":2`) {
		t.Fatalf("the versions are %s and %s", records[2].Data, records[1].Data)
	}
}

// TestRewrapRotatesTheKey is the key rotation of design 010 over the bridge:
// every wrapped data key moves, no value's ciphertext does, and the store
// reads under the new key when it returns.
func TestRewrapRotatesTheKey(t *testing.T) {
	bridge, _ := bound(t)
	ctx := t.Context()
	if _, err := bridge.WriteSecret(ctx, secret("sec_a", "github"), []byte("ghp_canary"), controller.MutationSecretCreated); err != nil {
		t.Fatal(err)
	}
	next := []byte("fedcba9876543210fedcba9876543210")
	n, err := bridge.Rewrap(ctx, key, next)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the rotation moved %d rows, want 1", n)
	}
	plaintext, version, err := bridge.OpenValue(ctx, "sec_a")
	if err != nil || string(plaintext) != "ghp_canary" || version != 1 {
		t.Fatalf("the value after the rotation: %q %d %v", plaintext, version, err)
	}
	// The key the rows were sealed under is no longer the key they hold.
	if _, err = bridge.Rewrap(ctx, key, next); err == nil {
		t.Fatal("a rotation from the old key was accepted after the rotation")
	}
}

// TestAStoreWithNoKeyRefusesAValue is the deployment with no key: the object
// is written and the value is refused with the one error the API turns into
// capability_unsupported, whichever store is underneath.
func TestAStoreWithNoKeyRefusesAValue(t *testing.T) {
	s, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	bridge := store.ForController(s, "default", store.Delivered)
	_, err = bridge.WriteSecret(t.Context(), secret("sec_a", "github"), []byte("ghp_canary"), controller.MutationSecretCreated)
	if !errors.Is(err, controller.ErrNoSecretKey) || !errors.Is(err, store.ErrNoSecretKey) {
		t.Fatalf("a value with no key: %v", err)
	}
}

// TestRewrapKeysRefusesAKeyThatIsNotOne holds the two inputs a rotation takes
// before it reads a single row.
func TestRewrapKeysRefusesAKeyThatIsNotOne(t *testing.T) {
	for _, tc := range []struct {
		name      string
		old, next []byte
	}{
		{"no old key", nil, key},
		{"no new key", key, nil},
		{"an old key of the wrong length", []byte("short"), key},
		{"a new key of the wrong length", key, []byte("short")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := store.RewrapKeys(tc.old, tc.next); err == nil {
				t.Fatal("the rotation was accepted")
			}
		})
	}
	old, next, err := store.RewrapKeys(key, []byte("fedcba9876543210fedcba9876543210"))
	if err != nil {
		t.Fatal(err)
	}
	wrapped, sealed, err := old.Seal([]byte("a value"))
	if err != nil {
		t.Fatal(err)
	}
	moved, err := store.RewrapKey(old, next, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(moved, wrapped) {
		t.Fatal("the rotation left the wrapped data key as it was")
	}
	// The value's own ciphertext is untouched, and the new key opens it.
	old.Adopt(next)
	plaintext, err := old.Open(moved, sealed)
	if err != nil || string(plaintext) != "a value" {
		t.Fatalf("the value after the rotation: %q %v", plaintext, err)
	}
	// A key that did not seal the row fails at the unwrap.
	if _, err = store.RewrapKey(next, old, wrapped); err == nil {
		t.Fatal("a rotation from the wrong key was accepted")
	}
	// The zero envelope wraps and adopts nothing.
	var zero store.Envelope
	if _, err = zero.Wrap([]byte("k")); !errors.Is(err, store.ErrNoSecretKey) {
		t.Fatalf("the zero envelope wrapped: %v", err)
	}
	zero.Adopt(next)
	if zero.Ready() {
		t.Fatal("the zero envelope took a key it does not hold")
	}
}

// TestSecretWriteRefusals drives the branches a failed write takes: a row
// that moved under another replica, an object with no id for the record to be
// about, and a stored row this build cannot read.
func TestSecretWriteRefusals(t *testing.T) {
	first, s := bound(t)
	second := store.ForController(s, "default", store.Delivered)
	ctx := t.Context()

	if _, err := first.WriteSecret(ctx, secret("sec_a", "github"), []byte("ghp_canary"), controller.MutationSecretCreated); err != nil {
		t.Fatal(err)
	}
	// The second replica writes at the version it never read, which is the
	// conditional write of design 010 refusing an overwrite.
	if _, err := second.WriteSecret(ctx, secret("sec_a", "github"), []byte("x"), controller.MutationSecretUpdated); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("a write at a version this replica did not read: %v", err)
	}
	// A record the sink would refuse never reaches the journal, so the write
	// it belongs to does not happen either.
	if _, err := first.WriteSecret(ctx, secret("", "nameless"), nil, controller.MutationSecretCreated); err == nil {
		t.Fatal("a secret with no id was written")
	}
	// A row whose object this build cannot read is an error and not an empty
	// collection: a caller that silently lost a secret would mount nothing
	// and report nothing.
	if err := s.Tx(ctx, func(tx store.Tx) error {
		_, err := tx.Desired().Put(ctx, store.Object{Kind: store.KindSecret, ID: "sec_bad", Owner: "alice", Name: "bad", Data: []byte("{")}, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.LoadSecrets(); err == nil {
		t.Fatal("a row that does not decode was loaded")
	}
}
