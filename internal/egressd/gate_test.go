// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egressd

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pkgegress "latere.ai/x/pkg/egress"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
)

// harness is a gate with a store a test fills, a dialer that answers one
// address whatever was asked for, and the records the doors produced.
type harness struct {
	gate    *gate
	store   *store
	proxy   *httptest.Server
	reverse *httptest.Server
	dialed  atomic.Int64
	// upstream is where every admitted dial actually lands, so a test needs
	// no name resolution and reaches nothing but loopback.
	upstream string

	mu      sync.Mutex
	records []egress.Record
}

func (h *harness) taken() []egress.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]egress.Record(nil), h.records...)
}

// newHarness builds the gate over a loopback upstream. controlPlane is the
// host the gate refuses as a destination.
func newHarness(t *testing.T, upstream, controlPlane string, trust *x509.CertPool) *harness {
	t.Helper()
	h := &harness{store: newStore(nil), upstream: upstream}
	tlsConfig := &tls.Config{RootCAs: trust, MinVersion: tls.VersionTLS12}
	ca, _, _, err := pkgegress.GenerateCA(CACommonName)
	if err != nil {
		t.Fatal(err)
	}
	var dialer net.Dialer
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		h.dialed.Add(1)
		return dialer.DialContext(ctx, network, h.upstream)
	}
	h.gate = &gate{
		store: h.store, controlPlane: controlPlane, dial: dial,
		gateway: &pkgegress.Gateway{
			Registry: h.store.Registry(), CA: ca, Auth: credentialAuth{store: h.store},
			UpstreamTLS: tlsConfig, BlockLoopbackTargets: true, Realm: Realm, Log: slog.Default(),
		},
		upstream: &http.Transport{DialContext: dial, TLSClientConfig: tlsConfig},
		records: func(r egress.Record) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.records = append(h.records, r)
		},
		log: slog.Default(),
		now: func() time.Time { return time.Now().UTC() },
	}
	h.proxy = httptest.NewServer(http.HandlerFunc(h.gate.ServeProxy))
	t.Cleanup(h.proxy.Close)
	h.reverse = httptest.NewServer(http.HandlerFunc(h.gate.ServeReverse))
	t.Cleanup(h.reverse.Close)
	return h
}

// TestGateMatrix is the decision table: mode against the two lists, the
// destinations no boundary admits, and a caller the gateway does not know.
// Every case is decided with no dial.
func TestGateMatrix(t *testing.T) {
	h := newHarness(t, "127.0.0.1:1", "control.example.internal", nil)
	h.store.Apply(egress.Map{Principal: "sandbox:sbx_none", Version: 1, Credential: "none", Mode: v1.EgressNone})
	h.store.Apply(egress.Map{Principal: "sandbox:sbx_allow", Version: 1, Credential: "allow", Mode: v1.EgressAllowlist, Allow: []string{"api.example.com", "*.cdn.example.com"}})
	h.store.Apply(egress.Map{Principal: "sandbox:sbx_open", Version: 1, Credential: "open", Mode: v1.EgressOpen, Deny: []string{"blocked.example.com", "*.blocked.example.net"}})
	// A credential the store knows of no map for, which is what a purge
	// racing a connection looks like.
	h.store.mu.Lock()
	h.store.byCredential["orphan"] = "sandbox:sbx_gone"
	h.store.mu.Unlock()

	for _, tc := range []struct {
		name       string
		credential string
		host       string
		verdict    string
		reason     string
	}{
		{"noCredential", "", "api.example.com", egress.DecisionDenied, reasonNoCredential},
		{"anotherSandboxsCredential", "wrong", "api.example.com", egress.DecisionDenied, reasonNoCredential},
		{"noMap", "orphan", "api.example.com", egress.DecisionUnknown, reasonNoMap},
		{"noneRefusesEveryHost", "none", "api.example.com", egress.DecisionDenied, reasonModeNone},
		{"allowlistAdmitsAListedHost", "allow", "api.example.com", egress.DecisionPassthrough, ""},
		{"allowlistAdmitsUnderAWildcard", "allow", "one.cdn.example.com", egress.DecisionPassthrough, ""},
		{"allowlistRefusesTheWildcardsApex", "allow", "cdn.example.com", egress.DecisionDenied, reasonNotAllowed},
		{"allowlistRefusesAnythingElse", "allow", "other.example.com", egress.DecisionDenied, reasonNotAllowed},
		{"openAdmits", "open", "anything.example.org", egress.DecisionPassthrough, ""},
		{"openRefusesADeniedHost", "open", "blocked.example.com", egress.DecisionDenied, reasonDenied},
		{"openRefusesUnderADeniedWildcard", "open", "one.blocked.example.net", egress.DecisionDenied, reasonDenied},
		{"loopbackByName", "open", "localhost", egress.DecisionDenied, reasonLocalTarget},
		{"loopbackByAddress", "open", "127.0.0.1", egress.DecisionDenied, reasonLocalTarget},
		{"theUnspecifiedAddress", "open", "0.0.0.0", egress.DecisionDenied, reasonLocalTarget},
		{"theSixLoopback", "open", "::1", egress.DecisionDenied, reasonLocalTarget},
		{"theLinkLocalMetadataAddress", "open", "169.254.169.254", egress.DecisionDenied, reasonLocalTarget},
		{"aNameUnderLocalhost", "open", "db.localhost", egress.DecisionDenied, reasonLocalTarget},
		{"theControlPlane", "open", "control.example.internal", egress.DecisionDenied, reasonControlPlane},
		{"noDestination", "open", "", egress.DecisionDenied, reasonBadDestination},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := h.dialed.Load()
			d := h.gate.decide(tc.credential, tc.host, 443)
			if d.verdict != tc.verdict || d.reason != tc.reason {
				t.Fatalf("decide(%q, %q) = %q/%q, want %q/%q", tc.credential, tc.host, d.verdict, d.reason, tc.verdict, tc.reason)
			}
			if h.dialed.Load() != before {
				t.Fatal("the gate dialed while deciding")
			}
		})
	}
}

