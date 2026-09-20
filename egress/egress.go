// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package egress compiles a sandbox's network boundary into the map its
// gateway holds. The map is the whole of what a gateway knows about a
// sandbox: which hosts it may reach, which credential authenticates it at the
// two doors, and which placeholder stands for which secret toward which
// hosts.
//
// The package computes and nothing else. It reaches manifest/v1, the
// placeholder primitives of latere.ai/x/pkg/egress/placeholder, and the
// standard library, so importing it opens no connection and reads no
// configuration, which is invariant 6 of the architecture.
//
// A value passes through: Compile is where the control plane decrypts one,
// and the map carries it to the gateway that substitutes it. Every type here
// that holds a value says so, and the rule around them is that a Map and an
// Entry travel the sync stream and reach nothing else. No response, record,
// event or log line is ever built from one.
package egress

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"latere.ai/x/pkg/egress/placeholder"

	v1 "latere.ai/x/cella/manifest/v1"
)

// PrincipalPrefix is what a sandbox's principal starts with. A gateway keys
// every map, decision and record by the principal, never by the name, because
// a name may be reused after a delete and an id never is.
const PrincipalPrefix = "sandbox:"

// Principal is the principal of a sandbox id.
func Principal(sandboxID string) string { return PrincipalPrefix + sandboxID }

// SandboxOf is the inverse of Principal, and the empty string for anything
// that is not a sandbox's principal.
func SandboxOf(principal string) string {
	id, ok := strings.CutPrefix(principal, PrincipalPrefix)
	if !ok {
		return ""
	}
	return id
}

// The encodings a secret's value takes when it replaces its placeholder. The
// scheme is the encoding and nothing more: the client writes whatever word
// goes before the value, because substitution replaces the placeholder where
// the client put it.
const (
	SchemeBearer = "bearer"
	SchemeBasic  = "basic"
	SchemeRaw    = "raw"
)

// The doors a connection arrives at.
const (
	DoorProxy   = "proxy"
	DoorReverse = "reverse"
)

// The decisions the gate takes, which are also the record's vocabulary.
const (
	// DecisionAllowed is a connection the boundary admits.
	DecisionAllowed = "allowed"
	// DecisionDenied is a connection the boundary refuses, before any dial.
	DecisionDenied = "denied"
	// DecisionUnknown is a caller the gateway holds no map for.
	DecisionUnknown = "unknown"
	// DecisionPassthrough is a host in scope with no credential bound to it,
	// tunnelled without termination, so the gateway reads none of it.
	DecisionPassthrough = "passthrough"
)

// SecretView is what the control plane knows of one mounted Secret when it
// compiles a map: which secret it is, the opaque token the sandbox holds in
// place of its value, the scope the secret's own owner set, and the value
// itself, read from the store for this compile and for nothing else.
//
// For the oauth_client_credentials kind the value is the client id and
// secret, split on the first colon by the gateway, and OAuth names the
// endpoint a token is minted at.
type SecretView struct {
	Name        string
	Kind        string
	Placeholder string
	Hosts       []string
	Ports       []int
	Inject      Inject
	Value       string
	OAuth       *OAuth
}

// OAuth is the client_credentials grant an oauth entry mints from, as the
// gateway needs it. The client id and secret are the entry's value, not
// fields here, so one field holds everything a read must never return.
type OAuth struct {
	TokenURL string `json:"tokenUrl"`
	Scope    string `json:"scope,omitempty"`
	Audience string `json:"audience,omitempty"`
}

// Inject is where in a request a secret's value replaces its placeholder:
// one header or one query parameter, never both, and the body only when the
// secret's owner opted into it.
type Inject struct {
	Header string
	Query  string
	Scheme string
	Body   bool
}

// Map is one sandbox's boundary as a gateway holds it. It travels the sync
// stream as JSON, so every field a gateway reads carries a tag; the fields
// the control plane keeps for itself do not travel.
type Map struct {
	Principal  string        `json:"principal"`
	Version    int64         `json:"version"`
	Credential string        `json:"credential"`
	Mode       v1.EgressMode `json:"mode"`
	Allow      []string      `json:"allow,omitempty"`
	Deny       []string      `json:"deny,omitempty"`
	Entries    []Entry       `json:"entries,omitempty"`
	// NotInjectable names the mounted secrets that got no entry, because
	// every host of their scope is denied to this sandbox. The controller
	// writes it into the sandbox's status so a caller learns that those
	// requests will leave unauthenticated rather than fail. A gateway has no
	// use for it, so it never goes on the wire.
	NotInjectable []string `json:"-"`
}

