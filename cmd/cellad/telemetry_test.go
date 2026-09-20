// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/internal/metrics"
	"latere.ai/x/cella/internal/store"
)

// scrape reads the internal listener's exposition.
func scrape(t *testing.T, internalURL string) string {
	t.Helper()
	code, body := get(t, internalURL+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("GET /metrics = %d: %s", code, body)
	}
	return body
}

// sample is the value of one series in an exposition, and false where the
// exposition carries no such line.
func sample(out, line string) (float64, bool) {
	for l := range strings.SplitSeq(out, "\n") {
		// A route label carries a method and a space, so the value is what
		// follows the last space and not the first.
		l = strings.TrimSpace(l)
		cut := strings.LastIndex(l, " ")
		if cut < 0 || l[:cut] != line {
			continue
		}
		v, err := strconv.ParseFloat(l[cut+1:], 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// TestScrapeSurfaceIsTheInternalListener is design 017's one scrape surface:
// the internal address serves it and no public route does.
func TestScrapeSurfaceIsTheInternalListener(t *testing.T) {
	publicURL, internalURL, stop := startServe(t)
	defer func() { stop() }()

	out := scrape(t, internalURL)
	for _, row := range metrics.Table {
		if row.Await != "" {
			if strings.Contains(out, "# TYPE "+row.Name+" ") {
				t.Errorf("a scrape carries %s, which waits on spec %s", row.Name, row.Await)
			}
			continue
		}
		if row.Kind == metrics.KindGauge {
			continue // A gauge with no value publishes no family.
		}
		if !strings.Contains(out, "# TYPE "+row.Name+" "+string(row.Kind)) {
			t.Errorf("a scrape carries no %s", row.Name)
		}
	}
	// The probes still answer on the same listener, so mounting the surface
	// took nothing away.
	if code, body := get(t, internalURL+"/readyz"); code != 200 || body != "ok\n" {
		t.Errorf("GET /readyz on the internal listener = %d %q", code, body)
	}
	if code, _ := get(t, publicURL+"/metrics"); code != 404 {
		t.Errorf("GET /metrics on the public listener = %d, want 404: the numbers are the installation's", code)
	}
}

// TestStartUpLineSaysWhetherTelemetryLeaves is design 017's one word on the
// first line an operator reads.
func TestStartUpLineSaysWhetherTelemetryLeaves(t *testing.T) {
	_, _, log, stop := startServeWithLog(t, nil)
	if !strings.Contains(log(), "telemetry=off") {
		t.Errorf("the start-up line does not say telemetry=off: %q", log())
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1/")
	_, _, log, stop = startServeWithLog(t, nil)
	defer func() { stop() }()
	if !strings.Contains(log(), "telemetry=otlp") {
		t.Errorf("the start-up line does not say telemetry=otlp: %q", log())
	}
}

// TestTelemetryModeReadsTheExporterVariables is the rule that this project
// reads no variable of its own: the exporter is pkg/otel's OTEL_* set, and
// OTEL_SDK_DISABLED turns every signal off with the endpoint still set.
func TestTelemetryModeReadsTheExporterVariables(t *testing.T) {
	if got := telemetryMode(); got != "off" {
		t.Errorf("with no endpoint, telemetryMode = %q", got)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.invalid:4318")
	if got := telemetryMode(); got != "otlp" {
		t.Errorf("with an endpoint, telemetryMode = %q", got)
	}
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if got := telemetryMode(); got != "off" {
		t.Errorf("with the SDK disabled, telemetryMode = %q", got)
	}
}

// TestObservabilityEndToEnd is design 017's acceptance run: serve on the
// native driver, create, exec, stop and delete, then read the scrape surface
// and see each counter the run owns move.
func TestObservabilityEndToEnd(t *testing.T) {
	issuer := issuertest.New(t)
	base, internalURL, _, stop := startServeWithLog(t, map[string]string{
		"CELLA_OIDC_ISSUERS":  issuer.URL(),
		"CELLA_OIDC_AUDIENCE": "cella,platform.example",
	})
	defer func() { stop() }()
	alice := issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"platform.example"}})

	request := func(method, path, body string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+alice)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, resp.StatusCode, data, want)
		}
		return data
	}

	before := scrape(t, internalURL)
	created := request("POST", "/v1/sandboxes",
		`{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"observed"},"spec":{}}`, 201)
	var obj struct {
		Status struct{ ID string } `json:"status"`
	}
	if err := json.Unmarshal(created, &obj); err != nil {
		t.Fatal(err)
	}
	path := "/v1/sandboxes/" + obj.Status.ID
	request("GET", path, "", 200)
	execOnce(t, base, obj.Status.ID, alice, `["sh","-c","exit 0"]`, `{"exit":0}`)
	execOnce(t, base, obj.Status.ID, alice, `["sh","-c","exit 7"]`, `{"exit":7}`)
	request("POST", path+"/stop", "", 200)
	request("DELETE", path, "", 202)
	request("GET", path, "", 404)

	out := scrape(t, internalURL)
	for _, tc := range []struct {
		series string
		want   float64
	}{
		{`cella_requests_total{code="",route="POST /v1/sandboxes",status="2xx"}`, 1},
		{`cella_requests_total{code="",route="GET /v1/sandboxes/{id}",status="2xx"}`, 1},
		{`cella_requests_total{code="",route="POST /v1/sandboxes/{id}/{verb}",status="2xx"}`, 1},
		{`cella_requests_total{code="",route="DELETE /v1/sandboxes/{id}",status="2xx"}`, 1},
		{`cella_requests_total{code="not_found",route="GET /v1/sandboxes/{id}",status="4xx"}`, 1},
		{`cella_request_duration_seconds_count{route="POST /v1/sandboxes"}`, 1},
		{`cella_sandbox_create_duration_seconds_count{driver="native",pool="miss"}`, 1},
		// The four requests above and the two exec sockets are six
		// decisions; the create asks a seventh, for the environment it
		// places the sandbox in. The built-in owner policy answers them on
		// the same seam an operator's endpoint would.
		{`cella_decisions_total{endpoint="authorizer",outcome="allow"}`, 7},
	} {
		got, ok := sample(out, tc.series)
		if !ok {
			t.Errorf("the exposition has no %s:\n%s", tc.series, out)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %g, want %g", tc.series, got, tc.want)
		}
	}
	for _, series := range []string{`cella_exec_total{exit="0"}`, `cella_exec_total{exit="nonzero"}`} {
		if got, ok := sample(out, series); !ok || got != 1 {
			t.Errorf("%s = %g, %v; want one", series, got, ok)
		}
	}
	// A hijacked stream is counted and not timed: its life is not a
	// request's latency.
	if got, ok := sample(out, `cella_requests_total{code="",route="GET /v1/sandboxes/{id}/exec",status="1xx"}`); !ok || got != 2 {
		t.Errorf("the exec route was counted %g times, %v; want two upgrades", got, ok)
	}
	if _, ok := sample(out, `cella_request_duration_seconds_count{route="GET /v1/sandboxes/{id}/exec"}`); ok {
		t.Error("a hijacked stream was observed into the latency histogram")
	}
	// The store histogram carries a series per operation before anything is
	// observed. This run keeps desired state in the snapshot of design 026,
	// which is not design 010's store, so the observations are zero and the
	// series still exist: a rate over them reads zero rather than absent.
	if got, ok := sample(out, `cella_store_query_duration_seconds_count{op="write"}`); !ok || got != 0 {
		t.Errorf("cella_store_query_duration_seconds_count{op=\"write\"} = %g, %v:\n%s", got, ok, out)
	}
	// The reaper ticks under its own lease, which is the gauge an alert on a
	// lease not held reads.
	if got, ok := sample(out, `cella_lease_held{name="reaper"}`); !ok || got != 1 {
		t.Errorf("cella_lease_held{name=\"reaper\"} = %g, %v; want the lease held", got, ok)
	}
	// The gauges answer from the controller's index and the journal's
	// backlog, which the first scrape of the run already carried.
	if !strings.Contains(before, "cella_events_pending") {
		t.Errorf("the first scrape carried no cella_events_pending:\n%s", before)
	}
	if strings.Contains(out, "sbx_") {
		t.Errorf("a label carries a sandbox id:\n%s", out)
	}
}

