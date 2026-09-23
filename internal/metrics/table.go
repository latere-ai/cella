// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metrics

// Kind is a metric's type as the Prometheus exposition names it.
type Kind string

// The three kinds design 017's table uses.
const (
	KindCounter   Kind = "counter"
	KindGauge     Kind = "gauge"
	KindHistogram Kind = "histogram"
)

// Row is one line of design 017's table: the metric a scrape reads, the
// labels it may carry, the bounds a histogram observes into, and the spec
// that owns the number. Await names the spec a row waits on where no code
// path records it yet; such a row is declared here, registered nowhere, and
// still holds the alert rules that name it.
type Row struct {
	Name    string
	Kind    Kind
	Labels  []string
	Buckets []float64
	Owners  []string
	Await   string
}

// The bucket sets, twelve bounds each, named once so a histogram and the
// test that holds it to design 017 read the same numbers.
var (
	// LatencyBuckets covers 5ms to 10s: one HTTP request, one webhook call,
	// one delivery attempt.
	LatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 10}
	// CreateBuckets covers 0.5s to 300s: an adoption at the low end and an
	// image pull on a cold node at the high one.
	CreateBuckets = []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 90, 120, 180, 300}
	// StoreBuckets covers 1ms to 5s: one statement against the store.
	StoreBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}
)

// The closed vocabularies a label takes. A labeled histogram is initialized
// over its vocabulary at start, so a series exists before its first
// observation and a rate over a quiet installation is zero rather than
// absent.
var (
	// Endpoints are the two webhooks design 017 measures as one family.
	Endpoints = []string{EndpointAuthorizer, EndpointAdmission}
	// DecisionOutcomes are what one webhook call produced.
	DecisionOutcomes = []string{OutcomeAllow, OutcomeDeny, OutcomeUnavailable, OutcomeCached}
	// DeliveryOutcomes are what one delivery attempt produced (design 009).
	DeliveryOutcomes = []string{OutcomeAcknowledged, OutcomeDeferred, OutcomeDropped}
	// RecoveryOutcomes are what one recovery attempt produced (design 005).
	RecoveryOutcomes = []string{OutcomeRecovered, OutcomeExhausted, OutcomeVolumeMissing}
	// AdoptionOutcomes are whether a create took a prewarmed entry.
	AdoptionOutcomes = []string{OutcomeAdopted, OutcomeMiss}
	// PoolResults label a create by where its environment came from.
	PoolResults = []string{PoolHit, PoolMiss}
	// StoreOps are the methods of the controller's store, which is the one
	// caller that reaches it.
	StoreOps = []string{OpLoad, OpSave, OpWrite, OpRemove, OpRebuild, OpEvents, OpAcquire}
	// Leases are the loops that run under one, by kind. The refill loop's
	// lease is keyed by environment and its kind is "pool", because an
	// environment per series would make the label unbounded.
	Leases = []string{LeaseReaper, LeaseJournal, LeasePool, LeaseEnvironments, LeaseScheduler}
	// Doors are where a connection reached the gateway (design 018).
	Doors = []string{DoorProxy, DoorReverse}
	// Decisions are what the boundary did with it.
	Decisions = []string{DecisionAllowed, DecisionDenied, DecisionUnknown, DecisionPassthrough}
	// Directions are which way the bytes went.
	Directions = []string{DirectionIn, DirectionOut}
	// Exits are how one exec ended.
	Exits = []string{ExitZero, ExitNonzero, ExitFailed}
	// PoolStates are what a prewarmed entry is doing.
	PoolStates = []string{PoolReady, PoolFilling}
)

