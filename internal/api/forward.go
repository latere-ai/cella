// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"

	"latere.ai/x/pkg/otel"
)

// ErrNoWriter is a request that found no replica to take it within its hold,
// or one whose forward failed after it reached a writer that then went away
// (spec 076). Design 008 answers it with 503 control_plane_unavailable:
// another replica becomes the writer shortly, and the caller's retry reaches
// it.
var ErrNoWriter = errors.New("no replica holds the writer lease")

// DefaultForwardHold is how long a request is held while no replica can take
// it, when CELLA_FORWARD_HOLD sets nothing.
const DefaultForwardHold = 15 * time.Second

// The outcomes of one request a standby forwarded, which design 017 counts.
const (
	ForwardForwarded = "forwarded"
	ForwardNoWriter  = "no_writer"
	ForwardFailed    = "failed"
)

// holdPoll is how often a held request looks again for somewhere to go.
const holdPoll = 100 * time.Millisecond

// ForwarderOptions configures the API slot of a process that may be a writer
// or a standby (spec 076).
type ForwarderOptions struct {
	// Local serves the request in this process and reports whether it did,
	// which is true once this process is the writer. Nil never serves here.
	Local func(w http.ResponseWriter, r *http.Request) bool
	// Changed returns a channel closed the next time Local may answer
	// differently, so a held request is served the moment this process
	// promotes rather than at its next poll. Nil polls only.
	Changed func() <-chan struct{}
	// Writer reads where the writer's public listener is reached, a URL
	// such as http://10.0.0.7:8080, or empty where no other replica holds
	// the writer lease. Nil never forwards.
	Writer func(ctx context.Context) (string, error)
	// Hold is how long a request waits for a replica to take it before it
	// is refused. Zero takes DefaultForwardHold.
	Hold time.Duration
	// Counted records one forward by its outcome. Nil counts nothing.
	Counted func(outcome string)
	Log     *slog.Logger
}

// Forwarder is the API slot of spec 076. A request is served here where this
// process is the writer, and otherwise goes to the writer as it arrived, a
// WebSocket and a stream included, and the writer verifies, authorizes and
// answers it: a standby verifies nothing itself, so a forwarded request is
// decided exactly as a direct one. While neither is possible the request is
// held, its body unread, and routed again each poll until the hold ends. A
// forward that could not connect sent nothing and is routed again; one that
// reached a writer is never sent twice.
type Forwarder struct {
	local   func(http.ResponseWriter, *http.Request) bool
	changed func() <-chan struct{}
	writer  func(context.Context) (string, error)
	hold    time.Duration
	counted func(string)
	log     *slog.Logger
	proxy   *httputil.ReverseProxy
}

// The context values a forward carries from the forwarder to the proxy's
// rewrite, its dialer and its error handler.
type (
	targetKey  struct{}
	notSentKey struct{}
)

// notConnected is a forward that reached no writer: nothing of the request
// was sent, so it may be routed again.
type notConnected struct{ err error }

func (e *notConnected) Error() string { return "no connection to the writer: " + e.err.Error() }
func (e *notConnected) Unwrap() error { return e.err }

// NewForwarder builds the API slot.
func NewForwarder(o ForwarderOptions) *Forwarder {
	f := &Forwarder{
		local: o.Local, changed: o.Changed, writer: o.Writer,
		hold: cmp.Or(o.Hold, DefaultForwardHold), counted: o.Counted, log: cmp.Or(o.Log, slog.Default()),
	}
	if f.counted == nil {
		f.counted = func(string) {}
	}
	// Every forward dials afresh: a pooled connection to a writer that has
	// since handed off would carry the next request to a process that is
	// going away. One in-cluster handshake per request is the price.
	transport := &http.Transport{DialContext: dialTarget, DisableKeepAlives: true}
	f.proxy = &httputil.ReverseProxy{
		Rewrite:   rewrite,
		Transport: otel.Transport(transport),
		ModifyResponse: func(*http.Response) error {
			f.counted(ForwardForwarded)
			return nil
		},
		// A following feed and a log are streams: each write reaches the
		// caller as the writer makes it.
		FlushInterval: -1,
		ErrorHandler:  f.failed,
		ErrorLog:      slog.NewLogLogger(f.log.Handler(), slog.LevelWarn),
	}
	return f
}