// Entry is one injectable secret's rule: replace this placeholder with that
// secret's value, in this place, toward these hosts and ports and nowhere
// else. Value is the one field of this package that is a credential, and it
// travels only on the sync stream, inside a map, toward a gateway that
// authenticated with the environment's own key.
type Entry struct {
	Secret      string   `json:"secret"`
	Kind        string   `json:"kind,omitempty"`
	Placeholder string   `json:"placeholder"`
	Hosts       []string `json:"hosts"`
	Ports       []int    `json:"ports,omitempty"`
	Header      string   `json:"header,omitempty"`
	Query       string   `json:"query,omitempty"`
	Scheme      string   `json:"scheme,omitempty"`
	Body        bool     `json:"body,omitempty"`
	Value       string   `json:"value,omitempty"`
	OAuth       *OAuth   `json:"oauth,omitempty"`
}

// DefaultPort is the port a secret's scope covers when it names none, and the
// port a destination is read at when the caller named none.
const DefaultPort = 443

// Errors Compile returns. Each carries the code the manifest contract's error
// table names, so the API answers a bad compile the way it answers a bad
// manifest.
var (
	// ErrNoPrincipal is a sandbox with no id. Compile runs after the
	// controller has assigned one.
	ErrNoPrincipal = errors.New("egress: the sandbox has no id to key its map by")
	// ErrNoMode is a manifest that did not go through Resolve.
	ErrNoMode = errors.New("egress: the sandbox has no egress mode; compile a resolved manifest")
	// ErrPlaceholder is a view whose placeholder is missing or is not one.
	ErrPlaceholder = errors.New("egress: a secret view carries no placeholder of the minted shape")
	// ErrSecretHostConflict is two mounted secrets whose scopes share a
	// host. Which value to substitute there has no answer, so the map is
	// refused rather than one of the two silently dropped.
	ErrSecretHostConflict = errors.New("egress: two mounted secrets scope one host")
)

// Compile turns a resolved sandbox and its mounted secrets into the map its
// gateway holds. It is pure: the same sandbox and the same views produce the
// same map, byte for byte, with no clock and no entropy read. The two random
// values a sandbox's boundary needs, its credential and one placeholder per
// mounted secret, are minted once at create by MintCredential and
// MintPlaceholder and travel in desired state, so recompiling a running
// sandbox never rotates what the sandbox already holds.
func Compile(sb v1.Sandbox, secrets []SecretView) (Map, error) {
	if sb.Status.ID == "" {
		return Map{}, ErrNoPrincipal
	}
	e := sb.Spec.Network.Egress
	if v1.EgressModeRank(e.Mode) < 0 {
		return Map{}, ErrNoMode
	}
	m := Map{
		Principal: Principal(sb.Status.ID),
		Mode:      e.Mode,
		Deny:      normalizePatterns(e.DeniedHosts),
	}
	if sb.Status.EgressState != nil {
		m.Credential = sb.Status.EgressState.Credential
		m.Version = sb.Status.EgressState.Version
	}
	allow := normalizePatterns(e.AllowedHosts)
	views := slices.Clone(secrets)
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	for _, view := range views {
		entry, ok, err := compileEntry(e.Mode, m.Deny, view)
		if err != nil {
			return Map{}, err
		}
		if !ok {
			m.NotInjectable = append(m.NotInjectable, view.Name)
			continue
		}
		// A mounted secret's hosts are reachable by construction: the
		// boundary the caller declared and the scope its owner declared are
		// one allow list. The hosted compiler refused the other reading,
		// where a credential could be bound to a host the network rule
		// blocked; here the join makes that state unreachable.
		allow = append(allow, entry.Hosts...)
		m.Entries = append(m.Entries, entry)
	}
	if err := refuseSharedHosts(m.Entries); err != nil {
		return Map{}, err
	}
	if e.Mode == v1.EgressAllowlist {
		m.Allow = dedupe(allow)
	}
	return m, nil
}

// compileEntry decides one mounted secret's fate under the boundary. A secret
// with no reachable host gets no entry and is named notInjectable, so the
// caller learns that the placeholder will leave the sandbox as an inert
// string rather than that the request failed.
func compileEntry(mode v1.EgressMode, deny []string, view SecretView) (Entry, bool, error) {
	if view.Placeholder == "" || !placeholder.Is(view.Placeholder) {
		return Entry{}, false, fmt.Errorf("%w: %s", ErrPlaceholder, view.Name)
	}
	if mode == v1.EgressNone {
		return Entry{}, false, nil
	}
	var hosts []string
	for _, host := range normalizePatterns(view.Hosts) {
		// A denied host wins over a secret's scope in every direction: a
		// deny that covers the pattern removes it, and a pattern that
		// covers a denied host would reach it, so it goes too.
		if v1.HostCovers(deny, host) || coversAny(host, deny) {
			continue
		}
		hosts = append(hosts, host)
	}
	if len(hosts) == 0 {
		return Entry{}, false, nil
	}
	ports := slices.Clone(view.Ports)
	if len(ports) == 0 {
		ports = []int{DefaultPort}
	}
	slices.Sort(ports)
	entry := Entry{
		Secret:      view.Name,
		Kind:        view.Kind,
		Placeholder: view.Placeholder,
		Hosts:       hosts,
		Ports:       slices.Compact(ports),
		Header:      view.Inject.Header,
		Query:       view.Inject.Query,
		Scheme:      view.Inject.Scheme,
		Body:        view.Inject.Body,
		Value:       view.Value,
	}
	if view.OAuth != nil {
		oauth := *view.OAuth
		entry.OAuth = &oauth
	}
	return entry, true, nil
}