// TestTheAPIDrawsAServerSpan is design 017's trace seam: /v1/ is wrapped and
// the probes are not, so a request carries the trace header back and a probe
// does not. The span's name is the route, which the wrapper behind the mux
// sets once the pattern is known.
func TestTheAPIDrawsAServerSpan(t *testing.T) {
	// A span exists only where a provider does, and a provider exists only
	// where the deployment set an endpoint. The collector takes what is
	// exported; the sampler takes every root span so one request is enough.
	var collected safeBuffer
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		collected.Write(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1.0")

	issuer := issuertest.New(t, issuertest.WithDefaultAudience("cella"))
	base, internalURL, _, stop := startServeWithLog(t, map[string]string{"CELLA_OIDC_ISSUERS": issuer.URL()})
	alice := issuer.Mint(issuertest.Claims{Sub: "alice"})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+alice)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.Header.Get("X-Trace-Id") == "" {
		t.Errorf("a /v1/ request drew no server span: the answer carries no trace header")
	}
	for _, url := range []string{base + "/livez", internalURL + "/readyz", internalURL + "/metrics"} {
		probe, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		answer, err := http.DefaultClient.Do(probe)
		if err != nil {
			t.Fatal(err)
		}
		_ = answer.Body.Close()
		if answer.Header.Get("X-Trace-Id") != "" {
			t.Errorf("%s drew a span; the probes and the scrape surface are not instrumented", url)
		}
	}
	// Stopping flushes the batch, so the collector holds the span this
	// request drew, named by the route the mux matched.
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(collected.String(), "GET /v1/sandboxes") {
		t.Errorf("the collector received no span named by the route: %d bytes", collected.Len())
	}
}

