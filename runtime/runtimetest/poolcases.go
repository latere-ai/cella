// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtimetest

import (
	"context"
	"maps"
	"sync"
	"time"

	"latere.ai/x/cella/runtime"
)

// prewarm makes one pool entry, waits for it to run, and deletes it at
// cleanup. The spec is the whole of what a prewarm may carry: an image, the
// resources, and the labels the control plane stamps its shape with.
func prewarm(t tb, d runtime.Driver, opts Options, id string, labels map[string]string) runtime.State {
	t.Helper()
	ref, err := d.Create(context.Background(), runtime.CreateSpec{ID: id, Image: opts.Image, Labels: labels, Prewarm: true})
	must(t, err, "Create prewarmed "+id)
	t.Cleanup(func() { _ = d.Delete(context.Background(), id) })
	need(t, ref.ID == id, "Create returned id %q, want %q", ref.ID, id)
	return waitPhase(t, d, id, runtime.Running)
}

// pooled reports whether the driver declares the capability, skipping the case
// with the contract's sentence where it does not.
func pooled(t tb, d runtime.Driver) bool {
	t.Helper()
	if d.Capabilities().Pool {
		return true
	}
	return false
}

// prewarmIsNotOwned holds the shape of an entry: nobody's sandbox until it is
// adopted. The owner filter is what the control plane's own reads use, so an
// entry that answered one would be served to a caller as a sandbox it holds.
func prewarmIsNotOwned(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	ctx := context.Background()
	if !pooled(t, d) {
		entry := runtime.CreateSpec{ID: "sbx_cnf_pool_off", Image: opts.Image, Prewarm: true}
		_, err := d.Create(ctx, entry)
		wantErr(t, err, runtime.ErrUnsupported, "Create with Prewarm on a driver without Pool")
		t.Skipf("Pool is not declared")
		return
	}
	const id = "sbx_cnf_pool_entry"
	state := prewarm(t, d, opts, id, map[string]string{"shape": "a"})
	expect(t, state.Pool, "a prewarmed entry reports Pool false")
	expect(t, state.Owner == "" && state.Name == "", "a prewarmed entry carries owner %q name %q", state.Owner, state.Name)
	expect(t, !state.CreatedAt.IsZero(), "CreatedAt is zero on a prewarmed entry")

	const owner = "alice@example.com"
	expect(t, !listed(t, d, runtime.Filter{Owner: owner}, id), "an owner filter selected the prewarmed %s", id)
	yes, no := true, false
	expect(t, listed(t, d, runtime.Filter{Pool: &yes}, id), "Filter.Pool true did not select the prewarmed %s", id)
	expect(t, !listed(t, d, runtime.Filter{Pool: &no}, id), "Filter.Pool false selected the prewarmed %s", id)

	must(t, d.Update(ctx, id, runtime.Change{Adopt: &runtime.Adoption{Owner: owner, Name: "adopted"}}), "Adopt "+id)
	expect(t, listed(t, d, runtime.Filter{Owner: owner}, id), "the owner filter does not select the adopted %s", id)
	expect(t, !listed(t, d, runtime.Filter{Pool: &yes}, id), "Filter.Pool true still selects the adopted %s", id)
	after, err := d.Inspect(ctx, id)
	must(t, err, "Inspect after adoption")
	expect(t, !after.Pool, "the adopted sandbox still reports Pool")
	expect(t, after.Phase == runtime.Running, "the adopted sandbox is %q, want Running", after.Phase)
}

// listed reports whether the filter selects the id.
func listed(t tb, d runtime.Driver, f runtime.Filter, id string) bool {
	t.Helper()
	states, err := d.List(context.Background(), f)
	must(t, err, "List")
	for _, s := range states {
		if s.ID == id {
			return true
		}
	}
	return false
}

