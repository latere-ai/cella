// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/egress"
	"latere.ai/x/cella/internal/egressd"
)

// TestTheSandboxTrustsThePublicRootsAndTheGateway is the trust file of spec
// 018 as a stock client inside the sandbox reads it. The upstream's
// certificate chains to a root the control plane reads as the system's,
// through its own SSL_CERT_FILE, which is where a public root sits in a
// deployment. curl in the sandbox reads the projected file through
// CURL_CA_BUNDLE and nothing else, so a host the gateway tunnels verifies only
// if the file holds the public roots, and a host the gateway terminates only
// if it holds the gateway's authority.
func TestTheSandboxTrustsThePublicRootsAndTheGateway(t *testing.T) {
	if _, err := osexec.LookPath("curl"); err != nil {
		t.Fatalf("this tier runs curl inside the sandbox, and curl is not on PATH: %v", err)
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "the upstream answered "+r.Host+r.URL.Path)
	}))
	defer upstream.Close()
	// The upstream's certificate is self-signed, so it is its own root, and
	// it is the only root the control plane reads: a file that trusted
	// anything else would not be what this test proves.
	root := certificatePEM(t, upstream)
	roots := filepath.Join(t.TempDir(), "roots.pem")
	if err := os.WriteFile(roots, []byte(root), 0o600); err != nil {
		t.Fatal(err)
	}

	proxyLn, reverseLn := doors(t)
	plane := startPlaneWith(t, proxyLn.Addr().String(), reverseLn.Addr().String(), map[string]string{
		"SSL_CERT_FILE": roots,
		// A control plane that stores a secret's value needs the key that
		// seals it, and the terminated case mounts one.
		"CELLA_SECRET_KEY": base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	ready := make(chan struct{})
	gateway := startGateway(t, plane, egressd.Options{
		ProxyListener: proxyLn, ReverseListener: reverseLn,
		UpstreamCAPEM: root,
		Dial:          dialTo(upstream.Listener.Addr().String()),
		Ready:         func() { close(ready) },
	})
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("the gateway never received its first snapshot")
	}

	t.Run("theFileHoldsTheRootsAndTheAuthority", func(t *testing.T) {
		sandbox := plane.create(t, `{"mode":"allowlist","allowedHosts":["upstream.example.com"]}`)
		held := projectedCertificates(t, plane, sandbox)
		want := []*x509.Certificate{upstream.Certificate(), parseCertificate(t, gateway.CAPEM())}
		if len(held) != len(want) {
			t.Fatalf("the sandbox's trust file holds %d certificates, want the root and the gateway's authority", len(held))
		}
		for i := range want {
			if !held[i].Equal(want[i]) {
				t.Fatalf("certificate %d of the trust file is %q, want %q", i, held[i].Subject, want[i].Subject)
			}
		}
	})

	t.Run("aTunneledHostVerifiesAgainstThePublicRoots", func(t *testing.T) {
		sandbox := plane.create(t, `{"mode":"allowlist","allowedHosts":["upstream.example.com"]}`)
		body := plane.exec(t, sandbox, "curl -sS --fail --max-time 20 https://upstream.example.com/tunneled")
		if !strings.Contains(body, "the upstream answered upstream.example.com/tunneled") {
			t.Fatalf("curl printed %q", body)
		}
		// No secret is bound to the host, so the gateway tunneled the
		// connection and the certificate curl verified was the upstream's.
		if decision := decisionFor(t, plane, sandbox, "upstream.example.com"); decision != egress.DecisionPassthrough {
			t.Fatalf("the connection was recorded %q, want %q", decision, egress.DecisionPassthrough)
		}
	})

	t.Run("aTerminatedHostVerifiesAgainstTheGatewaysAuthority", func(t *testing.T) {
		plane.applySecret(t, "vendor", `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Secret",`+
			`"metadata":{"name":"vendor"},"spec":{"scope":{"hosts":["vendor.invalid"]},"value":"`+theCanary+`"}}`)
		sandbox := plane.createMounting(t, "vendor", "VENDOR_TOKEN")
		// curl exits 60 when the certificate it is shown does not verify
		// against the file, before any status line. An exit of 0 with a
		// status is a TLS session with the gateway's own leaf that
		// verified. The status is the gateway's: its terminating engine
		// dials the upstream by name with a transport of its own, which the
		// tier's dial seam does not reach, and the name is under .invalid,
		// which never resolves, so the gateway answers 502 inside the
		// session and nothing leaves this host.
		status := strings.TrimSpace(plane.exec(t, sandbox,
			`curl -sS --max-time 20 -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $VENDOR_TOKEN" https://vendor.invalid/terminated`))
		if len(status) != 3 || status == "000" {
			t.Fatalf("curl printed the status %q, want one the gateway answered inside the session", status)
		}
		if decision := decisionFor(t, plane, sandbox, "vendor.invalid"); decision != egress.DecisionAllowed {
			t.Fatalf("the connection was recorded %q, want %q", decision, egress.DecisionAllowed)
		}
	})
}

