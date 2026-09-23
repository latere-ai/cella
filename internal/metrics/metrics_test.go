// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metrics_test

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/internal/metrics"
)

// full builds a registry with every pull gauge wired, as cmd/cellad does.
func full() *metrics.Registry {
	return metrics.New(metrics.Options{
		Environment: "default",
		Driver:      "native",
		Sandboxes:   func() map[string]int { return map[string]int{"Running": 2, "Stopped": 1} },
		Gateways:    func() int { return 3 },
		Pending:     func() (int, bool) { return 7, true },
		Queues: func() []metrics.Series {
			return []metrics.Series{{Labels: map[string]string{"environment": "default", "queue": "rollouts"}, Value: 4}}
		},
		Capacity: func() []metrics.Series {
			return []metrics.Series{
				{Labels: map[string]string{"environment": "default", "resource": "cpu", "kind": "declared"}, Value: 8},
				{Labels: map[string]string{"environment": "default", "resource": "cpu", "kind": "used"}, Value: 2.5},
			}
		},
	})
}

// exposition serves the registry once and returns what a scrape reads.
func exposition(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text format", ct)
	}
	return rec.Body.String()
}

// scrape returns the metric families a scrape carries, by name.
func scrape(t *testing.T, r *metrics.Registry) map[string]string {
	t.Helper()
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(exposition(t, r)))
	for sc.Scan() {
		if name, kind, ok := strings.Cut(strings.TrimPrefix(sc.Text(), "# TYPE "), " "); ok && strings.HasPrefix(sc.Text(), "# TYPE ") {
			out[name] = kind
		}
	}
	return out
}

