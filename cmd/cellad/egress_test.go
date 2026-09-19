// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/cella/egress"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/egressd"
)

// TestEgressEndToEnd is the whole boundary over one running control plane and
// one running gateway: the map is compiled and acknowledged before the
// sandbox exists, the workload's own environment points at the gateway, the
// host its manifest allowed is reachable through it, every other host is
// refused before any dial, a sandbox that may reach nothing reaches nothing,
// and every decision comes back as a record.
func TestEgressEndToEnd(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "the upstream answered "+r.Host+r.URL.Path)
	}))
	defer upstream.Close()
	trust := x509.NewCertPool()
	trust.AddCert(upstream.Certificate())

	proxyAddr, reverseAddr := freePort(t), freePort(t)
	plane := startPlane(t, proxyAddr, reverseAddr)

	// The gateway runs in this process so the test can name where an
	// admitted destination lands: it reaches loopback and never a resolver,
	// which is what keeps the tier hermetic.
	ready := make(chan struct{})
	gateway := startGateway(t, plane, egressd.Options{
		ProxyAddr: proxyAddr, ReverseAddr: reverseAddr,
		UpstreamCAPEM: certificatePEM(t, upstream),
		Dial:          dialTo(upstream.Listener.Addr().String()),
		Ready:         func() { close(ready) },
	})
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("the gateway never received its first snapshot")
	}

	t.Run("anAllowedHostIsReachableAndNoOtherIs", func(t *testing.T) {
		sandbox := plane.create(t, `{"mode":"allowlist","allowedHosts":["upstream.example.com"]}`)

		// The workload reads its own environment, which is what proves the
		// driver projected the boundary rather than the test knowing it.
		proxy := strings.TrimSpace(plane.exec(t, sandbox, "echo $HTTPS_PROXY"))
		if !strings.Contains(proxy, proxyAddr) || !strings.Contains(proxy, egress.ProxyUser+":") {
			t.Fatalf("the sandbox's HTTPS_PROXY is %q, want the gateway with the sandbox's own credential", proxy)
		}
		client := workloadClient(t, proxy, trust)

		resp, err := client.Get("https://upstream.example.com/things")
		if err != nil {
			t.Fatalf("the allowed host was not reachable: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), "the upstream answered upstream.example.com/things") {
			t.Fatalf("the upstream answered %q", body)
		}

		if _, err = client.Get("https://elsewhere.example.com/things"); err == nil {
			t.Fatal("a host outside the boundary was reachable")
		} else if !strings.Contains(err.Error(), http.StatusText(http.StatusForbidden)) {
			t.Fatalf("the refusal was %v, want the gateway's refusal before any dial", err)
		}

		// A tunnel is recorded when it ends, so the workload's own idle
		// connection is closed first, as a process leaving would close it.
		client.CloseIdleConnections()
		records := plane.records(t, sandbox, 2)
		byHost := map[string]string{}
		for _, r := range records {
			byHost[r.Host] = r.Decision
			if r.Principal != egress.Principal(sandbox) {
				t.Errorf("record = %+v, want the sandbox's own principal", r)
			}
		}
		if byHost["upstream.example.com"] != egress.DecisionPassthrough {
			t.Errorf("the allowed host was recorded as %q", byHost["upstream.example.com"])
		}
		if byHost["elsewhere.example.com"] != egress.DecisionDenied {
			t.Errorf("the refused host was recorded as %q", byHost["elsewhere.example.com"])
		}
	})

	t.Run("noneReachesNothing", func(t *testing.T) {
		sandbox := plane.create(t, `{"mode":"none"}`)
		proxy := strings.TrimSpace(plane.exec(t, sandbox, "echo $HTTPS_PROXY"))
		client := workloadClient(t, proxy, trust)
		if _, err := client.Get("https://upstream.example.com/things"); err == nil {
			t.Fatal("a sandbox whose boundary admits no host reached one")
		}
		records := plane.records(t, sandbox, 1)
		if records[0].Decision != egress.DecisionDenied {
			t.Fatalf("record = %+v, want a refusal", records[0])
		}
	})

	t.Run("theReverseDoorCarriesTheSameBoundary", func(t *testing.T) {
		sandbox := plane.create(t, `{"mode":"allowlist","allowedHosts":["upstream.example.com"]}`)
		gatewayURL := strings.TrimSpace(plane.exec(t, sandbox, "echo $CELLA_GATEWAY_URL"))
		credential := strings.TrimSpace(plane.exec(t, sandbox, "echo $CELLA_GATEWAY_CREDENTIAL"))
		if gatewayURL == "" || credential == "" {
			t.Fatalf("the reverse door was not projected: %q, %q", gatewayURL, credential)
		}
		for _, tc := range []struct {
			host string
			want int
		}{
			{"upstream.example.com", http.StatusOK},
			{"elsewhere.example.com", http.StatusForbidden},
		} {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gatewayURL+"/"+tc.host+"/things", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(egress.CredentialHeader, credential)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("the reverse door answered %d for %s, want %d", resp.StatusCode, tc.host, tc.want)
			}
		}
	})

	t.Run("theSandboxTrustsTheGatewaysAuthority", func(t *testing.T) {
		sandbox := plane.create(t, `{"mode":"allowlist","allowedHosts":["upstream.example.com"]}`)
		path := strings.TrimSpace(plane.exec(t, sandbox, "echo $SSL_CERT_FILE"))
		if path == "" {
			t.Fatal("the trust variables were not set")
		}
		projected := strings.TrimSpace(plane.exec(t, sandbox, "cat "+path))
		if !strings.Contains(projected, "BEGIN CERTIFICATE") || strings.TrimSpace(gateway.CAPEM()) != projected {
			t.Fatalf("the sandbox holds %q, want the gateway's own authority", projected)
		}
	})

	// The canary of spec 018, in the form this slice can hold it: no
	// placeholder, credential or authority key appears in a record. The
	// values themselves join with the Secret kind, and this test grows the
	// value case there.
	t.Run("TestSecretValuesNeverEnterASandbox", func(t *testing.T) {
		sandbox := plane.create(t, `{"mode":"allowlist","allowedHosts":["upstream.example.com"]}`)
		credential := strings.TrimSpace(plane.exec(t, sandbox, "echo $CELLA_GATEWAY_CREDENTIAL"))
		client := workloadClient(t, strings.TrimSpace(plane.exec(t, sandbox, "echo $HTTPS_PROXY")), trust)
		placeholder := egress.MintPlaceholder()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://upstream.example.com/things", nil)
		if err != nil {
			t.Fatal(err)
		}
		// A placeholder the gateway has no entry for leaves the sandbox as
		// the opaque token it is, and reaches the upstream unchanged.
		req.Header.Set("Authorization", "Bearer "+placeholder)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		client.CloseIdleConnections()
		for _, r := range plane.records(t, sandbox, 1) {
			raw, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{placeholder, credential} {
				if secret != "" && strings.Contains(string(raw), secret) {
					t.Fatalf("a record carries %q: %s", secret, raw)
				}
			}
		}
	})

	// The API strips the control plane's own record of the boundary from
	// every answer, so the credential a workload holds is not readable
	// through the API by anyone else.
	t.Run("theCredentialIsNotInTheAPIsAnswer", func(t *testing.T) {
		sandbox := plane.create(t, `{"mode":"allowlist","allowedHosts":["upstream.example.com"]}`)
		body := plane.get(t, "/v1/sandboxes/"+sandbox)
		for _, field := range []string{"egressState", "credential"} {
			if strings.Contains(body, field) {
				t.Fatalf("the API answered with %q in it: %s", field, body)
			}
		}
	})
}

