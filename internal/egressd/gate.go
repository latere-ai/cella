// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egressd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	pkgegress "latere.ai/x/pkg/egress"

	"latere.ai/x/cella/egress"
)

// The reasons the gate refuses, which travel in the record so a caller sees
// which rule decided rather than only that something did.
const (
	reasonNoCredential   = "NoCredential"
	reasonLocalTarget    = "LocalTarget"
	reasonNonPublic      = "NonPublicAddress"
	reasonNoMap          = "NoMap"
	reasonModeNone       = "EgressNone"
	reasonNotAllowed     = "NotOnTheAllowList"
	reasonDenied         = "OnTheDeniedList"
	reasonBadDestination = "BadDestination"
)

// Realm is what the proxy door answers a request with no credential.
const Realm = "cella-egress"

// gate is the policy in front of both doors. Every connection is decided
// here, before any dial: which sandbox is asking, whether its boundary admits
// the destination, and whether anything is substituted on the way.
type gate struct {
	store *store
	// private are the non-public ranges the operator lets the gateway dial,
	// CELLA_EGRESS_PRIVATE_RANGES. Every other address that is not public is
	// refused: the gateway sits on its operator's network, and a sandbox
	// would otherwise reach that network through it.
	private []netip.Prefix
	// dial opens the connection to an upstream. In an installation it is
	// pkg/egress's PublicDialer over private, which refuses an address that
	// is not public whatever name led to it; an operator whose gateway sits
	// behind another proxy replaces it.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
	// gateway is the substitution engine's own CONNECT proxy, which takes
	// the connections that have something to substitute.
	gateway *pkgegress.Gateway
	// upstream is the reverse door's client toward the destination. It
	// dials through the same seam, so one gateway reaches upstreams one
	// way whichever door the request came in at.
	upstream *http.Transport
	records  func(egress.Record)
	log      *slog.Logger
	now      func() time.Time
}

// decision is what the gate concluded about one connection.
type decision struct {
	principal string
	verdict   string
	reason    string
	// terminate says the destination has something to substitute, so the
	// connection is decrypted rather than tunneled.
	terminate bool
}

func (d decision) allowed() bool {
	return d.verdict == egress.DecisionAllowed || d.verdict == egress.DecisionPassthrough
}

// decide is the whole of the boundary's rule, in the order the acceptance
// criteria state it. It reads no body and opens no connection. A name is
// decided here by the boundary alone; where it leads is decided again when
// the connection is dialed, by the address it resolved to.
func (g *gate) decide(credential, host string, _ int) decision {
	principal, ok := g.store.Principal(credential)
	if !ok {
		return decision{verdict: egress.DecisionDenied, reason: reasonNoCredential}
	}
	d := decision{principal: principal}
	if host == "" {
		d.verdict, d.reason = egress.DecisionDenied, reasonBadDestination
		return d
	}
	switch {
	case isLocalTarget(host):
		d.verdict, d.reason = egress.DecisionDenied, reasonLocalTarget
		return d
	case g.isNonPublicAddress(host):
		d.verdict, d.reason = egress.DecisionDenied, reasonNonPublic
		return d
	}
	m, held := g.store.Map(principal)
	if !held {
		d.verdict, d.reason = egress.DecisionUnknown, reasonNoMap
		return d
	}
	if !m.Admits(host) {
		d.verdict = egress.DecisionDenied
		switch m.Mode {
		case "none":
			d.reason = reasonModeNone
		case "open":
			d.reason = reasonDenied
		default:
			d.reason = reasonNotAllowed
		}
		return d
	}
	d.terminate = g.store.HasSecretFor(principal, host)
	if d.terminate {
		d.verdict = egress.DecisionAllowed
		return d
	}
	d.verdict = egress.DecisionPassthrough
	return d
}

// isLocalTarget reports whether a destination names the machine the gateway
// runs on or the link it sits on. Such a destination would reach the
// gateway's own listeners, or a cloud's metadata service, from inside the
// boundary and on a path no network rule of the sandbox's own sees.
func isLocalTarget(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

// isNonPublicAddress reports whether a destination written as an address is
// one the gateway does not dial: not public, and outside every range the
// operator named. It answers before the dial for the case the address is
// plain to see; a name that resolves to such an address is refused by the
// dialer.
func (g *gate) isNonPublicAddress(host string) bool {
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return false
	}
	return pkgegress.CheckPublic(ip, g.private) != nil
}

// refusedAtDial reports whether a dial failed because the address the name
// resolved to is not one the gateway dials.
func refusedAtDial(err error) bool {
	return errors.Is(err, pkgegress.ErrNonPublicDestination)
}