// series reports the value of one sample line, and false where the exposition
// carries no such series.
func series(t *testing.T, r *metrics.Registry, line string) bool {
	t.Helper()
	for l := range strings.SplitSeq(exposition(t, r), "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

// TestScrapeCarriesTheRegisteredTable is the scrape surface of design 017:
// every registered family, with the type the table declares.
func TestScrapeCarriesTheRegisteredTable(t *testing.T) {
	r := full()
	// The two pushed gauges exist once their loop has reported, so a scrape
	// that must carry every family reports on them first.
	r.LeaseHeld(metrics.LeaseReaper, true)
	r.PoolSize(0, 0)
	got := scrape(t, r)
	for _, row := range metrics.Table {
		if row.Await != "" {
			continue
		}
		kind, ok := got[row.Name]
		if !ok {
			t.Errorf("a scrape carries no %s", row.Name)
			continue
		}
		if kind != string(row.Kind) {
			t.Errorf("%s is exposed as %s, the table declares %s", row.Name, kind, row.Kind)
		}
	}
	for name := range got {
		if !metrics.Registered(name) {
			t.Errorf("a scrape carries %s, which the table does not register", name)
		}
	}
}

// TestSeriesExistAtStart is design 017's rule for a labeled histogram: a
// series exists before its first observation, so a rate over a quiet
// installation is zero and not absent.
func TestSeriesExistAtStart(t *testing.T) {
	out := exposition(t, full())
	for _, want := range []string{
		`cella_webhook_duration_seconds_count{endpoint="authorizer"} 0`,
		`cella_webhook_duration_seconds_count{endpoint="admission"} 0`,
		`cella_store_query_duration_seconds_count{op="write"} 0`,
		`cella_store_query_duration_seconds_count{op="acquire"} 0`,
		`cella_sandbox_create_duration_seconds_count{driver="native",pool="hit"} 0`,
		`cella_sandbox_create_duration_seconds_count{driver="native",pool="miss"} 0`,
		`cella_event_delivery_duration_seconds_count 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the exposition has no series %q", want)
		}
	}
}

// TestPullGaugesReadTheirIndex proves the three gauges a scrape asks for read
// the closure they were built with, with the environment and driver labels
// the registry owns.
func TestPullGaugesReadTheirIndex(t *testing.T) {
	r := full()
	for _, want := range []string{
		`cella_sandboxes{driver="native",environment="default",phase="Running"} 2`,
		`cella_sandboxes{driver="native",environment="default",phase="Stopped"} 1`,
		`cella_gateways_connected{environment="default"} 3`,
		`cella_events_pending 7`,
		`cella_queue_depth{environment="default",queue="rollouts"} 4`,
		`cella_capacity{environment="default",kind="declared",resource="cpu"} 8`,
		`cella_capacity{environment="default",kind="used",resource="cpu"} 2.5`,
	} {
		if !series(t, r, want) {
			t.Errorf("the exposition has no series %q", want)
		}
	}
}

// TestGaugesWithoutAnIndexPublishNothing is the nil closure: a control plane
// with no journal, no hub or no controller publishes no series rather than a
// zero an alert would read as an answer.
func TestGaugesWithoutAnIndexPublishNothing(t *testing.T) {
	out := exposition(t, metrics.New(metrics.Options{Environment: "default"}))
	for _, name := range []string{"cella_sandboxes", "cella_gateways_connected", "cella_events_pending", "cella_lease_held", "cella_pool_size", "cella_queue_depth", "cella_capacity"} {
		if strings.Contains(out, "\n"+name) || strings.HasPrefix(out, name) {
			t.Errorf("a registry with no index published %s", name)
		}
	}
}

// TestPushedGaugesAppearOnlyOnceReported is the lease rule: a replica that
// runs no delivery loop publishes no journal series, so an alert on a lease
// not held cannot fire on a loop that does not exist.
func TestPushedGaugesAppearOnlyOnceReported(t *testing.T) {
	r := full()
	if series(t, r, `cella_lease_held{name="journal"} 0`) {
		t.Error("a lease no loop has reported on is in the exposition")
	}
	r.LeaseHeld(metrics.LeaseReaper, true)
	r.LeaseHeld(metrics.LeasePool, false)
	if !series(t, r, `cella_lease_held{name="reaper"} 1`) {
		t.Error("the reaper reported its lease held and the exposition says otherwise")
	}
	if !series(t, r, `cella_lease_held{name="pool"} 0`) {
		t.Error("the pool loop reported its lease lost and the exposition says otherwise")
	}
	r.LeaseHeld(metrics.LeaseReaper, false)
	if !series(t, r, `cella_lease_held{name="reaper"} 0`) {
		t.Error("the reaper lost its lease and the exposition still says it holds it")
	}

	if series(t, r, `cella_pool_size{environment="default",state="ready"} 0`) {
		t.Error("an environment whose refill loop has not ticked publishes a pool size")
	}
	r.PoolSize(4, 1)
	if !series(t, r, `cella_pool_size{environment="default",state="ready"} 4`) ||
		!series(t, r, `cella_pool_size{environment="default",state="filling"} 1`) {
		t.Errorf("the pool size the refill loop reported is not in the exposition:\n%s", exposition(t, r))
	}
}

// TestCountersRecordWhatTheyOwn moves every pushed instrument once and reads
// the series back, which is the shape each owner package's own test asserts
// through its interface.
func TestCountersRecordWhatTheyOwn(t *testing.T) {
	r := full()
	r.Request("/v1/sandboxes", "2xx", "")
	r.Request("/v1/sandboxes/{id}", "4xx", "not_found")
	r.RequestDuration("/v1/sandboxes", 120*time.Millisecond)
	r.Exec(metrics.ExitZero)
	r.Exec(metrics.ExitFailed)
	r.EgressConnection(metrics.DecisionAllowed, metrics.DoorProxy, 30, 70)
	r.EgressConnection(metrics.DecisionDenied, metrics.DoorReverse, 0, 0)
	r.GatewaySnapshot()
	r.SandboxCreated(metrics.PoolHit, 2*time.Second)
	r.PoolAdoption(metrics.OutcomeAdopted)
	r.SandboxPreempted()
	r.ReaperAction("Expired", "deleted")
	r.RecoveryAttempt(metrics.OutcomeRecovered)
	r.TokenReminted()
	r.Decision(metrics.EndpointAuthorizer, metrics.OutcomeAllow, 5*time.Millisecond)
	r.Decision(metrics.EndpointAdmission, metrics.OutcomeUnavailable, time.Second)
	r.EventDelivered(metrics.OutcomeAcknowledged)
	r.EventDeliveryDuration(15 * time.Millisecond)
	r.StoreQuery(metrics.OpWrite, 3*time.Millisecond)

	for _, want := range []string{
		`cella_requests_total{code="",route="/v1/sandboxes",status="2xx"} 1`,
		`cella_requests_total{code="not_found",route="/v1/sandboxes/{id}",status="4xx"} 1`,
		`cella_request_duration_seconds_count{route="/v1/sandboxes"} 1`,
		`cella_exec_total{exit="0"} 1`,
		`cella_exec_total{exit="failed"} 1`,
		`cella_egress_connections_total{decision="allowed",door="proxy"} 1`,
		`cella_egress_connections_total{decision="denied",door="reverse"} 1`,
		`cella_egress_bytes_total{direction="in",door="proxy"} 30`,
		`cella_egress_bytes_total{direction="out",door="proxy"} 70`,
		`cella_gateway_snapshots_total 1`,
		`cella_sandbox_create_duration_seconds_count{driver="native",pool="hit"} 1`,
		`cella_pool_adoptions_total{outcome="adopted"} 1`,
		`cella_preemptions_total 1`,
		`cella_reaper_actions_total{action="deleted",rule="Expired"} 1`,
		`cella_recovery_attempts_total{outcome="recovered"} 1`,
		`cella_tokens_reminted_total 1`,
		`cella_decisions_total{endpoint="authorizer",outcome="allow"} 1`,
		`cella_decisions_total{endpoint="admission",outcome="unavailable"} 1`,
		`cella_webhook_duration_seconds_count{endpoint="authorizer"} 1`,
		`cella_events_delivered_total{outcome="acknowledged"} 1`,
		`cella_event_delivery_duration_seconds_count 1`,
		`cella_store_query_duration_seconds_count{op="write"} 1`,
	} {
		if !series(t, r, want) {
			t.Errorf("after recording, the exposition has no series %q", want)
		}
	}
	// A connection that moved no bytes publishes no byte series, so a door
	// with no traffic is absent rather than a zero that reads as measured.
	if series(t, r, `cella_egress_bytes_total{direction="in",door="reverse"} 0`) {
		t.Error("a connection that carried no bytes wrote a byte series")
	}
}

// TestCachedDecisionIsCountedAndNotTimed is design 006's cache: an allow held
// in the cache made no call, so it moves the counter and not the histogram.
func TestCachedDecisionIsCountedAndNotTimed(t *testing.T) {
	r := full()
	r.Decision(metrics.EndpointAuthorizer, metrics.OutcomeCached, 0)
	if !series(t, r, `cella_decisions_total{endpoint="authorizer",outcome="cached"} 1`) {
		t.Error("a cached decision was not counted")
	}
	if !series(t, r, `cella_webhook_duration_seconds_count{endpoint="authorizer"} 0`) {
		t.Error("a cached decision was timed as if a call had been made")
	}
}

// TestLabelValuesAreBounded holds every label value a scrape carries to the
// vocabularies design 017 fixes, so no sandbox id, subject, name or path can
// reach a series. The route label is the mux pattern, which is a template and
// never a path.
func TestLabelValuesAreBounded(t *testing.T) {
	r := full()
	r.Request("/v1/sandboxes/{id}", "2xx", "")
	r.Exec(metrics.ExitNonzero)
	r.EgressConnection(metrics.DecisionPassthrough, metrics.DoorReverse, 1, 1)
	r.SandboxCreated(metrics.PoolMiss, time.Second)
	r.PoolAdoption(metrics.OutcomeMiss)
	r.ReaperAction("AutoStop", "stopped")
	r.RecoveryAttempt(metrics.OutcomeExhausted)
	r.Decision(metrics.EndpointAdmission, metrics.OutcomeDeny, time.Millisecond)
	r.EventDelivered(metrics.OutcomeDropped)
	r.StoreQuery(metrics.OpAcquire, time.Millisecond)
	r.LeaseHeld(metrics.LeaseJournal, true)
	r.PoolSize(1, 0)

	bounded := map[string][]string{
		"endpoint":    metrics.Endpoints,
		"door":        metrics.Doors,
		"decision":    metrics.Decisions,
		"direction":   metrics.Directions,
		"exit":        metrics.Exits,
		"op":          metrics.StoreOps,
		"name":        metrics.Leases,
		"pool":        metrics.PoolResults,
		"state":       metrics.PoolStates,
		"driver":      {"native"},
		"environment": {"default"},
	}
	for _, kv := range labelPairs(exposition(t, r)) {
		allowed, checked := bounded[kv[0]]
		if !checked {
			continue
		}
		if !contains(allowed, kv[1]) {
			t.Errorf("the label %s carries %q, which is outside its vocabulary %v", kv[0], kv[1], allowed)
		}
	}
}

var pairPattern = regexp.MustCompile(`([a-z_]+)="([^"]*)"`)

func labelPairs(out string) [][2]string {
	var pairs [][2]string
	for _, m := range pairPattern.FindAllStringSubmatch(out, -1) {
		pairs = append(pairs, [2]string{m[1], m[2]})
	}
	return pairs
}

func contains(list []string, v string) bool {
	return slices.Contains(list, v)
}

// TestExitOfAndStatusClass are the two mappings a caller uses to stay inside
// the vocabulary.
func TestExitOfAndStatusClass(t *testing.T) {
	if metrics.ExitOf(0) != metrics.ExitZero || metrics.ExitOf(3) != metrics.ExitNonzero {
		t.Error("an exit code does not map to the exit vocabulary")
	}
	for code, want := range map[int]string{100: "1xx", 200: "2xx", 301: "3xx", 404: "4xx", 503: "5xx", 0: "0"} {
		if got := metrics.StatusClass(code); got != want {
			t.Errorf("StatusClass(%d) = %q, want %q", code, got, want)
		}
	}
}

// TestConcurrentRecordingIsSafe drives the pushed instruments and a scrape at
// once, which is what a running control plane does.
func TestConcurrentRecordingIsSafe(t *testing.T) {
	r := full()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				r.Request("/v1/sandboxes", "2xx", "")
				r.LeaseHeld(metrics.LeaseReaper, true)
				r.PoolSize(1, 1)
				r.StoreQuery(metrics.OpLoad, time.Millisecond)
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			_ = exposition(t, r)
		}
	})
	wg.Wait()
	if !series(t, r, `cella_requests_total{code="",route="/v1/sandboxes",status="2xx"} 400`) {
		t.Errorf("the counter did not reach 400:\n%s", exposition(t, r))
	}
}

