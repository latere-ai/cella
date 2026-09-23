// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"latere.ai/x/cella/egress"
	"latere.ai/x/cella/internal/metrics"
)

// request is one count of cella_requests_total, as the recorder kept it.
type request struct{ route, status, code string }

// apiRecorder is design 017's seam under test.
type apiRecorder struct {
	mu        sync.Mutex
	requests  []request
	durations []string
	execs     []string
	egress    [][4]any
	snapshots int
}

func (r *apiRecorder) Request(route, status, code string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, request{route, status, code})
}

func (r *apiRecorder) RequestDuration(route string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.durations = append(r.durations, route)
}

func (r *apiRecorder) Exec(exit string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.execs = append(r.execs, exit)
}

func (r *apiRecorder) EgressConnection(decision, door string, in, out int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.egress = append(r.egress, [4]any{decision, door, in, out})
}

func (r *apiRecorder) GatewaySnapshot() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshots++
}

func (r *apiRecorder) counts() []request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

// awaitCount returns the count of one route once the handler has taken it.
// A stream's count lands after the handler returns, which is after the
// client read the last frame, so a reader that has seen the frame waits.
func (r *apiRecorder) awaitCount(t *testing.T, route string) request {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		counts := r.counts()
		if i := slices.IndexFunc(counts, func(c request) bool { return c.route == route }); i >= 0 {
			return counts[i]
		}
		if time.Now().After(deadline) {
			t.Fatalf("the route %s was not counted: %v", route, counts)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// timed reports the routes that were observed into the latency histogram.
func (r *apiRecorder) timed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.durations)
}