// refused files a terminated request the dialer did not send because its
// host resolved to an address that is not public. The engine answered the
// workload 403; one tunnel carries many requests, so each is filed on its
// own, beside the tunnel's record.
func (g *gate) refused(principal string, req *http.Request) {
	host, port := splitHostPort(req.URL.Host, 443)
	g.record(egress.Record{
		Principal: principal, Door: egress.DoorProxy, Host: host, Port: port,
		Decision: egress.DecisionDenied, Reason: reasonNonPublic, Method: req.Method, Path: req.URL.Path,
	})
}

// record files one connection, with whatever the door saw of it.
func (g *gate) record(r egress.Record) {
	if g.records == nil {
		return
	}
	if r.At.IsZero() {
		r.At = g.now()
	}
	g.records(r)
}

// ServeProxy is the proxy door: a CONNECT tunnel per destination, which is
// what a client configured with HTTPS_PROXY opens.
func (g *gate) ServeProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "this gateway accepts CONNECT", http.StatusMethodNotAllowed)
		return
	}
	credential := proxyCredential(r.Header.Get("Proxy-Authorization"))
	host, port := splitHostPort(r.Host, 443)
	d := g.decide(credential, host, port)
	if d.principal == "" {
		w.Header().Set("Proxy-Authenticate", `Basic realm="`+Realm+`"`)
		http.Error(w, "this gateway needs the sandbox's own credential", http.StatusProxyAuthRequired)
		return
	}
	if !d.allowed() {
		g.record(egress.Record{Principal: d.principal, Door: egress.DoorProxy, Host: host, Port: port, Decision: d.verdict, Reason: d.reason})
		http.Error(w, "this sandbox's boundary does not admit "+host, http.StatusForbidden)
		return
	}
	if d.terminate {
		// The destination has a credential bound to it, so the connection
		// is terminated and substituted by the engine that owns that.
		g.gateway.ServeHTTP(w, r)
		g.record(egress.Record{Principal: d.principal, Door: egress.DoorProxy, Host: host, Port: port, Decision: d.verdict})
		return
	}
	g.tunnel(w, r, d, host, port)
}

