// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"latere.ai/x/cella/internal/metrics"
)

// Metrics is what the API and the gateway hub count: the rows of design 017's
// table that designs 008 and 018 own. The registry of design 017 satisfies it;
// a test passes a fake.
type Metrics interface {
	// Request counts one request by the route pattern the mux matched, the
	// status class, and the error code the envelope carried.
	Request(route, status, code string)
	// RequestDuration observes one request's latency. A hijacked route is
	// counted and not timed: a stream's life is not a request's latency.
	RequestDuration(route string, d time.Duration)
	// Exec counts one session by how it ended. An attach is an exec session
	// with a terminal, so both routes count here: design 017's table has one
	// row for the pair.
	Exec(exit string)
	// EgressConnection counts one connection record as it arrives on a
	// gateway's sync stream, with the bytes it carried each way.
	EgressConnection(decision, door string, in, out int64)
	// GatewaySnapshot counts one boundary snapshot sent to a joining gateway.
	GatewaySnapshot()
}

// nopMetrics is what an API built with no recorder counts.
type nopMetrics struct{}

func (nopMetrics) Request(string, string, string)                {}
func (nopMetrics) RequestDuration(string, time.Duration)         {}
func (nopMetrics) Exec(string)                                   {}
func (nopMetrics) EgressConnection(string, string, int64, int64) {}
func (nopMetrics) GatewaySnapshot()                              {}

// routeSlot is what design 017 calls the context value a middleware behind the
// mux fills. The route is the mux pattern, which is known only after the mux
// has matched, and the code is the error envelope's, which is known only once
// a handler has refused. Both are read by the wrapper the request entered
// through, so the slot is a pointer and not a value.
type routeSlot struct {
	route string
	code  string
	// subject, sandbox and requestID are what the one log line per request
	// carries beside the route: who asked, which sandbox where the route
	// names one, and the id the answer's own header holds.
	subject   string
	sandbox   string
	requestID string
}

type slotKey struct{}

// slotOf is the slot this request carries, or nil outside the API handler.
func slotOf(ctx context.Context) *routeSlot {
	slot, _ := ctx.Value(slotKey{}).(*routeSlot)
	return slot
}

// noteCode records the error envelope's code on the request that produced it,
// so cella_requests_total carries design 008's code and not only its status.
func noteCode(w http.ResponseWriter, code string) {
	for {
		if o, ok := w.(*observed); ok {
			o.code = code
			return
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = u.Unwrap()
	}
}

// observed is the response writer the request's own count reads: the status
// the handler wrote, whether the connection was hijacked for a stream, and the
// envelope's code where one was written.
//
// Flush, Hijack and Unwrap are passed through, because the routes under this
// wrapper stream, upgrade to WebSocket, and are themselves wrapped.
type observed struct {
	http.ResponseWriter
	status   int
	written  bool
	hijacked bool
	code     string
	// yaml is design 008's negotiated syntax for this answer. It rides the
	// writer because respond is reached from handlers several calls down that
	// hold no request.
	yaml bool
}

func (o *observed) WriteHeader(code int) {
	if !o.written {
		o.status, o.written = code, true
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *observed) Write(p []byte) (int, error) {
	if !o.written {
		o.status, o.written = http.StatusOK, true
	}
	return o.ResponseWriter.Write(p)
}

func (o *observed) Unwrap() http.ResponseWriter { return o.ResponseWriter }

func (o *observed) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *observed) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := o.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("the response writer cannot be hijacked")
	}
	conn, rw, err := hj.Hijack()
	if err == nil {
		o.hijacked = true
	}
	return conn, rw, err
}

// class is the status this request is counted under. A hijacked connection
// answered 101 through the connection itself, which no writer saw.
func (o *observed) class() string {
	switch {
	case o.hijacked && !o.written:
		return metrics.StatusClass(http.StatusSwitchingProtocols)
	case !o.written:
		return metrics.StatusClass(http.StatusOK)
	}
	return metrics.StatusClass(o.status)
}

// observe is the one count and the one log line per request, deferred by
// ServeHTTP. A request that never reached the mux carries no route, which is
// the count of a bearer that was refused before any endpoint was chosen.
//
// The line is emitted under the request's own context, so the trace id and
// the span id reach both paths of the log tee and a line read out of the
// container's output joins the trace it belongs to.
func (h *handler) observe(ctx context.Context, slot *routeSlot, o *observed, started time.Time) {
	elapsed := time.Since(started)
	status, code := o.class(), envelopeCode(slot, o)
	if callerGone(ctx, o) {
		status, code = metrics.StatusClass(statusClientClosed), ClientClosed
	}
	h.metrics.Request(slot.route, status, code)
	if !o.hijacked {
		h.metrics.RequestDuration(slot.route, elapsed)
	}
	h.log.LogAttrs(ctx, slog.LevelInfo, "request",
		slog.String("route", slot.route),
		slog.String("status", status),
		slog.String("code", code),
		slog.Duration("duration", elapsed),
		slog.String("subject", slot.subject),
		slog.String("sandbox", slot.sandbox),
		slog.String("request_id", slot.requestID),
	)
}

