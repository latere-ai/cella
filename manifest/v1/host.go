// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"path"
	"slices"
	"strings"
)

// HostInfo is what a worker reports about the machine it runs on: the
// operating system and architecture, the host folders it grants a sandbox,
// and the network it allows (spec 082). A driver that confines a process on
// a machine somebody owns reports it, and the control plane resolves a
// manifest's workspace.host and egress against what every live worker
// reports. A driver that reports nothing serves no workspace.host.
type HostInfo struct {
	OS      string      `json:"os"`
	Arch    string      `json:"arch"`
	Grants  []HostGrant `json:"grants,omitempty"`
	Network HostNetwork `json:"network"`
}

// HostGrant is one host folder a sandbox may use as its workspace: an
// absolute, clean path, writable or read-only.
type HostGrant struct {
	Path  string `json:"path"`
	Write bool   `json:"write,omitempty"`
}

// HostNetwork is the network the host allows a sandbox: none, or an allow
// list of hosts. It narrows what a manifest asks for and never widens it.
type HostNetwork struct {
	Mode  string   `json:"mode"`
	Hosts []string `json:"hosts,omitempty"`
}

// HostStatus is one live worker's host as an environment's status.hosts
// lists it.
type HostStatus struct {
	Worker string   `json:"worker"`
	Host   HostInfo `json:"host"`
}

// Equal reports whether two hosts report the same facts: the operating
// system, the architecture, the grants and the network, each list compared
// in order. A worker reconnecting from the same machine reports the same
// facts, and one using a copied key from another machine does not.
func (h HostInfo) Equal(other HostInfo) bool {
	return h.OS == other.OS && h.Arch == other.Arch &&
		slices.Equal(h.Grants, other.Grants) &&
		h.Network.Mode == other.Network.Mode && slices.Equal(h.Network.Hosts, other.Network.Hosts)
}

// Grant is the grant that covers p, a clean absolute path, and whether one
// does. Where two grants cover it the longer one decides, so a read-only
// folder inside a writable one stays read-only.
func (h HostInfo) Grant(p string) (HostGrant, bool) {
	var found HostGrant
	held := false
	for _, g := range h.Grants {
		if !Within(g.Path, p) {
			continue
		}
		if !held || len(g.Path) > len(found.Path) {
			found, held = g, true
		}
	}
	return found, held
}

// Within reports whether p is root or a path below it. Both are compared as
// clean slash-separated paths, so "/a/bc" is not within "/a/b" and a path
// that climbs out with ".." is not within anything it leaves.
func Within(root, p string) bool {
	if root == "" || p == "" {
		return false
	}
	root, p = path.Clean(root), path.Clean(p)
	return p == root || root == "/" || strings.HasPrefix(p, root+"/")
}

// HostNetworkOf is what every one of hosts that reports a network allows:
// none when any of them allows none, and otherwise an allow list of the hosts
// every one of them names. A host whose network mode is empty reports no
// network of its own and narrows nothing. The second result is false when no
// host reports a network, which is an environment held to the egress modes
// it declares.
func HostNetworkOf(hosts []HostInfo) (HostNetwork, bool) {
	var out HostNetwork
	reported := false
	for _, h := range hosts {
		switch {
		case h.Network.Mode == "":
			continue
		case h.Network.Mode != string(EgressAllowlist):
			return HostNetwork{Mode: string(EgressNone)}, true
		case !reported:
			out = HostNetwork{Mode: string(EgressAllowlist), Hosts: slices.Clone(h.Network.Hosts)}
			reported = true
		default:
			out.Hosts = slices.DeleteFunc(out.Hosts, func(host string) bool { return !slices.Contains(h.Network.Hosts, host) })
		}
	}
	return out, reported
}

// NarrowEgress is what a sandbox asking for asked gets on a host allowing
// host (spec 081's table): no network on a host that allows none; on one
// with an allow list, the asked hosts the list also covers for an allow
// list, the host's list for open less what the denied list names, and no
// network for none. It never widens what was asked.
func NarrowEgress(asked Egress, host HostNetwork) Egress {
	if host.Mode != string(EgressAllowlist) {
		return Egress{Mode: EgressNone}
	}
	switch asked.Mode {
	case EgressAllowlist:
		var kept []string
		for _, pattern := range asked.AllowedHosts {
			if HostCovers(host.Hosts, pattern) {
				kept = append(kept, pattern)
			}
		}
		if len(kept) == 0 {
			return Egress{Mode: EgressNone}
		}
		return Egress{Mode: EgressAllowlist, AllowedHosts: kept}
	case EgressOpen:
		var kept []string
		for _, h := range host.Hosts {
			if !HostCovers(asked.DeniedHosts, h) {
				kept = append(kept, h)
			}
		}
		if len(kept) == 0 {
			return Egress{Mode: EgressNone}
		}
		return Egress{Mode: EgressAllowlist, AllowedHosts: kept}
	}
	return Egress{Mode: EgressNone}
}
