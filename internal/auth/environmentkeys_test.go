// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"latere.ai/x/cella/internal/auth"
)

// registry is the key registry the key half records into. The store's own
// registry is held to the same contract by the suite of design 010 and by its
// own test; this one records what it was asked, so the half's choices are read
// back here.
type registry struct {
	records []auth.KeyRecord
	revoked map[string][2]time.Time
	err     error
}

func (r *registry) Record(_ context.Context, k auth.KeyRecord) error {
	if r.err != nil {
		return r.err
	}
	r.records = append(r.records, k)
	return nil
}

func (r *registry) Revoke(_ context.Context, jti string, at, exp time.Time) error {
	if r.err != nil {
		return r.err
	}
	r.revoked[jti] = [2]time.Time{at, exp}
	return nil
}

func (r *registry) List(_ context.Context, environment, cursor string, limit int) ([]auth.KeyRecord, string, error) {
	if r.err != nil {
		return nil, "", r.err
	}
	var out []auth.KeyRecord
	for _, k := range r.records {
		if k.Environment == environment && k.JTI > cursor && len(out) < limit {
			out = append(out, k)
		}
	}
	return out, "", nil
}

// TestEnvironmentKeysRecordAndList: the mint records each key with the
// subject that asked and the instants the token carries, a key the registry
// could not record is not handed out, a revocation goes to the registry, which
// marks the key and refuses it in one transaction, and a listing is the
// registry's. Without a registry the mint and the revocation work as they did
// and nothing is listed.
func TestEnvironmentKeysRecordAndList(t *testing.T) {
	ctx := context.Background()
	const ttl = time.Hour
	signer := newSigner(t, key(t, 1))
	if _, err := auth.NewEnvironmentKeys(nil, nil, nil, ttl); err == nil {
		t.Error("a key half with no signer was built")
	}
	if _, err := auth.NewEnvironmentKeys(signer, nil, nil, 0); err == nil {
		t.Error("a key half whose keys expire at their mint was built")
	}

	reg := &registry{revoked: map[string][2]time.Time{}}
	revocations := newList()
	keys, err := auth.NewEnvironmentKeys(signer, revocations, reg, ttl)
	if err != nil {
		t.Fatal(err)
	}
	if keys.TTL() != ttl {
		t.Errorf("the key lifetime is %v, want %v", keys.TTL(), ttl)
	}
	token, err := keys.Mint(ctx, "eu-gpu", "https://issuer.example|ops")
	if err != nil {
		t.Fatalf("the mint failed: %v", err)
	}
	if len(reg.records) != 1 {
		t.Fatalf("the mint recorded %d keys", len(reg.records))
	}
	want := auth.KeyRecord{JTI: token.JTI, Environment: "eu-gpu", Subject: "https://issuer.example|ops",
		MintedAt: token.IssuedAt, ExpiresAt: token.ExpiresAt}
	if got := reg.records[0]; got != want || got.MintedAt.IsZero() {
		t.Errorf("the mint recorded %+v, want %+v", got, want)
	}
	listed, _, err := keys.List(ctx, "eu-gpu", "", 50)
	if err != nil || len(listed) != 1 || listed[0].JTI != token.JTI {
		t.Errorf("the list is %+v, %v, want the key just minted", listed, err)
	}

	before := time.Now()
	if err = keys.Revoke(ctx, token.JTI); err != nil {
		t.Fatalf("the revocation failed: %v", err)
	}
	marks, held := reg.revoked[token.JTI]
	switch {
	case !held:
		t.Fatal("the revocation did not reach the registry")
	case marks[0].Before(before) || marks[1].Sub(marks[0]) != ttl:
		t.Errorf("the revocation marked %v and refused until %v", marks[0], marks[1])
	case len(revocations.rows) != 0:
		t.Error("the revocation list was written beside the registry, in a second transaction")
	}
	if err = keys.Revoke(ctx, ""); err == nil {
		t.Error("a revocation with no jti was accepted")
	}

	// A key the registry could not keep is not handed out, so every key a
	// caller holds is one the listing shows.
	reg.err = errors.New("the store is unreachable")
	if token, err = keys.Mint(ctx, "eu-gpu", "https://issuer.example|ops"); err == nil || token.Value != "" {
		t.Errorf("a key the registry refused was handed out: %q, %v", token.Value, err)
	}

	// Without a registry the half mints and revokes as before, through the
	// revocation list alone, and lists nothing.
	bare, err := auth.NewEnvironmentKeys(signer, revocations, nil, ttl)
	if err != nil {
		t.Fatal(err)
	}
	token, err = bare.Mint(ctx, "eu-gpu", "https://issuer.example|ops")
	if err != nil || token.Value == "" {
		t.Fatalf("the mint without a registry failed: %v", err)
	}
	if err = bare.Revoke(ctx, token.JTI); err != nil {
		t.Fatal(err)
	}
	if _, held := revocations.rows[token.JTI]; !held {
		t.Error("a revocation without a registry did not reach the revocation list")
	}
	if listed, next, err := bare.List(ctx, "eu-gpu", "", 50); err != nil || len(listed) != 0 || next != "" {
		t.Errorf("a half with no registry listed %+v, %q, %v", listed, next, err)
	}
	unkept, err := auth.NewEnvironmentKeys(signer, nil, nil, ttl)
	if err != nil {
		t.Fatal(err)
	}
	if err = unkept.Revoke(ctx, token.JTI); err != nil {
		t.Errorf("a half with no list refused a revocation it has nowhere to write: %v", err)
	}
}
