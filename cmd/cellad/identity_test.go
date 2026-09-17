// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
)

// identity fills the variables spec 006 makes required, so a test about
// a listener is not also a test about the issuers. The issuer is a stub
// on loopback, which is what makes the node start with no network. A
// value the caller set is kept.
func identity(t *testing.T, m map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{
		"CELLA_OIDC_ISSUERS": issuertest.New(t).URL(),
		"CELLA_PUBLIC_URL":   "https://cella.example.com",
		"CELLA_TOKEN_KEY":    signingKeyPEM(t, 1),
	}
	maps.Copy(out, m)
	return out
}

// signingKeys holds the generated keys, because a 2048-bit RSA key costs
// more than every assertion in this package together.
var signingKeys struct {
	sync.Mutex
	keys []*rsa.PrivateKey
}

// signingKey is the nth generated key, counting from one.
func signingKey(t *testing.T, n int) *rsa.PrivateKey {
	t.Helper()
	signingKeys.Lock()
	defer signingKeys.Unlock()
	for len(signingKeys.keys) < n {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		signingKeys.keys = append(signingKeys.keys, key)
	}
	return signingKeys.keys[n-1]
}

// signingKeyPEM renders the first n generated keys as one PEM value,
// which is what CELLA_TOKEN_KEY carries.
func signingKeyPEM(t *testing.T, n int) string {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= n; i++ {
		block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(signingKey(t, i))}
		if err := pem.Encode(&b, block); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

// TestServeRefusesToStartWithoutIdentity is spec 006's row: cellad
// refuses to start with no issuer, or with a CELLA_TOKEN_KEY holding no
// RSA key, and every refusal is one line that names the variable.
func TestServeRefusesToStartWithoutIdentity(t *testing.T) {
	garbage := "-----BEGIN RSA PRIVATE KEY-----\nQQ==\n-----END RSA PRIVATE KEY-----\n"
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{{
		name: "no issuer at all",
		env:  map[string]string{"CELLA_OIDC_ISSUERS": ""},
		want: "CELLA_OIDC_ISSUERS names no issuer",
	}, {
		name: "no signing key",
		env:  map[string]string{"CELLA_TOKEN_KEY": ""},
		want: "CELLA_TOKEN_KEY is unset",
	}, {
		name: "a signing key that is no RSA key",
		env:  map[string]string{"CELLA_TOKEN_KEY": garbage},
		want: "is no RSA private key",
	}, {
		name: "no public URL to issue under",
		env:  map[string]string{"CELLA_PUBLIC_URL": ""},
		want: "CELLA_PUBLIC_URL",
	}, {
		name: "an issuer that does not answer",
		env:  map[string]string{"CELLA_OIDC_ISSUERS": "http://127.0.0.1:1"},
		want: "CELLA_OIDC_ISSUERS: issuer http://127.0.0.1:1",
	}, {
		name: "an authorizer with no bearer",
		env:  map[string]string{"CELLA_AUTHORIZER_URL": "https://authz.example.com/decide"},
		want: "CELLA_AUTHORIZER_TOKEN is unset",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var errOut bytes.Buffer
			e := identity(t, tc.env)
			e["CELLA_DATA_DIR"] = t.TempDir()
			e["CELLA_PUBLIC_ADDR"] = "127.0.0.1:0"
			e["CELLA_INTERNAL_ADDR"] = "127.0.0.1:0"
			code := run(t.Context(), nil, env(e), io.Discard, &errOut)
			if code != 1 {
				t.Fatalf("exit %d, want 1; stderr %q", code, errOut.String())
			}
			if n := strings.Count(errOut.String(), "\n"); n != 1 {
				t.Errorf("stderr is %d line(s); a start-up failure is one line:\n%s", n, errOut.String())
			}
			if !strings.Contains(errOut.String(), tc.want) {
				t.Errorf("stderr = %q, want a line naming %q", errOut.String(), tc.want)
			}
		})
	}
}

// TestServeAnswersTheKeySetAndNamesItsAuthorizer: the public listener
// serves the set every workload token verifies against, and the line the
// node logs says which authorizer is in force.
func TestServeAnswersTheKeySetAndNamesItsAuthorizer(t *testing.T) {
	publicURL, internalURL, out, stop := startServeWithLog(t, nil)
	defer stop()

	code, body := get(t, publicURL+"/.well-known/jwks.json")
	if code != 200 {
		t.Fatalf("GET the key set = %d %q", code, body)
	}
	var doc struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Keys) != 1 || doc.Keys[0]["kty"] != "RSA" || doc.Keys[0]["kid"] == "" {
		t.Fatalf("the key set is %s; one RSA key under its kid is what CELLA_TOKEN_KEY carried", body)
	}
	// The set is the public listener's: a third service reaches it, and
	// the cluster's own listener carries the probes alone.
	if code, _ := get(t, internalURL+"/.well-known/jwks.json"); code != 404 {
		t.Errorf("the internal listener answered the key set with %d", code)
	}
	if got := out(); !strings.Contains(got, "authorizer=owner policy") {
		t.Errorf("the node logged %q; with no endpoint configured the owner policy decides", got)
	}
}

// startServeWithLog is startServe with the node's own log, for a test
// about what the node says at start.
func startServeWithLog(t *testing.T, extra map[string]string) (publicURL, internalURL string, log func() string, stop func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var out syncBuffer
	var errOut bytes.Buffer
	e := identity(t, extra)
	e["CELLA_DATA_DIR"] = t.TempDir()
	e["CELLA_PUBLIC_ADDR"] = "127.0.0.1:0"
	e["CELLA_INTERNAL_ADDR"] = "127.0.0.1:0"
	codec := make(chan int, 1)
	go func() { codec <- run(ctx, nil, env(e), &out, &errOut) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if m := listening.FindStringSubmatch(out.String()); m != nil {
			return "http://" + m[1], "http://" + m[2], out.String, func() int {
				cancel()
				select {
				case code := <-codec:
					return code
				case <-time.After(gracePeriod + 10*time.Second):
					t.Fatal("serve did not stop")
					return -1
				}
			}
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

// TestAnUnreachableAuthorizerIsNotAReadinessCheck is spec 006's rule
// that availability is not readiness: an endpoint that does not answer
// fails the requests that need a decision, and does not take the replica
// out of rotation. The node starts against a dead endpoint, because
// nothing dials it until a request needs a decision.
func TestAnUnreachableAuthorizerIsNotAReadinessCheck(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()

	publicURL, internalURL, out, stop := startServeWithLog(t, map[string]string{
		"CELLA_AUTHORIZER_URL":   url,
		"CELLA_AUTHORIZER_TOKEN": "a-bearer",
	})
	defer stop()

	if got := out(); !strings.Contains(got, "authorizer=authorizer") {
		t.Errorf("the node logged %q; an endpoint was configured", got)
	}
	for _, base := range []string{publicURL, internalURL} {
		if code, body := get(t, base+"/readyz"); code != 200 || body != "ok\n" {
			t.Errorf("GET %s/readyz = %d %q; a flapping endpoint fails requests, not replicas", base, code, body)
		}
	}
}
