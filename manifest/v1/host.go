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

// HostNetwork is the network the host allows a sandbox: none, an allow list
// of hosts, or open, any public host but those Blocked names. Under open,
// Hosts are the ports beyond 443 and 80 the host admits, each as host:port.
// It narrows what a manifest asks for and never widens it.
type HostNetwork struct {
	Mode    string   `json:"mode"`
	Hosts   []string `json:"hosts,omitempty"`
	Blocked []string `json:"blocked,omitempty"`
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
		h.Network.Mode == other.Network.Mode && slices.Equal(h.Network.Hosts, other.Network.Hosts) &&
		slices.Equal(h.Network.Blocked, other.Network.Blocked)
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
// none when any of them allows none; open when every one of them is open,
// blocking what any of them blocks and admitting the ports every one of them
// admits; and otherwise an allow list of the hosts every allow list names,
// less each host an open one blocks. A host whose network mode is empty
// reports no network of its own and narrows nothing. The second result is
// false when no host reports a network, which is an environment held to the
// egress modes it declares.
func HostNetworkOf(hosts []HostInfo) (HostNetwork, bool) {
	var (
		list, open     HostNetwork
		listed, opened bool
	)
	for _, h := range hosts {
		switch h.Network.Mode {
		case "":
			continue
		case string(EgressAllowlist):
			if !listed {
				list, listed = HostNetwork{Mode: string(EgressAllowlist), Hosts: slices.Clone(h.Network.Hosts)}, true
				continue
			}
			list.Hosts = slices.DeleteFunc(list.Hosts, func(host string) bool { return !slices.Contains(h.Network.Hosts, host) })
		case string(EgressOpen):
			if !opened {
				open, opened = HostNetwork{Mode: string(EgressOpen), Hosts: slices.Clone(h.Network.Hosts), Blocked: slices.Clone(h.Network.Blocked)}, true
				continue
			}
			open.Hosts = slices.DeleteFunc(open.Hosts, func(host string) bool { return !slices.Contains(h.Network.Hosts, host) })
			for _, b := range h.Network.Blocked {
				if !slices.Contains(open.Blocked, b) {
					open.Blocked = append(open.Blocked, b)
				}
			}
		default:
			return HostNetwork{Mode: string(EgressNone)}, true
		}
	}
	switch {
	case listed && opened:
		list.Hosts = unblocked(list.Hosts, open.Blocked)
		return list, true
	case listed:
		return list, true
	case opened:
		return open, true
	}
	return HostNetwork{}, false
}

// NarrowEgress is what a sandbox asking for asked gets on a host allowing
// host (spec 081's table): no network on a host that allows none; on one
// with an allow list, the asked hosts the list also covers for an allow
// list, the host's list for open less what the denied list names, and no
// network for none; on an open one, the asked hosts it does not block for
// an allow list, open denying what either denies for open, and no network
// for none. It never widens what was asked.
func NarrowEgress(asked Egress, host HostNetwork) Egress {
	switch host.Mode {
	case string(EgressAllowlist):
		return narrowToList(asked, host)
	case string(EgressOpen):
		return narrowToOpen(asked, host)
	}
	return Egress{Mode: EgressNone}
}

// narrowToList is NarrowEgress on a host with an allow list.
func narrowToList(asked Egress, host HostNetwork) Egress {
	var kept []string
	switch asked.Mode {
	case EgressAllowlist:
		for _, pattern := range asked.AllowedHosts {
			if HostCovers(host.Hosts, pattern) {
				kept = append(kept, pattern)
			}
		}
	case EgressOpen:
		for _, h := range host.Hosts {
			if !HostCovers(asked.DeniedHosts, h) {
				kept = append(kept, h)
			}
		}
	}
	if len(kept) == 0 {
		return Egress{Mode: EgressNone}
	}
	return Egress{Mode: EgressAllowlist, AllowedHosts: kept}
}

// narrowToOpen is NarrowEgress on a host that admits any public host but
// those it blocks.
func narrowToOpen(asked Egress, host HostNetwork) Egress {
	switch asked.Mode {
	case EgressAllowlist:
		if kept := unblocked(asked.AllowedHosts, host.Blocked); len(kept) > 0 {
			return Egress{Mode: EgressAllowlist, AllowedHosts: kept}
		}
	case EgressOpen:
		denied := slices.Clone(asked.DeniedHosts)
		for _, b := range host.Blocked {
			if !slices.Contains(denied, b) {
				denied = append(denied, b)
			}
		}
		return Egress{Mode: EgressOpen, DeniedHosts: denied}
	}
	return Egress{Mode: EgressNone}
}

// unblocked is the patterns of an allow list that share no host with a
// blocked entry: one a blocked entry covers, and one that covers a blocked
// entry, such as *.example.com over a blocked a.example.com, are dropped
// whole, since an allow list cannot carry an exception.
func unblocked(patterns, blocked []string) []string {
	var kept []string
	for _, p := range patterns {
		if !slices.ContainsFunc(blocked, func(b string) bool { return HostPatternCovers(b, p) || HostPatternCovers(p, b) }) {
			kept = append(kept, p)
		}
	}
	return kept
}
