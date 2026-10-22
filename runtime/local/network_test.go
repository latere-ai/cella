// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package local

import (
	"errors"
	"os"
	"slices"
	"testing"

	"latere.ai/x/pkg/hostsandbox"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// TestAnyIsRefusedUnderSRT: a network of any host under srt, which admits
// only the hosts it is given, is refused when the driver is built, with an
// error that says the network cannot be enforced, never run as something
// wider or narrower.
func TestAnyIsRefusedUnderSRT(t *testing.T) {
	h := newHost(t)
	_, err := New(Options{DataDir: tempDir(t), Home: h.home, Isolation: hostsandbox.IsolationSRT, Network: Network{Mode: NetworkAny, Blocked: []string{"pastebin.com"}}})
	if !errors.Is(err, ErrNetworkUnenforceable) || !errors.Is(err, hostsandbox.ErrUnenforceableNetwork) {
		t.Fatalf("any under srt built with %v", err)
	}
	// The sandbox runtime's stand-in is srt as well.
	shim(t)
	if _, err := New(Options{DataDir: tempDir(t), Home: h.home, Network: Network{Mode: NetworkAny}}); !errors.Is(err, ErrNetworkUnenforceable) {
		t.Errorf("any under the stand-in for srt built with %v", err)
	}
}

// TestADenyListIsCheckedWhereItIsRead: a deny list on a network other than
// any, and an entry hostsandbox would refuse at a command's start, are
// refused when the driver is built.
func TestADenyListIsCheckedWhereItIsRead(t *testing.T) {
	shim(t)
	h := newHost(t)
	if _, err := New(Options{DataDir: tempDir(t), Home: h.home, Network: Network{Mode: NetworkAllowlist, Hosts: []string{"github.com"}, Blocked: []string{"pastebin.com"}}}); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a deny list on an allow list built with %v", err)
	}
	for _, entry := range []string{"2130706433", "0x7f.0.0.1", "exa mple.com", "example.com:0", "example.com:65536", "-example.com", "fe80::1%en0"} {
		if validEntry(entry) {
			t.Errorf("the entry %q reads as valid", entry)
		}
	}
	for _, entry := range []string{"pastebin.com", "*.example.com", "example.com:8443", "203.0.113.7", "2001:db8::1", "[2001:db8::1]:443", "Example.COM."} {
		if !validEntry(entry) {
			t.Errorf("the entry %q reads as invalid", entry)
		}
	}
}

// TestHostEntry: a refusal on 443 or 80 is allowed by the host alone, and
// one on any other port by host:port; what is not a host or a port is
// refused.
func TestHostEntry(t *testing.T) {
	for _, c := range []struct {
		host string
		port int
		want string
	}{
		{"Example.com.", 443, "example.com"},
		{"example.com", 80, "example.com"},
		{"example.com", 22, "example.com:22"},
		{"2001:db8::1", 8443, "[2001:db8::1]:8443"},
	} {
		if got, err := HostEntry(c.host, c.port); err != nil || got != c.want {
			t.Errorf("HostEntry(%q, %d) = %q, %v; want %q", c.host, c.port, got, err, c.want)
		}
	}
	for _, c := range []struct {
		host string
		port int
	}{{"exa mple.com", 443}, {"", 443}, {"example.com", 0}, {"example.com", 70000}, {"10", 443}} {
		if _, err := HostEntry(c.host, c.port); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("HostEntry(%q, %d) answered %v", c.host, c.port, err)
		}
	}
}

