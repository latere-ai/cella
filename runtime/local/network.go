// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package local

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"latere.ai/x/pkg/hostsandbox"

	driver "latere.ai/x/cella/runtime"
)

// The ports every host a network admits by name is reached on: 443 for a
// CONNECT and 80 for a plain request. An entry names any other as host:port.
const (
	tunnelPort = "443"
	plainPort  = "80"
)

// HostEntry is the entry that lets a command reach host on port, as
// Network.Hosts and Options.Allowed name one: the host alone for 443 or 80,
// which an entry admits both of, and host:port for any other port. It
// refuses a host hostsandbox would not report, and a port that is not one.
func HostEntry(host string, port int) (string, error) {
	host = normalize(host)
	if !validHost(host) {
		return "", fmt.Errorf("%w: %q is not a host name or an address", driver.ErrInvalid, host)
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("%w: %d is not a port", driver.ErrInvalid, port)
	}
	if p := strconv.Itoa(port); p != tunnelPort && p != plainPort {
		return net.JoinHostPort(host, p), nil
	}
	return host, nil
}

// splitEntry is an entry's host and its port, "" for an entry that names
// none.
func splitEntry(entry string) (host, port string) {
	if h, p, err := net.SplitHostPort(entry); err == nil {
		return normalize(h), p
	}
	return normalize(entry), ""
}

// normalize is a host lowercased and trimmed, without a trailing dot, as
// hostsandbox compares one.
func normalize(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

// validEntry reports whether a deny-list entry reads as hostsandbox reads
// one: a host, an address or a "*." wildcard over a host, alone or with a
// port from 1 to 65535. What it accepts hostsandbox accepts, so a host
// policy it passes never fails at a command's start.
func validEntry(entry string) bool {
	host := entry
	if h, port, err := net.SplitHostPort(entry); err == nil {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return false
		}
		host = h
	}
	return validHost(strings.TrimPrefix(normalize(host), "*."))
}

// validHost reports whether a normalized host is an address without a zone
// or a host name: at most 253 characters, in labels of 1 to 63 lower-case
// ASCII letters, digits, hyphens and underscores that neither begin nor end
// with a hyphen, the last of them not a number, which a resolver would read
// as an address.
func validHost(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Zone() == ""
	}
	if host == "" || len(host) > 253 {
		return false
	}
	last := ""
	for label := range strings.SplitSeq(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			if c := label[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return false
			}
		}
		last = label
	}
	return !numeric(last)
}

// numeric reports whether a label is a number in decimal, or 0x followed by
// hexadecimal digits or none.
func numeric(label string) bool {
	digits, base := label, "0123456789"
	if rest, ok := strings.CutPrefix(label, "0x"); ok {
		digits, base = rest, "0123456789abcdef"
		if digits == "" {
			return true
		}
	}
	return strings.Trim(digits, base) == ""
}

// Allow is the network with entry allowed, an entry as HostEntry builds one,
// as a person who allowed a refused host would have it saved: under an
// allow list it joins Hosts; under any it lifts the network's own block of
// its host by name, narrowing a block on every port to the ports still
// refused, and one with a port beyond 443 and 80 joins Hosts. It refuses a
// network of none, which reaches no host, and a host a wildcard entry
// blocks, which only narrowing or removing that entry lifts.
func (n Network) Allow(entry string) (Network, error) {
	host, port := splitEntry(entry)
	switch n.Mode {
	case NetworkAllowlist:
		n.Hosts = clean(append(slices.Clone(n.Hosts), entry))
		return n, nil
	case NetworkAny:
		if wildcard, ok := wildcardBlocking(n.Blocked, host, port); ok {
			return Network{}, fmt.Errorf("%w: %s is blocked by %s, which allowing one host does not lift; narrow or remove that entry", driver.ErrInvalid, entry, wildcard)
		}
		n.Blocked = unblock(n.Blocked, n.Blocked, n.Hosts, host, port)
		if port != "" {
			n.Hosts = clean(append(slices.Clone(n.Hosts), entry))
		}
		return n, nil
	}
	return Network{}, fmt.Errorf("%w: a network of %q reaches no host; a host is allowed on an allow list or on any", driver.ErrInvalid, n.Mode)
}

// wildcardBlocking is the "*." entry of a deny list that blocks host on
// port, "" for the standard ports, and whether one does.
func wildcardBlocking(blocked []string, host, port string) (string, bool) {
	for _, entry := range blocked {
		h, p := splitEntry(entry)
		domain, ok := strings.CutPrefix(h, "*.")
		if !ok || !strings.HasSuffix(host, "."+domain) {
			continue
		}
		if p == "" || p == port || (port == "" && (p == tunnelPort || p == plainPort)) {
			return entry, true
		}
	}
	return "", false
}

// widen is a command's network with the entries the person allowed beyond
// the host's own. Under an allow list each joins it. Under a deny list each
// lifts the host's own block of its host by name (own is the host's deny
// list), and one with a port beyond 443 and 80 admits that port. An entry
// blocking the host on every port is narrowed to the ports still refused,
// so allowing one port lifts that port alone. A wildcard entry is never
// lifted, and neither is a block the sandbox's own manifest asked for.
func widen(n hostsandbox.Network, own, allowed []string) hostsandbox.Network {
	switch n.Mode {
	case hostsandbox.NetworkAllowlist:
		for _, entry := range allowed {
			if !slices.Contains(n.Domains, entry) {
				n.Domains = append(n.Domains, entry)
			}
		}
	case hostsandbox.NetworkDenylist:
		for _, entry := range allowed {
			host, port := splitEntry(entry)
			n.Blocked = unblock(n.Blocked, own, n.Domains, host, port)
			if port != "" && !slices.Contains(n.Domains, entry) {
				n.Domains = append(n.Domains, entry)
			}
		}
	}
	return n
}

// unblock is a deny list with host lifted on port, or on the standard ports
// for "": each entry of the host's own list that names host on that port is
// dropped, and one that names host alone, blocking it on every port, is
// replaced by entries for the ports that stay blocked, the standard ones
// and any other domains admits for host.
func unblock(blocked, own, domains []string, host, port string) []string {
	var out []string
	for _, entry := range blocked {
		h, p := splitEntry(entry)
		if h != host || !slices.Contains(own, entry) {
			out = append(out, entry)
			continue
		}
		switch {
		case port == "" && (p == tunnelPort || p == plainPort):
		case port != "" && p == port:
		case p == "":
			for _, still := range stillBlocked(domains, host, port) {
				if !slices.Contains(out, still) {
					out = append(out, still)
				}
			}
		default:
			out = append(out, entry)
		}
	}
	return out
}

// stillBlocked is what a block of host on every port leaves once port is
// lifted, "" for the standard ones: the standard ports unless they are
// lifted, and every other port domains admits for host but the one lifted.
func stillBlocked(domains []string, host, port string) []string {
	var out []string
	if port != "" {
		out = append(out, net.JoinHostPort(host, tunnelPort), net.JoinHostPort(host, plainPort))
	}
	for _, d := range domains {
		if h, p := splitEntry(d); h == host && p != "" && p != port {
			out = append(out, net.JoinHostPort(host, p))
		}
	}
	return out
}