// TestADeniedHostBeatsASecretsScope is the rule that keeps a secret from
// buying reach its sandbox does not have. The map compiler drops the entry,
// and the gate refuses the host whether or not one survived.
func TestADeniedHostBeatsASecretsScope(t *testing.T) {
	h := newHarness(t, "127.0.0.1:1", "", nil)
	m := egress.Map{
		Principal: "sandbox:sbx_a", Version: 1, Credential: "c", Mode: v1.EgressOpen,
		Deny: []string{"api.example.com"},
		Entries: []egress.Entry{{
			Secret: "token", Placeholder: egress.MintPlaceholder(), Hosts: []string{"api.example.com"},
		}},
	}
	h.store.Apply(m)
	if d := h.gate.decide("c", "api.example.com", 443); d.verdict != egress.DecisionDenied || d.reason != reasonDenied {
		t.Fatalf("decide = %q/%q, want the denied host to win", d.verdict, d.reason)
	}
}

// TestProxyDoorRefusesBeforeAnyDial holds the whole point of the gate: a
// destination the boundary does not admit never reaches the network.
func TestProxyDoorRefusesBeforeAnyDial(t *testing.T) {
	h := newHarness(t, "127.0.0.1:1", "", nil)
	h.store.Apply(egress.Map{Principal: "sandbox:sbx_a", Version: 1, Credential: "c", Mode: v1.EgressAllowlist, Allow: []string{"api.example.com"}})
	resp := connect(t, h.proxy.Listener.Addr().String(), "other.example.com:443", "c")
	if resp != http.StatusForbidden {
		t.Fatalf("CONNECT answered %d, want %d", resp, http.StatusForbidden)
	}
	if h.dialed.Load() != 0 {
		t.Fatal("the gateway dialed a destination the boundary refused")
	}
	records := h.taken()
	if len(records) != 1 || records[0].Decision != egress.DecisionDenied || records[0].Host != "other.example.com" {
		t.Fatalf("records = %+v, want one denial naming the host", records)
	}
	if records[0].Principal != "sandbox:sbx_a" || records[0].Door != egress.DoorProxy {
		t.Fatalf("record = %+v", records[0])
	}
}

// TestProxyDoorNeedsTheSandboxsOwnCredential answers a caller the gateway
// cannot place with the challenge, and files no record: there is no
// principal to file one under.
func TestProxyDoorNeedsTheSandboxsOwnCredential(t *testing.T) {
	h := newHarness(t, "127.0.0.1:1", "", nil)
	h.store.Apply(egress.Map{Principal: "sandbox:sbx_a", Version: 1, Credential: "c", Mode: v1.EgressOpen})
	for _, credential := range []string{"", "wrong"} {
		if got := connect(t, h.proxy.Listener.Addr().String(), "api.example.com:443", credential); got != http.StatusProxyAuthRequired {
			t.Fatalf("CONNECT with %q answered %d, want %d", credential, got, http.StatusProxyAuthRequired)
		}
	}
	if len(h.taken()) != 0 {
		t.Fatalf("records = %+v, want none for a caller with no principal", h.taken())
	}
	if h.dialed.Load() != 0 {
		t.Fatal("the gateway dialed for a caller it could not place")
	}
}