// TestWiden: under an allow list an allowed entry joins it; under a deny
// list it lifts the host's own block by name, narrowing a block on every
// port to the ports still refused, admits a port beyond the standard ones,
// and lifts neither a wildcard nor a block the sandbox's manifest asked for.
func TestWiden(t *testing.T) {
	list := widen(hostsandbox.Network{Mode: hostsandbox.NetworkAllowlist, Domains: []string{"github.com"}}, nil, []string{"example.com", "github.com", "example.com:22"})
	if !slices.Equal(list.Domains, []string{"github.com", "example.com", "example.com:22"}) {
		t.Errorf("an allow list widened to %v", list.Domains)
	}
	own := []string{"pastebin.com", "gist.example.org", "*.example.net", "paste.example.com:8443"}
	deny := func() hostsandbox.Network {
		return hostsandbox.Network{
			Mode: hostsandbox.NetworkDenylist, Domains: []string{"gist.example.org:22"},
			Blocked: []string{"pastebin.com", "gist.example.org", "*.example.net", "paste.example.com:8443", "denied.example.com"},
		}
	}
	for _, c := range []struct {
		name        string
		allowed     []string
		wantBlocked []string
		wantDomains []string
	}{
		{"a host on the standard ports", []string{"pastebin.com"},
			[]string{"gist.example.org", "*.example.net", "paste.example.com:8443", "denied.example.com"}, []string{"gist.example.org:22"}},
		{"a host blocked on every port, on the standard ports", []string{"gist.example.org"},
			[]string{"pastebin.com", "gist.example.org:22", "*.example.net", "paste.example.com:8443", "denied.example.com"}, []string{"gist.example.org:22"}},
		{"a host blocked on every port, on another port", []string{"pastebin.com:8080"},
			[]string{"pastebin.com:443", "pastebin.com:80", "gist.example.org", "*.example.net", "paste.example.com:8443", "denied.example.com"},
			[]string{"gist.example.org:22", "pastebin.com:8080"}},
		{"a host blocked on one port", []string{"paste.example.com:8443"},
			[]string{"pastebin.com", "gist.example.org", "*.example.net", "denied.example.com"}, []string{"gist.example.org:22", "paste.example.com:8443"}},
		{"a host a wildcard blocks", []string{"a.example.net"},
			deny().Blocked, []string{"gist.example.org:22"}},
		{"a host the manifest denied", []string{"denied.example.com"},
			deny().Blocked, []string{"gist.example.org:22"}},
		{"a port no entry blocks", []string{"github.com:22"},
			deny().Blocked, []string{"gist.example.org:22", "github.com:22"}},
	} {
		got := widen(deny(), own, c.allowed)
		if !slices.Equal(got.Blocked, c.wantBlocked) || !slices.Equal(got.Domains, c.wantDomains) {
			t.Errorf("%s: blocked %v and domains %v, want %v and %v", c.name, got.Blocked, got.Domains, c.wantBlocked, c.wantDomains)
		}
	}
	if none := widen(hostsandbox.Network{Mode: hostsandbox.NetworkNone}, nil, []string{"example.com"}); none.Mode != hostsandbox.NetworkNone || len(none.Domains) != 0 {
		t.Errorf("no network widened to %+v", none)
	}
}

// TestAllowedWidensOneSandbox: what Allowed names for a sandbox reaches its
// next command alone; a sandbox with no network is not widened; and each
// command with a network reports its refusals with its sandbox and labels,
// while one with none reports nothing.
func TestAllowedWidensOneSandbox(t *testing.T) {
	shim(t)
	h := newHost(t)
	var refused []Refusal
	d := h.open(t, Options{
		Lookup: os.LookupEnv, Network: Network{Mode: NetworkAllowlist, Hosts: []string{"github.com", "example.com"}},
		Allowed: func(sandbox string, labels map[string]string) []string {
			if labels["session"] == "ses_a" {
				return []string{"example.com:22"}
			}
			return nil
		},
		OnRefused: func(r Refusal) { refused = append(refused, r) },
	})
	asked := driver.Egress{Mode: string(v1.EgressAllowlist), AllowedHosts: []string{"github.com"}}
	sandbox(t, d, driver.CreateSpec{ID: "sbx_a", Labels: map[string]string{"session": "ses_a"}, Egress: asked})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_b", Labels: map[string]string{"session": "ses_b"}, Egress: asked})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_none", Labels: map[string]string{"session": "ses_a"}, Egress: driver.Egress{Mode: string(v1.EgressNone)}})
	a := d.stage(mustLoad(t, d, "sbx_a"), "probe", []string{"true"}, nil, "/")
	b := d.stage(mustLoad(t, d, "sbx_b"), "probe", []string{"true"}, nil, "/")
	none := d.stage(mustLoad(t, d, "sbx_none"), "probe", []string{"true"}, nil, "/")
	if !slices.Equal(a.Network.Domains, []string{"github.com", "example.com:22"}) {
		t.Errorf("the allowed sandbox's command reaches %v", a.Network.Domains)
	}
	if !slices.Equal(b.Network.Domains, []string{"github.com"}) {
		t.Errorf("another session's command reaches %v", b.Network.Domains)
	}
	if none.Network.Mode != hostsandbox.NetworkNone || none.OnRefused != nil {
		t.Errorf("a sandbox with no network renders %+v, reporting %v", none.Network, none.OnRefused != nil)
	}
	if a.OnRefused == nil {
		t.Fatal("a command with a network reports no refusal")
	}
	a.OnRefused("pastebin.com", 443)
	if len(refused) != 1 || refused[0].Sandbox != "sbx_a" || refused[0].Labels["session"] != "ses_a" ||
		refused[0].Host != "pastebin.com" || refused[0].Port != 443 {
		t.Errorf("the refusal reads %+v", refused)
	}
}