// The label values, written once so a recorder and a rule name the same
// string. Each belongs to a vocabulary above.
const (
	EndpointAuthorizer = "authorizer"
	EndpointAdmission  = "admission"

	OutcomeAllow         = "allow"
	OutcomeDeny          = "deny"
	OutcomeUnavailable   = "unavailable"
	OutcomeCached        = "cached"
	OutcomeAcknowledged  = "acknowledged"
	OutcomeDeferred      = "deferred"
	OutcomeDropped       = "dropped"
	OutcomeRecovered     = "recovered"
	OutcomeExhausted     = "exhausted"
	OutcomeVolumeMissing = "volume_missing"
	OutcomeAdopted       = "adopted"
	OutcomeMiss          = "miss"

	PoolHit     = "hit"
	PoolMiss    = "miss"
	PoolReady   = "ready"
	PoolFilling = "filling"

	OpLoad    = "load"
	OpSave    = "save"
	OpWrite   = "write"
	OpRemove  = "remove"
	OpRebuild = "rebuild"
	OpEvents  = "events"
	OpAcquire = "acquire"

	LeaseReaper       = "reaper"
	LeaseJournal      = "journal"
	LeasePool         = "pool"
	LeaseEnvironments = "environments"
	LeaseScheduler    = "scheduler"

	DoorProxy   = "proxy"
	DoorReverse = "reverse"

	DecisionAllowed     = "allowed"
	DecisionDenied      = "denied"
	DecisionUnknown     = "unknown"
	DecisionPassthrough = "passthrough"

	DirectionIn  = "in"
	DirectionOut = "out"

	ExitZero    = "0"
	ExitNonzero = "nonzero"
	ExitFailed  = "failed"
)

// The specs a row waits on. A row with one of these is in the table, in the
// rules file, and in no registry: the loop that would move it has not landed.
const (
	awaitAPI       = "008"
	awaitScheduler = "020"
	awaitWorkers   = "021"
)

// Table is design 017's metric table as this binary emits it. It is the
// single declaration: the registry builds its instruments from it, the rules
// test holds every name an alert reads to it, and TestMetricsTable holds it
// to the table in specs/017-observability.md row for row.
var Table = []Row{
	{Name: "cella_requests_total", Kind: KindCounter, Labels: []string{"route", "status", "code"}, Owners: []string{"008"}},
	{Name: "cella_request_duration_seconds", Kind: KindHistogram, Labels: []string{"route"}, Buckets: LatencyBuckets, Owners: []string{"008"}},
	{Name: "cella_rate_limited_total", Kind: KindCounter, Labels: []string{"limit"}, Owners: []string{"008"}, Await: awaitAPI},
	{Name: "cella_sandboxes", Kind: KindGauge, Labels: []string{"phase", "environment", "driver"}, Owners: []string{"005"}},
	{Name: "cella_sandbox_create_duration_seconds", Kind: KindHistogram, Labels: []string{"driver", "pool"}, Buckets: CreateBuckets, Owners: []string{"005", "020"}},
	{Name: "cella_reaper_actions_total", Kind: KindCounter, Labels: []string{"rule", "action"}, Owners: []string{"005"}},
	{Name: "cella_recovery_attempts_total", Kind: KindCounter, Labels: []string{"outcome"}, Owners: []string{"005"}},
	{Name: "cella_tokens_reminted_total", Kind: KindCounter, Owners: []string{"005", "006"}},
	{Name: "cella_decisions_total", Kind: KindCounter, Labels: []string{"endpoint", "outcome"}, Owners: []string{"006", "007"}},
	{Name: "cella_webhook_duration_seconds", Kind: KindHistogram, Labels: []string{"endpoint"}, Buckets: LatencyBuckets, Owners: []string{"006", "007"}},
	{Name: "cella_events_pending", Kind: KindGauge, Owners: []string{"009"}},
	{Name: "cella_events_delivered_total", Kind: KindCounter, Labels: []string{"outcome"}, Owners: []string{"009"}},
	{Name: "cella_event_delivery_duration_seconds", Kind: KindHistogram, Buckets: LatencyBuckets, Owners: []string{"009"}},
	{Name: "cella_store_query_duration_seconds", Kind: KindHistogram, Labels: []string{"op"}, Buckets: StoreBuckets, Owners: []string{"010"}},
	{Name: "cella_lease_held", Kind: KindGauge, Labels: []string{"name"}, Owners: []string{"010"}},
	{Name: "cella_exec_total", Kind: KindCounter, Labels: []string{"exit"}, Owners: []string{"008"}},
	{Name: "cella_egress_connections_total", Kind: KindCounter, Labels: []string{"decision", "door"}, Owners: []string{"018"}},
	{Name: "cella_egress_bytes_total", Kind: KindCounter, Labels: []string{"direction", "door"}, Owners: []string{"018"}},
	{Name: "cella_gateways_connected", Kind: KindGauge, Labels: []string{"environment"}, Owners: []string{"018"}},
	{Name: "cella_gateway_snapshots_total", Kind: KindCounter, Owners: []string{"018"}},
	{Name: "cella_environments", Kind: KindGauge, Labels: []string{"phase", "reason"}, Owners: []string{"021"}, Await: awaitWorkers},
	{Name: "cella_workers_connected", Kind: KindGauge, Labels: []string{"environment"}, Owners: []string{"021"}, Await: awaitWorkers},
	{Name: "cella_operations_redelivered_total", Kind: KindCounter, Owners: []string{"021"}, Await: awaitWorkers},
	{Name: "cella_queue_depth", Kind: KindGauge, Labels: []string{"environment", "queue"}, Owners: []string{"020"}},
	{Name: "cella_capacity", Kind: KindGauge, Labels: []string{"environment", "resource", "kind"}, Owners: []string{"020"}},
	{Name: "cella_preemptions_total", Kind: KindCounter, Owners: []string{"020"}},
	{Name: "cella_pool_size", Kind: KindGauge, Labels: []string{"environment", "state"}, Owners: []string{"020"}},
	{Name: "cella_pool_adoptions_total", Kind: KindCounter, Labels: []string{"outcome"}, Owners: []string{"020"}},
	{Name: "cella_set_replicas", Kind: KindGauge, Labels: []string{"phase"}, Owners: []string{"020"}, Await: awaitScheduler},
}

