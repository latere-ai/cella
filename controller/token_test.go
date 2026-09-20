// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// fakeTokens is the mint and the revocation list of spec 006 as the
// controller holds them: a counter for the jti, a lifetime a case chooses,
// and the rows a revocation writes. The signer itself is internal/auth's and
// is proven there; what is proven here is when the controller asks.
type fakeTokens struct {
	clock    *fakeClock
	mu       sync.Mutex
	lifetime time.Duration
	minted   int
	sandbox  []string
	revoked  []string
	swept    []time.Time
	mintErr  error
	revErr   error
}

func newTokens(clock *fakeClock, lifetime time.Duration) *fakeTokens {
	return &fakeTokens{clock: clock, lifetime: lifetime}
}

func (f *fakeTokens) Mint(_ context.Context, obj v1.Sandbox) (string, string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mintErr != nil {
		return "", "", time.Time{}, f.mintErr
	}
	if obj.Status.ID == "" {
		return "", "", time.Time{}, errors.New("a workload token names a sandbox")
	}
	f.minted++
	f.sandbox = append(f.sandbox, obj.Status.ID)
	jti := fmt.Sprintf("jti-%d", f.minted)
	exp := f.clock.Now().Add(f.lifetime)
	// The cap of spec 006: a token never outlives the sandbox it speaks for.
	if !obj.Status.ExpiresAt.IsZero() && obj.Status.ExpiresAt.Before(exp) {
		exp = obj.Status.ExpiresAt
	}
	return "token-" + jti, jti, exp, nil
}

func (f *fakeTokens) Revoke(_ context.Context, jti string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.revErr != nil {
		return f.revErr
	}
	f.revoked = append(f.revoked, jti)
	return nil
}

func (f *fakeTokens) Forget(_ context.Context, before time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.swept = append(f.swept, before)
	return 0, nil
}

func (f *fakeTokens) read() (minted int, revoked []string, swept int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.minted, slices.Clone(f.revoked), len(f.swept)
}

func (f *fakeTokens) fail(mint, revoke error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mintErr, f.revErr = mint, revoke
}

// Update records what the controller re-projected. The fake driver's other
// methods come from the Nop, whose Update accepts everything and keeps
// nothing, which no token case could read back.
func (d *fakeDriver) Update(_ context.Context, id string, c driver.Change) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.updateErr != nil {
		return d.updateErr
	}
	s, ok := d.states[id]
	if !ok {
		return driver.ErrNotFound
	}
	if c.Labels != nil {
		s.Labels = maps.Clone(*c.Labels)
		d.states[id] = s
	}
	if len(c.Token) > 0 {
		d.projected[id] = string(c.Token)
	}
	return nil
}

// tokenOf is what the driver holds for one sandbox: what the create projected
// until an update replaces it.
func (d *fakeDriver) tokenOf(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.projected[id]
}

// withTokens is one controller over the fake mint, with the lifetime a case
// wants its tokens to have.
func withTokens(t *testing.T, lifetime time.Duration, o Options) (*Controller, *fakeDriver, *fakeClock, *fakeTokens) {
	t.Helper()
	clock := newClock()
	d := newDriver(clock)
	tokens := newTokens(clock, lifetime)
	o.Driver, o.Clock, o.Tokens = d, clock, tokens
	o.Environment, o.Log = "default", slog.New(slog.DiscardHandler)
	if o.Store == nil && o.DataDir == "" {
		o.DataDir = t.TempDir()
	}
	c, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, d, clock, tokens
}

