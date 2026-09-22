// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// portProxyPattern is design 023's proxy route. It names no method, because
// the proxy forwards whichever one the caller sends.
const portProxyPattern = "/v1/sandboxes/{id}/ports/{name}/{path...}"

// proxyHeaderTimeout is how long the proxy waits for the response headers of
// the server inside, design 023's figure. The body that follows streams for
// as long as the server sends it.
const proxyHeaderTimeout = 60 * time.Second

// proxyPrefixSegments is how many segments of the escaped path name the route
// rather than the server inside: the empty one before the first slash, then
// v1, sandboxes, the sandbox, ports and the port's name.
const proxyPrefixSegments = 6

// portProxy serves design 023's HTTP proxy to a declared port. The port is
// resolved from the sandbox's own declaration and the connection is opened by
// the driver's Dial with the sandbox's id, so no host, port, path or header a
// caller writes decides where the request goes.
func (h *handler) portProxy(w http.ResponseWriter, r *http.Request) {
	obj, ok := h.dialGate(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	port, declared := declaredPort(obj, name)
	if !declared {
		respondError(w, &manifest.Error{Code: "not_found", Detail: "the sandbox declares no port named " + name})
		return
	}
	if obj.Status.Phase != runtime.Running {
		respondError(w, &manifest.Error{Code: "upstream_unavailable",
			Detail: "the sandbox is " + obj.Status.Phase + ", and a port is reached only while it runs"})
		return
	}
	dialer, ok := h.dialerOf(w, obj)
	if !ok {
		return
	}
	h.touch(r, obj)
	h.proxyTo(dialer, obj.Status.ID, port).ServeHTTP(w, r)
}

// declaredPort is the number the sandbox declared under a name. The list is
// the stored manifest's, which the resolver held to unique names.
func declaredPort(obj v1.Sandbox, name string) (int, bool) {
	for _, p := range obj.Spec.Network.Ports {
		if p.Name == name {
			return p.Port, true
		}
	}
	return 0, false
}

// proxyTo builds the proxy for one request. The transport is the request's
// own with keep-alives off, so a connection is never pooled and no request
// rides a connection another sandbox's request opened; its dial ignores the
// address it is handed, which is the rewritten URL's and never the caller's.
func (h *handler) proxyTo(dialer runtime.Dialer, id string, port int) *httputil.ReverseProxy {
	inside := net.JoinHostPort("localhost", strconv.Itoa(port))
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.Dial(ctx, id, port)
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: proxyHeaderTimeout,
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			prefix, escaped := proxyPath(pr.In.URL)
			out := pr.Out
			out.URL.Scheme = "http"
			out.URL.Host = inside
			out.URL.RawPath = escaped
			out.URL.Path = unescapedPath(escaped)
			out.URL.RawQuery = pr.In.URL.RawQuery
			// The port is at localhost inside the sandbox, which is what a
			// server that checks the host it is asked for expects.
			out.Host = inside
			// The bearer is the caller's to this server and never the
			// workload's to read.
			out.Header.Del("Authorization")
			pr.SetXForwarded()
			out.Header.Set("X-Forwarded-Prefix", prefix)
		},
		Transport: transport,
		// A response streams as it arrives: a server inside that sends
		// events or a long body is read by the caller as it writes.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			respondError(w, &manifest.Error{Code: "upstream_unavailable", Detail: err.Error()})
		},
		ErrorLog: slog.NewLogLogger(h.log.Handler(), slog.LevelWarn),
	}
}

// proxyPath splits the caller's escaped path into the route's prefix and the
// suffix the server inside receives, with the caller's escaping kept so a
// segment carrying an escaped slash reaches the server as one segment.
func proxyPath(u *url.URL) (prefix, suffix string) {
	segments := strings.Split(u.EscapedPath(), "/")
	if len(segments) <= proxyPrefixSegments {
		return strings.Join(segments, "/"), "/"
	}
	return strings.Join(segments[:proxyPrefixSegments], "/"), "/" + strings.Join(segments[proxyPrefixSegments:], "/")
}

// unescapedPath is the decoded form of an escaped path. The mux already
// decoded the whole path once, so the suffix decodes; one that did not would
// be sent as it came.
func unescapedPath(escaped string) string {
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return escaped
	}
	return decoded
}