// Registered reports whether a scrape of this binary can carry the metric.
func Registered(name string) bool {
	for _, r := range Table {
		if r.Name == name {
			return r.Await == ""
		}
	}
	return false
}

// Declared reports whether the name is in the table at all, registered or
// awaiting the spec that moves it.
func Declared(name string) bool {
	for _, r := range Table {
		if r.Name == name {
			return true
		}
	}
	return false
}

// LabelsOf is the labels the table gives a metric, and false where the table
// does not name it.
func LabelsOf(name string) ([]string, bool) {
	for _, r := range Table {
		if r.Name == name {
			return r.Labels, true
		}
	}
	return nil, false
}

// help is the one sentence the exposition carries per metric family. A scrape
// is read by an operator who has not read design 017.
var help = map[string]string{
	"cella_requests_total":                  "API requests by route, status class and error code",
	"cella_request_duration_seconds":        "API request latency by route, excluding hijacked streams",
	"cella_sandboxes":                       "desired sandboxes by phase",
	"cella_sandbox_create_duration_seconds": "how long one create took, by driver and whether it adopted a pool entry",
	"cella_reaper_actions_total":            "lifecycle rules the reaper enforced, by rule and action",
	"cella_recovery_attempts_total":         "recovery attempts on a lost sandbox, by outcome",
	"cella_tokens_reminted_total":           "workload tokens re-minted before expiry",
	"cella_decisions_total":                 "authorizer and admission calls by outcome",
	"cella_webhook_duration_seconds":        "authorizer and admission call latency",
	"cella_events_pending":                  "records the journal holds undelivered",
	"cella_events_delivered_total":          "delivery attempts by outcome",
	"cella_event_delivery_duration_seconds": "how long one delivery attempt took",
	"cella_store_query_duration_seconds":    "store operation latency by operation",
	"cella_lease_held":                      "whether this replica holds the named lease",
	"cella_exec_total":                      "exec sessions by how they ended",
	"cella_egress_connections_total":        "connections the boundary decided on, by decision and door",
	"cella_egress_bytes_total":              "bytes across the boundary, by direction and door",
	"cella_gateways_connected":              "gateways of the environment holding a sync stream",
	"cella_gateway_snapshots_total":         "boundary snapshots sent to a joining gateway",
	"cella_pool_size":                       "prewarmed entries by state",
	"cella_queue_depth":                     "sandboxes waiting in each queue of a queued environment",
	"cella_capacity":                        "each quantity an environment declares, and what its sandboxes hold of it",
	"cella_pool_adoptions_total":            "creates that took a prewarmed entry, and those that did not",
	"cella_preemptions_total":               "sandboxes the scheduler stopped to place one of higher priority",
}