// TestWithGatewaysPublishesOneFamily is the rule that makes the exposition
// parseable: a name is one family however often the wiring attaches to it.
func TestWithGatewaysPublishesOneFamily(t *testing.T) {
	r := metrics.New(metrics.Options{Environment: "default"})
	r.WithGateways(nil)
	if strings.Contains(exposition(t, r), "cella_gateways_connected") {
		t.Error("a nil closure published the gateway gauge")
	}
	r.WithGateways(func() int { return 1 })
	r.WithGateways(func() int { return 4 })
	out := exposition(t, r)
	if n := strings.Count(out, "# TYPE cella_gateways_connected gauge"); n != 1 {
		t.Errorf("the exposition carries %d gateway families, want one:\n%s", n, out)
	}
	if !series(t, r, `cella_gateways_connected{environment="default"} 4`) {
		t.Errorf("the second attachment did not replace the first:\n%s", out)
	}
}

// TestPendingPublishesNoSeriesWhenTheStoreCannotAnswer is why the closure
// answers with an ok: a zero on a store that did not answer reads as an empty
// queue, and the alert on a backlog would go quiet exactly when it should not.
func TestPendingPublishesNoSeriesWhenTheStoreCannotAnswer(t *testing.T) {
	answered := true
	r := metrics.New(metrics.Options{
		Environment: "default",
		Pending:     func() (int, bool) { return 4, answered },
	})
	if !series(t, r, "cella_events_pending 4") {
		t.Errorf("the backlog the store answered is not in the exposition:\n%s", exposition(t, r))
	}
	answered = false
	if out := exposition(t, r); strings.Contains(out, "cella_events_pending") {
		t.Errorf("a store that did not answer published a series:\n%s", out)
	}
}