// TestACommandGetsTheHostsNetworkNow: a sandbox's record keeps the egress it
// got when it was created, and a command started after the host narrowed
// gets the narrower network.
func TestACommandGetsTheHostsNetworkNow(t *testing.T) {
	shim(t)
	h := newHost(t)
	data := tempDir(t)
	wide := h.open(t, Options{DataDir: data, Lookup: os.LookupEnv, Network: Network{Mode: NetworkAllowlist, Hosts: []string{"github.com", "example.com"}}})
	sandbox(t, wide, driver.CreateSpec{ID: "sbx_wide", Egress: driver.Egress{Mode: string(v1.EgressOpen)}})
	narrow := h.open(t, Options{DataDir: data, Lookup: os.LookupEnv, Network: Network{Mode: NetworkAllowlist, Hosts: []string{"github.com"}}})
	if got := narrow.stage(mustLoad(t, narrow, "sbx_wide"), "probe", []string{"true"}, nil, "/").Network; !slices.Equal(got.Domains, []string{"github.com"}) {
		t.Errorf("a command after the host narrowed reaches %v", got.Domains)
	}
	closed := h.open(t, Options{DataDir: data, Lookup: os.LookupEnv})
	if got := closed.stage(mustLoad(t, closed, "sbx_wide"), "probe", []string{"true"}, nil, "/").Network; got.Mode != hostsandbox.NetworkNone {
		t.Errorf("a command after the host allowed none renders %+v", got)
	}
}

// TestNetworkAllow: allowing a host saves it as a person would have it: on
// an allow list it joins the hosts; on any it lifts the host's block and
// admits a port beyond the standard ones; none and a wildcard's block are
// refused.
func TestNetworkAllow(t *testing.T) {
	list, err := Network{Mode: NetworkAllowlist, Hosts: []string{"github.com"}}.Allow("example.com:22")
	if err != nil || !slices.Equal(list.Hosts, []string{"github.com", "example.com:22"}) {
		t.Errorf("an allow list allowed %+v, %v", list, err)
	}
	anyHost := Network{Mode: NetworkAny, Hosts: []string{"gist.example.org:22"}, Blocked: []string{"pastebin.com", "gist.example.org", "*.example.net"}}
	got, err := anyHost.Allow("pastebin.com")
	if err != nil || !slices.Equal(got.Blocked, []string{"gist.example.org", "*.example.net"}) || !slices.Equal(got.Hosts, anyHost.Hosts) {
		t.Errorf("any allowed a blocked host as %+v, %v", got, err)
	}
	got, err = anyHost.Allow("gist.example.org:8443")
	if err != nil || !slices.Equal(got.Blocked, []string{"pastebin.com", "gist.example.org:443", "gist.example.org:80", "gist.example.org:22", "*.example.net"}) ||
		!slices.Equal(got.Hosts, []string{"gist.example.org:22", "gist.example.org:8443"}) {
		t.Errorf("any allowed one port of a host blocked on every port as %+v, %v", got, err)
	}
	if _, err := anyHost.Allow("a.example.net"); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a host a wildcard blocks was allowed: %v", err)
	}
	if _, err := anyHost.Allow("example.net"); err != nil {
		t.Errorf("the host a wildcard does not cover was refused: %v", err)
	}
	if _, err := (Network{Mode: NetworkNone}).Allow("example.com"); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("none allowed a host: %v", err)
	}
}
