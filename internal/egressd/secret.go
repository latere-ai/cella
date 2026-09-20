// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egressd

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"slices"
	"strings"

	pkgegress "latere.ai/x/pkg/egress"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
)

// valueOf is the value the control plane sent with one entry, encoded the way
// the scheme says it is written into the request.
//
// The value travels in the map, which travels on the sync stream: the control
// plane decrypts it at compile and the gateway holds it in memory for as long
// as it holds the map. An oauth entry has no static value to write, because
// what goes into the request is a token the resolver mints, so this returns
// nothing for one and the entry takes a resolver instead.
func valueOf(e egress.Entry) []byte {
	if e.Kind == v1SecretOAuthClientCredentials || e.Value == "" {
		return nil
	}
	if e.Scheme != egress.SchemeBasic {
		return []byte(e.Value)
	}
	// basic is the encoding and nothing more: the client wrote "Basic " in
	// front of the placeholder and what replaces it is the base64 of the
	// pair. The value was held to having two halves when it was written.
	user, password, _ := strings.Cut(e.Value, ":")
	return []byte(base64.StdEncoding.EncodeToString([]byte(user + ":" + password)))
}

// v1SecretOAuthClientCredentials is the kind whose value is a grant rather
// than a credential. It is spelled here rather than imported, because this
// role reaches the substitution engine and the boundary types and nothing of
// the manifest contract.
const v1SecretOAuthClientCredentials = "oauth_client_credentials"

// resolver is one entry's minted credential and the inputs it was built from.
// The fingerprint is what decides whether a map at a higher version may keep
// the token this resolver has cached: the same endpoint, the same client and
// the same secret mint the same token, so a re-push that changed something
// else does not throw one away.
type resolver struct {
	fingerprint string
	credentials *pkgegress.OAuthClientCredentials
}

// fingerprintOf is every input a minted token depends on.
func fingerprintOf(e egress.Entry) string {
	if e.OAuth == nil {
		return ""
	}
	return strings.Join([]string{e.OAuth.TokenURL, e.OAuth.Scope, e.OAuth.Audience, e.Value}, "\x00")
}

// resolverFor is the entry's token source, reused where the inputs have not
// changed. The caller holds the store's write lock.
func (s *store) resolverFor(principal string, e egress.Entry) func(context.Context) ([]byte, error) {
	if e.OAuth == nil || e.Value == "" {
		return nil
	}
	fingerprint := fingerprintOf(e)
	held := s.resolvers[principal]
	if held == nil {
		held = map[string]*resolver{}
		s.resolvers[principal] = held
	}
	if current, ok := held[e.Secret]; ok && current.fingerprint == fingerprint {
		return current.credentials.Resolve
	}
	id, secret, _ := strings.Cut(e.Value, ":")
	next := &resolver{
		fingerprint: fingerprint,
		credentials: &pkgegress.OAuthClientCredentials{
			TokenURL:     e.OAuth.TokenURL,
			ClientID:     id,
			ClientSecret: secret,
			Scope:        e.OAuth.Scope,
			Audience:     e.OAuth.Audience,
			// The token endpoint is reached the way every other upstream
			// is, so an operator's own authority and a gateway behind
			// another proxy hold for it too.
			HTTPClient: s.tokenClient,
		},
	}
	held[e.Secret] = next
	return next.credentials.Resolve
}