// safeBuffer is a bytes.Buffer a collector's handler writes to while the
// test reads it.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *safeBuffer) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Len()
}

// TestCanaryNeverReachesALogLine is design 017's redaction, driven through
// the wiring cellad installs rather than the handler alone: every canary is
// written through the default logger and none of them appears on stderr.
func TestCanaryNeverReachesALogLine(t *testing.T) {
	var stderr bytes.Buffer
	tel := startTelemetry(t.Context(), "cellad-test", &stderr)
	defer tel.shutdown()

	canaries := map[string]string{
		"env":                     "canary-env",
		"value":                   "canary-value",
		"token":                   "canary-token",
		"credential":              "canary-credential",
		"secret":                  "canary-secret",
		"Authorization":           "Bearer canary-authorization",
		"Proxy-Authorization":     "Basic canary-proxy",
		"Cella-Egress-Credential": "canary-egress",
	}
	const placeholder = "cph_abcdefghijklmnopqrstuvwxyz234567"
	for key, value := range canaries {
		tel.log.Info("a line", key, value)
		slog.Default().Info("a line through the default", key, value)
		tel.log.With(key, value).Info("a carried line")
	}
	tel.log.Info("a line carrying " + placeholder)
	slog.Default().Info("a line", "upstream", placeholder)

	out := stderr.String()
	for key, value := range canaries {
		if strings.Contains(out, value) {
			t.Errorf("the canary under %q reached stderr: %s", key, out)
		}
	}
	if strings.Contains(out, placeholder) {
		t.Errorf("a placeholder reached stderr: %s", out)
	}
	if !strings.Contains(out, metrics.Redacted) {
		t.Errorf("nothing was redacted; the check would pass vacuously: %s", out)
	}
}

// TestTelemetryExportsOverOTLP is design 017's rule that a role with no
// scrape surface still reports: with the endpoint set, the spans and the log
// records reach the collector, and the canary is redacted on that path too.
func TestTelemetryExportsOverOTLP(t *testing.T) {
	var collected safeBuffer
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		collected.Write(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)

	var stderr bytes.Buffer
	tel := startTelemetry(t.Context(), "cellad-test", &stderr)
	if tel.mode != "otlp" {
		t.Fatalf("with an endpoint set, the mode is %q", tel.mode)
	}
	tel.log.Info("a line the bridge takes", "token", "canary-on-the-bridge")
	tel.shutdown()

	if collected.Len() == 0 {
		t.Fatal("the collector received nothing")
	}
	if strings.Contains(collected.String(), "canary-on-the-bridge") {
		t.Error("the canary reached the OTLP bridge: redaction is on one path only")
	}
	if !strings.Contains(collected.String(), metrics.Redacted) {
		t.Error("the bridge carried no redacted value; the check would pass vacuously")
	}
}

