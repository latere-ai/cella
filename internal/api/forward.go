// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"latere.ai/x/pkg/otel"
)

// ErrNoWriter is a request a standby held for the whole of its hold without
// finding a writer to forward it to, or one whose forward failed after it
// reached a writer that then went away (spec 076). Design 008 answers it with
// 503 control_plane_unavailable: another replica becomes the writer shortly,
// and the caller's retry reaches it.
var ErrNoWriter = errors.New("no replica holds the writer lease")

// DefaultForwardHold is how long a standby holds a request while no writer
// can be reached, when CELLA_FORWARD_HOLD sets nothing.
const DefaultForwardHold = 15 * time.Second

// The outcomes of one forwarded request, which design 017 counts.
const (
	ForwardForwarded = "forwarded"
	ForwardNoWriter  = "no_writer"
	ForwardFailed    = "failed"
)

// ForwardOutcomes is the closed vocabulary of a forward's outcome.
var ForwardOutcomes = []string{ForwardForwarded, ForwardNoWriter, ForwardFailed}

// writerPoll is how often a held request looks again for a writer, and how
// long the writer's address is trusted before it is read again.
const writerPoll = 100 * time.Millisecond

// ForwarderOptions configures the forwarder a standby answers the API with.
type ForwarderOptions struct {
	// Writer reads where the writer's public listener is reached, a URL
	// such as http://10.0.0.7:8080, or empty when no replica holds the
	// writer lease.
	Writer func(ctx context.Context) (string, error)
	// Hold is how long a request waits for a writer it can reach before it
	// is refused. Zero takes DefaultForwardHold.
	Hold time.Duration
	// Counted records one forward by its outcome. Nil counts nothing.
	Counted func(outcome string)
	Log     *slog.Logger
}

// Forwarder is a standby's half of the API: every request goes to the writer
// as it arrived, a WebSocket and a stream included, and the writer verifies,
// authorizes and answers it. A standby verifies nothing itself, so a forwarded
// request is decided exactly as a direct one. While no writer can be reached
// the request is held, its body unread, and forwarded once one can; a dial
// that fails is tried again inside the hold, and a request that reached a
// writer is never sent twice.
type Forwarder struct {
	writer  func(ctx context.Context) (string, error)
	hold    time.Duration
	counted func(string)
	log     *slog.Logger
	proxy   *httputil.ReverseProxy

	mu     sync.Mutex
	cached string
	read   time.Time
}

// holdKey carries the instant a forwarded request's hold ends to the dialer,
// and targetKey the writer's address the request was held for to the rewrite.
type (
	holdKey   struct{}
	targetKey struct{}
)

// NewForwarder builds the forwarder.
func NewForwarder(o ForwarderOptions) *Forwarder {
	f := &Forwarder{
		writer: o.Writer, hold: cmp.Or(o.Hold, DefaultForwardHold),
		counted: o.Counted, log: cmp.Or(o.Log, slog.Default()),
	}
	if f.counted == nil {
		f.counted = func(string) {}
	}
	// Every forward dials afresh: a pooled connection to a writer that has
	// since handed off would carry the next request to a process that is
	// going away. One in-cluster handshake per request is the price.
	transport := &http.Transport{DialContext: f.dial, DisableKeepAlives: true}
	f.proxy = &httputil.ReverseProxy{
		Rewrite:   f.rewrite,
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

// ServeHTTP holds the request until a writer can be reached or the hold ends,
// and forwards it.
func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := requestID(r)
	w.Header().Set(RequestIDHeader, id)
	deadline := time.Now().Add(f.hold)
	target, err := f.await(r.Context(), deadline)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		f.counted(ForwardNoWriter)
		f.log.WarnContext(r.Context(), "a request found no writer within the hold", "hold", f.hold, "err", err)
		respondError(w, ErrNoWriter)
		return
	}
	ctx := context.WithValue(context.WithValue(r.Context(), holdKey{}, deadline), targetKey{}, target)
	out := r.WithContext(ctx)
	out.Header = r.Header.Clone()
	out.Header.Set(RequestIDHeader, id)
	f.proxy.ServeHTTP(w, out)
}

// rewrite points the request at the writer. The path is the one the caller
// sent, taken from the request line: the public listener strips its base
// before the API sees a request, and the writer's listener serves the same
// base. The Host is the caller's, so the writer composes what it writes
// under the address the caller used.
func (f *Forwarder) rewrite(pr *httputil.ProxyRequest) {
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

// dial connects to the writer the lease names now, whatever address the
// request was rewritten to, and tries again until the request's hold ends: a
// writer that just handed off refuses the connection, and its successor's
// address appears in the lease a moment later.
func (f *Forwarder) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	deadline, _ := ctx.Value(holdKey{}).(time.Time)
	var dialer net.Dialer
	for {
		target, err := f.await(ctx, deadline)
		if err != nil {
			return nil, err
		}
		u, err := url.Parse(target)
		if err != nil {
			return nil, err
		}
		host := u.Host
		if u.Port() == "" {
			port := "80"
			if u.Scheme == "https" {
				port = "443"
			}
			host = net.JoinHostPort(u.Hostname(), port)
		}
		conn, err := dialer.DialContext(ctx, network, host)
		if err == nil {
			return conn, nil
		}
		f.forget()
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, errors.Join(ErrNoWriter, err)
		}
		if err := sleepCtx(ctx, writerPoll); err != nil {
			return nil, err
		}
	}
}

// await returns the writer's address, reading the lease each poll until one
// appears, the deadline passes or the caller leaves.
func (f *Forwarder) await(ctx context.Context, deadline time.Time) (string, error) {
	for {
		target := f.address()
		var err error
		if target == "" {
			target, err = f.refresh(ctx)
		}
		if err == nil && target != "" {
			return target, nil
		}
		if err != nil {
			f.log.WarnContext(ctx, "the writer lease could not be read", "err", err)
		}
		if time.Now().After(deadline) {
			return "", errors.Join(ErrNoWriter, err)
		}
		if err := sleepCtx(ctx, writerPoll); err != nil {
			return "", err
		}
	}
}

// address is the writer's address as last read, while it is fresh, which
// spares a lease read to every request of a burst.
func (f *Forwarder) address() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cached != "" && time.Since(f.read) < writerPoll {
		return f.cached
	}
	return ""
}

// refresh reads the writer lease again.
func (f *Forwarder) refresh(ctx context.Context) (string, error) {
	target, err := f.writer(ctx)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	f.cached, f.read = target, time.Now()
	f.mu.Unlock()
	return target, nil
}

// forget drops the address a dial just failed on, so the next attempt reads
// the lease again.
func (f *Forwarder) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cached = ""
}

// failed answers a forward that did not complete. A caller that left is
// answered nothing; any other failure is the writer gone, before or after the
// request reached it, and the caller's retry finds the next one.
func (f *Forwarder) failed(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	outcome := ForwardFailed
	if errors.Is(err, ErrNoWriter) {
		outcome = ForwardNoWriter
	}
	f.counted(outcome)
	f.log.WarnContext(r.Context(), "a forwarded request did not complete", "err", err)
	respondError(w, ErrNoWriter)
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