// ServeHTTP serves, forwards or holds one request.
func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := requestID(r)
	w.Header().Set(RequestIDHeader, id)
	r.Header.Set(RequestIDHeader, id)
	// The body is read by whichever answer the request gets. A forward
	// whose dial failed read none of it, and the transport's close of it
	// must not end it for the next attempt; the server closes it when the
	// handler returns.
	if r.Body != nil {
		r.Body = io.NopCloser(r.Body)
	}
	deadline := time.Now().Add(f.hold)
	for {
		var changed <-chan struct{}
		if f.changed != nil {
			changed = f.changed()
		}
		if f.local != nil && f.local(w, r) {
			return
		}
		if f.writer != nil {
			target, err := f.writer(r.Context())
			if err != nil {
				f.log.WarnContext(r.Context(), "the writer lease could not be read", "err", err)
			}
			if target != "" && f.forward(w, r, target) {
				return
			}
		}
		if r.Context().Err() != nil {
			return
		}
		if time.Now().After(deadline) {
			f.counted(ForwardNoWriter)
			f.log.WarnContext(r.Context(), "a request found no replica to take it within the hold", "hold", f.hold.String())
			respondError(w, ErrNoWriter)
			return
		}
		timer := time.NewTimer(holdPoll)
		select {
		case <-r.Context().Done():
			timer.Stop()
			return
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// forward sends the request to one writer and reports whether it was sent. A
// forward that could not connect wrote nothing and returns false, so the
// caller routes the request again.
func (f *Forwarder) forward(w http.ResponseWriter, r *http.Request, target string) bool {
	var notSent atomic.Bool
	ctx := context.WithValue(context.WithValue(r.Context(), targetKey{}, target), notSentKey{}, &notSent)
	f.proxy.ServeHTTP(w, r.WithContext(ctx))
	return !notSent.Load()
}

// RefuseNoWriter answers a request no replica could take within its hold, with
// the request id it carried or one minted for it: 503 control_plane_unavailable.
func RefuseNoWriter(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(RequestIDHeader, requestID(r))
	respondError(w, ErrNoWriter)
}

// rewrite points the request at the writer. The path is the one the caller
// sent, taken from the request line: the public listener strips its base
// before the API sees a request, and the writer's listener serves the same
// base. The Host is the caller's, so the writer composes what it writes
// under the address the caller used.
func rewrite(pr *httputil.ProxyRequest) {
	target, _ := pr.In.Context().Value(targetKey{}).(string)
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		u = &url.URL{Scheme: "http", Host: "writer.invalid"}
	}
	sent, err := url.ParseRequestURI(pr.In.RequestURI)
	if err != nil {
		sent = pr.In.URL
	}
	pr.Out.URL = &url.URL{Scheme: u.Scheme, Host: u.Host, Path: sent.Path, RawPath: sent.RawPath, RawQuery: sent.RawQuery}
	pr.Out.Host = pr.In.Host
	pr.SetXForwarded()
}

// dialTarget connects to the writer the request was routed to. A dial that
// fails is notConnected: nothing was sent.
func dialTarget(ctx context.Context, network, _ string) (net.Conn, error) {
	target, _ := ctx.Value(targetKey{}).(string)
	u, err := url.Parse(target)
	if err != nil {
		return nil, &notConnected{err: err}
	}
	host := u.Host
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		host = net.JoinHostPort(u.Hostname(), port)
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, host)
	if err != nil {
		return nil, &notConnected{err: err}
	}
	return conn, nil
}

// failed answers a forward that did not complete. A caller that left is
// answered nothing; a forward that reached no writer is left for the
// forwarder to route again; any other failure is the writer gone after the
// request reached it, and the caller's retry finds the next one.
func (f *Forwarder) failed(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	var unreached *notConnected
	if errors.As(err, &unreached) {
		if notSent, ok := r.Context().Value(notSentKey{}).(*atomic.Bool); ok {
			notSent.Store(true)
			return
		}
	}
	f.counted(ForwardFailed)
	f.log.WarnContext(r.Context(), "a forwarded request did not complete", "err", err)
	respondError(w, ErrNoWriter)
}
