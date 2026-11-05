// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"latere.ai/x/pkg/authz"
)

// requestKey carries the request block ServeHTTP reads at the door.
type requestKey struct{}

// withRequestInfo is ctx carrying the request block of every question r's
// handler asks the authorizer: the request id this answer carries, the
// caller's address as clientIP reads it, and the user agent. It is read once,
// at the door, so every question of one request names the id its envelope,
// its log line and its events name.
func withRequestInfo(ctx context.Context, r *http.Request, id string, trusted []netip.Prefix) context.Context {
	info := authz.Caller{ID: id, IP: clientIP(r, trusted), UserAgent: r.UserAgent()}
	return context.WithValue(ctx, requestKey{}, info)
}

// requestInfo is the request block withRequestInfo put on r. A request that
// did not come through the door carries none, and is read as it arrived: the
// peer and the user agent, with no id.
func requestInfo(r *http.Request) authz.Caller {
	if info, ok := r.Context().Value(requestKey{}).(authz.Caller); ok {
		return info
	}
	return authz.Caller{IP: clientIP(r, nil), UserAgent: r.UserAgent()}
}

// clientIP is the caller's address the authorizer's request.ip carries: the
// peer with its port removed, unless the peer is inside one of the trusted
// ranges, CELLA_TRUSTED_PROXIES. Then it is the rightmost X-Forwarded-For
// entry outside them. Each proxy appends the address it heard from, so the
// walk runs right to left and stops at the first hop no trusted proxy is:
// everything left of it was written by that hop and may be anything it chose.
//
// The header from a peer outside the ranges is never read, so a caller that
// reaches the listener directly cannot name its own address, and with no
// ranges the header is never read at all. An entry that is not an address
// ends the walk, and so does running out of entries, and both keep the peer:
// the answer is always an address a trusted hop vouched for, never a string
// from the header.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	if len(trusted) == 0 {
		return peer
	}
	if addr, err := netip.ParseAddr(peer); err != nil || !inside(addr.Unmap(), trusted) {
		return peer
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for _, entry := range slices.Backward(hops) {
		hop, ok := forwardedAddr(entry)
		if !ok {
			break
		}
		if !inside(hop, trusted) {
			return hop.String()
		}
	}
	return peer
}

// forwardedAddr parses one X-Forwarded-For entry: an address, or an address
// with a port, which some proxies append, bracketed for IPv6. An IPv4 address
// mapped into IPv6 is reported as IPv4, so it meets an IPv4 range.
func forwardedAddr(entry string) (netip.Addr, bool) {
	entry = strings.TrimSpace(entry)
	if ap, err := netip.ParseAddrPort(entry); err == nil {
		return ap.Addr().Unmap(), true
	}
	addr, err := netip.ParseAddr(entry)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// inside reports whether addr is in one of the ranges.
func inside(addr netip.Addr, ranges []netip.Prefix) bool {
	return slices.ContainsFunc(ranges, func(p netip.Prefix) bool { return p.Contains(addr) })
}