// adoptRewritesTheRecord holds the other half of the match rule: every field
// an entry could not be prewarmed with is written by the adoption, and the two
// instants the deadline rules count from move to it.
func adoptRewritesTheRecord(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	ctx := context.Background()
	if !pooled(t, d) {
		t.Skipf("Pool is not declared")
		return
	}
	const id = "sbx_cnf_pool_adopt"
	entry := prewarm(t, d, opts, id, map[string]string{"shape": "a"})
	expect(t, entry.AutoStop == 0 && entry.AutoDelete == 0 && entry.ExpiresAt.IsZero(),
		"a prewarmed entry carries a deadline: %+v", entry)
	// The instants are compared against the prewarm's, so the clock must
	// have moved between them on every driver, including one that stamps
	// whole seconds.
	time.Sleep(time.Second)

	labels := map[string]string{"team": "a"}
	adoption := runtime.Adoption{
		Owner: "alice@example.com", Name: "adopted", Labels: labels,
		Env:       map[string]string{"GREETING": "adopted"},
		Lifecycle: runtime.Lifecycle{TTL: time.Hour, AutoStop: time.Minute, AutoDelete: 2 * time.Minute},
		Token:     []byte("adopted.workload.token"),
	}
	must(t, d.Update(ctx, id, runtime.Change{Adopt: &adoption}), "Adopt "+id)
	s, err := d.Inspect(ctx, id)
	must(t, err, "Inspect after adoption")
	expect(t, s.Owner == adoption.Owner, "owner %q, want %q", s.Owner, adoption.Owner)
	expect(t, s.Name == adoption.Name, "name %q, want %q", s.Name, adoption.Name)
	expect(t, maps.Equal(s.Labels, labels), "labels %v, want %v", s.Labels, labels)
	expect(t, s.AutoStop == time.Minute && s.AutoDelete == 2*time.Minute, "lifecycle after adoption: %+v", s)
	expect(t, s.ExpiresAt.Equal(s.CreatedAt.Add(time.Hour)), "ExpiresAt %v, want CreatedAt+1h %v", s.ExpiresAt, s.CreatedAt.Add(time.Hour))
	expect(t, s.CreatedAt.After(entry.CreatedAt), "CreatedAt %v is not after the prewarm's %v", s.CreatedAt, entry.CreatedAt)
	expect(t, !s.LastActivityAt.Before(s.CreatedAt), "LastActivityAt %v is before the adoption %v", s.LastActivityAt, s.CreatedAt)
	expect(t, s.Phase == runtime.Running, "the adopted sandbox is %q, want Running", s.Phase)

	got := script(t, d, opts, id, `printf '%s' "$GREETING"`)
	expect(t, got == "adopted", "the adopted environment is %q, want %q", got, "adopted")
	got = script(t, d, opts, id, `cat "$CELLA_TOKEN_FILE"`)
	expect(t, got == string(adoption.Token), "the adopted token is %q, want %q", got, adoption.Token)
}

// prewarmAndAdoptIsExclusive is spec 004's case: two adopters, one entry, one
// winner. The loser is told the entry is gone rather than being handed a
// sandbox another caller owns.
func prewarmAndAdoptIsExclusive(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	ctx := context.Background()
	if !pooled(t, d) {
		t.Skipf("Pool is not declared")
		return
	}
	const id = "sbx_cnf_pool_race"
	prewarm(t, d, opts, id, nil)
	owners := [2]string{"alice@example.com", "bob@example.com"}
	var (
		wg   sync.WaitGroup
		errs [2]error
	)
	for i, owner := range owners {
		wg.Go(func() {
			errs[i] = d.Update(ctx, id, runtime.Change{Adopt: &runtime.Adoption{Owner: owner, Name: "adopted"}})
		})
	}
	wg.Wait()
	won := -1
	for i, err := range errs {
		if err == nil {
			need(t, won < 0, "both adoptions of %s succeeded", id)
			won = i
			continue
		}
		wantErr(t, err, runtime.ErrNotFound, "the losing adoption of "+id)
	}
	need(t, won >= 0, "neither adoption of %s succeeded: %v", id, errs)
	s, err := d.Inspect(ctx, id)
	must(t, err, "Inspect after the race")
	expect(t, s.Owner == owners[won], "the winner is %q and the record says %q", owners[won], s.Owner)
	expect(t, !s.Pool, "the raced entry still reports Pool")
}

// adoptRefusals holds the three the contract fixes: an adoption is one act, a
// workspace the entry does not have is not adopted, and a sandbox somebody
// owns is not an entry.
func adoptRefusals(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	ctx := context.Background()
	if !pooled(t, d) {
		err := d.Update(ctx, "sbx_cnf_pool_off", runtime.Change{Adopt: &runtime.Adoption{Owner: "alice"}})
		wantErr(t, err, runtime.ErrUnsupported, "Adopt on a driver without Pool")
		t.Skipf("Pool is not declared")
		return
	}
	const id = "sbx_cnf_pool_refusals"
	prewarm(t, d, opts, id, nil)
	labels := map[string]string{"a": "1"}
	err := d.Update(ctx, id, runtime.Change{Adopt: &runtime.Adoption{Owner: "alice"}, Labels: &labels})
	wantErr(t, err, runtime.ErrInvalid, "Adopt beside another change")
	err = d.Update(ctx, id, runtime.Change{Adopt: &runtime.Adoption{
		Owner: "alice", Workspace: runtime.Workspace{Path: "/elsewhere"}}})
	wantErr(t, err, runtime.ErrInvalid, "Adopt onto another workspace path")
	s, err := d.Inspect(ctx, id)
	must(t, err, "Inspect after the refusals")
	expect(t, s.Pool && s.Owner == "", "a refused adoption changed the entry: %+v", s)

	const owned = "sbx_cnf_pool_owned"
	create(t, d, opts, runtime.CreateSpec{ID: owned, Name: "owned", Owner: "alice"})
	err = d.Update(ctx, owned, runtime.Change{Adopt: &runtime.Adoption{Owner: "bob"}})
	wantErr(t, err, runtime.ErrNotFound, "Adopt of a sandbox that is not an entry")
}