// TestRequestsAreCountedByRoute is design 017's first two rows: the route is
// the mux pattern and never the path, the status is the class, and a refusal
// carries design 008's code.
func TestRequestsAreCountedByRoute(t *testing.T) {
	f := setup(t, nil)
	f.request(http.MethodPost, "/v1/sandboxes", f.alice, createBody, http.StatusCreated)
	f.request(http.MethodGet, "/v1/sandboxes/sbx_00000000000000000000000000", f.alice, "", http.StatusNotFound)
	f.request(http.MethodGet, "/v1/sandboxes", "", "", http.StatusUnauthorized)

	got := f.metrics.counts()
	want := []request{
		{"POST /v1/sandboxes", "2xx", ""},
		{"GET /v1/sandboxes/{id}", "4xx", "not_found"},
		// A request refused before the mux chose an endpoint carries no
		// route: no endpoint was reached.
		{"", "4xx", "unauthenticated"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("counted %v, want %v", got, want)
	}
	// The path carries a sandbox id and the label does not, which is what
	// keeps the series bounded.
	for _, c := range got {
		if strings.Contains(c.route, "sbx_") {
			t.Errorf("the route label carries a sandbox id: %q", c.route)
		}
	}
	if timed := f.metrics.timed(); len(timed) != 3 {
		t.Errorf("timed %v, want one observation per request", timed)
	}
}

// TestStreamsAreCountedAndNotTimed is design 017's rule for a hijacked route:
// a stream's life is not a request's latency.
func TestStreamsAreCountedAndNotTimed(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("streamed")
	_, r := f.attachTo("/v1/sandboxes/"+obj.Status.ID+"/exec", f.alice, `{"command":["sh","-c","exit 0"]}`)
	if last, _ := r.ended(t); last != `{"exit":0}` {
		t.Fatalf("the session ended %q", last)
	}

	// The count is taken when the handler returns, which is after the last
	// frame reached the client, so the count is awaited rather than read.
	stream := f.metrics.awaitCount(t, "GET /v1/sandboxes/{id}/exec")
	if stream.status != "1xx" {
		t.Errorf("the upgraded stream counted as %s, want the switching-protocols class", stream.status)
	}
	if slices.Contains(f.metrics.timed(), "GET /v1/sandboxes/{id}/exec") {
		t.Error("a hijacked stream was observed into the latency histogram")
	}
}

// TestExecSessionsAreCounted is design 017's exec row: one count per session,
// by the exit code the last frame carried.
func TestExecSessionsAreCounted(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("counted")
	for _, tc := range []struct {
		command string
		frame   string
	}{{"exit 0", `{"exit":0}`}, {"exit 3", `{"exit":3}`}} {
		_, r := f.attachTo("/v1/sandboxes/"+obj.Status.ID+"/exec", f.alice,
			`{"command":["sh","-c","`+tc.command+`"]}`)
		if last, _ := r.ended(t); last != tc.frame {
			t.Fatalf("the session ended %q, want %q", last, tc.frame)
		}
	}
	if got := f.metrics.execs; !slices.Equal(got, []string{metrics.ExitZero, metrics.ExitNonzero}) {
		t.Errorf("exec exits %v, want a zero then a nonzero", got)
	}
}

// TestEgressRecordsAreCounted is design 017's rule that the connection counts
// are the control plane's: they are taken as the gateway's records arrive on
// the sync stream and never emitted by the gateway itself.
func TestEgressRecordsAreCounted(t *testing.T) {
	rec := &apiRecorder{}
	hub := NewEgressHub(EgressHubOptions{Environment: "default", Metrics: rec})
	err := hub.onRecord(egress.Record{
		Principal: "sandbox:sbx_1", Decision: egress.DecisionAllowed, Door: egress.DoorProxy,
		Host: "api.example.com", Port: 443, BytesIn: 120, BytesOut: 340,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.onRecord(egress.Record{Principal: "sandbox:sbx_1", Decision: egress.DecisionDenied, Door: egress.DoorReverse}); err != nil {
		t.Fatal(err)
	}
	want := [][4]any{
		{egress.DecisionAllowed, egress.DoorProxy, int64(120), int64(340)},
		{egress.DecisionDenied, egress.DoorReverse, int64(0), int64(0)},
	}
	if !slices.Equal(rec.egress, want) {
		t.Errorf("egress counts %v, want %v", rec.egress, want)
	}
	// A record that names no sandbox is refused and counted nowhere.
	if err := hub.onRecord(egress.Record{Decision: egress.DecisionAllowed, Door: egress.DoorProxy}); err == nil {
		t.Fatal("a record naming no sandbox was kept")
	}
	if len(rec.egress) != 2 {
		t.Errorf("a refused record was counted: %v", rec.egress)
	}
}

// TestNoRecorderCountsNothing is the default: an API built with no recorder
// answers exactly as it did before design 017.
func TestNoRecorderCountsNothing(t *testing.T) {
	h := &handler{metrics: nopMetrics{}}
	h.metrics.Request("/v1/sandboxes", "2xx", "")
	h.metrics.RequestDuration("/v1/sandboxes", time.Second)
	h.metrics.Exec(metrics.ExitZero)
	h.metrics.EgressConnection(egress.DecisionAllowed, egress.DoorProxy, 1, 1)
	h.metrics.GatewaySnapshot()
	hub := NewEgressHub(EgressHubOptions{Environment: "default"})
	if hub.metrics == nil {
		t.Error("a hub with no recorder holds no default")
	}
}

// TestRequestSpans is design 017's trace rule: one server span per request,
// named by the route pattern, carrying the subject, the request id and the
// sandbox the route names.
func TestRequestSpans(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(t.Context())
	})

	f := setup(t, nil)
	obj := f.sandbox("traced")
	ctx, span := provider.Tracer("test").Start(t.Context(), "client")
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/sandboxes/"+obj.Status.ID, nil)
	req.Header.Set("Authorization", "Bearer "+f.alice)
	// The handler is driven directly, so the span under test is the one the
	// route wrapper named and not a transport's.
	rr := httptest.NewRecorder()
	f.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET the sandbox = %d: %s", rr.Code, rr.Body)
	}
	span.End()

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("the request drew no span")
	}
	// The wrapper renames the span it was called under, which here is the
	// client span the case opened.
	named := spans[len(spans)-1]
	if named.Name != "GET /v1/sandboxes/{id}" {
		t.Errorf("the span is named %q, want the route pattern", named.Name)
	}
	attrs := map[string]string{}
	for _, kv := range named.Attributes {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	for _, key := range []string{"http.route", "cella.subject", "cella.request_id", "cella.sandbox"} {
		if attrs[key] == "" {
			t.Errorf("the span carries no %s: %v", key, attrs)
		}
	}
	// The subject is the qualified one design 006 fixes: the issuer and the
	// claim, which is what an authorizer decides on.
	if !strings.HasSuffix(attrs["cella.subject"], "|alice") {
		t.Errorf("the span names the subject %q, want the qualified alice", attrs["cella.subject"])
	}
	if attrs["cella.sandbox"] != obj.Status.ID {
		t.Errorf("the span names the sandbox %q, want %q", attrs["cella.sandbox"], obj.Status.ID)
	}
}