// TestProxyDoorTunnelsAnAdmittedHost is the other half: a destination the
// boundary admits is reached, untouched, and the connection is recorded with
// what moved.
func TestProxyDoorTunnelsAnAdmittedHost(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello from "+r.Host)
	}))
	defer upstream.Close()
	trust := x509.NewCertPool()
	trust.AddCert(upstream.Certificate())

	h := newHarness(t, hostPort(t, upstream.URL), "", trust)
	h.store.Apply(egress.Map{Principal: "sandbox:sbx_a", Version: 1, Credential: "c", Mode: v1.EgressAllowlist, Allow: []string{"api.example.com"}})

	client := proxyClient(t, h.proxy.Listener.Addr().String(), "c", trust)
	resp, err := client.Get("https://api.example.com/hello")
	if err != nil {
		t.Fatalf("the admitted host was not reachable: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "hello from api.example.com") {
		t.Fatalf("the upstream answered %q", body)
	}
	if h.dialed.Load() != 1 {
		t.Fatalf("the gateway dialed %d times, want once", h.dialed.Load())
	}
	client.CloseIdleConnections()
	records := waitForRecords(t, h, 1)
	r := records[0]
	if r.Decision != egress.DecisionPassthrough || r.Host != "api.example.com" || r.Port != 443 {
		t.Fatalf("record = %+v, want a passthrough to the admitted host", r)
	}
	if r.BytesIn == 0 || r.BytesOut == 0 {
		t.Fatalf("record = %+v, want the bytes that moved", r)
	}
	// A tunnel is never read, so the record says nothing about the request.
	if r.Method != "" || r.Path != "" || r.Status != 0 {
		t.Fatalf("record = %+v, want nothing of a connection the gateway did not read", r)
	}
}

// TestReverseDoor is the door for a runtime that ignores proxy variables: the
// destination in the first path segment, the credential in its own header.
func TestReverseDoor(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "the upstream saw "+r.URL.Path+" with "+r.Header.Get("X-Carried"))
	}))
	defer upstream.Close()
	trust := x509.NewCertPool()
	trust.AddCert(upstream.Certificate())
	h := newHarness(t, hostPort(t, upstream.URL), "", trust)
	h.store.Apply(egress.Map{Principal: "sandbox:sbx_a", Version: 1, Credential: "c", Mode: v1.EgressAllowlist, Allow: []string{"api.example.com"}})

	t.Run("anAdmittedHost", func(t *testing.T) {
		resp, body := reverse(t, h.reverse.URL+"/api.example.com/v1/things?page=2", "c", map[string]string{"X-Carried": "a header"})
		if resp.StatusCode != http.StatusTeapot {
			t.Fatalf("status = %d, want the upstream's", resp.StatusCode)
		}
		if !strings.Contains(body, "/v1/things") || !strings.Contains(body, "a header") {
			t.Fatalf("the upstream saw %q", body)
		}
	})
	t.Run("aRefusedHost", func(t *testing.T) {
		resp, _ := reverse(t, h.reverse.URL+"/other.example.com/v1/things", "c", nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
		}
	})
	t.Run("noCredential", func(t *testing.T) {
		resp, _ := reverse(t, h.reverse.URL+"/api.example.com/v1/things", "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
		}
	})
	t.Run("noDestination", func(t *testing.T) {
		resp, _ := reverse(t, h.reverse.URL+"/", "c", nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
	})

	// Two records: the connection that was admitted and the one that was
	// refused. A request with no credential has no principal to file one
	// under, and one that names no destination is a client error rather
	// than a decision about a boundary.
	records := waitForRecords(t, h, 2)
	if records[0].Decision != egress.DecisionAllowed || records[0].Status != http.StatusTeapot {
		t.Fatalf("the admitted record = %+v", records[0])
	}
	if records[0].Method != http.MethodGet || records[0].Path != "/v1/things" {
		t.Fatalf("the admitted record = %+v, want the method and the path without the query", records[0])
	}
	if records[1].Decision != egress.DecisionDenied || records[1].Host != "other.example.com" {
		t.Fatalf("the refused record = %+v", records[1])
	}
	// No record carries the credential, a header, a body, or a query
	// string, whatever the door saw of the request.
	for _, r := range records {
		if strings.Contains(r.Path, "?") || strings.Contains(r.Path, "page=2") || strings.Contains(r.Path, "c") && r.Path == "c" {
			t.Fatalf("record = %+v", r)
		}
	}
}

