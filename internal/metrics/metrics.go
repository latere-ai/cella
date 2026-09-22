// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package metrics is the one registry of design 017. It holds the table's
// instruments, serves the Prometheus exposition on the internal listener of
// design 002, and carries the redacting log handler every line passes
// through.
//
// No package outside cmd/cellad imports it. A package that records declares
// the narrow method set it calls over standard-library types and takes it as
// an option; *Registry satisfies each of those interfaces structurally, which
// is what keeps the root packages of design 001 free of an import under
// internal/ while still counting what they own.
//
// Counters and histograms are pushed by the code path that owns the number.
// Gauges are pulled at scrape time from an index that already holds the
// answer, except the two a scrape cannot ask for: whether a loop holds its
// lease, and how many prewarmed entries the environment keeps, both of which
// are known only inside the loop's tick and are pushed from there.
package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	pkgmetrics "latere.ai/x/pkg/metrics"
)

// Options is what the registry needs that it cannot count itself: the two
// labels every installation fixes, and the indexes the pull gauges read.
// Each closure is optional; a nil one publishes no series for its family.
type Options struct {
	// Environment is the name of the environment this control plane drives,
	// and Driver the runtime it drives it with. They label the gauges design
	// 017 gives those labels and nothing else.
	Environment string
	Driver      string
	// Sandboxes counts desired sandboxes by phase. It reads the controller's
	// cached index and never the driver.
	Sandboxes func() map[string]int
	// Gateways is how many gateways of the environment hold a sync stream.
	Gateways func() int
	// Pending is how many records the journal has not delivered, and false
	// where the store could not answer. A scrape publishes no series rather
	// than a zero, because a zero is what an alert reads as an empty queue.
	Pending func() (int, bool)
	// Queues is how many sandboxes wait in each queue, labelled environment
	// and queue, and Capacity each declared quantity beside its sum in use,
	// labelled environment, resource and kind. Both read the controller's
	// desired state.
	Queues   func() []Series
	Capacity func() []Series
}

// Series is one labelled value a pull gauge reads from an index its caller
// holds.
type Series struct {
	Labels map[string]string
	Value  float64
}

// Registry is design 017's table, instantiated.
type Registry struct {
	reg         *pkgmetrics.Registry
	environment string
	driver      string

	requests    *pkgmetrics.Counter
	reaper      *pkgmetrics.Counter
	recovery    *pkgmetrics.Counter
	reminted    *pkgmetrics.Counter
	decisions   *pkgmetrics.Counter
	delivered   *pkgmetrics.Counter
	execs       *pkgmetrics.Counter
	connections *pkgmetrics.Counter
	egressBytes *pkgmetrics.Counter
	snapshots   *pkgmetrics.Counter
	adoptions   *pkgmetrics.Counter
	preemptions *pkgmetrics.Counter

	requestDuration *pkgmetrics.Histogram
	createDuration  *pkgmetrics.Histogram
	webhook         *pkgmetrics.Histogram
	delivery        *pkgmetrics.Histogram
	storeQuery      *pkgmetrics.Histogram

	// mu guards the two pushed gauges. A lease appears only once its loop
	// has reported on it, so a replica that runs no delivery publishes no
	// journal series and the alert on a lease not held cannot fire on a
	// loop that does not exist.
	mu     sync.Mutex
	leases map[string]bool
	pool   map[string]int
	// gateways is the hub's connection count, attached after construction
	// because the hub is built after the registry. Its family is registered
	// on the first attachment and never again.
	gateways func() int
}

// New builds the registry: every instrument of the table whose owning spec
// has landed, with each labelled histogram initialised over its vocabulary so
// a series exists before the first observation.
func New(o Options) *Registry {
	reg := pkgmetrics.NewRegistry()
	r := &Registry{
		reg:         reg,
		environment: o.Environment,
		driver:      o.Driver,
		leases:      map[string]bool{},
		pool:        map[string]int{},
	}
	counter := func(name string) *pkgmetrics.Counter { return reg.Counter(name, help[name]) }
	histogram := func(name string, buckets []float64) *pkgmetrics.Histogram {
		return reg.Histogram(name, help[name], buckets)
	}
	r.requests = counter("cella_requests_total")
	r.reaper = counter("cella_reaper_actions_total")
	r.recovery = counter("cella_recovery_attempts_total")
	r.reminted = counter("cella_tokens_reminted_total")
	r.decisions = counter("cella_decisions_total")
	r.delivered = counter("cella_events_delivered_total")
	r.execs = counter("cella_exec_total")
	r.connections = counter("cella_egress_connections_total")
	r.egressBytes = counter("cella_egress_bytes_total")
	r.snapshots = counter("cella_gateway_snapshots_total")
	r.adoptions = counter("cella_pool_adoptions_total")
	r.preemptions = counter("cella_preemptions_total")

	r.requestDuration = histogram("cella_request_duration_seconds", LatencyBuckets)
	r.createDuration = histogram("cella_sandbox_create_duration_seconds", CreateBuckets)
	r.webhook = histogram("cella_webhook_duration_seconds", LatencyBuckets)
	r.delivery = histogram("cella_event_delivery_duration_seconds", LatencyBuckets)
	r.storeQuery = histogram("cella_store_query_duration_seconds", StoreBuckets)

	// The closed vocabularies: one series each before anything is observed.
	for _, endpoint := range Endpoints {
		r.webhook.Init(map[string]string{"endpoint": endpoint})
	}
	for _, op := range StoreOps {
		r.storeQuery.Init(map[string]string{"op": op})
	}
	for _, result := range PoolResults {
		r.createDuration.Init(map[string]string{"driver": o.Driver, "pool": result})
	}
	r.delivery.Init(nil)

	r.gauges(o)
	return r
}

