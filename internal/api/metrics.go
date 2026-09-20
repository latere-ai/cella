// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"time"

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

// observe is the one count per request, deferred by ServeHTTP. A request that
// never reached the mux carries no route, which is the count of a bearer that
// was refused before any endpoint was chosen.
func (h *handler) observe(slot *routeSlot, o *observed, started time.Time) {
	h.metrics.Request(slot.route, o.class(), envelopeCode(slot, o))
	if !o.hijacked {
		h.metrics.RequestDuration(slot.route, time.Since(started))
	}
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

// handle mounts one route behind the mux. The wrapper is design 017's
// middleware: it fills the slot with the pattern the mux matched and names the
// server span after it, which is the only point at which the pattern is known.
func (h *handler) handle(pattern string, fn http.HandlerFunc) {
	h.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if slot := slotOf(r.Context()); slot != nil {
			slot.route = r.Pattern
		}
		nameSpan(r.Context(), r.Pattern)
		stampSandbox(r.Context(), r.PathValue("id"))
		fn(w, r)
	})
}

// nameSpan renames the server span after the route pattern, so a trace lists
// one span per endpoint rather than one per path.
func nameSpan(ctx context.Context, route string) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() && route != "" {
		span.SetName(route)
		span.SetAttributes(attribute.String("http.route", route))
	}
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

// stampSandbox adds the sandbox a route names, once the handler has resolved
// it. It is a separate call because the id is a path value the mux produced
// and not something the outer handler could know.
func stampSandbox(ctx context.Context, id string) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() && id != "" {
		span.SetAttributes(attribute.String("cella.sandbox_id", id))
	}
}