// substitutions turns a map's entries into the substitution engine's, and
// takes only the entries that carry something to substitute: an entry with
// neither a value nor a token source would replace a placeholder with
// nothing, which is worse than leaving the placeholder in place, where it is
// an inert string the upstream refuses.
//
// The caller holds the store's write lock.
func (s *store) substitutions(m egress.Map) []pkgegress.Entry {
	var out []pkgegress.Entry
	for _, e := range m.Entries {
		entry := pkgegress.Entry{
			Placeholder:    []byte(e.Placeholder),
			Secret:         valueOf(e),
			AllowedHosts:   slices.Clone(e.Hosts),
			SubstituteBody: e.Body,
			Resolve:        s.resolverFor(m.Principal, e),
		}
		if len(entry.Secret) == 0 && entry.Resolve == nil {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// placed is one entry's own substitution table, which is what makes the
// injection place a rule rather than a hint: the engine replaces a
// placeholder wherever it finds it, so an entry that may write only into one
// header is applied to that header alone.
//
// The caller holds no lock.
func (s *store) placed(principal string, e egress.Entry) *pkgegress.Map {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byEntry := s.entries[principal]
	if byEntry == nil {
		return nil
	}
	return byEntry[e.Placeholder]
}

// substitutePlaced rewrites one outbound request, entry by entry, in the
// place each entry names. It is the reverse door's path, where this role
// builds the request itself and can hold every entry to its own placement; a
// placeholder anywhere else in the request goes out verbatim.
func (g *gate) substitutePlaced(ctx context.Context, principal, host string, port int, out *http.Request) error {
	m, held := g.store.Map(principal)
	if !held {
		return nil
	}
	for _, e := range m.Entries {
		if !entryAdmits(e, host, port) {
			continue
		}
		single := g.store.placed(principal, e)
		if single.Empty() {
			continue
		}
		if err := substituteInPlace(ctx, host, out, e, single); err != nil {
			return err
		}
	}
	return nil
}

// entryAdmits reports whether one entry substitutes toward this destination.
// The host rule is the contract's own, which the engine holds the same entry
// to, so the two can only agree about a pattern.
func entryAdmits(e egress.Entry, host string, port int) bool {
	if len(e.Ports) > 0 && !slices.Contains(e.Ports, port) {
		return false
	}
	return v1.HostMatches(e.Hosts, host)
}

// substituteInPlace applies one entry's table to the one header or the one
// query parameter it names, and to the body where its owner opted in.
func substituteInPlace(ctx context.Context, host string, out *http.Request, e egress.Entry, single *pkgegress.Map) error {
	switch {
	case e.Header != "":
		values := out.Header.Values(e.Header)
		for i, value := range values {
			next, changed, err := single.SubstituteValueContext(ctx, host, value)
			if err != nil {
				return err
			}
			if changed {
				values[i] = next
			}
		}
	case e.Query != "":
		query := out.URL.Query()
		values, named := query[e.Query]
		if !named {
			break
		}
		rewritten := false
		for i, value := range values {
			next, changed, err := single.SubstituteValueContext(ctx, host, value)
			if err != nil {
				return err
			}
			if changed {
				values[i], rewritten = next, true
			}
		}
		if rewritten {
			query[e.Query] = values
			out.URL.RawQuery = query.Encode()
		}
	}
	if !e.Body {
		return nil
	}
	return substituteBody(ctx, host, out, single)
}

// substituteBody runs the engine's body rule over the request's body alone.
// The scratch request carries the body and the two headers that frame it and
// nothing else, so the pass cannot reach a header or a query parameter this
// entry may not write into.
func substituteBody(ctx context.Context, host string, out *http.Request, single *pkgegress.Map) error {
	if out.Body == nil || out.ContentLength <= 0 {
		return nil
	}
	scratch := &http.Request{
		Method:        out.Method,
		URL:           &url.URL{Scheme: "https", Host: host, Path: out.URL.Path},
		Header:        http.Header{"Content-Type": {out.Header.Get("Content-Type")}},
		Body:          out.Body,
		ContentLength: out.ContentLength,
	}
	if _, err := pkgegress.SubstituteHTTPRequestContext(ctx, host, scratch, single); err != nil {
		return err
	}
	out.Body, out.ContentLength, out.GetBody = scratch.Body, scratch.ContentLength, scratch.GetBody
	if _, framed := out.Header["Content-Length"]; framed {
		out.Header.Set("Content-Length", scratch.Header.Get("Content-Length"))
	}
	return nil
}