// gauges registers the scrape-time callbacks. Each reads an index that is
// already current: the controller's map of desired sandboxes, the hub's
// connection count, the journal's undelivered count, and the two the loops
// pushed.
func (r *Registry) gauges(o Options) {
	if o.Sandboxes != nil {
		r.reg.Gauge("cella_sandboxes", help["cella_sandboxes"], func() []pkgmetrics.LabeledValue {
			counts := o.Sandboxes()
			out := make([]pkgmetrics.LabeledValue, 0, len(counts))
			for phase, n := range counts {
				out = append(out, pkgmetrics.LabeledValue{
					Labels: map[string]string{"phase": phase, "environment": r.environment, "driver": r.driver},
					Value:  float64(n),
				})
			}
			return out
		})
	}
	if o.Gateways != nil {
		r.reg.Gauge("cella_gateways_connected", help["cella_gateways_connected"], func() []pkgmetrics.LabeledValue {
			return []pkgmetrics.LabeledValue{{
				Labels: map[string]string{"environment": r.environment},
				Value:  float64(o.Gateways()),
			}}
		})
	}
	for name, read := range map[string]func() []Series{"cella_queue_depth": o.Queues, "cella_capacity": o.Capacity} {
		if read == nil {
			continue
		}
		r.reg.Gauge(name, help[name], func() []pkgmetrics.LabeledValue {
			series := read()
			out := make([]pkgmetrics.LabeledValue, 0, len(series))
			for _, s := range series {
				out = append(out, pkgmetrics.LabeledValue{Labels: s.Labels, Value: s.Value})
			}
			return out
		})
	}
	if o.Pending != nil {
		r.reg.Gauge("cella_events_pending", help["cella_events_pending"], func() []pkgmetrics.LabeledValue {
			n, ok := o.Pending()
			if !ok {
				return nil
			}
			return []pkgmetrics.LabeledValue{{Value: float64(n)}}
		})
	}
	r.reg.Gauge("cella_lease_held", help["cella_lease_held"], func() []pkgmetrics.LabeledValue {
		r.mu.Lock()
		defer r.mu.Unlock()
		out := make([]pkgmetrics.LabeledValue, 0, len(r.leases))
		for name, held := range r.leases {
			out = append(out, pkgmetrics.LabeledValue{
				Labels: map[string]string{"name": name},
				Value:  boolValue(held),
			})
		}
		return out
	})
	r.reg.Gauge("cella_pool_size", help["cella_pool_size"], func() []pkgmetrics.LabeledValue {
		r.mu.Lock()
		defer r.mu.Unlock()
		out := make([]pkgmetrics.LabeledValue, 0, len(r.pool))
		for state, n := range r.pool {
			out = append(out, pkgmetrics.LabeledValue{
				Labels: map[string]string{"environment": r.environment, "state": state},
				Value:  float64(n),
			})
		}
		return out
	})
}

// WithGateways registers the gateway gauge after construction, for a control
// plane whose hub is built after the registry. A second call replaces the
// closure and publishes no second family: one family per name is what makes
// the exposition parseable.
func (r *Registry) WithGateways(connected func() int) {
	if connected == nil {
		return
	}
	r.mu.Lock()
	first := r.gateways == nil
	r.gateways = connected
	r.mu.Unlock()
	if !first {
		return
	}
	connected = func() int {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.gateways()
	}
	r.reg.Gauge("cella_gateways_connected", help["cella_gateways_connected"], func() []pkgmetrics.LabeledValue {
		return []pkgmetrics.LabeledValue{{
			Labels: map[string]string{"environment": r.environment},
			Value:  float64(connected()),
		}}
	})
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// ServeHTTP writes the exposition. It is mounted at GET /metrics on the
// internal listener of design 002 and on no public route: the numbers are
// the installation's and not a caller's.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	r.reg.WritePrometheus(w)
}

// ---------------------------------------------------------------------------
// What the API records (design 008)
// ---------------------------------------------------------------------------

// Request counts one request by the route pattern the mux matched, the status
// class, and the error code the envelope carried, which is empty on a
// success.
func (r *Registry) Request(route, status, code string) {
	r.requests.Inc(map[string]string{"route": route, "status": status, "code": code})
}