// TestTheEgressSubcommandConnects runs the role the way an operator does,
// through the binary's own subcommand, and proves the stream is real: a
// sandbox whose boundary needs a gateway is created only when one
// acknowledged its map.
func TestTheEgressSubcommandConnects(t *testing.T) {
	proxyAddr, reverseAddr := freePort(t), freePort(t)
	plane := startPlane(t, proxyAddr, reverseAddr)

	var out syncBuffer
	var errOut bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	codec := make(chan int, 1)
	go func() {
		codec <- run(ctx, []string{"egress"}, env(map[string]string{
			"CELLA_URL":                 plane.url,
			"CELLA_ENVIRONMENT_KEY":     plane.environmentKey(t),
			"CELLA_EGRESS_PROXY_ADDR":   proxyAddr,
			"CELLA_EGRESS_REVERSE_ADDR": reverseAddr,
		}), &out, &errOut)
	}()
	defer func() {
		cancel()
		select {
		case code := <-codec:
			if code != 0 {
				t.Errorf("the egress role exited %d; stderr %q", code, errOut.String())
			}
		case <-time.After(30 * time.Second):
			t.Error("the egress role did not stop")
		}
	}()

	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(out.String(), "egress proxy=") {
		select {
		case code := <-codec:
			t.Fatalf("the egress role exited %d before listening; stderr %q", code, errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the egress role never reported its doors; stdout %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "environment=default") {
		t.Fatalf("the role did not read its environment out of the key: %q", out.String())
	}
	// A boundary that needs a gateway is the assertion: the create waits
	// for an acknowledgement and fails without one.
	sandbox := plane.create(t, `{"mode":"allowlist","allowedHosts":["upstream.example.com"]}`)
	if sandbox == "" {
		t.Fatal("the create returned no sandbox")
	}
}

// TestTheEgressSubcommandRefusesABadConfiguration keeps a misconfigured role
// from binding a door it could never fill.
func TestTheEgressSubcommandRefusesABadConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"noControlPlane", map[string]string{"CELLA_ENVIRONMENT_KEY": "a.b.c"}, "CELLA_URL is unset"},
		{"noKey", map[string]string{"CELLA_URL": "http://127.0.0.1:8080"}, "CELLA_ENVIRONMENT_KEY is unset"},
		{"aKeyThatIsNotAnEnvironmentKey", map[string]string{"CELLA_URL": "http://127.0.0.1:8080", "CELLA_ENVIRONMENT_KEY": "a.b.c"}, "CELLA_ENVIRONMENT_KEY"},
		{"aControlPlaneInTheClear", map[string]string{"CELLA_URL": "http://cella.example.com", "CELLA_ENVIRONMENT_KEY": "a.b.c"}, "CELLA_INSECURE_CONTROL_PLANE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errOut bytes.Buffer
			if code := run(t.Context(), []string{"egress"}, env(tc.env), io.Discard, &errOut); code != 1 {
				t.Fatalf("exit %d, want 1; stderr %q", code, errOut.String())
			}
			if !strings.Contains(errOut.String(), tc.want) {
				t.Fatalf("stderr = %q, want a line naming %q", errOut.String(), tc.want)
			}
		})
	}
	var errOut bytes.Buffer
	if code := run(t.Context(), []string{"egress", "-no-such-flag"}, env(nil), io.Discard, &errOut); code != 2 {
		t.Fatalf("exit %d, want a usage error", code)
	}
}