// TestCreateMintsAndProjects is step 5 of design 005's create order: the
// sandbox is created with an identity, the driver holds it, and the control
// plane keeps the key that ends it.
func TestCreateMintsAndProjects(t *testing.T) {
	c, d, clock, tokens := withTokens(t, 24*time.Hour, Options{})
	obj := created(t, c, "work")
	id := obj.Status.ID

	if got := d.tokenOf(id); got != "token-jti-1" {
		t.Fatalf("the driver was given %q, want the minted token", got)
	}
	minted, revoked, _ := tokens.read()
	if minted != 1 || len(revoked) != 0 {
		t.Fatalf("the create minted %d and revoked %v, want one mint and no revocation", minted, revoked)
	}
	held := c.objects[id].Status.TokenState
	if held == nil || held.JTI != "jti-1" {
		t.Fatalf("the control plane holds %+v, want the jti of the token it minted", held)
	}
	if !held.IssuedAt.Equal(clock.Now()) || !held.ExpiresAt.Equal(clock.Now().Add(24*time.Hour)) {
		t.Fatalf("the record reads %+v, want the mint and the expiry", held)
	}
	// The record is the control plane's own. A caller reads the sandbox and
	// never the key by which its identity is ended.
	if obj.Status.TokenState != nil {
		t.Errorf("the create's answer carries the token record: %+v", obj.Status.TokenState)
	}
	read, err := c.Get(t.Context(), id, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if read.Status.TokenState != nil {
		t.Errorf("a read carries the token record: %+v", read.Status.TokenState)
	}
	if list := c.List(); len(list) != 1 || list[0].Status.TokenState != nil {
		t.Errorf("a list carries the token record")
	}
}

// TestCreateFailureRevokes is the undo of step 5: a sandbox that never
// started leaves no token anyone could use.
func TestCreateFailureRevokes(t *testing.T) {
	c, d, _, tokens := withTokens(t, 24*time.Hour, Options{})
	d.set(func(d *fakeDriver) { d.createErr = errors.New("the driver refused") })
	obj := workspace()
	obj.Metadata.Name = "work"
	got, err := c.Create(t.Context(), obj, "alice", 0)
	if err == nil {
		t.Fatal("the refused create was reported as a success")
	}
	minted, revoked, _ := tokens.read()
	if minted != 1 || !slices.Equal(revoked, []string{"jti-1"}) {
		t.Fatalf("the failed create minted %d and revoked %v, want the one it minted revoked", minted, revoked)
	}
	if got.Status.TokenState != nil {
		t.Errorf("the failed sandbox holds a token record: %+v", got.Status.TokenState)
	}
	if held := c.objects[got.Status.ID].Status.TokenState; held != nil {
		t.Errorf("the stored record holds a revoked token: %+v", held)
	}
}

// TestCreateRefusesWhenTheMintFails leaves no sandbox running without the
// identity its manifest was admitted with.
func TestCreateRefusesWhenTheMintFails(t *testing.T) {
	c, d, _, tokens := withTokens(t, 24*time.Hour, Options{})
	tokens.fail(errors.New("the signer is unavailable"), nil)
	obj := workspace()
	obj.Metadata.Name = "work"
	if _, err := c.Create(t.Context(), obj, "alice", 0); err == nil {
		t.Fatal("a sandbox was created without the identity it was to carry")
	}
	if len(d.order) != 0 {
		t.Fatalf("the driver was asked for %v after the mint failed", d.order)
	}
}

// TestNoTokensNoProjection is the hosted regression restated in the contract
// that replaced the hosted rule: a control plane that mints nothing creates
// sandboxes that carry nothing.
func TestNoTokensNoProjection(t *testing.T) {
	c, d, _ := newFake(t, Options{})
	obj := created(t, c, "work")
	if got := d.tokenOf(obj.Status.ID); got != "" {
		t.Fatalf("a token %q was projected by a control plane that mints none", got)
	}
	if held := c.objects[obj.Status.ID].Status.TokenState; held != nil {
		t.Fatalf("a token record %+v was written by a control plane that mints none", held)
	}
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatalf("the reaper's token rule failed with no mint behind it: %v", err)
	}
}

// TestTokenReprojection is design 005's token rule under the fake clock: at
// two thirds of the lifetime and not one instant before, the token is
// re-minted, re-projected and the one it replaced revoked, in one act.
func TestTokenReprojection(t *testing.T) {
	const lifetime = 3 * time.Hour
	c, d, clock, tokens := withTokens(t, lifetime, Options{})
	obj := created(t, c, "work")
	id := obj.Status.ID

	// One instant before the two thirds nothing happens.
	clock.Advance(2*time.Hour - time.Nanosecond)
	if acted, err := c.Reap(t.Context()); err != nil || acted != 0 {
		t.Fatalf("the rule fired before two thirds of the lifetime: acted=%d err=%v", acted, err)
	}
	if minted, _, _ := tokens.read(); minted != 1 {
		t.Fatalf("%d tokens were minted before the rule was due", minted)
	}

	clock.Advance(time.Nanosecond)
	if acted, err := c.Reap(t.Context()); err != nil || acted != 1 {
		t.Fatalf("the rule did not fire at two thirds of the lifetime: acted=%d err=%v", acted, err)
	}
	minted, revoked, _ := tokens.read()
	if minted != 2 || !slices.Equal(revoked, []string{"jti-1"}) {
		t.Fatalf("the rotation minted %d and revoked %v, want a second mint and the first revoked", minted, revoked)
	}
	if got := d.tokenOf(id); got != "token-jti-2" {
		t.Fatalf("the sandbox holds %q, want the re-minted token", got)
	}
	held := c.objects[id].Status.TokenState
	if held == nil || held.JTI != "jti-2" || !held.IssuedAt.Equal(clock.Now()) {
		t.Fatalf("the record reads %+v, want the token the sandbox now holds", held)
	}
	// The rule is not due again until two thirds of the new lifetime.
	if acted, err := c.Reap(t.Context()); err != nil || acted != 0 {
		t.Fatalf("the rule fired twice for one token: acted=%d err=%v", acted, err)
	}
}