// TestEgressRoleServesNoScrapeSurface is design 017's rule that the gateway
// opens its two doors and its one outbound stream and no listener beyond
// them: its telemetry leaves over OTLP and its connection counts are the
// control plane's, taken as its records arrive.
func TestEgressRoleServesNoScrapeSurface(t *testing.T) {
	proxyAddr, reverseAddr := freePort(t), freePort(t)
	plane := startPlane(t, proxyAddr, reverseAddr)

	var out syncBuffer
	ctx, cancel := context.WithCancel(t.Context())
	codec := make(chan int, 1)
	go func() {
		codec <- run(ctx, []string{"egress"}, env(map[string]string{
			"CELLA_URL":                 plane.url,
			"CELLA_ENVIRONMENT_KEY":     plane.environmentKey(t),
			"CELLA_EGRESS_PROXY_ADDR":   proxyAddr,
			"CELLA_EGRESS_REVERSE_ADDR": reverseAddr,
		}), &out, io.Discard)
	}()
	defer func() {
		cancel()
		select {
		case <-codec:
		case <-time.After(30 * time.Second):
			t.Error("the egress role did not stop")
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(out.String(), "egress proxy=") {
		if time.Now().After(deadline) {
			t.Fatalf("the egress role never reported its doors; stdout %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "telemetry=off") {
		t.Errorf("the gateway's start-up line does not say telemetry=off: %q", out.String())
	}
	// Neither door serves an exposition: they speak the boundary's
	// protocols and nothing else.
	for _, addr := range []string{proxyAddr, reverseAddr} {
		code, body := get(t, "http://"+addr+"/metrics")
		if code == http.StatusOK && strings.Contains(body, "# TYPE cella_") {
			t.Errorf("the gateway served an exposition on %s", addr)
		}
	}
}

// TestVocabulariesAgree holds the label values a package passes to the table
// that declares them. The root packages of design 001 cannot import the
// registry, so each repeats the words it uses; this is the one place that
// reaches both and is where a drift is caught.
func TestVocabulariesAgree(t *testing.T) {
	for _, tc := range []struct{ from, declared string }{
		{controller.PoolHit, metrics.PoolHit},
		{controller.PoolMiss, metrics.PoolMiss},
		{controller.MetricAdopted, metrics.OutcomeAdopted},
		{controller.MetricMiss, metrics.OutcomeMiss},
		{controller.MetricRecovered, metrics.OutcomeRecovered},
		{controller.MetricExhausted, metrics.OutcomeExhausted},
		{controller.MetricLeaseReaper, metrics.LeaseReaper},
		{controller.MetricLeasePool, metrics.LeasePool},
		{events.MetricLeaseJournal, metrics.LeaseJournal},
		{events.MetricAcknowledged, metrics.OutcomeAcknowledged},
		{events.MetricDeferred, metrics.OutcomeDeferred},
		{events.MetricDropped, metrics.OutcomeDropped},
		{auth.MetricEndpointAuthorizer, metrics.EndpointAuthorizer},
		{auth.MetricAllow, metrics.OutcomeAllow},
		{auth.MetricDeny, metrics.OutcomeDeny},
		{auth.MetricUnavailable, metrics.OutcomeUnavailable},
		{store.OpLoad, metrics.OpLoad},
		{store.OpSave, metrics.OpSave},
		{store.OpWrite, metrics.OpWrite},
		{store.OpRemove, metrics.OpRemove},
		{store.OpRebuild, metrics.OpRebuild},
		{store.OpEvents, metrics.OpEvents},
		{store.OpAcquire, metrics.OpAcquire},
	} {
		if tc.from != tc.declared {
			t.Errorf("a package passes %q where the table declares %q", tc.from, tc.declared)
		}
	}
	// Every word a package passes is in the vocabulary the table publishes.
	for _, word := range []string{controller.MetricLeaseReaper, controller.MetricLeasePool, events.MetricLeaseJournal} {
		if !slices.Contains(metrics.Leases, word) {
			t.Errorf("%q is passed as a lease name and is outside %v", word, metrics.Leases)
		}
	}
}

// TestAdmissionOutcomesMapToTheVocabulary is design 007's three results on
// design 017's decision counter.
func TestAdmissionOutcomesMapToTheVocabulary(t *testing.T) {
	for result, want := range map[string]string{
		"allow":   metrics.OutcomeAllow,
		"refused": metrics.OutcomeDeny,
		"error":   metrics.OutcomeUnavailable,
		"":        metrics.OutcomeUnavailable,
	} {
		if got := admissionOutcome(result); got != want {
			t.Errorf("admissionOutcome(%q) = %q, want %q", result, got, want)
		}
	}
}

// TestPhaseCountsFolds is the scrape-time answer for cella_sandboxes.
func TestPhaseCountsFolds(t *testing.T) {
	got := phaseCounts([]string{"Running", "Running", "Stopped"})
	if got["Running"] != 2 || got["Stopped"] != 1 || len(got) != 2 {
		t.Errorf("phaseCounts = %v", got)
	}
}

// execOnce runs one command over the exec socket and holds the last frame to
// what the session should have answered.
func execOnce(t *testing.T, base, id, token, command, want string) {
	t.Helper()
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	dialer := websocket.Dialer{Subprotocols: []string{"cella.exec.v1"}, HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(t.Context(),
		"ws"+strings.TrimPrefix(base, "http")+"/v1/sandboxes/"+id+"/exec", header)
	if err != nil {
		t.Fatalf("dialing the exec socket: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"command":`+command+`}`)); err != nil {
		t.Fatal(err)
	}
	last := ""
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if kind == websocket.TextMessage {
			last = string(data)
		}
	}
	if last != want {
		t.Fatalf("the session ended %q, want %q", last, want)
	}
}
