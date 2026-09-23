// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egressd

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	pkgegress "latere.ai/x/pkg/egress"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
)

// seen is what one upstream saw of a request, which is the whole of what a
// substitution test asserts: the value arrived where it was meant to, or the
// placeholder arrived untouched.
type seen struct {
	Authorization string `json:"authorization"`
	APIKey        string `json:"apiKey"`
	Query         string `json:"query"`
	Body          string `json:"body"`
	Host          string `json:"host"`
}

// upstreamEcho answers every request with what it saw, over TLS, so a
// terminated connection's rewrite is visible and a tunneled one's is not.
func upstreamEcho(t *testing.T) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(seen{
			Authorization: r.Header.Get("Authorization"),
			APIKey:        r.Header.Get("X-Api-Key"),
			Query:         r.URL.RawQuery,
			Body:          string(body),
			Host:          r.Host,
		})
	}))
	t.Cleanup(server.Close)
	trust := x509.NewCertPool()
	trust.AddCert(server.Certificate())
	return server, trust
}

func decodeSeen(t *testing.T, body string) seen {
	t.Helper()
	var got seen
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("the upstream answered %q: %v", body, err)
	}
	return got
}

// TestValueOf is the encoding half of the scheme: basic is base64 of the
// pair, everything else is the value as it was written, and an oauth entry
// has no static value at all.
func TestValueOf(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry egress.Entry
		want  string
	}{
		{"bearer is verbatim", egress.Entry{Scheme: egress.SchemeBearer, Value: "ghp_x"}, "ghp_x"},
		{"raw is verbatim", egress.Entry{Scheme: egress.SchemeRaw, Value: "k-1"}, "k-1"},
		{"basic is the base64 of the pair", egress.Entry{Scheme: egress.SchemeBasic, Value: "user:pass"}, basic("user", "pass")},
		{"an oauth entry has none", egress.Entry{Kind: "oauth_client_credentials", Value: "id:secret"}, ""},
		{"an entry with no value has none", egress.Entry{Scheme: egress.SchemeBearer}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(valueOf(tc.entry)); got != tc.want {
				t.Fatalf("valueOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// substituting is a harness holding one map with the given entries, keyed to
// credential "c" under mode allowlist over the entries' own hosts.
func substituting(t *testing.T, upstreamHost string, trust *x509.CertPool, entries ...egress.Entry) *harness {
	t.Helper()
	h := newHarness(t, upstreamHost, "", trust)
	var allow []string
	for _, e := range entries {
		allow = append(allow, e.Hosts...)
	}
	h.store.Apply(egress.Map{
		Principal: "sandbox:sbx_a", Version: 1, Credential: "c",
		Mode: v1.EgressAllowlist, Allow: append(allow, "elsewhere.example.com"), Entries: entries,
	})
	return h
}

func bearerEntry(placeholder string, hosts ...string) egress.Entry {
	return egress.Entry{
		Secret: "github", Kind: "static", Placeholder: placeholder, Hosts: hosts,
		Ports: []int{443}, Header: "Authorization", Scheme: egress.SchemeBearer, Value: "ghp_real",
	}
}

// TestSubstitutionIsScopedAndPlaced is the security property of spec 018 as
// this slice implements it: the value replaces its placeholder toward a host
// the secret's owner named, and a placeholder sent anywhere else, or into a
// place the secret does not name, leaves as the opaque token it is.
func TestSubstitutionIsScopedAndPlaced(t *testing.T) {
	upstream, trust := upstreamEcho(t)
	placeholder := egress.MintPlaceholder()
	queryPlaceholder := egress.MintPlaceholder()
	basicPlaceholder := egress.MintPlaceholder()
	h := substituting(t, hostPort(t, upstream.URL), trust,
		bearerEntry(placeholder, "api.example.com"),
		egress.Entry{
			Secret: "search", Kind: "static", Placeholder: queryPlaceholder,
			Hosts: []string{"search.example.com"}, Ports: []int{443},
			Query: "api_key", Scheme: egress.SchemeRaw, Value: "query-value",
		},
		egress.Entry{
			Secret: "registry", Kind: "static", Placeholder: basicPlaceholder,
			Hosts: []string{"registry.example.com"}, Ports: []int{443},
			Header: "Authorization", Scheme: egress.SchemeBasic, Value: "user:pass",
		},
	)

	// The proxy door's terminated branch is pkg/egress.Gateway's own MITM,
	// which forwards through a transport this role cannot hand a dialer, so
	// a hermetic test drives the substitution the way that branch does:
	// the registry map the store compiled, over the request the engine
	// would rewrite. The routing into that branch is
	// TestAHostWithACredentialIsTerminated, and the whole path over a real
	// connection is the reverse door below.
	engine, _ := h.store.Registry().Get("sandbox:sbx_a")

	t.Run("theEngineSubstitutesTowardAnInScopeHost", func(t *testing.T) {
		req := outbound(t, "https://api.example.com/v1/things", map[string]string{"Authorization": "Bearer " + placeholder})
		if _, err := pkgegress.SubstituteHTTPRequestContext(t.Context(), "api.example.com", req, engine); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer ghp_real" {
			t.Fatalf("the request carries %q, want the value", got)
		}
	})

	t.Run("theEnginePassesThePlaceholderTowardEveryOtherHost", func(t *testing.T) {
		req := outbound(t, "https://elsewhere.example.com/v1/things", map[string]string{"Authorization": "Bearer " + placeholder})
		if _, err := pkgegress.SubstituteHTTPRequestContext(t.Context(), "elsewhere.example.com", req, engine); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer "+placeholder {
			t.Fatalf("the request carries %q, want the placeholder verbatim", got)
		}
	})

	t.Run("theBasicSchemeIsTheBase64OfThePair", func(t *testing.T) {
		req := outbound(t, "https://registry.example.com/v2/", map[string]string{"Authorization": "Basic " + basicPlaceholder})
		if _, err := pkgegress.SubstituteHTTPRequestContext(t.Context(), "registry.example.com", req, engine); err != nil {
			t.Fatal(err)
		}
		if got := req.Header.Get("Authorization"); got != "Basic "+basic("user", "pass") {
			t.Fatalf("the request carries %q", got)
		}
	})

	t.Run("aQueryParameterIsSubstituted", func(t *testing.T) {
		got := decodeSeen(t, reverseBody(t, h, "/search.example.com/v1/q?api_key="+queryPlaceholder))
		if !strings.Contains(got.Query, "api_key=query-value") {
			t.Fatalf("the upstream saw the query %q", got.Query)
		}
	})

	t.Run("theReverseDoorHoldsEachEntryToItsPlace", func(t *testing.T) {
		// The workload put the header secret's placeholder in a header its
		// owner did not name, and the query secret's placeholder in a
		// header at all. Neither is substituted.
		_, body := reverse(t, h.reverse.URL+"/api.example.com/v1/things", "c",
			map[string]string{"X-Api-Key": placeholder, "Authorization": "Bearer " + placeholder})
		got := decodeSeen(t, body)
		if got.Authorization != "Bearer ghp_real" {
			t.Fatalf("the named header saw %q, want the value", got.Authorization)
		}
		if got.APIKey != placeholder {
			t.Fatalf("an unnamed header saw %q, want the placeholder verbatim", got.APIKey)
		}
	})

	t.Run("theReverseDoorLeavesAQueryPlaceholderInAHeader", func(t *testing.T) {
		_, body := reverse(t, h.reverse.URL+"/search.example.com/v1/q", "c",
			map[string]string{"X-Api-Key": queryPlaceholder})
		got := decodeSeen(t, body)
		if got.APIKey != queryPlaceholder {
			t.Fatalf("a header saw %q, want the query secret's placeholder verbatim", got.APIKey)
		}
	})
}

// TestSubstitutionHoldsThePortScope is the other half of the scope: an entry
// substitutes toward its own ports and no others.
func TestSubstitutionHoldsThePortScope(t *testing.T) {
	upstream, trust := upstreamEcho(t)
	placeholder := egress.MintPlaceholder()
	entry := bearerEntry(placeholder, "api.example.com")
	entry.Ports = []int{8443}
	h := substituting(t, hostPort(t, upstream.URL), trust, entry)
	_, body := reverse(t, h.reverse.URL+"/api.example.com/v1/things", "c",
		map[string]string{"Authorization": "Bearer " + placeholder})
	if got := decodeSeen(t, body); got.Authorization != "Bearer "+placeholder {
		t.Fatalf("the upstream saw %q on a port the scope excludes", got.Authorization)
	}
}

// TestBodySubstitutionIsOptIn holds the body rule: an entry whose owner asked
// for it rewrites a small textual body, and one that did not never does.
func TestBodySubstitutionIsOptIn(t *testing.T) {
	upstream, trust := upstreamEcho(t)
	opted, closed := egress.MintPlaceholder(), egress.MintPlaceholder()
	inBody := bearerEntry(opted, "body.example.com")
	inBody.Body = true
	inBody.Secret = "in-body"
	header := bearerEntry(closed, "header.example.com")
	h := substituting(t, hostPort(t, upstream.URL), trust, inBody, header)

	got := decodeSeen(t, reversePost(t, h, "/body.example.com/v1/things",
		`{"token":"`+opted+`"}`, "application/json"))
	if !strings.Contains(got.Body, "ghp_real") {
		t.Fatalf("the upstream saw the body %q, want the value", got.Body)
	}
	got = decodeSeen(t, reversePost(t, h, "/header.example.com/v1/things",
		`{"token":"`+closed+`"}`, "application/json"))
	if !strings.Contains(got.Body, closed) {
		t.Fatalf("the upstream saw the body %q, want the placeholder verbatim", got.Body)
	}
	// A body the rule does not admit is never read, so a placeholder in one
	// leaves as it is.
	got = decodeSeen(t, reversePost(t, h, "/body.example.com/v1/things", opted, "application/octet-stream"))
	if got.Body != opted {
		t.Fatalf("a binary body was rewritten: %q", got.Body)
	}
}

// TestOAuthKind mints a token at a stub endpoint, substitutes it, and keeps
// the client secret on this side of the gateway. A second map at a higher
// version reuses the cached token, because nothing the token depends on
// changed.
func TestOAuthKind(t *testing.T) {
	upstream, trust := upstreamEcho(t)
	var mints atomic.Int64
	var sawSecret atomic.Bool
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		user, password, _ := r.BasicAuth()
		if user == "client-id" && password == "client-secret" {
			sawSecret.Store(true)
		}
		n := mints.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"minted-%d","expires_in":3600}`, n)
	}))
	defer tokens.Close()

	placeholder := egress.MintPlaceholder()
	entry := egress.Entry{
		Secret: "vendor", Kind: "oauth_client_credentials", Placeholder: placeholder,
		Hosts: []string{"vendor.example.com"}, Ports: []int{443},
		Header: "Authorization", Scheme: egress.SchemeBearer, Value: "client-id:client-secret",
		OAuth: &egress.OAuth{TokenURL: tokens.URL, Scope: "read"},
	}
	h := substituting(t, hostPort(t, upstream.URL), trust, entry)

	got := decodeSeen(t, reverseBody(t, h, "/vendor.example.com/v1/things?x=1",
		"Authorization", "Bearer "+placeholder))
	if got.Authorization != "Bearer minted-1" {
		t.Fatalf("the upstream saw %q, want the minted token", got.Authorization)
	}
	if !sawSecret.Load() {
		t.Fatal("the token endpoint never saw the client credentials")
	}

	// A map at a higher version whose grant did not change keeps the token
	// the resolver already holds.
	h.store.Apply(egress.Map{
		Principal: "sandbox:sbx_a", Version: 2, Credential: "c",
		Mode: v1.EgressAllowlist, Allow: entry.Hosts, Entries: []egress.Entry{entry},
	})
	got = decodeSeen(t, reverseBody(t, h, "/vendor.example.com/v1/things",
		"Authorization", "Bearer "+placeholder))
	if got.Authorization != "Bearer minted-1" || mints.Load() != 1 {
		t.Fatalf("the upstream saw %q after %d mints, want the cached token", got.Authorization, mints.Load())
	}

	// A rotated client secret is a different grant, so the next request
	// mints again.
	rotated := entry
	rotated.Value = "client-id:rotated-secret"
	h.store.Apply(egress.Map{
		Principal: "sandbox:sbx_a", Version: 3, Credential: "c",
		Mode: v1.EgressAllowlist, Allow: entry.Hosts, Entries: []egress.Entry{rotated},
	})
	got = decodeSeen(t, reverseBody(t, h, "/vendor.example.com/v1/things",
		"Authorization", "Bearer "+placeholder))
	if got.Authorization != "Bearer minted-2" {
		t.Fatalf("the upstream saw %q after a rotation, want a fresh token", got.Authorization)
	}
}

// TestASecretGoesWithItsPrincipal holds the registry's own boundary: a map
// that no longer carries an entry, and a principal that is purged, take their
// token sources and their tables with them.
func TestASecretGoesWithItsPrincipal(t *testing.T) {
	upstream, trust := upstreamEcho(t)
	placeholder := egress.MintPlaceholder()
	h := substituting(t, hostPort(t, upstream.URL), trust, bearerEntry(placeholder, "api.example.com"))
	if got := len(h.store.entries["sandbox:sbx_a"]); got != 1 {
		t.Fatalf("the store holds %d entry tables, want 1", got)
	}
	h.store.Apply(egress.Map{
		Principal: "sandbox:sbx_a", Version: 2, Credential: "c",
		Mode: v1.EgressAllowlist, Allow: []string{"api.example.com"},
	})
	if got := len(h.store.entries["sandbox:sbx_a"]); got != 0 {
		t.Fatalf("the store holds %d entry tables after the secret went, want 0", got)
	}
	// The next request toward the host leaves with the placeholder, which is
	// what a withdrawn secret means to a running workload.
	got := decodeSeen(t, reverseBody(t, h, "/api.example.com/v1/things",
		"Authorization", "Bearer "+placeholder))
	if got.Authorization != "Bearer "+placeholder {
		t.Fatalf("the upstream saw %q, want the placeholder verbatim", got.Authorization)
	}
	h.store.Remove("sandbox:sbx_a")
	if _, held := h.store.entries["sandbox:sbx_a"]; held {
		t.Fatal("a purged principal kept its entry tables")
	}
	if _, held := h.store.resolvers["sandbox:sbx_a"]; held {
		t.Fatal("a purged principal kept its token sources")
	}
	// A snapshot replaces every table, so nothing of the old world is left.
	h.store.Replace([]egress.Map{{Principal: "sandbox:sbx_b", Version: 1, Credential: "d", Mode: v1.EgressOpen}})
	if len(h.store.entries) != 1 || len(h.store.resolvers) != 0 {
		t.Fatalf("after a snapshot the store holds %d tables and %d token sources", len(h.store.entries), len(h.store.resolvers))
	}
	// A principal the gateway holds no map for substitutes nothing.
	if err := h.gate.substitutePlaced(t.Context(), "sandbox:sbx_gone", "api.example.com", 443, &http.Request{}); err != nil {
		t.Fatalf("substituting for an unknown principal: %v", err)
	}
}

// outbound is one request as the engine receives it on a terminated
// connection: the workload's own, rebuilt toward the destination.
func outbound(t *testing.T, target string, headers map[string]string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return req
}

// reverseBody sends one GET at the reverse door with optional header pairs.
func reverseBody(t *testing.T, h *harness, path string, headers ...string) string {
	t.Helper()
	set := map[string]string{}
	for i := 0; i+1 < len(headers); i += 2 {
		set[headers[i]] = headers[i+1]
	}
	_, body := reverse(t, h.reverse.URL+path, "c", set)
	return body
}

// reversePost sends one body at the reverse door.
func reversePost(t *testing.T, h *harness, path, body, contentType string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.reverse.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(egress.CredentialHeader, "c")
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = int64(len(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(answer)
}