// TestTokenRuleRunsUnderTheReaperLoop drives the rule through RunReaper, so
// the rule a tick applies is the rule the loop runs and not one a test calls
// by hand.
func TestTokenRuleRunsUnderTheReaperLoop(t *testing.T) {
	const lifetime = 3 * time.Hour
	c, d, clock, tokens := withTokens(t, lifetime, Options{})
	obj := created(t, c, "work")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go c.RunReaper(ctx)
	clock.Advance(2 * time.Hour)
	clock.ticks <- clock.Now()
	waitFor(t, "the identity to be re-minted", func() bool { return d.tokenOf(obj.Status.ID) == "token-jti-2" })
	if _, revoked, _ := tokens.read(); !slices.Equal(revoked, []string{"jti-1"}) {
		t.Fatalf("the loop revoked %v, want the token it replaced", revoked)
	}
}

// TestCappedTokenIsNotRotated: a token that already ends when its sandbox
// does is not re-minted, because the cap of spec 006 would mint the same
// expiry again and the rule would fire on every tick.
func TestCappedTokenIsNotRotated(t *testing.T) {
	c, d, clock, tokens := withTokens(t, 24*time.Hour, Options{})
	obj := workspace()
	obj.Metadata.Name = "work"
	obj.Spec.Lifecycle.TTL = "1h"
	if _, err := c.Create(t.Context(), obj, "alice", 0); err != nil {
		t.Fatal(err)
	}
	clock.Advance(59 * time.Minute)
	if acted, err := c.Reap(t.Context()); err != nil || acted != 0 {
		t.Fatalf("a token capped by its sandbox was rotated: acted=%d err=%v", acted, err)
	}
	if minted, _, _ := tokens.read(); minted != 1 {
		t.Fatalf("%d tokens were minted for a sandbox whose identity ends with it", minted)
	}
	_ = d
}

// TestRotationFailureKeepsTheOldToken: a re-projection that does not reach
// the sandbox ends the token it could not deliver, not the one the workload
// is still using.
func TestRotationFailureKeepsTheOldToken(t *testing.T) {
	c, d, clock, tokens := withTokens(t, 3*time.Hour, Options{})
	obj := created(t, c, "work")
	id := obj.Status.ID
	d.set(func(d *fakeDriver) { d.updateErr = errors.New("the driver refused") })
	clock.Advance(2 * time.Hour)

	if _, err := c.Reap(t.Context()); err == nil {
		t.Fatal("a rotation that could not be projected was reported as done")
	}
	minted, revoked, _ := tokens.read()
	if minted != 2 || !slices.Equal(revoked, []string{"jti-2"}) {
		t.Fatalf("the failed rotation minted %d and revoked %v, want the undelivered token revoked", minted, revoked)
	}
	if got := d.tokenOf(id); got != "token-jti-1" {
		t.Fatalf("the sandbox holds %q, want the token it started with", got)
	}
	if held := c.objects[id].Status.TokenState; held == nil || held.JTI != "jti-1" {
		t.Fatalf("the record reads %+v, want the token the sandbox still holds", held)
	}
}

