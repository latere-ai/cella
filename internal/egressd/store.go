// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egressd

import (
	"maps"
	"net/http"
	"slices"
	"sync"

	pkgegress "latere.ai/x/pkg/egress"

	"latere.ai/x/cella/egress"
)

// store is what one gateway knows: the boundary of every principal the
// control plane has pushed, indexed by the credential both doors
// authenticate, together with the substitution engine's own registry, which
// holds the entries that carry a value.
//
// The two live side by side rather than in one structure because they answer
// different questions at different moments: the boundary decides whether a
// connection happens at all, before any dial, and the registry decides what
// is rewritten inside one that did.
type store struct {
	mu           sync.RWMutex
	maps         map[string]egress.Map // principal to its boundary
	byCredential map[string]string     // credential to principal
	registry     *pkgegress.Registry
	// entries is one table per entry, keyed by principal and placeholder.
	// The registry answers "what may be substituted toward this host"; these
	// answer "what may be substituted in this one place", which is what makes
	// an entry's injection placement a rule on the door this role builds the
	// request on.
	entries map[string]map[string]*pkgegress.Map
	// resolvers are the token sources of the oauth entries, kept across a
	// map at a higher version so a re-push does not throw away a token that
	// is still good.
	resolvers map[string]map[string]*resolver
	// tokenClient is what a resolver mints with: the gateway's own upstream
	// path, so the operator's authority and the dial seam reach the token
	// endpoint too.
	tokenClient *http.Client
}

func newStore(tokenClient *http.Client) *store {
	return &store{
		maps:         map[string]egress.Map{},
		byCredential: map[string]string{},
		registry:     pkgegress.NewRegistry(),
		entries:      map[string]map[string]*pkgegress.Map{},
		resolvers:    map[string]map[string]*resolver{},
		tokenClient:  tokenClient,
	}
}

// Apply takes one map when its version is above the held one. It reports
// whether the map was applied; either way the caller acknowledges the version
// it now holds, so the control plane learns that a repeat was a repeat.
func (s *store) Apply(m egress.Map) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if held, ok := s.maps[m.Principal]; ok && held.Version >= m.Version {
		return false
	}
	s.setLocked(m)
	return true
}

// Replace is the snapshot: after it the gateway holds exactly these maps and
// no others, so a purge that arrived while the stream was down still lands.
func (s *store) Replace(maps []egress.Map) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for principal := range s.maps {
		s.registry.Delete(principal)
	}
	s.maps = make(map[string]egress.Map, len(maps))
	s.byCredential = make(map[string]string, len(maps))
	s.entries = make(map[string]map[string]*pkgegress.Map, len(maps))
	s.resolvers = make(map[string]map[string]*resolver, len(maps))
	for _, m := range maps {
		s.setLocked(m)
	}
}

// Remove drops one principal, which is what a purge and a delete both mean.
func (s *store) Remove(principal string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if held, ok := s.maps[principal]; ok {
		delete(s.byCredential, held.Credential)
	}
	delete(s.maps, principal)
	delete(s.entries, principal)
	delete(s.resolvers, principal)
	s.registry.Delete(principal)
}

func (s *store) setLocked(m egress.Map) {
	if held, ok := s.maps[m.Principal]; ok && held.Credential != m.Credential {
		delete(s.byCredential, held.Credential)
	}
	s.maps[m.Principal] = m
	if m.Credential != "" {
		s.byCredential[m.Credential] = m.Principal
	}
	compiled := s.substitutions(m)
	s.registry.Set(m.Principal, compiled)
	byEntry := make(map[string]*pkgegress.Map, len(compiled))
	for _, entry := range compiled {
		byEntry[string(entry.Placeholder)] = pkgegress.NewMap([]pkgegress.Entry{entry})
	}
	s.entries[m.Principal] = byEntry
	// A secret the map no longer carries takes its token source with it.
	for name := range s.resolvers[m.Principal] {
		if !slices.ContainsFunc(m.Entries, func(e egress.Entry) bool { return e.Secret == name }) {
			delete(s.resolvers[m.Principal], name)
		}
	}
}

// Principal answers which sandbox a credential belongs to. It is the whole of
// the gateway's authentication: the credential travels in the map, so nothing
// the gateway checks reaches a key set or rotates under a running process.
func (s *store) Principal(credential string) (string, bool) {
	if credential == "" {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	principal, ok := s.byCredential[credential]
	return principal, ok
}

// Map is one principal's boundary.
func (s *store) Map(principal string) (egress.Map, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.maps[principal]
	return m, ok
}

// Versions is what the gateway holds, which its hello carries so the control
// plane sees at a glance what a reconnect changed.
func (s *store) Versions() map[string]int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]int64, len(s.maps))
	for principal, m := range s.maps {
		out[principal] = m.Version
	}
	return out
}

// Principals is every principal the gateway holds, sorted.
func (s *store) Principals() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Sorted(maps.Keys(s.maps))
}

// HasSecretFor reports whether this principal has something to substitute
// toward this host, which is what decides between terminating the connection
// and tunnelling it untouched.
func (s *store) HasSecretFor(principal, host string) bool {
	s.mu.RLock()
	registry := s.registry
	s.mu.RUnlock()
	m, found := registry.Get(principal)
	return found && m.HostHasSecret(host)
}

// Registry is the substitution engine's own store, which the gateway reads on
// a terminated connection.
func (s *store) Registry() *pkgegress.Registry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.registry
}
