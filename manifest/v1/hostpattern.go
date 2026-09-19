// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import "strings"

// The host pattern algebra of the manifest contract's host rule, in the
// schema package because two packages that never import each other share it:
// the resolver, which holds an update to the boundary it may not widen, and
// the egress compiler, which holds a secret's scope to the reach its sandbox
// was granted. One implementation means the two can never disagree about what
// a wildcard covers.

// NormalizeHost prepares a host or a host pattern for comparison: lowercase,
// no surrounding space, no trailing dot, no port. For every pattern the host
// rule admits the result is what the gateway's own matcher computes, so a
// pattern the resolver accepted matches the same destinations there.
func NormalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	// A port is stripped only from an unambiguous host:port. An address with
	// more than one colon is an IPv6 literal, which the host rule refuses
	// whole rather than cut in the middle.
	if i := strings.LastIndexByte(host, ':'); i >= 0 && i == strings.IndexByte(host, ':') && !strings.Contains(host, "]") {
		host = host[:i]
	}
	return host
}

// SplitHostPattern normalizes a pattern and splits a leading "*." off it. The
// empty domain is a pattern with nothing in it.
func SplitHostPattern(pattern string) (domain string, wildcard bool) {
	s := NormalizeHost(pattern)
	if s == "" {
		return "", false
	}
	if rest, ok := strings.CutPrefix(s, "*."); ok {
		return rest, true
	}
	return s, false
}

// HostPatternCovers reports whether outer permits every concrete host inner
// can match. It is directional: an exact pattern never covers a wildcard,
// because the wildcard reaches names the exact one does not, and a wildcard
// never covers its own apex, which is how hostmatch and a cluster's own DNS
// policy read "*.example.com".
func HostPatternCovers(outer, inner string) bool {
	od, ow := SplitHostPattern(outer)
	id, iw := SplitHostPattern(inner)
	if od == "" || id == "" {
		return false
	}
	switch {
	case !ow && !iw:
		return od == id
	case ow && !iw:
		return strings.HasSuffix(id, "."+od)
	case ow && iw:
		return id == od || strings.HasSuffix(id, "."+od)
	default:
		return false
	}
}

// HostCovers reports whether any pattern in the set covers inner in full. An
// empty set covers nothing.
func HostCovers(patterns []string, inner string) bool {
	for _, p := range patterns {
		if HostPatternCovers(p, inner) {
			return true
		}
	}
	return false
}

// HostMatches reports whether a concrete destination host is admitted by the
// pattern set. It differs from HostCovers in what the second argument is: a
// name to reach, not a pattern to contain, so "*.example.com" admits
// "a.example.com" and not "example.com".
func HostMatches(patterns []string, host string) bool {
	host = NormalizeHost(host)
	if host == "" {
		return false
	}
	for _, p := range patterns {
		d, wildcard := SplitHostPattern(p)
		if d == "" {
			continue
		}
		if wildcard {
			if strings.HasSuffix(host, "."+d) {
				return true
			}
			continue
		}
		if host == d {
			return true
		}
	}
	return false
}