// TestDeleteRevokes: the identity dies with the sandbox rather than with its
// own exp, which is what makes a deleted sandbox's token refused at once.
func TestDeleteRevokes(t *testing.T) {
	c, _, _, tokens := withTokens(t, 24*time.Hour, Options{})
	obj := created(t, c, "work")
	if _, err := c.Act(t.Context(), obj.Status.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	if _, revoked, _ := tokens.read(); !slices.Equal(revoked, []string{"jti-1"}) {
		t.Fatalf("the delete revoked %v, want the sandbox's own token", revoked)
	}
}

// TestReapedSandboxRevokes: the rules that end a sandbox end its identity
// too, whichever of them fired.
func TestReapedSandboxRevokes(t *testing.T) {
	c, _, clock, tokens := withTokens(t, 24*time.Hour, Options{})
	obj := workspace()
	obj.Metadata.Name = "work"
	obj.Spec.Lifecycle.TTL = "1h"
	if _, err := c.Create(t.Context(), obj, "alice", 0); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	if acted, err := c.Reap(t.Context()); err != nil || acted != 1 {
		t.Fatalf("the expired sandbox was not reaped: acted=%d err=%v", acted, err)
	}
	if _, revoked, _ := tokens.read(); !slices.Equal(revoked, []string{"jti-1"}) {
		t.Fatalf("the reaped sandbox left %v revoked, want its own token", revoked)
	}
}

// TestSweepRunsOnTheTick: the revocation list forgets a row once no token
// could present it, on the same tick that applies the rules.
func TestSweepRunsOnTheTick(t *testing.T) {
	c, _, clock, tokens := withTokens(t, 24*time.Hour, Options{})
	created(t, c, "work")
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, _, swept := tokens.read()
	if swept != 1 {
		t.Fatalf("the tick swept %d times, want once", swept)
	}
	tokens.mu.Lock()
	at := tokens.swept[0]
	tokens.mu.Unlock()
	if !at.Equal(clock.Now()) {
		t.Errorf("the sweep read %v, want the tick's own instant %v", at, clock.Now())
	}
}

// TestRecoveryMintsAndRevokes: a recovered sandbox carries a new identity and
// the one the lost sandbox held is ended, so a copy still running somewhere
// speaks for nobody.
func TestRecoveryMintsAndRevokes(t *testing.T) {
	st := newDurable(true)
	c, d, _, tokens := withTokens(t, 24*time.Hour, Options{Store: st})
	obj := created(t, c, "work")
	id := obj.Status.ID
	vanish(d, id)

	if acted, err := c.Reap(t.Context()); err != nil || acted != 1 {
		t.Fatalf("the lost sandbox was not recovered: acted=%d err=%v", acted, err)
	}
	minted, revoked, _ := tokens.read()
	if minted != 2 || !slices.Equal(revoked, []string{"jti-1"}) {
		t.Fatalf("the recovery minted %d and revoked %v, want a new token and the lost one revoked", minted, revoked)
	}
	if got := d.tokenOf(id); got != "token-jti-2" {
		t.Fatalf("the recreated sandbox holds %q, want the newly minted token", got)
	}
	if held := c.objects[id].Status.TokenState; held == nil || held.JTI != "jti-2" {
		t.Fatalf("the recovered record reads %+v, want the newly minted token", held)
	}
}

// TestRecoveryAdoptionReprojectsBeforeRevoking: an adopted sandbox is running
// with the token it was created with, so the new one reaches it before the
// old one is ended and the workload never holds a revoked token.
func TestRecoveryAdoptionReprojectsBeforeRevoking(t *testing.T) {
	clock := newClock()
	fake := newDriver(clock)
	tokens := newTokens(clock, 24*time.Hour)
	armed := false
	c := withDriver(t, newDurable(true), adopting{fakeDriver: fake, armed: &armed}, clock,
		Options{Tokens: tokens})
	obj := created(t, c, "work")
	id := obj.Status.ID
	vanish(fake, id)
	armed = true

	if acted, err := c.Reap(t.Context()); err != nil || acted != 1 {
		t.Fatalf("the adopted sandbox was not recovered: acted=%d err=%v", acted, err)
	}
	if got := fake.tokenOf(id); got != "token-jti-2" {
		t.Fatalf("the adopted sandbox holds %q, want the re-minted token", got)
	}
	if _, revoked, _ := tokens.read(); !slices.Equal(revoked, []string{"jti-1"}) {
		t.Fatalf("the adoption revoked %v, want the token it replaced", revoked)
	}
}

// TestDueForRotation is the rule's own table: the fraction, the instant it
// falls on, and every shape of record that is not due at all.
func TestDueForRotation(t *testing.T) {
	now := epoch.Add(4 * time.Hour)
	state := func(issued, expires time.Time) *v1.TokenState {
		return &v1.TokenState{JTI: "jti-1", IssuedAt: issued, ExpiresAt: expires}
	}
	for _, tc := range []struct {
		name    string
		state   *v1.TokenState
		expires time.Time
		want    bool
	}{
		{"noRecordIsNeverDue", nil, time.Time{}, false},
		{"atTwoThirds", state(now.Add(-2*time.Hour), now.Add(time.Hour)), time.Time{}, true},
		{"beforeTwoThirds", state(now.Add(-2*time.Hour+time.Second), now.Add(time.Hour)), time.Time{}, false},
		{"pastTheExpiry", state(now.Add(-3*time.Hour), now.Add(-time.Second)), time.Time{}, true},
		{"cappedBySandboxExpiry", state(now.Add(-2*time.Hour), now.Add(time.Hour)), now.Add(time.Hour), false},
		{"capLaterThanTheToken", state(now.Add(-2*time.Hour), now.Add(time.Hour)), now.Add(2 * time.Hour), true},
		{"noExpiryIsNeverDue", state(now.Add(-time.Hour), time.Time{}), time.Time{}, false},
		{"expiryBeforeTheMint", state(now, now.Add(-time.Hour)), time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dueForRotation(tc.state, tc.expires, now); got != tc.want {
				t.Errorf("dueForRotation = %v, want %v", got, tc.want)
			}
		})
	}
}