// refuseSharedHosts is the first of the two defects the reference designs
// had: two secrets scoped to one host collapsed silently into whichever the
// map compiled last. Two patterns that can match one concrete host are
// refused instead, naming both secrets.
func refuseSharedHosts(entries []Entry) error {
	for i := range entries {
		for j := i + 1; j < len(entries); j++ {
			for _, a := range entries[i].Hosts {
				for _, b := range entries[j].Hosts {
					if hostsOverlap(a, b) {
						return fmt.Errorf("%w: %s and %s both scope %s", ErrSecretHostConflict, entries[i].Secret, entries[j].Secret, a)
					}
				}
			}
		}
	}
	return nil
}

// hostsOverlap reports whether two patterns can match one concrete host. It
// is the symmetric reading of the coverage rule: either pattern containing
// the other is an overlap, and nothing else is, because two wildcards that do
// not contain one another name disjoint scopes.
func hostsOverlap(a, b string) bool {
	return v1.HostPatternCovers(a, b) || v1.HostPatternCovers(b, a)
}

// coversAny reports whether pattern contains any of the patterns in set.
func coversAny(pattern string, set []string) bool {
	for _, s := range set {
		if v1.HostPatternCovers(pattern, s) {
			return true
		}
	}
	return false
}

// Admits is the reachability half of the boundary: may this sandbox open a
// connection to this host at all. It is the same question the driver's own
// network rule answers, asked of the map, so the two enforcement points
// cannot disagree about a pattern.
func (m Map) Admits(host string) bool {
	switch m.Mode {
	case v1.EgressNone:
		return false
	case v1.EgressAllowlist:
		return v1.HostMatches(m.Allow, host)
	case v1.EgressOpen:
		return !v1.HostMatches(m.Deny, host)
	default:
		return false
	}
}

// EntryFor is the injection half: which mounted secret, if any, substitutes
// toward this host and port. A host with no entry is tunnelled untouched on
// the proxy door, so a sandbox's own pinned trust keeps working there.
func (m Map) EntryFor(host string, port int) *Entry {
	for i := range m.Entries {
		e := &m.Entries[i]
		if !v1.HostMatches(e.Hosts, host) {
			continue
		}
		if len(e.Ports) > 0 && !slices.Contains(e.Ports, port) {
			continue
		}
		return e
	}
	return nil
}

// credentialBytes is the width of a gateway credential: 32 bytes, 256 bits.
const credentialBytes = 32

// credentialEncoding is unpadded base32, lowercased, so a credential is a
// single token that needs no escaping in the proxy URL's userinfo.
var credentialEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// randomBytes fills b from the operating system's source of randomness. It
// is a variable so a test can drive the branch where that source fails, which
// no supported platform reaches.
var randomBytes = func(b []byte) error {
	_, err := rand.Read(b)
	return err
}

// MintCredential returns the credential both gateway doors authenticate. It
// is minted once per sandbox at create and lives exactly as long as the
// sandbox: the workload holds it in its own environment and never re-reads
// it, so nothing the gateway checks rotates under a running process.
func MintCredential() (string, error) {
	buf := make([]byte, credentialBytes)
	if err := randomBytes(buf); err != nil {
		return "", err
	}
	return strings.ToLower(credentialEncoding.EncodeToString(buf)), nil
}

// MintPlaceholder returns a fresh opaque token for one sandbox's copy of one
// secret. It is the second defect the reference designs had: a placeholder
// derived from the secret's name is a value a neighbouring sandbox can guess
// and send, so every placeholder is minted per sandbox from 160 bits.
func MintPlaceholder() string { return placeholder.Mint() }

// IsPlaceholder reports whether a value has the minted placeholder shape. It
// is a defensive check, not a boundary: nothing decides access by it.
func IsPlaceholder(s string) bool { return placeholder.Is(s) }

// normalizePatterns lowercases, trims and drops the empties of a pattern
// list, so the map carries the same spelling the gateway matches against.
func normalizePatterns(patterns []string) []string {
	var out []string
	for _, p := range patterns {
		if n := v1.NormalizeHost(p); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// dedupe sorts and removes repeats, which makes the map's lists a set and the
// compile's output stable whatever order the caller wrote them in.
func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