// ---- the harness ----

// plane is a running control plane with a caller's bearer and the signing key
// its environment keys are minted with.
type plane struct {
	url    string
	bearer string
	signer *auth.Signer
	stop   func() int
}

// startPlane runs cellad serve on loopback, pointed at the gateway addresses
// the test reserved.
func startPlane(t *testing.T, proxyAddr, reverseAddr string) *plane {
	t.Helper()
	issuer := issuertest.New(t)
	key := signingKey(t, 1)
	e := map[string]string{
		"CELLA_OIDC_ISSUERS":        issuer.URL(),
		"CELLA_RUNTIME":             "native",
		"CELLA_ALLOW_UNSAFE_NATIVE": "true",
		"CELLA_PUBLIC_URL":          "https://control.example.com",
		"CELLA_TOKEN_KEY":           signingKeyPEM(t, 1),
		"CELLA_DATA_DIR":            t.TempDir(),
		"CELLA_PUBLIC_ADDR":         "127.0.0.1:0",
		"CELLA_INTERNAL_ADDR":       "127.0.0.1:0",
		"CELLA_GATEWAY":             proxyAddr,
		"CELLA_GATEWAY_REVERSE":     reverseAddr,
		"CELLA_EGRESS_ACK_TIMEOUT":  "10s",
	}
	var out syncBuffer
	var errOut bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	codec := make(chan int, 1)
	go func() { codec <- run(ctx, nil, env(e), &out, &errOut) }()
	p := &plane{
		bearer: issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"cella"}}),
		signer: newSigner(t, key, "https://control.example.com"),
		stop: func() int {
			cancel()
			select {
			case code := <-codec:
				return code
			case <-time.After(gracePeriod + 10*time.Second):
				return -1
			}
		},
	}
	t.Cleanup(func() { p.stop() })
	deadline := time.Now().Add(15 * time.Second)
	for {
		if m := listening.FindStringSubmatch(out.String()); m != nil {
			p.url = "http://" + m[1]
			return p
		}
		select {
		case code := <-codec:
			t.Fatalf("serve exited %d before listening; stderr %q", code, errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never reported its listeners; stdout %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newSigner(t *testing.T, key *rsa.PrivateKey, issuer string) *auth.Signer {
	t.Helper()
	signer, err := auth.NewSigner(auth.SignerOptions{Issuer: issuer, Audience: "cella", Keys: []*rsa.PrivateKey{key}})
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// environmentKey mints what the gateway authenticates its one stream with.
// The keys route of the Environment kind is spec 021's; a test mints with
// the same signer the control plane verifies against.
func (p *plane) environmentKey(t *testing.T) string {
	t.Helper()
	token, err := p.signer.MintEnvironmentKey("default", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return token.Value
}

// create applies a manifest whose only interesting field is its boundary and
// answers the sandbox's id.
func (p *plane) create(t *testing.T, egressJSON string) string {
	t.Helper()
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","spec":{"network":{"egress":` + egressJSON + `}}}`
	status, answer := p.do(t, http.MethodPost, "/v1/sandboxes", strings.NewReader(body))
	if status != http.StatusCreated {
		t.Fatalf("POST /v1/sandboxes = %d %s", status, answer)
	}
	var obj struct {
		Status struct {
			ID         string `json:"id"`
			Conditions []struct {
				Type, Status, Reason string
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(answer), &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Status.ID == "" {
		t.Fatalf("the create answered %s", answer)
	}
	// The native environment enforces no egress rule, so the condition says
	// the boundary is recorded and not confined even with a gateway holding
	// the map.
	for _, c := range obj.Status.Conditions {
		if c.Type == "EgressEnforced" && c.Reason != "NotEnforcedByDriver" {
			t.Errorf("EgressEnforced = %+v, want the native driver named", c)
		}
	}
	return obj.Status.ID
}

// exec runs one shell command inside the sandbox and answers what it printed.
func (p *plane) exec(t *testing.T, sandbox, command string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"command": []string{"sh", "-c", command}})
	if err != nil {
		t.Fatal(err)
	}
	status, answer := p.do(t, http.MethodPost, "/v1/sandboxes/"+sandbox+"/exec?wait=1", bytes.NewReader(body))
	if status != http.StatusOK {
		t.Fatalf("exec = %d %s", status, answer)
	}
	var result struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err = json.Unmarshal([]byte(answer), &result); err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exec %q exited %d: %s", command, result.ExitCode, result.Stderr)
	}
	return result.Stdout
}

// records waits for the sandbox's connections to reach the control plane.
func (p *plane) records(t *testing.T, sandbox string, n int) []egress.Record {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		status, answer := p.do(t, http.MethodGet, "/v1/sandboxes/"+sandbox+"/egress", nil)
		if status != http.StatusOK {
			t.Fatalf("GET the sandbox's egress = %d %s", status, answer)
		}
		var page struct {
			Items []egress.Record `json:"items"`
		}
		if err := json.Unmarshal([]byte(answer), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) >= n {
			return page.Items
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d records after the deadline, want %d", len(page.Items), n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (p *plane) get(t *testing.T, path string) string {
	t.Helper()
	status, answer := p.do(t, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, status, answer)
	}
	return answer
}

func (p *plane) do(t *testing.T, method, path string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, p.url+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+p.bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(answer)
}

// startGateway runs the egress role in this process, against the plane.
func startGateway(t *testing.T, p *plane, o egressd.Options) *egressd.Gateway {
	t.Helper()
	o.URL = p.url
	o.Key = p.environmentKey(t)
	gateway, err := egressd.New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = gateway.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return gateway
}

// dialTo answers every admitted destination with one loopback address, so the
// tier reaches no resolver and no network.
func dialTo(address string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, address)
	}
}

// workloadClient is a client configured the way the projected environment
// configures one: the proxy from HTTPS_PROXY, the trust from the upstream's
// own authority.
func workloadClient(t *testing.T, proxy string, trust *x509.CertPool) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse(proxy)
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

// freePort reserves a loopback address by binding and releasing it, so the
// control plane can be told where the gateway will be before it is there.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	if err = ln.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func certificatePEM(t *testing.T, server *httptest.Server) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
}