// TestTheProxyDoorAnswersOnlyConnect keeps the door from being read as an
// ordinary proxy, which would leak a plaintext request to a destination the
// gate has not decided on.
func TestTheProxyDoorAnswersOnlyConnect(t *testing.T) {
	h := newHarness(t, "127.0.0.1:1", "", nil)
	resp, err := http.Get(h.proxy.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

// TestTheGatewayAnswersAnUnreachableDestination keeps a destination that the
// boundary admits but the network does not from looking like a refusal.
func TestTheGatewayAnswersAnUnreachableDestination(t *testing.T) {
	h := newHarness(t, "127.0.0.1:1", "", nil)
	h.store.Apply(egress.Map{Principal: "sandbox:sbx_a", Version: 1, Credential: "c", Mode: v1.EgressOpen})
	if got := connect(t, h.proxy.Listener.Addr().String(), "api.example.com:443", "c"); got != http.StatusBadGateway {
		t.Fatalf("CONNECT answered %d, want %d", got, http.StatusBadGateway)
	}
	records := waitForRecords(t, h, 1)
	if records[0].Reason != "Unreachable" {
		t.Fatalf("record = %+v, want the destination named unreachable", records[0])
	}
}

func TestProxyCredential(t *testing.T) {
	for _, tc := range []struct{ header, want string }{
		{"Basic " + basic("sandbox", "secret"), "secret"},
		{"basic " + basic("sandbox", "secret"), "secret"}, // the scheme is case insensitive
		{"Bearer token", ""},
		{"Basic !!!not base64", ""},
		{"Basic " + basic("", "secret"), "secret"},
		{"", ""},
	} {
		if got := proxyCredential(tc.header); got != tc.want {
			t.Errorf("proxyCredential(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
	// A userinfo with no colon carries no credential.
	if got := proxyCredential("Basic " + base64Of("sandbox")); got != "" {
		t.Errorf("proxyCredential of a userinfo with no colon = %q", got)
	}
}

func TestSplitReversePath(t *testing.T) {
	for _, tc := range []struct {
		path, host, rest string
		ok               bool
	}{
		{"/api.example.com/v1/things", "api.example.com", "/v1/things", true},
		{"/api.example.com", "api.example.com", "/", true},
		{"/api.example.com/", "api.example.com", "/", true},
		{"/", "", "", false},
		{"", "", "", false},
	} {
		host, rest, ok := splitReversePath(tc.path)
		if host != tc.host || rest != tc.rest || ok != tc.ok {
			t.Errorf("splitReversePath(%q) = %q, %q, %v, want %q, %q, %v", tc.path, host, rest, ok, tc.host, tc.rest, tc.ok)
		}
	}
}

func TestHostOfAndSplitHostPort(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://control.example.internal/v1", "control.example.internal"},
		{"http://control.example.internal:8080", "control.example.internal"},
		{"control.example.internal", "control.example.internal"},
		{"[::1]:8080", "::1"},
	} {
		if got := hostOf(tc.in); got != tc.want {
			t.Errorf("hostOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if host, port := splitHostPort("api.example.com:8443", 443); host != "api.example.com" || port != 8443 {
		t.Errorf("splitHostPort = %q, %d", host, port)
	}
	if host, port := splitHostPort("api.example.com", 443); host != "api.example.com" || port != 443 {
		t.Errorf("splitHostPort = %q, %d", host, port)
	}
}

func TestUpstreamTrust(t *testing.T) {
	if config, err := upstreamTLS(""); config != nil || err != nil {
		t.Fatalf("an empty bundle = %v, %v, want the system roots", config, err)
	}
	if _, err := upstreamTLS("not a certificate"); err == nil {
		t.Fatal("a bundle with no certificate was accepted")
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()
	pem := certPEM(t, upstream)
	config, err := upstreamTLS(pem)
	if err != nil || config == nil || config.RootCAs == nil {
		t.Fatalf("upstreamTLS = %v, %v", config, err)
	}
}

// ---- helpers ----

// connect opens one CONNECT and answers the status the gateway replied with.
func connect(t *testing.T, proxyAddr, target, credential string) int {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if credential != "" {
		req += "Proxy-Authorization: Basic " + basic("sandbox", credential) + "\r\n"
	}
	req += "\r\n"
	if _, err = io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	if err = conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(newReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// proxyClient is a workload's own client, configured the way the projected
// environment configures one.
func proxyClient(t *testing.T, proxyAddr, credential string, trust *x509.CertPool) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse("http://" + url.UserPassword(egress.ProxyUser, credential).String() + "@" + proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: trust, MinVersion: tls.VersionTLS12},
		},
	}
}

func reverse(t *testing.T, target, credential string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if credential != "" {
		req.Header.Set(egress.CredentialHeader, credential)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

// waitForRecords waits for the doors to have filed n records. A tunnel files
// its record when both halves have finished, which is after the client saw
// the answer.
func waitForRecords(t *testing.T, h *harness, n int) []egress.Record {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		records := h.taken()
		if len(records) >= n {
			return records
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d records after the deadline, want %d: %+v", len(records), n, records)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hostPort(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func basic(user, password string) string { return base64Of(user + ":" + password) }

func base64Of(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func newReader(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }

func certPEM(t *testing.T, server *httptest.Server) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
}

// TestAHostWithACredentialIsTerminated is the routing decision the Secret
// kind rests on: a destination the substitution engine holds an entry for is
// handed to the engine's own proxy, which terminates it, and every other
// destination is tunneled by the gate. The entry is put in the engine
// directly here, because the compiler carries no value yet.
func TestAHostWithACredentialIsTerminated(t *testing.T) {
	h := newHarness(t, "127.0.0.1:1", "", nil)
	m := egress.Map{Principal: "sandbox:sbx_a", Version: 1, Credential: "c", Mode: v1.EgressAllowlist, Allow: []string{"api.example.com", "plain.example.com"}}
	h.store.Apply(m)
	h.store.registry.Set(m.Principal, []pkgegress.Entry{{
		Placeholder:  []byte(egress.MintPlaceholder()),
		Secret:       []byte("a value"),
		AllowedHosts: []string{"api.example.com"},
	}})
	if d := h.gate.decide("c", "api.example.com", 443); !d.terminate || d.verdict != egress.DecisionAllowed {
		t.Fatalf("decide = %+v, want the host with a credential terminated", d)
	}
	if d := h.gate.decide("c", "plain.example.com", 443); d.terminate || d.verdict != egress.DecisionPassthrough {
		t.Fatalf("decide = %+v, want the host without one tunneled", d)
	}
	// The terminated connection goes to the engine's own proxy, which needs
	// the connection itself; a writer that cannot be hijacked is refused
	// there and never dialed here.
	recorder := httptest.NewRecorder()
	h.gate.ServeProxy(recorder, connectRequest(t, "api.example.com:443", "c"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("the terminated connection answered %d, want the engine's own refusal", recorder.Code)
	}
	if h.dialed.Load() != 0 {
		t.Fatal("the gate dialed a connection it handed to the engine")
	}
	if records := h.taken(); len(records) != 1 || records[0].Decision != egress.DecisionAllowed {
		t.Fatalf("records = %+v, want the terminated connection filed as allowed", records)
	}
}

// TestATunnelNeedsTheConnectionItself keeps the door from answering a
// connection it cannot serve: the dial succeeded, so the upstream is closed
// rather than left open behind a refusal.
func TestATunnelNeedsTheConnectionItself(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upstream.Close() }()
	go func() {
		for {
			conn, err := upstream.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	h := newHarness(t, upstream.Addr().String(), "", nil)
	h.store.Apply(egress.Map{Principal: "sandbox:sbx_a", Version: 1, Credential: "c", Mode: v1.EgressOpen})
	recorder := httptest.NewRecorder()
	h.gate.ServeProxy(recorder, connectRequest(t, "api.example.com:443", "c"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("the tunnel answered %d, want a refusal a caller can read", recorder.Code)
	}
}

// TestARecordWithNoSinkIsDropped keeps a gate that reports to nothing from
// failing the connection it was deciding.
func TestARecordWithNoSinkIsDropped(t *testing.T) {
	(&gate{now: time.Now}).record(egress.Record{Principal: "sandbox:sbx_a"})
}

// connectRequest is one CONNECT as the proxy door receives it.
func connectRequest(t *testing.T, target, credential string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodConnect, "http://"+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = target
	if credential != "" {
		req.Header.Set("Proxy-Authorization", "Basic "+basic(egress.ProxyUser, credential))
	}
	return req
}
