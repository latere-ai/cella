// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egressd

import (
	"slices"
	"testing"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
)

// boundary is one sandbox's map at a version, with its own credential.
func boundary(id string, version int64, mode v1.EgressMode, hosts ...string) egress.Map {
	m := egress.Map{
		Principal:  egress.Principal(id),
		Version:    version,
		Credential: "credential-" + id,
		Mode:       mode,
	}
	switch mode {
	case v1.EgressAllowlist:
		m.Allow = hosts
	case v1.EgressOpen:
		m.Deny = hosts
	}
	return m
}

// TestApplyTakesOnlyAHigherVersion is what makes a put idempotent: a repeat
// is acknowledged and changes nothing, and an older map never overwrites a
// newer one after a reconnect crossed with a push.
func TestApplyTakesOnlyAHigherVersion(t *testing.T) {
	s := newStore()
	if !s.Apply(boundary("sbx_a", 2, v1.EgressAllowlist, "api.example.com")) {
		t.Fatal("the first map was not applied")
	}
	for _, version := range []int64{1, 2} {
		if s.Apply(boundary("sbx_a", version, v1.EgressAllowlist, "widened.example.com")) {
			t.Fatalf("a map at version %d was applied over version 2", version)
		}
	}
	m, _ := s.Map(egress.Principal("sbx_a"))
	if !slices.Equal(m.Allow, []string{"api.example.com"}) {
		t.Fatalf("allow = %v, want the version the gateway holds", m.Allow)
	}
	if !s.Apply(boundary("sbx_a", 3, v1.EgressAllowlist, "next.example.com")) {
		t.Fatal("a higher version was not applied")
	}
	if m, _ = s.Map(egress.Principal("sbx_a")); !slices.Equal(m.Allow, []string{"next.example.com"}) {
		t.Fatalf("allow = %v, want the higher version", m.Allow)
	}
}

// TestSnapshotIsAuthoritative is what makes a reconnect lossless: whatever
// the gateway held, after a snapshot it holds exactly the snapshot, so a
// purge that arrived while the stream was down still lands.
func TestSnapshotIsAuthoritative(t *testing.T) {
	s := newStore()
	s.Apply(boundary("sbx_a", 1, v1.EgressAllowlist, "a.example.com"))
	s.Apply(boundary("sbx_gone", 1, v1.EgressAllowlist, "b.example.com"))
	s.Replace([]egress.Map{boundary("sbx_a", 2, v1.EgressAllowlist, "a.example.com"), boundary("sbx_new", 1, v1.EgressOpen)})
	if got := s.Principals(); !slices.Equal(got, []string{egress.Principal("sbx_a"), egress.Principal("sbx_new")}) {
		t.Fatalf("principals = %v, want exactly the snapshot's", got)
	}
	if _, ok := s.Principal("credential-sbx_gone"); ok {
		t.Fatal("the credential of a principal the snapshot dropped still admits")
	}
	if versions := s.Versions(); versions[egress.Principal("sbx_a")] != 2 {
		t.Fatalf("versions = %v, want the snapshot's", versions)
	}
}

func TestRemoveDropsThePrincipalAndItsCredential(t *testing.T) {
	s := newStore()
	s.Apply(boundary("sbx_a", 1, v1.EgressAllowlist, "a.example.com"))
	s.Remove(egress.Principal("sbx_a"))
	if _, ok := s.Map(egress.Principal("sbx_a")); ok {
		t.Fatal("the map survived the purge")
	}
	if _, ok := s.Principal("credential-sbx_a"); ok {
		t.Fatal("the credential survived the purge")
	}
	// Removing what is not there is what a purge crossing a delete looks
	// like, and is not an error.
	s.Remove(egress.Principal("sbx_never"))
}

// TestACredentialThatChangedStopsAdmitting holds the index to the map: a
// sandbox whose map arrives with another credential is reachable by the new
// one and by nothing else.
func TestACredentialThatChangedStopsAdmitting(t *testing.T) {
	s := newStore()
	s.Apply(boundary("sbx_a", 1, v1.EgressAllowlist, "a.example.com"))
	next := boundary("sbx_a", 2, v1.EgressAllowlist, "a.example.com")
	next.Credential = "rotated"
	s.Apply(next)
	if _, ok := s.Principal("credential-sbx_a"); ok {
		t.Fatal("the old credential still admits")
	}
	principal, ok := s.Principal("rotated")
	if !ok || principal != egress.Principal("sbx_a") {
		t.Fatalf("the new credential answers %q, %v", principal, ok)
	}
	if _, ok = s.Principal(""); ok {
		t.Fatal("the empty credential admits")
	}
}

// TestNoEntryCarriesAValueYet is the seam the Secret kind fills. Until it
// does, a placeholder leaves the sandbox as the opaque token it is rather
// than being replaced with nothing, which would break the request in a way
// the caller could not read.
func TestNoEntryCarriesAValueYet(t *testing.T) {
	s := newStore()
	m := boundary("sbx_a", 1, v1.EgressAllowlist, "api.example.com")
	m.Entries = []egress.Entry{{
		Secret: "token", Placeholder: egress.MintPlaceholder(),
		Hosts: []string{"api.example.com"}, Header: "Authorization",
	}}
	s.Apply(m)
	if s.HasSecretFor(m.Principal, "api.example.com") {
		t.Fatal("an entry with no value was registered for substitution")
	}
	registry, _ := s.Registry().Get(m.Principal)
	if !registry.Empty() {
		t.Fatal("the substitution engine holds an entry that substitutes nothing")
	}
}
