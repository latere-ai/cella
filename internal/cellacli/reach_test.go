// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli_test

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/cella/internal/cellacli"
)

// TestTheFlagsOverrideTheVariables is design 011's reaching rule on the
// command: each flag overrides the variable of the same meaning, a token
// from either wins over a token file from either, and the file is read when
// no token is set.
func TestTheFlagsOverrideTheVariables(t *testing.T) {
	var mu sync.Mutex
	var bearer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearer = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"next":""}`))
	}))
	t.Cleanup(server.Close)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		flags []string
		vars  map[string]string
		want  string
	}{{
		name:  "the token flag wins over the variable",
		flags: []string{"--token", "from-the-flag"},
		vars:  map[string]string{"CELLA_URL": server.URL, "CELLA_TOKEN": "from-the-variable"},
		want:  "from-the-flag",
	}, {
		name:  "the token variable wins over the file flag",
		flags: []string{"--token-file", file},
		vars:  map[string]string{"CELLA_URL": server.URL, "CELLA_TOKEN": "from-the-variable"},
		want:  "from-the-variable",
	}, {
		name:  "the file flag is read when no token is set",
		flags: []string{"--token-file", file},
		vars:  map[string]string{"CELLA_URL": server.URL},
		want:  "from-the-file",
	}, {
		name:  "the address flag wins over the variable",
		flags: []string{"--url", server.URL, "--token", "t"},
		vars:  map[string]string{"CELLA_URL": "http://127.0.0.1:1"},
		want:  "t",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"get", "sandboxes"}, tc.flags...)
			got := runWith(t, cellacli.Env{Args: args, Getenv: environment(tc.vars)})
			if got.code != 0 {
				t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
			}
			mu.Lock()
			defer mu.Unlock()
			if bearer != "Bearer "+tc.want {
				t.Fatalf("the request carried %q, want the bearer %q", bearer, tc.want)
			}
		})
	}
	bad := runWith(t, cellacli.Env{Args: []string{"get", "sandboxes", "--url", "ftp://example.com", "--token", "t"}, Getenv: environment(nil)})
	if bad.code != 2 || !strings.Contains(bad.stderr, "no http or https address") {
		t.Fatalf("an address of another scheme exited %d with %q", bad.code, bad.stderr)
	}
}

// TestTheCertificateAuthorityFlagIsReadBeforeACall: --ca adds one authority
// beside the system roots, and a file that is not there or holds no
// certificate is a usage error before any request.
func TestTheCertificateAuthorityFlagIsReadBeforeACall(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"next":""}`))
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	authority := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(authority, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	noCertificate := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(noCertificate, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := environment(map[string]string{"CELLA_URL": server.URL, "CELLA_TOKEN": "t"})
	if got := runWith(t, cellacli.Env{Args: []string{"get", "sandboxes"}, Getenv: vars}); got.code != 7 {
		t.Fatalf("a certificate nothing trusts exited %d, want 7: %q", got.code, got.stderr)
	}
	if got := runWith(t, cellacli.Env{Args: []string{"get", "sandboxes", "--ca", authority}, Getenv: vars}); got.code != 0 {
		t.Fatalf("the authority --ca named was not trusted: exit %d, %q", got.code, got.stderr)
	}
	for name, tc := range map[string]struct{ path, want string }{
		"a file that is not there":   {filepath.Join(dir, "absent.pem"), "certificate authority"},
		"a file with no certificate": {noCertificate, "holds no certificate"},
	} {
		got := runWith(t, cellacli.Env{Args: []string{"get", "sandboxes", "--ca", tc.path}, Getenv: vars})
		if got.code != 2 || !strings.Contains(got.stderr, tc.want) {
			t.Errorf("%s exited %d with %q", name, got.code, got.stderr)
		}
	}
}

// TestTheCommandReadsTheKindsItServesSingularOrPlural: a person writes
// either form and any case, and a kind this command does not serve is a
// usage error that names the ones it does.
func TestTheCommandReadsTheKindsItServesSingularOrPlural(t *testing.T) {
	p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"next":""}`))
	})
	for _, tc := range []struct{ kind, path string }{
		{"sandbox", "/v1/sandboxes"}, {"Sandboxes", "/v1/sandboxes"}, {"secret", "/v1/secrets"}, {"SECRETS", "/v1/secrets"},
	} {
		if got := p.run(t, "", "get", tc.kind); got.code != 0 || p.last().Path != tc.path {
			t.Errorf("get %s exited %d and called %s", tc.kind, got.code, p.last().Path)
		}
	}
	for _, kind := range []string{"volume", "sandboxs", "environments"} {
		got := p.run(t, "", "get", kind)
		if got.code != 2 || !strings.Contains(got.stderr, "sandbox, secret") {
			t.Errorf("get %s exited %d with %q", kind, got.code, got.stderr)
		}
	}
}

// TestNoBearerIsNamedByItsVariables: a command with no token anywhere is a
// usage error that names the two variables to set, and it sends no request
// without a bearer.
func TestNoBearerIsNamedByItsVariables(t *testing.T) {
	p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"next":""}`))
	})
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{"a file that is not there": filepath.Join(dir, "absent"), "an empty file": empty} {
		got := runWith(t, cellacli.Env{Args: []string{"get", "sandboxes"}, Getenv: environment(map[string]string{
			"CELLA_URL": p.server.URL, "CELLA_TOKEN_FILE": file,
		})})
		if got.code != 2 || !strings.Contains(got.stderr, "CELLA_TOKEN,") || !strings.Contains(got.stderr, "CELLA_TOKEN_FILE") {
			t.Errorf("%s exited %d with %q", name, got.code, got.stderr)
		}
	}
	if len(p.seen()) != 0 {
		t.Fatal("a command with no bearer reached the server")
	}
}
