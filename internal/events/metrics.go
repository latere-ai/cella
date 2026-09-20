// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import "time"

// The label values delivery passes, fixed by design 017.
const (
	// MetricLeaseJournal is the kind of the lease delivery runs under.
	MetricLeaseJournal = "journal"
	// The outcomes of one attempt: the sink took it, it is held for a later
	// pass, or it is ended.
	MetricAcknowledged = "acknowledged"
	MetricDeferred     = "deferred"
	MetricDropped      = "dropped"
)

// Metrics is what delivery counts: the rows of design 017's table design 009
// owns. The registry of design 017 satisfies it; a test passes a fake.
type Metrics interface {
	// EventDelivered counts one attempt by what it produced: acknowledged,
	// deferred, or dropped.
	EventDelivered(outcome string)
	// EventDeliveryDuration observes one attempt on the sink. A record
	// dropped before a request was made is counted and not timed.
	EventDeliveryDuration(d time.Duration)
	// LeaseHeld is the delivery loop reporting, on its own pass, whether
	// this replica holds the journal lease.
	LeaseHeld(name string, held bool)
}

// nopMetrics is what a deliverer built with no recorder counts.
type nopMetrics struct{}

func (nopMetrics) EventDelivered(string)               {}
func (nopMetrics) EventDeliveryDuration(time.Duration) {}
func (nopMetrics) LeaseHeld(string, bool)              {}