// ClientClosed is the code a request is counted and logged under when its
// caller went away while the handler ran and the handler then answered with a
// server failure. The cancellation reached a driver, authorizer or store call
// through the request's context, and the failure it produced says nothing
// about that dependency, so it is not counted as the dependency's. The code is
// recorded and never written: nobody is left to read an envelope.
const ClientClosed = "client_closed"

// statusClientClosed is the status such a request is counted under: the 499
// that proxies log for a client that closed its request. Its class is 4xx, so
// the status label keeps its five values.
const statusClientClosed = 499

// callerGone reports whether the request's caller went away before a server
// failure was answered. net/http cancels a request's context while its
// handler runs only when the client's connection closed, and a hijacked
// connection is the handler's own and never cancels it, so a done context at
// the deferred count of a 5xx is the caller's leaving.
func callerGone(ctx context.Context, o *observed) bool {
	return !o.hijacked && o.written && o.status >= http.StatusInternalServerError && ctx.Err() != nil
}

// envelopeCode prefers the code the handler recorded on the writer and falls
// back to the slot, so a refusal written before the mux matched is still
// counted under its code.
func envelopeCode(slot *routeSlot, o *observed) string {
	if o.code != "" {
		return o.code
	}
	return slot.code
}

// handle mounts one route whose answer is one object or one page, in the
// syntax design 008 lets the request negotiate.
func (h *handler) handle(pattern string, fn http.HandlerFunc) { h.route(pattern, fn, true) }

// stream mounts one route whose content type is the route's own: an archive,
// a file body, a frame, a log, a framed stream or a socket. Nothing about
// those is negotiable, so an Accept naming the route's own type is honored
// rather than refused.
func (h *handler) stream(pattern string, fn http.HandlerFunc) { h.route(pattern, fn, false) }

// route mounts one pattern behind the mux. The wrapper is design 017's
// middleware: it fills the slot with the pattern the mux matched and names the
// server span after it, which is the only point at which the pattern is known.
// It is also where design 008's content negotiation runs, because a refusal
// owed to the caller has to reach it before the handler acts.
func (h *handler) route(pattern string, fn http.HandlerFunc, negotiates bool) {
	h.patterns = append(h.patterns, pattern)
	h.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if slot := slotOf(r.Context()); slot != nil {
			slot.route, slot.sandbox = r.Pattern, r.PathValue("id")
		}
		nameSpan(r.Context(), r.Pattern)
		stampSandbox(r.Context(), r.PathValue("id"))
		if negotiates && !acceptable(w, r) {
			return
		}
		fn(w, r)
	})
}

// nameSpan renames the server span after the route pattern, so a trace lists
// one span per endpoint rather than one per path, and gives the pattern's
// path to http.route on the span and on the request metrics. The metrics take
// it through the labeler the OpenTelemetry handler put on the context: the
// mux sets the pattern on the copy of the request it was handed, which the
// handler never sees, so without the labeler every request was measured with
// no route.
func nameSpan(ctx context.Context, pattern string) {
	if pattern == "" {
		return
	}
	route := routePath(pattern)
	if l, ok := otelhttp.LabelerFromContext(ctx); ok && route != "" {
		l.Add(attribute.String("http.route", route))
	}
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetName(pattern)
		span.SetAttributes(attribute.String("http.route", route))
	}
}

// routePath is a mux pattern without its method: http.route is the path
// template alone, and the method is on the span and the metrics already.
func routePath(pattern string) string {
	if i := strings.IndexByte(pattern, '/'); i >= 0 {
		return pattern[i:]
	}
	return ""
}

// stampSpan puts design 017's request attributes on the server span: who
// asked, under which request id, and which sandbox where the route names one.
// The trace id is the span's own and is on every log line of the request.
func stampSpan(ctx context.Context, subject, requestID string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	attrs := make([]attribute.KeyValue, 0, 2)
	if subject != "" {
		attrs = append(attrs, attribute.String("cella.subject", subject))
	}
	if requestID != "" {
		attrs = append(attrs, attribute.String("cella.request_id", requestID))
	}
	span.SetAttributes(attrs...)
}

// stampSandbox adds the sandbox a route names. The value is the path segment
// the mux matched, which design 008 lets a caller write as an id or as a
// name, so the attribute is cella.sandbox and not cella.sandbox_id: it is
// what the caller asked for and not always what the store calls it.
func stampSandbox(ctx context.Context, key string) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() && key != "" {
		span.SetAttributes(attribute.String("cella.sandbox", key))
	}
}
