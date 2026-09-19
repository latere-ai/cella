// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/cella/egress"
	driver "latere.ai/x/cella/runtime"
)

const testCAPEM = "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n"

// TestNativeProjectsTheGateway is the driver's half of the boundary. The
// native environment has no mount namespace, so the authority lives beside
// the sandbox's own directory and the trust variables name that path; what a
// container gets at a reserved path, a process gets at a real one.
func TestNativeProjectsTheGateway(t *testing.T) {
	root := t.TempDir()
	d, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	spec := driver.CreateSpec{
		ID: "sbx_a", Name: "one", Owner: "alice@example.com",
		Env: map[string]string{"GREETING": "hello"},
		Egress: driver.Egress{
			Mode: "allowlist", AllowedHosts: []string{"api.example.com"},
			ProxyAddr: "127.0.0.1:3128", ReverseAddr: "127.0.0.1:8080",
			Credential: "abc", CAPEM: testCAPEM,
		},
	}
	if _, err = d.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	rec, err := d.load("sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Env["GREETING"] != "hello" {
		t.Fatalf("the workload's own environment was lost: %v", rec.Env)
	}
	want := "http://sandbox:abc@127.0.0.1:3128"
	if rec.Env["HTTPS_PROXY"] != want || rec.Env["https_proxy"] != want {
		t.Fatalf("the proxy = %q, want %q", rec.Env["HTTPS_PROXY"], want)
	}
	path := filepath.Join(root, "sbx_a", caFile)
	if rec.Env["SSL_CERT_FILE"] != path {
		t.Fatalf("the trust variables name %q, want %q", rec.Env["SSL_CERT_FILE"], path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != testCAPEM {
		t.Fatalf("the authority = %q", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("the authority is mode %o, want it read-only", info.Mode().Perm())
	}
	// Every key the projection set is one a manifest may not set, so the
	// workload's own environment can never collide with the boundary's.
	for key := range rec.Env {
		if key == "GREETING" {
			continue
		}
		if !strings.Contains(strings.Join(egress.ReservedEnv(), " "), key) {
			t.Errorf("the driver set %q, which the boundary does not reserve", key)
		}
	}
}

// TestNativeWithoutAGatewaySetsNothing keeps an installation that runs no
// gateway unchanged.
func TestNativeWithoutAGatewaySetsNothing(t *testing.T) {
	root := t.TempDir()
	d, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if _, err = d.Create(t.Context(), driver.CreateSpec{ID: "sbx_a", Name: "one", Owner: "alice@example.com", Env: map[string]string{"GREETING": "hello"}}); err != nil {
		t.Fatal(err)
	}
	rec, err := d.load("sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range egress.ReservedEnv() {
		if _, ok := rec.Env[key]; ok {
			t.Errorf("%s was set with no gateway", key)
		}
	}
	if _, err = os.Stat(filepath.Join(root, "sbx_a", caFile)); !os.IsNotExist(err) {
		t.Error("an authority was projected with no gateway")
	}
}
