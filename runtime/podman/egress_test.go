// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"testing"
	"time"

	"latere.ai/x/cella/egress"
	driver "latere.ai/x/cella/runtime"
)

const testCAPEM = "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n"

// TestPodmanProjectsTheGateway is the driver's half of the boundary: the
// workload is pointed at the two doors with its own credential, and the
// authority the proxy door signs with is inside the container before it
// starts.
func TestPodmanProjectsTheGateway(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{
		ID: "sbx_a", Name: "one", Owner: "alice@example.com", Image: "img",
		Env: map[string]string{"GREETING": "hello"},
		Egress: driver.Egress{
			Mode: "allowlist", AllowedHosts: []string{"api.example.com"},
			ProxyAddr: "gateway.example.internal:3128", ReverseAddr: "gateway.example.internal:8080",
			Credential: "abc", CAPEM: testCAPEM,
		},
	})
	c := f.container(t, "sbx_a")
	if c.env["GREETING"] != "hello" {
		t.Fatalf("the workload's own environment was lost: %v", c.env)
	}
	want := "http://sandbox:abc@gateway.example.internal:3128"
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if c.env[key] != want {
			t.Errorf("%s = %q, want %q", key, c.env[key], want)
		}
	}
	if c.env["SSL_CERT_FILE"] != egress.CAPath || c.env["CURL_CA_BUNDLE"] != egress.CAPath {
		t.Errorf("the trust variables name %q, want %q", c.env["SSL_CERT_FILE"], egress.CAPath)
	}
	if c.env["CELLA_GATEWAY_URL"] != "http://gateway.example.internal:8080" || c.env["CELLA_GATEWAY_CREDENTIAL"] != "abc" {
		t.Errorf("the reverse door = %q, %q", c.env["CELLA_GATEWAY_URL"], c.env["CELLA_GATEWAY_CREDENTIAL"])
	}
	file, ok := c.files[egress.CAPath]
	if !ok {
		t.Fatalf("the authority is not at %s; the container holds %v", egress.CAPath, c.files)
	}
	if string(file.body) != testCAPEM {
		t.Errorf("the authority = %q", file.body)
	}
	if file.mode != 0o444 {
		t.Errorf("the authority is mode %o, want it read-only to the workload", file.mode)
	}
}

// TestPodmanWithoutAGatewaySetsNothing keeps an installation that runs no
// gateway unchanged.
func TestPodmanWithoutAGatewaySetsNothing(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Name: "one", Owner: "alice@example.com", Image: "img", Env: map[string]string{"GREETING": "hello"}})
	c := f.container(t, "sbx_a")
	for _, key := range egress.ReservedEnv() {
		if _, ok := c.env[key]; ok {
			t.Errorf("%s was set with no gateway", key)
		}
	}
	if _, ok := c.files[egress.CAPath]; ok {
		t.Error("an authority was projected with no gateway")
	}
}

// TestPodmanRefusesACreateThatCannotBeProjected leaves no container behind
// when the authority cannot be written: a sandbox that cannot trust its
// gateway is not a sandbox inside its boundary.
func TestPodmanRefusesACreateThatCannotBeProjected(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	f.fault("PUT "+compat+"/containers/"+containerName("sbx_a")+"/archive", 500)
	_, err := d.Create(t.Context(), driver.CreateSpec{
		ID: "sbx_a", Name: "one", Owner: "alice@example.com", Image: "img",
		Egress: driver.Egress{ProxyAddr: "gateway.example.internal:3128", Credential: "abc", CAPEM: testCAPEM},
	})
	if err == nil {
		t.Fatal("a create whose authority could not be projected was reported as a success")
	}
	if f.containerCount() != 0 {
		t.Fatal("the refused create left a container behind")
	}
}

// TestPodmanProjectsTheTrustBundle: the file the trust variables name holds
// the driver's public roots and then the gateway's authority, so a host the
// gateway tunnels verifies against the roots and one it terminates against
// the authority.
func TestPodmanProjectsTheTrustBundle(t *testing.T) {
	f := newFake(t)
	const roots = "-----BEGIN CERTIFICATE-----\nroots\n-----END CERTIFICATE-----\n"
	d, err := New(Options{Socket: f.socket, PullTimeout: 5 * time.Second, TrustRoots: []byte(roots)})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Preflight(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	create(t, d, driver.CreateSpec{
		ID: "sbx_a", Name: "one", Owner: "alice@example.com", Image: "img",
		Egress: driver.Egress{ProxyAddr: "gateway.example.internal:3128", Credential: "abc", CAPEM: testCAPEM},
	})
	file, ok := f.container(t, "sbx_a").files[egress.CAPath]
	if !ok {
		t.Fatalf("no trust file at %s", egress.CAPath)
	}
	if string(file.body) != roots+testCAPEM {
		t.Fatalf("the trust file = %q, want the roots and then the authority", file.body)
	}
}
