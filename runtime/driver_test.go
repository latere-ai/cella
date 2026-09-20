// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtime_test

import (
	"errors"
	"testing"

	runtime "latere.ai/x/cella/runtime"
)

// TestFilterSelects holds the narrowing every driver's List runs through, so
// one field added to Filter reaches every driver at once and is read the same
// way by each.
func TestFilterSelects(t *testing.T) {
	yes, no := true, false
	entry := runtime.State{ID: "sbx_pool", Phase: runtime.Running, Pool: true}
	owned := runtime.State{ID: "sbx_owned", Owner: "alice", Phase: runtime.Stopped}
	for _, c := range []struct {
		name  string
		f     runtime.Filter
		state runtime.State
		want  bool
	}{
		{"empty", runtime.Filter{}, entry, true},
		{"owner matches", runtime.Filter{Owner: "alice"}, owned, true},
		{"owner excludes the entry", runtime.Filter{Owner: "alice"}, entry, false},
		{"phase matches", runtime.Filter{Phase: runtime.Running}, entry, true},
		{"phase differs", runtime.Filter{Phase: runtime.Running}, owned, false},
		{"pool true", runtime.Filter{Pool: &yes}, entry, true},
		{"pool true excludes a sandbox", runtime.Filter{Pool: &yes}, owned, false},
		{"pool false", runtime.Filter{Pool: &no}, owned, true},
		{"pool false excludes an entry", runtime.Filter{Pool: &no}, entry, false},
		{"ids", runtime.Filter{IDs: []string{"sbx_pool"}}, entry, true},
		{"ids exclude", runtime.Filter{IDs: []string{"sbx_other"}}, entry, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.f.Selects(c.state); got != c.want {
				t.Fatalf("Selects = %v, want %v", got, c.want)
			}
		})
	}
}

// TestChangeAdoption holds the exclusivity rule: an adoption writes the whole
// record, so a change that carries one and anything else is two acts in one
// call and is refused before a driver touches its engine.
func TestChangeAdoption(t *testing.T) {
	labels := map[string]string{"a": "1"}
	env := map[string]string{"A": "1"}
	lifecycle := runtime.Lifecycle{}
	adoption := &runtime.Adoption{Owner: "alice"}
	for _, c := range []struct {
		name string
		ch   runtime.Change
		want error
		got  bool
	}{
		{name: "nothing", ch: runtime.Change{}},
		{name: "a mutation", ch: runtime.Change{Labels: &labels}},
		{name: "an adoption", ch: runtime.Change{Adopt: adoption}, got: true},
		{name: "with labels", ch: runtime.Change{Adopt: adoption, Labels: &labels}, want: runtime.ErrInvalid},
		{name: "with env", ch: runtime.Change{Adopt: adoption, Env: &env}, want: runtime.ErrInvalid},
		{name: "with lifecycle", ch: runtime.Change{Adopt: adoption, Lifecycle: &lifecycle}, want: runtime.ErrInvalid},
		{name: "with a token", ch: runtime.Change{Adopt: adoption, Token: []byte("t")}, want: runtime.ErrInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.ch.Adoption()
			if !errors.Is(err, c.want) {
				t.Fatalf("Adoption error = %v, want %v", err, c.want)
			}
			if (got != nil) != c.got {
				t.Fatalf("Adoption = %v, want carried %v", got, c.got)
			}
		})
	}
}

// TestCheckPrewarm holds the other half: an entry belongs to nobody, so a
// create that asks for one and carries a caller's own half is a mistake and
// not a sandbox.
func TestCheckPrewarm(t *testing.T) {
	for _, c := range []struct {
		name string
		spec runtime.CreateSpec
		want error
	}{
		{name: "not a prewarm", spec: runtime.CreateSpec{ID: "sbx_a", Owner: "alice", Command: []string{"sh"}}},
		{name: "an entry", spec: runtime.CreateSpec{ID: "sbx_a", Image: "img", Prewarm: true, Labels: map[string]string{"shape": "a"}}},
		{name: "owner", spec: runtime.CreateSpec{Prewarm: true, Owner: "alice"}, want: runtime.ErrInvalid},
		{name: "name", spec: runtime.CreateSpec{Prewarm: true, Name: "one"}, want: runtime.ErrInvalid},
		{name: "command", spec: runtime.CreateSpec{Prewarm: true, Command: []string{"sh"}}, want: runtime.ErrInvalid},
		{name: "args", spec: runtime.CreateSpec{Prewarm: true, Args: []string{"-c"}}, want: runtime.ErrInvalid},
		{name: "token", spec: runtime.CreateSpec{Prewarm: true, Token: []byte("t")}, want: runtime.ErrInvalid},
		{name: "boundary", spec: runtime.CreateSpec{Prewarm: true, Egress: runtime.Egress{Mode: "none"}}, want: runtime.ErrInvalid},
		{name: "environment", spec: runtime.CreateSpec{Prewarm: true, Env: map[string]string{"A": "1"}}, want: runtime.ErrInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.spec.CheckPrewarm(); !errors.Is(err, c.want) {
				t.Fatalf("CheckPrewarm = %v, want %v", err, c.want)
			}
		})
	}
}

// TestEgressIsZero is what CheckPrewarm reads a boundary by: every field, so a
// door or a credential on a prewarm is caught rather than only a mode.
func TestEgressIsZero(t *testing.T) {
	if !(runtime.Egress{}).IsZero() {
		t.Fatal("the zero boundary is not zero")
	}
	for _, e := range []runtime.Egress{
		{Mode: "none"}, {AllowedHosts: []string{"example.com"}}, {DeniedHosts: []string{"example.com"}},
		{ProxyAddr: "gateway:3128"}, {ReverseAddr: "gateway:8443"}, {Credential: "cred"}, {CAPEM: "pem"},
	} {
		if e.IsZero() {
			t.Fatalf("%+v reads as the zero boundary", e)
		}
	}
}
