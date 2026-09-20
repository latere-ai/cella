// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// prewarmed makes one pool entry on the fake engine.
func prewarmed(t *testing.T, d *Driver, id string) {
	t.Helper()
	create(t, d, driver.CreateSpec{ID: id, Image: "img", Prewarm: true, Labels: map[string]string{"shape": "a"}})
}

// TestPoolEntryIsUnowned is the shape spec 020 fixes: an entry belongs to
// nobody, the record carries the pool stamp, and the container runs the idle
// process the driver gives a sandbox with no command.
func TestPoolEntryIsUnowned(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	prewarmed(t, d, "sbx_pool")
	state, err := d.Inspect(t.Context(), "sbx_pool")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Pool || state.Owner != "" || state.Name != "" {
		t.Fatalf("the entry is %+v, want an unowned pool entry", state)
	}
	yes, no := true, false
	if got := ids(t, d, driver.Filter{Pool: &yes}); len(got) != 1 || got[0] != "sbx_pool" {
		t.Errorf("Filter.Pool true selects %v, want the entry", got)
	}
	if got := ids(t, d, driver.Filter{Pool: &no}); len(got) != 0 {
		t.Errorf("Filter.Pool false selects %v, want nothing", got)
	}
	if got := f.container(t, "sbx_pool").command; len(got) == 0 {
		t.Errorf("the entry's container runs %v, want the driver's idle process", got)
	}
}

// ids lists the sandboxes the filter selects.
func ids(t *testing.T, d *Driver, f driver.Filter) []string {
	t.Helper()
	states, err := d.List(t.Context(), f)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range states {
		out = append(out, s.ID)
	}
	return out
}

// TestAdoptionWritesTheRecordAndProjects holds the whole of one adoption: the
// record carries the caller's half, the clocks move to the adoption, and the
// two files the sandbox holds inside itself are written into the container
// that was already running.
func TestAdoptionWritesTheRecordAndProjects(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	prewarmed(t, d, "sbx_pool")
	entry, err := d.Inspect(t.Context(), "sbx_pool")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second + 10*time.Millisecond) // the engine's clock is whole seconds
	adoption := driver.Adoption{
		Owner: "alice", Name: "adopted", Labels: map[string]string{"team": "a"},
		Env:       map[string]string{"GREETING": "hello"},
		Lifecycle: driver.Lifecycle{TTL: time.Hour, AutoStop: time.Minute},
		Token:     []byte("workload"),
		Egress:    driver.Egress{CAPEM: "-----BEGIN CERTIFICATE-----", ProxyAddr: "gateway:3128", Credential: "cred"},
	}
	if err := d.Update(t.Context(), "sbx_pool", driver.Change{Adopt: &adoption}); err != nil {
		t.Fatal(err)
	}
	state, err := d.Inspect(t.Context(), "sbx_pool")
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case state.Pool:
		t.Error("the adopted sandbox still reports Pool")
	case state.Owner != "alice" || state.Name != "adopted":
		t.Errorf("the adopted sandbox is %q/%q, want alice/adopted", state.Owner, state.Name)
	case state.Labels["team"] != "a":
		t.Errorf("the adopted labels are %v", state.Labels)
	case !state.CreatedAt.After(entry.CreatedAt):
		t.Errorf("CreatedAt %v is not after the prewarm's %v", state.CreatedAt, entry.CreatedAt)
	case !state.ExpiresAt.Equal(state.CreatedAt.Add(time.Hour)):
		t.Errorf("ExpiresAt %v, want the adoption plus the ttl %v", state.ExpiresAt, state.CreatedAt.Add(time.Hour))
	case state.AutoStop != time.Minute:
		t.Errorf("AutoStop %v, want a minute", state.AutoStop)
	}
	c := f.container(t, "sbx_pool")
	if got := string(c.files[driver.TokenPath].body); got != "workload" {
		t.Errorf("the adopted token is %q, want %q", got, "workload")
	}
	if len(c.files) < 2 {
		t.Errorf("the adopted container holds %v, want the token and the gateway's authority", c.files)
	}
	// The record's environment is what the next Exec carries, which is how
	// both container drivers hand a running sandbox a new environment.
	records, err := d.records(t.Context(), "sbx_pool")
	if err != nil {
		t.Fatal(err)
	}
	if got := records[0].env["GREETING"]; got != "hello" {
		t.Errorf("the adopted environment holds GREETING=%q, want hello", got)
	}
	if got := records[0].env[driver.TokenFileEnv]; got != driver.TokenPath {
		t.Errorf("the adopted environment names the token at %q, want %q", got, driver.TokenPath)
	}
}