// TestOneLogLinePerRequest is design 017's log volume: one line at INFO per
// request, carrying the route, the status, the code, the duration, the
// subject, the sandbox the route names and the request id, and none per
// frame of a stream.
func TestOneLogLinePerRequest(t *testing.T) {
	f, sink := setupLogging(t)
	obj := f.sandbox("logged")
	sink.clear()
	f.request(http.MethodGet, "/v1/sandboxes/"+obj.Status.ID, f.alice, "", http.StatusOK)

	line := logLines(t, sink, 1)[0]
	for key, want := range map[string]any{
		"msg":     "request",
		"level":   "INFO",
		"route":   "GET /v1/sandboxes/{id}",
		"status":  "2xx",
		"code":    "",
		"sandbox": obj.Status.ID,
	} {
		if got := line[key]; got != want {
			t.Errorf("the line's %s is %v, want %v", key, got, want)
		}
	}
	if s, _ := line["subject"].(string); !strings.HasSuffix(s, "|alice") {
		t.Errorf("the line names the subject %q", s)
	}
	if id, _ := line["request_id"].(string); id == "" {
		t.Errorf("the line carries no request id: %v", line)
	}
	if _, ok := line["duration"]; !ok {
		t.Errorf("the line carries no duration: %v", line)
	}
}

// TestOneLogLinePerStreamAndNonePerFrame is the other half of the volume
// rule: a stream that carried many frames is still one line.
func TestOneLogLinePerStreamAndNonePerFrame(t *testing.T) {
	f, sink := setupLogging(t)
	obj := f.sandbox("streamed")
	sink.clear()
	_, r := f.attachTo("/v1/sandboxes/"+obj.Status.ID+"/exec", f.alice,
		`{"command":["sh","-c","for i in 1 2 3 4 5; do printf 'line%s\n' $i; done; exit 0"]}`)
	if last, _ := r.ended(t); last != `{"exit":0}` {
		t.Fatalf("the session ended %q", last)
	}
	if got := logLines(t, sink, 1)[0]["route"]; got != "GET /v1/sandboxes/{id}/exec" {
		t.Errorf("the line's route is %v", got)
	}
}

// logSink is where a case reads the lines the handler wrote. The handler
// writes from the request's own goroutine and a stream's line lands after the
// client has seen the last frame, so the sink is synchronized and the case
// waits for the line rather than racing it.
type logSink struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *logSink) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// setupLogging is the fixture with its log line written to a sink.
func setupLogging(t *testing.T) (*fixture, *logSink) {
	t.Helper()
	sink := &logSink{}
	f := setup(t, nil)
	f.h.(*handler).log = slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return f, sink
}

// logLines waits for want lines and returns them, so a case states the volume
// rule rather than the timing of a deferred write.
func logLines(t *testing.T, sink *logSink, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out := parseLines(t, sink.String())
		switch {
		case len(out) == want:
			// One more read after a pause catches a second line the
			// handler had not written yet, which is the failure this rule
			// is about.
			time.Sleep(50 * time.Millisecond)
			if extra := parseLines(t, sink.String()); len(extra) != want {
				t.Fatalf("the handler wrote %d lines, want %d: %s", len(extra), want, sink.String())
			}
			return out
		case len(out) > want, time.Now().After(deadline):
			t.Fatalf("the handler wrote %d lines, want %d: %s", len(out), want, sink.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func parseLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Fatalf("a log line does not parse: %v: %s", err, l)
		}
		lines = append(lines, line)
	}
	return lines
}