// RequestDuration observes one request's latency. A hijacked route calls
// Request and not this: a stream's life is not a request's latency.
func (r *Registry) RequestDuration(route string, d time.Duration) {
	r.requestDuration.Observe(map[string]string{"route": route}, d.Seconds())
}

// Exec counts one exec session by how it ended: the exit code as "0" or
// "nonzero", or "failed" where the driver never produced one.
func (r *Registry) Exec(exit string) {
	r.execs.Inc(map[string]string{"exit": exit})
}

// ExitOf is the exec vocabulary for one exit code.
func ExitOf(code int) string {
	if code == 0 {
		return ExitZero
	}
	return ExitNonzero
}

// StatusClass is the status label: the class and not the code, so the label
// stays bounded at five values.
func StatusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	case code > 0:
		return "1xx"
	}
	return strconv.Itoa(code)
}

// ---------------------------------------------------------------------------
// What the boundary records (design 018)
// ---------------------------------------------------------------------------

// EgressConnection counts one connection record as it arrives on the sync
// stream, with the bytes it carried in each direction. The record is the
// gateway's; the count is the control plane's.
func (r *Registry) EgressConnection(decision, door string, in, out int64) {
	r.connections.Inc(map[string]string{"decision": decision, "door": door})
	if in > 0 {
		r.egressBytes.Add(map[string]string{"direction": DirectionIn, "door": door}, uint64(in))
	}
	if out > 0 {
		r.egressBytes.Add(map[string]string{"direction": DirectionOut, "door": door}, uint64(out))
	}
}

// GatewaySnapshot counts one boundary snapshot sent to a gateway that joined.
func (r *Registry) GatewaySnapshot() { r.snapshots.Inc(nil) }

// ---------------------------------------------------------------------------
// What the controller records (design 005, design 020)
// ---------------------------------------------------------------------------

// SandboxCreated observes one create, labelled by whether it adopted a
// prewarmed entry. The driver label is the registry's, not the caller's.
func (r *Registry) SandboxCreated(pool string, d time.Duration) {
	r.createDuration.Observe(map[string]string{"driver": r.driver, "pool": pool}, d.Seconds())
}

// PoolAdoption counts a create against the pool: adopted, or a miss on an
// environment that keeps one.
func (r *Registry) PoolAdoption(outcome string) {
	r.adoptions.Inc(map[string]string{"outcome": outcome})
}

// PoolSize is the refill loop reporting what it found, since a scrape cannot
// ask the driver for it.
func (r *Registry) PoolSize(ready, filling int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pool[PoolReady] = ready
	r.pool[PoolFilling] = filling
}

// SandboxPreempted counts one sandbox the scheduler stopped to place one of
// higher priority.
func (r *Registry) SandboxPreempted() { r.preemptions.Inc(nil) }

// ReaperAction counts one lifecycle rule the reaper enforced.
func (r *Registry) ReaperAction(rule, action string) {
	r.reaper.Inc(map[string]string{"rule": rule, "action": action})
}

// RecoveryAttempt counts one attempt on a lost sandbox by what it produced.
func (r *Registry) RecoveryAttempt(outcome string) {
	r.recovery.Inc(map[string]string{"outcome": outcome})
}

// TokenReminted counts one workload token replaced before it expired.
func (r *Registry) TokenReminted() { r.reminted.Inc(nil) }

// LeaseHeld is a loop reporting, on its own tick, whether this replica holds
// the lease it runs under.
func (r *Registry) LeaseHeld(name string, held bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leases[name] = held
}

// ---------------------------------------------------------------------------
// What identity and admission record (designs 006 and 007)
// ---------------------------------------------------------------------------

// Decision counts one webhook call and observes how long it took. A decision
// served from the cache is counted and not timed: no call was made.
func (r *Registry) Decision(endpoint, outcome string, d time.Duration) {
	r.decisions.Inc(map[string]string{"endpoint": endpoint, "outcome": outcome})
	if outcome != OutcomeCached {
		r.webhook.Observe(map[string]string{"endpoint": endpoint}, d.Seconds())
	}
}

// ---------------------------------------------------------------------------
// What events and the store record (designs 009 and 010)
// ---------------------------------------------------------------------------

// EventDelivered counts one delivery attempt by what it produced.
func (r *Registry) EventDelivered(outcome string) {
	r.delivered.Inc(map[string]string{"outcome": outcome})
}

// EventDeliveryDuration observes one attempt on the sink. A record dropped
// before a request was made is counted and not timed: nothing was attempted.
func (r *Registry) EventDeliveryDuration(d time.Duration) {
	r.delivery.Observe(nil, d.Seconds())
}

// StoreQuery observes one store operation, named by the method the controller
// called and never by the statement it ran.
func (r *Registry) StoreQuery(op string, d time.Duration) {
	r.storeQuery.Observe(map[string]string{"op": op}, d.Seconds())
}
