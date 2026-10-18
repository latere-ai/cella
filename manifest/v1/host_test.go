// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"slices"
	"testing"
)

// TestGrantPicksTheLongestFolder: a path is covered by the grant it is in,
// and of two the longer decides, so a read-only folder inside a writable one
// stays read-only; a path beside a grant, or one that climbs out of it, is in
// none.
func TestGrantPicksTheLongestFolder(t *testing.T) {
	h := HostInfo{Grants: []HostGrant{{Path: "/home/dev", Write: true}, {Path: "/home/dev/vendor"}}}
	for _, c := range []struct {
		path  string
		held  bool
		write bool
	}{
		{"/home/dev", true, true},
		{"/home/dev/site/index.html", true, true},
		{"/home/dev/vendor/lib", true, false},
		{"/home/devil", false, false},
		{"/home/dev/../etc", false, false},
		{"/etc", false, false},
	} {
		g, held := h.Grant(c.path)
		if held != c.held || g.Write != c.write {
			t.Errorf("%s gives %+v %v, want held %v write %v", c.path, g, held, c.held, c.write)
		}
	}
	if Within("", "/a") || Within("/a", "") || !Within("/", "/anything") {
		t.Error("Within reads an empty path or the root wrongly")
	}
}

// TestHostNetworkOfIsWhatEveryHostAllows: the hosts every host names, none
// when one allows none, and no report when no host reports a network.
func TestHostNetworkOfIsWhatEveryHostAllows(t *testing.T) {
	a := HostInfo{Network: HostNetwork{Mode: "allowlist", Hosts: []string{"github.com", "pypi.org"}}}
	b := HostInfo{Network: HostNetwork{Mode: "allowlist", Hosts: []string{"github.com"}}}
	none := HostInfo{Network: HostNetwork{Mode: "none"}}
	silent := HostInfo{}
	if got, ok := HostNetworkOf([]HostInfo{a, b, silent}); !ok || got.Mode != "allowlist" || !slices.Equal(got.Hosts, []string{"github.com"}) {
		t.Errorf("two allow lists give %+v %v", got, ok)
	}
	if got, ok := HostNetworkOf([]HostInfo{a, none}); !ok || got.Mode != "none" {
		t.Errorf("an allow list and none give %+v %v", got, ok)
	}
	if _, ok := HostNetworkOf([]HostInfo{silent}); ok {
		t.Error("a host that reports no network reported one")
	}
	if _, ok := HostNetworkOf(nil); ok {
		t.Error("no host reported a network")
	}
}

// TestNarrowEgressFollowsTheTable is spec 081's network table as a function:
// a host that allows none gives none whatever was asked; a host with an
// allow list keeps the asked hosts it covers, gives its own list to open
// less what the denied list names, and none to none.
func TestNarrowEgressFollowsTheTable(t *testing.T) {
	list := HostNetwork{Mode: "allowlist", Hosts: []string{"github.com", "*.npmjs.org"}}
	for _, c := range []struct {
		name  string
		asked Egress
		host  HostNetwork
		want  Egress
	}{
		{"none host, none asked", Egress{Mode: EgressNone}, HostNetwork{Mode: "none"}, Egress{Mode: EgressNone}},
		{"none host, allowlist asked", Egress{Mode: EgressAllowlist, AllowedHosts: []string{"github.com"}}, HostNetwork{Mode: "none"}, Egress{Mode: EgressNone}},
		{"none host, open asked", Egress{Mode: EgressOpen}, HostNetwork{Mode: "none"}, Egress{Mode: EgressNone}},
		{"allowlist host, none asked", Egress{Mode: EgressNone}, list, Egress{Mode: EgressNone}},
		{"allowlist host, allowlist asked", Egress{Mode: EgressAllowlist, AllowedHosts: []string{"github.com", "example.com", "registry.npmjs.org"}}, list,
			Egress{Mode: EgressAllowlist, AllowedHosts: []string{"github.com", "registry.npmjs.org"}}},
		{"allowlist host, allowlist asked outside it", Egress{Mode: EgressAllowlist, AllowedHosts: []string{"example.com"}}, list, Egress{Mode: EgressNone}},
		{"allowlist host, open asked", Egress{Mode: EgressOpen}, list, Egress{Mode: EgressAllowlist, AllowedHosts: []string{"github.com", "*.npmjs.org"}}},
		{"allowlist host, open asked denying it all", Egress{Mode: EgressOpen, DeniedHosts: []string{"github.com", "*.npmjs.org"}}, list, Egress{Mode: EgressNone}},
	} {
		got := NarrowEgress(c.asked, c.host)
		if got.Mode != c.want.Mode || !slices.Equal(got.AllowedHosts, c.want.AllowedHosts) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// TestHostInfoEqual compares every fact a replacement is decided on.
func TestHostInfoEqual(t *testing.T) {
	a := HostInfo{OS: "linux", Arch: "amd64", Grants: []HostGrant{{Path: "/p", Write: true}}, Network: HostNetwork{Mode: "allowlist", Hosts: []string{"x.example"}}}
	b := a
	b.Grants = slices.Clone(a.Grants)
	if !a.Equal(b) {
		t.Error("two equal hosts differ")
	}
	for _, change := range []func(*HostInfo){
		func(h *HostInfo) { h.OS = "darwin" },
		func(h *HostInfo) { h.Arch = "arm64" },
		func(h *HostInfo) { h.Grants = []HostGrant{{Path: "/p"}} },
		func(h *HostInfo) { h.Network.Mode = "none" },
		func(h *HostInfo) { h.Network.Hosts = nil },
	} {
		c := a
		c.Grants = slices.Clone(a.Grants)
		change(&c)
		if a.Equal(c) {
			t.Errorf("a host that changed reads equal: %+v", c)
		}
	}
}
