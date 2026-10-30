// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// countedUses is the snapshot store with its use seam counted, and failing
// on demand, so a test reads how many stamps reached the store and not only
// what the last one was.
type countedUses struct {
	*fileStore
	writes []time.Time
	fail   error
}

func (s *countedUses) WriteSecretUse(ctx context.Context, obj v1.Secret) error {
	if s.fail != nil {
		return s.fail
	}
	s.writes = append(s.writes, obj.Status.LastUsedAt)
	return s.fileStore.WriteSecretUse(ctx, obj)
}

// usedController is a controller over a counted snapshot store and a clock the
// test moves, with one secret and one sandbox that mounts it.
func usedController(t *testing.T) (*Controller, *countedUses, *fakeClock, string, string) {
	t.Helper()
	opened, err := OpenSealedFileStore(t.TempDir(), newSealer(t))
	if err != nil {
		t.Fatal(err)
	}
	store := &countedUses{fileStore: opened.(*fileStore)}
	clock := newClock()
	c := openController(t, Options{
		Store: store, Clock: clock, Egress: &gateway{},
		Driver: &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}},
	})
	ctx := t.Context()
	secret, err := c.CreateSecret(ctx, aSecret("github", "api.github.com", "ghp_canary"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	obj, err := realized(ctx, c, mounting("github", "GITHUB_TOKEN"), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	return c, store, clock, obj.Status.ID, secret.Status.ID
}

// lastUsed is the stamp a read of the secret answers.
func lastUsed(t *testing.T, c *Controller, id string) time.Time {
	t.Helper()
	got, err := c.GetSecret(t.Context(), id, "alice")
	if err != nil {
		t.Fatal(err)
	}
	return got.Status.LastUsedAt
}

// TestSecretUsedWritesOncePerResolution is the throttle: the stamp moves to a
// use at least the resolution after the one it holds and to nothing nearer,
// a late report never moves it back, a time ahead of the control plane's own
// is taken as now, and every stamp is UTC to the second.
func TestSecretUsedWritesOncePerResolution(t *testing.T) {
	c, store, clock, sandbox, secret := usedController(t)
	ctx := t.Context()
	if got := lastUsed(t, c, secret); !got.IsZero() {
		t.Fatalf("a secret never used is stamped %v", got)
	}
	clock.Advance(time.Hour)
	first := epoch.Add(time.Second + 300*time.Millisecond)
	steps := []struct {
		name   string
		at     time.Time
		stamp  time.Time
		writes int
	}{
		{"the first use", first, first.Truncate(time.Second), 1},
		{"a use inside the resolution", first.Add(v1.SecretLastUsedResolution - time.Second), first.Truncate(time.Second), 1},
		{"a use at the resolution", first.Add(v1.SecretLastUsedResolution), first.Add(v1.SecretLastUsedResolution).Truncate(time.Second), 2},
		{"a late report", first, first.Add(v1.SecretLastUsedResolution).Truncate(time.Second), 2},
		{"a use ahead of this clock", clock.Now().Add(time.Hour), clock.Now(), 3},
		{"a report with no time", time.Time{}, clock.Now(), 3},
	}
	for _, step := range steps {
		if err := c.SecretUsed(ctx, sandbox, secret, step.at); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := lastUsed(t, c, secret); !got.Equal(step.stamp) || got.Location() != time.UTC {
			t.Errorf("after %s the stamp is %v, want %v in UTC", step.name, got, step.stamp)
		}
		if len(store.writes) != step.writes {
			t.Errorf("after %s the store took %d writes, want %d", step.name, len(store.writes), step.writes)
		}
	}
	// A secret in continuous use for an hour, one request a second, is
	// written once per resolution and not once per request.
	store.writes = nil
	start := clock.Now().Add(v1.SecretLastUsedResolution)
	clock.Advance(2 * time.Hour)
	for i := range 3600 {
		if err := c.SecretUsed(ctx, sandbox, secret, start.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if want := int(time.Hour / v1.SecretLastUsedResolution); len(store.writes) != want {
		t.Errorf("an hour of requests wrote %d stamps, want %d", len(store.writes), want)
	}
}

// TestSecretUsedNeedsAMount: only a secret a sandbox of this control plane
// binds is stamped, and a failed write leaves the stamp for the next report.
func TestSecretUsedNeedsAMount(t *testing.T) {
	c, store, clock, sandbox, secret := usedController(t)
	ctx := t.Context()
	other, err := c.CreateSecret(ctx, aSecret("other", "api.other.com", "x"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		sandbox, secret string
		want            error
	}{
		"a secret the sandbox does not bind": {sandbox, other.Status.ID, ErrSecretNotMounted},
		"an unknown secret":                  {sandbox, v1.SecretIDPrefix + "absent", ErrNotFound},
		"an unknown sandbox":                 {"sbx_absent", secret, ErrNotFound},
	} {
		if err := c.SecretUsed(ctx, tc.sandbox, tc.secret, clock.Now()); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if len(store.writes) != 0 {
		t.Fatalf("a refused report wrote %v", store.writes)
	}

	store.fail = errors.New("the store is gone")
	if err := c.SecretUsed(ctx, sandbox, secret, clock.Now()); !errors.Is(err, store.fail) {
		t.Fatalf("a failed write answered %v", err)
	}
	if got := lastUsed(t, c, secret); !got.IsZero() {
		t.Fatalf("a failed write left the stamp %v", got)
	}
	store.fail = nil
	if err := c.SecretUsed(ctx, sandbox, secret, clock.Now()); err != nil || len(store.writes) != 1 {
		t.Fatalf("the report after a failed write: %v, %d writes", err, len(store.writes))
	}

	if _, err := c.DeleteSecret(ctx, secret); err != nil {
		t.Fatal(err)
	}
	clock.Advance(v1.SecretLastUsedResolution)
	if err := c.SecretUsed(ctx, sandbox, secret, clock.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted secret: %v", err)
	}
}

// TestAStoreWithoutTheSeamStampsNothing: a store that holds secrets and has no
// SecretUses serves every secret and stamps no use, and a report is not an
// error there.
func TestAStoreWithoutTheSeamStampsNothing(t *testing.T) {
	ctx := t.Context()
	opened, err := OpenSealedFileStore(t.TempDir(), newSealer(t))
	if err != nil {
		t.Fatal(err)
	}
	bare := struct {
		Store
		Secrets
	}{opened, opened.(Secrets)}
	c := openController(t, Options{
		Store: bare, Egress: &gateway{},
		Driver: &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}},
	})
	secret, err := c.CreateSecret(ctx, aSecret("github", "api.github.com", "ghp_canary"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	obj, err := realized(ctx, c, mounting("github", "GITHUB_TOKEN"), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SecretUsed(ctx, obj.Status.ID, secret.Status.ID, time.Now()); err != nil {
		t.Fatalf("a store without the seam refused the report: %v", err)
	}
	if got := lastUsed(t, c, secret.Status.ID); !got.IsZero() {
		t.Fatalf("a store without the seam stamped %v", got)
	}
}

// TestTheFileStoreKeepsASecretsLastUse: the stamp reaches the snapshot and
// survives a reopen, leaves the value and its version alone, is kept by an
// update, and is not written for a row the snapshot no longer holds.
func TestTheFileStoreKeepsASecretsLastUse(t *testing.T) {
	dir := t.TempDir()
	sealer := newSealer(t)
	ctx := t.Context()
	first, err := OpenSealedFileStore(dir, sealer)
	if err != nil {
		t.Fatal(err)
	}
	c := openController(t, Options{Store: first, Driver: &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}}, Egress: &gateway{}})
	secret, err := c.CreateSecret(ctx, aSecret("github", "api.github.com", "ghp_canary"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	obj, err := realized(ctx, c, mounting("github", "GITHUB_TOKEN"), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.SecretUsed(ctx, obj.Status.ID, secret.Status.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	stamp := lastUsed(t, c, secret.Status.ID)
	if stamp.IsZero() {
		t.Fatal("the use was not stamped")
	}
	if _, err = c.UpdateSecret(ctx, aSecret("github", "api.github.com", "ghp_rotated"), secret.Status.ID); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(t, c, secret.Status.ID); !got.Equal(stamp) {
		t.Fatalf("an update moved the stamp from %v to %v", stamp, got)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := OpenSealedFileStore(dir, sealer)
	if err != nil {
		t.Fatal(err)
	}
	next := openController(t, Options{Store: second, Driver: &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}}, Egress: &gateway{}})
	got, err := next.GetSecret(ctx, "github", "alice")
	if err != nil || !got.Status.LastUsedAt.Equal(stamp) || got.Status.Version != 2 {
		t.Fatalf("after a reopen: %+v %v", got.Status, err)
	}
	value, version, err := second.(Secrets).OpenValue(ctx, secret.Status.ID)
	if err != nil || string(value) != "ghp_rotated" || version != 2 {
		t.Fatalf("the value after a stamp and a reopen: %q %d %v", value, version, err)
	}
	gone := got
	gone.Status.ID = v1.SecretIDPrefix + "absent"
	if err = second.(SecretUses).WriteSecretUse(ctx, gone); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a stamp for a row the snapshot does not hold: %v", err)
	}
}
