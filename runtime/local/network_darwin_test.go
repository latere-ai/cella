// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package local

import (
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/hostsandbox"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// TestAnyUnderSeatbelt is the any row of the network table: under Seatbelt,
// in this test binary that calls hostsandbox.Main, a host whose network is
// any declares open, registers its deny list, narrows each ask to it, and
// renders a command's network as a deny list with the ports it admits. The
// file builds on macOS alone, where Seatbelt runs.
func TestAnyUnderSeatbelt(t *testing.T) {
	withIsolation(t, hostsandbox.IsolationSeatbelt)
	h := newHost(t)
	host := Network{Mode: NetworkAny, Hosts: []string{"github.com:22"}, Blocked: []string{"Pastebin.com", "*.example.net"}}
	d := h.open(t, Options{Network: host, Lookup: os.LookupEnv})
	if caps := d.Capabilities(); !slices.Contains(caps.Egress, v1.EgressOpen) || !caps.EgressLocal {
		t.Errorf("a host whose network is any declares %+v", caps)
	}
	if n := d.Host().Network; n.Mode != string(v1.EgressOpen) || !slices.Equal(n.Blocked, []string{"pastebin.com", "*.example.net"}) ||
		!slices.Equal(n.Hosts, []string{"github.com:22"}) {
		t.Errorf("the host registers %+v", n)
	}
	for _, c := range []struct {
		name  string
		asked v1.Egress
		want  hostsandbox.Network
	}{
		{"none asked", v1.Egress{Mode: v1.EgressNone}, hostsandbox.Network{Mode: hostsandbox.NetworkNone}},
		{"allowlist asked", v1.Egress{Mode: v1.EgressAllowlist, AllowedHosts: []string{"github.com", "pastebin.com", "a.example.net"}},
			hostsandbox.Network{Mode: hostsandbox.NetworkAllowlist, Domains: []string{"github.com"}}},
		{"open asked", v1.Egress{Mode: v1.EgressOpen},
			hostsandbox.Network{Mode: hostsandbox.NetworkDenylist, Domains: []string{"github.com:22"}, Blocked: []string{"pastebin.com", "*.example.net"}}},
		{"open asked denying", v1.Egress{Mode: v1.EgressOpen, DeniedHosts: []string{"evil.example.org"}},
			hostsandbox.Network{Mode: hostsandbox.NetworkDenylist, Domains: []string{"github.com:22"}, Blocked: []string{"evil.example.org", "pastebin.com", "*.example.net"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			resolved, err := resolveOn(t, d, v1.SandboxSpec{Network: v1.Network{Egress: c.asked}})
			if err != nil {
				t.Fatalf("resolve refused %+v: %v", c.asked, err)
			}
			got := resolved.Sandbox.Spec.Network.Egress
			id := "sbx_any"
			sandbox(t, d, driver.CreateSpec{ID: id, Egress: driver.Egress{Mode: string(got.Mode), AllowedHosts: got.AllowedHosts, DeniedHosts: got.DeniedHosts}})
			n := d.stage(mustLoad(t, d, id), "probe", []string{"true"}, nil, "/").Network
			if n.Mode != c.want.Mode || !slices.Equal(n.Domains, c.want.Domains) || !slices.Equal(n.Blocked, c.want.Blocked) {
				t.Errorf("the driver renders %+v, want %+v", n, c.want)
			}
		})
	}
}

// TestARefusalIsReported: a command under Seatbelt that asks its network for
// a host it does not admit is refused, and the driver reports the host, the
// port and the sandbox, under an allow list and under any.
func TestARefusalIsReported(t *testing.T) {
	withIsolation(t, hostsandbox.IsolationSeatbelt)
	h := newHost(t)
	for _, c := range []struct {
		name  string
		host  Network
		asked driver.Egress
	}{
		{"allowlist", Network{Mode: NetworkAllowlist, Hosts: []string{"github.com"}}, driver.Egress{Mode: string(v1.EgressOpen)}},
		{"any", Network{Mode: NetworkAny, Blocked: []string{"pastebin.com"}}, driver.Egress{Mode: string(v1.EgressOpen)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				refused []Refusal
			)
			d := h.open(t, Options{Network: c.host, Lookup: os.LookupEnv, OnRefused: func(r Refusal) {
				mu.Lock()
				refused = append(refused, r)
				mu.Unlock()
			}})
			sandbox(t, d, driver.CreateSpec{ID: "sbx_refused", Labels: map[string]string{"session": "ses_r"}, Egress: c.asked})
			if _, errOut, code, err := run(t, d, "sbx_refused", driver.ExecRequest{Command: []string{
				"/usr/bin/curl", "-sS", "-o", "/dev/null", "--max-time", "10", "https://pastebin.com/",
			}}); err != nil || code == 0 {
				t.Fatalf("a refused host was reached: exit %d, %v, %s", code, err, errOut)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				mu.Lock()
				got := slices.Clone(refused)
				mu.Unlock()
				if len(got) > 0 {
					if r := got[0]; r.Sandbox != "sbx_refused" || r.Labels["session"] != "ses_r" || r.Host != "pastebin.com" || r.Port != 443 {
						t.Errorf("the refusal reads %+v", r)
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("no refusal was reported")
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}