// projectedCertificates reads the file SSL_CERT_FILE names inside the sandbox
// and parses every certificate in it, in order.
func projectedCertificates(t *testing.T, p *plane, sandbox string) []*x509.Certificate {
	t.Helper()
	path := strings.TrimSpace(p.exec(t, sandbox, "echo $SSL_CERT_FILE"))
	if path == "" {
		t.Fatal("the trust variables were not set")
	}
	for _, key := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO", "CURL_CA_BUNDLE"} {
		if got := strings.TrimSpace(p.exec(t, sandbox, "printenv "+key)); got != path {
			t.Fatalf("%s names %q, want the file SSL_CERT_FILE names, %q", key, got, path)
		}
	}
	rest := []byte(p.exec(t, sandbox, "cat "+path))
	var out []*x509.Certificate
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("the trust file holds a block that is no certificate: %v", err)
		}
		out = append(out, cert)
	}
	return out
}

func parseCertificate(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatalf("no PEM block in %q", certPEM)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// decisionFor is the decision the gateway recorded for the sandbox's
// connection to host. A tunnel is recorded when it ends, and curl has exited
// by the time this is read, so the record is on its way.
func decisionFor(t *testing.T, p *plane, sandbox, host string) string {
	t.Helper()
	for _, r := range p.records(t, sandbox, 1) {
		if r.Host == host {
			return r.Decision
		}
	}
	t.Fatalf("no record names %s", host)
	return ""
}

// TestServeRefusesAGatewayWithoutPublicRoots: a control plane that points
// sandboxes at a gateway and finds no public roots refuses to start, because
// every sandbox it made would verify the gateway's leaf and no other
// certificate. One with no gateway sets no trust variable and reads nothing,
// so the same file does not stop it.
func TestServeRefusesAGatewayWithoutPublicRoots(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("no certificate here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var errOut syncBuffer
	code := run(t.Context(), nil, env(identity(t, map[string]string{
		"CELLA_DATA_DIR":      t.TempDir(),
		"CELLA_PUBLIC_ADDR":   "127.0.0.1:0",
		"CELLA_INTERNAL_ADDR": "127.0.0.1:0",
		"CELLA_GATEWAY":       "127.0.0.1:3128",
		"SSL_CERT_FILE":       empty,
	})), io.Discard, &errOut)
	if code != 1 {
		t.Fatalf("a control plane with a gateway and no public roots exited %d; stderr %q", code, errOut.String())
	}
	if got := errOut.String(); !strings.Contains(got, "public roots") || !strings.Contains(got, "SSL_CERT_FILE") || !strings.Contains(got, empty) {
		t.Fatalf("the refusal = %q, want the roots, the variable and the file named", got)
	}
	startPlaneWith(t, "", "", map[string]string{"SSL_CERT_FILE": empty})
}

// TestTheWorkerRefusesWithoutPublicRoots: the control plane decides whether a
// worker's sandboxes are pointed at a gateway, so a worker that finds no
// public roots refuses to start whatever it serves.
func TestTheWorkerRefusesWithoutPublicRoots(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.pem")
	var out, errOut syncBuffer
	code := run(t.Context(), []string{"worker"}, env(map[string]string{
		"CELLA_URL":             "http://127.0.0.1:1",
		"CELLA_ENVIRONMENT_KEY": "a-key",
		"CELLA_DATA_DIR":        t.TempDir(),
		"SSL_CERT_FILE":         missing,
	}), &out, &errOut)
	if code != 1 {
		t.Fatalf("a worker with no public roots exited %d; stderr %q", code, errOut.String())
	}
	if got := errOut.String(); !strings.Contains(got, "public roots") || !strings.Contains(got, missing) {
		t.Fatalf("the refusal = %q, want the roots and the file named", got)
	}
}