// TestAdoptionIsExclusiveAcrossDrivers is the compare-and-swap. Two driver
// instances over one engine hold no lock in common, so the record generation
// is the whole of the exclusivity: one writes it and the other is told the
// entry is gone.
func TestAdoptionIsExclusiveAcrossDrivers(t *testing.T) {
	f := newFake(t)
	first := f.driver(t)
	second := f.driver(t)
	prewarmed(t, first, "sbx_pool")
	var (
		wg   sync.WaitGroup
		errs [2]error
	)
	for i, d := range []*Driver{first, second} {
		wg.Go(func() {
			errs[i] = d.Update(t.Context(), "sbx_pool", driver.Change{
				Adopt: &driver.Adoption{Owner: []string{"alice", "bob"}[i], Name: "adopted"}})
		})
	}
	wg.Wait()
	won := -1
	for i, err := range errs {
		switch {
		case err == nil && won >= 0:
			t.Fatalf("both adoptions won: %v", errs)
		case err == nil:
			won = i
		case !errors.Is(err, driver.ErrNotFound):
			t.Fatalf("the losing adoption is %v, want ErrNotFound", err)
		}
	}
	if won < 0 {
		t.Fatalf("neither adoption won: %v", errs)
	}
	state, err := first.Inspect(t.Context(), "sbx_pool")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alice", "bob"}[won]; state.Owner != want {
		t.Errorf("the record says %q, want the winner %q", state.Owner, want)
	}
}

// TestAdoptionRefusals covers the three the contract names and the fourth the
// engine imposes: a sandbox that is not an entry, a workspace the entry does
// not have, an adoption beside another change, and an id that is not one.
func TestAdoptionRefusals(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_owned", Image: "img", Owner: "alice", Name: "owned"})
	prewarmed(t, d, "sbx_pool")
	labels := map[string]string{"a": "1"}
	for _, c := range []struct {
		name string
		id   string
		want error
		ch   driver.Change
	}{
		{"owned", "sbx_owned", driver.ErrNotFound, driver.Change{Adopt: &driver.Adoption{Owner: "bob"}}},
		{"absent", "sbx_missing", driver.ErrNotFound, driver.Change{Adopt: &driver.Adoption{Owner: "bob"}}},
		{"id", "not a sandbox id", driver.ErrInvalid, driver.Change{Adopt: &driver.Adoption{Owner: "bob"}}},
		{"workspace", "sbx_pool", driver.ErrInvalid, driver.Change{Adopt: &driver.Adoption{Owner: "bob", Workspace: driver.Workspace{Path: "/elsewhere"}}}},
		{"lifecycle", "sbx_pool", driver.ErrInvalid, driver.Change{Adopt: &driver.Adoption{Owner: "bob", Lifecycle: driver.Lifecycle{TTL: -time.Hour}}}},
		{"mixed", "sbx_pool", driver.ErrInvalid, driver.Change{Adopt: &driver.Adoption{Owner: "bob"}, Labels: &labels}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := d.Update(t.Context(), c.id, c.ch); !errors.Is(err, c.want) {
				t.Fatalf("Update = %v, want %v", err, c.want)
			}
		})
	}
	state, err := d.Inspect(t.Context(), "sbx_pool")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Pool || state.Owner != "" {
		t.Errorf("a refused adoption changed the entry: %+v", state)
	}
}

// TestPrewarmRefusesACallersCreate holds the other half of the contract check:
// a prewarm that carries an owner, a command or an identity is a caller's
// create with the flag set by mistake.
func TestPrewarmRefusesACallersCreate(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	for _, c := range []struct {
		name string
		spec driver.CreateSpec
	}{
		{"owner", driver.CreateSpec{ID: "sbx_x", Image: "img", Prewarm: true, Owner: "alice"}},
		{"command", driver.CreateSpec{ID: "sbx_x", Image: "img", Prewarm: true, Command: []string{"sh"}}},
		{"token", driver.CreateSpec{ID: "sbx_x", Image: "img", Prewarm: true, Token: []byte("t")}},
		{"boundary", driver.CreateSpec{ID: "sbx_x", Image: "img", Prewarm: true, Egress: driver.Egress{Mode: "allowlist"}}},
		{"env", driver.CreateSpec{ID: "sbx_x", Image: "img", Prewarm: true, Env: map[string]string{"A": "1"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := d.Create(t.Context(), c.spec); !errors.Is(err, driver.ErrInvalid) {
				t.Fatalf("Create = %v, want ErrInvalid", err)
			}
		})
	}
}

// TestAdoptionDiscardsTheEntryWhenTheProjectionFails is the claim-then-project
// order's consequence. The entry is already claimed when the token cannot be
// written, so it is deleted rather than returned to the pool carrying half of
// one caller's identity.
func TestAdoptionDiscardsTheEntryWhenTheProjectionFails(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	prewarmed(t, d, "sbx_pool")
	f.fault("PUT "+compat+"/containers/"+containerName("sbx_pool")+"/archive", http.StatusInternalServerError)
	err := d.Update(t.Context(), "sbx_pool", driver.Change{
		Adopt: &driver.Adoption{Owner: "alice", Name: "adopted", Token: []byte("workload")}})
	if err == nil {
		t.Fatal("the adoption reported success with no token projected")
	}
	if _, err := d.Inspect(t.Context(), "sbx_pool"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("the half-adopted entry is still there: %v", err)
	}
}