// tunnel splices the workload's connection to the destination without
// terminating it, so a sandbox's own pinned trust keeps working toward a host
// the gateway has no credential for.
func (g *gate) tunnel(w http.ResponseWriter, r *http.Request, d decision, host string, port int) {
	started := g.now()
	rec := egress.Record{Principal: d.principal, Door: egress.DoorProxy, Host: host, Port: port, Decision: d.verdict, At: started}
	upstream, err := g.dial(r.Context(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if refusedAtDial(err) {
		rec.Decision, rec.Reason = egress.DecisionDenied, reasonNonPublic
		g.record(rec)
		http.Error(w, "the gateway does not connect to "+host+": it resolves to an address that is not public", http.StatusForbidden)
		return
	}
	if err != nil {
		g.log.WarnContext(r.Context(), "the gateway could not reach an admitted destination", "host", host, "err", err)
		rec.Reason = "Unreachable"
		g.record(rec)
		http.Error(w, "the destination did not answer", http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "this server cannot tunnel", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()
	if _, err = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// Bytes the client sent behind the CONNECT head sit in the server's own
	// buffer; they belong to the tunnel and go up first.
	var out, in atomic.Int64
	if n := buffered.Reader.Buffered(); n > 0 {
		early, err := buffered.Peek(n)
		if err != nil {
			return
		}
		written, err := upstream.Write(early)
		out.Add(int64(written))
		if err != nil {
			return
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		n, _ := io.Copy(upstream, client)
		out.Add(n)
		_ = upstream.Close()
		_ = client.Close()
	}()
	n, _ := io.Copy(client, upstream)
	in.Add(n)
	_ = upstream.Close()
	_ = client.Close()
	<-done
	rec.BytesOut, rec.BytesIn = out.Load(), in.Load()
	rec.DurationMS = g.now().Sub(started).Milliseconds()
	g.record(rec)
}

// ServeReverse is the reverse door: plain HTTP inside the environment, the
// destination in the first path segment, the credential in its own header.
// It is what a runtime that ignores proxy variables and private authorities
// uses, and it leaves Authorization free for the upstream's own placeholder.
func (g *gate) ServeReverse(w http.ResponseWriter, r *http.Request) {
	started := g.now()
	authority, rest, ok := splitReversePath(r.URL.Path)
	// The segment is host or host:port, as a CONNECT's authority is, and
	// the host alone is what the boundary and the address checks read.
	host, port := splitHostPort(authority, 443)
	credential := strings.TrimSpace(r.Header.Get(egress.CredentialHeader))
	d := g.decide(credential, host, port)
	if d.principal == "" {
		http.Error(w, "this gateway needs the sandbox's own credential", http.StatusUnauthorized)
		return
	}
	if !ok {
		http.Error(w, "the first path segment names the destination host", http.StatusBadRequest)
		return
	}
	rec := egress.Record{
		Principal: d.principal, Door: egress.DoorReverse, Host: host, Port: port,
		Decision: d.verdict, Reason: d.reason, Method: r.Method, Path: rest, At: started,
	}
	if !d.allowed() {
		g.record(rec)
		http.Error(w, "this sandbox's boundary does not admit "+host, http.StatusForbidden)
		return
	}
	// The reverse door always terminates: the sandbox spoke plain HTTP to
	// it, so there is nothing to tunnel and the request is rebuilt toward
	// the destination over TLS.
	rec.Decision = egress.DecisionAllowed
	out, err := g.upstreamRequest(r, host, port, rest, d.principal)
	if err != nil {
		rec.Reason = "BadRequest"
		g.record(rec)
		http.Error(w, "the request could not be rebuilt for the destination", http.StatusBadRequest)
		return
	}
	resp, err := g.upstream.RoundTrip(out)
	if refusedAtDial(err) {
		rec.Decision, rec.Reason = egress.DecisionDenied, reasonNonPublic
		g.record(rec)
		http.Error(w, "the gateway does not connect to "+host+": it resolves to an address that is not public", http.StatusForbidden)
		return
	}
	if err != nil {
		rec.Reason = "Unreachable"
		g.record(rec)
		http.Error(w, "the destination did not answer", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	copied, _ := io.Copy(w, resp.Body)
	rec.Status = resp.StatusCode
	rec.BytesIn = copied
	rec.DurationMS = g.now().Sub(started).Milliseconds()
	g.record(rec)
}

// upstreamRequest rebuilds the sandbox's request toward the destination and
// substitutes every placeholder the destination is in scope for. A
// substitution that cannot be made fails the request rather than sending a
// placeholder the caller meant to be a value.
func (g *gate) upstreamRequest(r *http.Request, host string, port int, path, principal string) (*http.Request, error) {
	target := &url.URL{Scheme: "https", Host: authorityOf(host, port), Path: path, RawQuery: r.URL.RawQuery}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
	if err != nil {
		return nil, err
	}
	for key, values := range r.Header {
		if strings.EqualFold(key, egress.CredentialHeader) || strings.EqualFold(key, "Host") {
			continue
		}
		for _, v := range values {
			out.Header.Add(key, v)
		}
	}
	out.ContentLength = r.ContentLength
	// Each entry is applied to the one place its owner named, which is the
	// placement rule of spec 018: a placeholder anywhere else in the request
	// leaves as the opaque token it is.
	if err = g.substitutePlaced(r.Context(), principal, host, port, out); err != nil {
		return nil, err
	}
	return out, nil
}

// proxyCredential reads the password half of a Basic proxy credential. A
// stock client sends the proxy URL's userinfo this way, so the sandbox's
// credential arrives without the workload doing anything.
func proxyCredential(header string) string {
	trimmed := strings.TrimSpace(header)
	scheme, value, ok := strings.Cut(trimmed, " ")
	// The scheme is case insensitive, and a client that writes it in
	// another case is a client whose sandbox would otherwise be refused at
	// its own door.
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	_, password, ok := strings.Cut(string(raw), ":")
	if !ok {
		return ""
	}
	return password
}

// splitReversePath cuts the destination out of the first path segment.
func splitReversePath(path string) (host, rest string, ok bool) {
	trimmed := strings.TrimPrefix(path, "/")
	host, rest, found := strings.Cut(trimmed, "/")
	if host == "" {
		return "", "", false
	}
	if !found {
		return host, "/", true
	}
	return host, "/" + rest, true
}

// authorityOf writes a destination back as a URL's host: the host alone on
// the default port, as it was asked for, and host:port on any other.
func authorityOf(host string, port int) string {
	if port == 443 && !strings.Contains(host, ":") {
		return host
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port))
}

// splitHostPort reads a destination written as host or host:port.
func splitHostPort(authority string, def int) (string, int) {
	if host, port, err := net.SplitHostPort(authority); err == nil {
		if n, err := strconv.Atoi(port); err == nil {
			return host, n
		}
		return host, def
	}
	return authority, def
}

// upstreamTLS is the trust the gateway itself dials with: the system roots,
// plus any authority the operator named. It is empty in an installation and
// set by a test tier to its own upstream's.
func upstreamTLS(bundlePEM string) (*tls.Config, error) {
	if strings.TrimSpace(bundlePEM) == "" {
		return nil, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM([]byte(bundlePEM)) {
		return nil, errors.New("CELLA_EGRESS_CA_BUNDLE holds no certificate")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}
