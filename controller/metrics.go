// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"time"

	driver "latere.ai/x/cella/runtime"
)

// Metrics is what the controller counts: the rows of design 017's table that
// designs 005 and 020 own. It is declared here and not imported, because a
// root package of design 001 reaches the contract types and nothing else; the
// registry of design 017 satisfies it structurally and is named nowhere in
// this package.
//
// Every method is one call on a path that already decided something. None of
// them takes a sandbox id, a subject or a name: design 017's labels are
// bounded, and the vocabularies below are the whole of what this package
// passes.
type Metrics interface {
	// SandboxCreated observes one create, by whether it adopted a prewarmed
	// entry (PoolHit) or made an environment of its own (PoolMiss).
	SandboxCreated(pool string, d time.Duration)
	// PoolAdoption counts a create against the environment's pool. It is
	// called only where the environment keeps one.
	PoolAdoption(outcome string)
	// PoolSize is the refill loop reporting what it found. A scrape cannot
	// ask for it: reading the pool is a driver call.
	PoolSize(ready, filling int)
	// ReaperAction counts one lifecycle rule the reaper enforced, by the
	// rule's reason and what it did.
	ReaperAction(rule, action string)
	// RecoveryAttempt counts one attempt on a lost sandbox by what it
	// produced.
	RecoveryAttempt(outcome string)
	// TokenReminted counts one workload token replaced before it expired.
	TokenReminted()
	// SandboxPreempted counts one sandbox the scheduler stopped to place
	// one of higher priority.
	SandboxPreempted()
	// LeaseHeld is a loop reporting, on its own tick, whether this replica
	// holds the lease it runs under.
	LeaseHeld(name string, held bool)
}

// The label values this package passes, fixed by design 017 and repeated here
// because the registry that holds them is not importable from a root package.
// A test in cmd/cellad, which reaches both, holds the two lists equal.
const (
	// PoolHit and PoolMiss say where a create's environment came from.
	PoolHit  = "hit"
	PoolMiss = "miss"
	// MetricAdopted and MetricMiss are the same question asked of the pool
	// rather than of the create.
	MetricAdopted = "adopted"
	MetricMiss    = "miss"
	// MetricRecovered and MetricExhausted are what one recovery attempt
	// produced. Design 017's third outcome, volume_missing, arrives with
	// the volumes of design 019.
	MetricRecovered = "recovered"
	MetricExhausted = "exhausted"
	// MetricLeaseReaper, MetricLeasePool and MetricLeaseEnvironments are the
	// leases this package's loops run under, by kind. The refill loop's
	// lease is keyed by environment; the label is not, because an
	// environment per series would make it unbounded.
	MetricLeaseReaper       = "reaper"
	MetricLeasePool         = "pool"
	MetricLeaseEnvironments = "environments"
	// The actions the reaper takes on a rule.
	ActionStopped = "stopped"
	ActionDeleted = "deleted"
)

// nopMetrics is what a controller built with no recorder counts. Design 017
// is off by default, and a test that does not care about the numbers builds
// the controller as it always did.
type nopMetrics struct{}

func (nopMetrics) SandboxCreated(string, time.Duration) {}
func (nopMetrics) PoolAdoption(string)                  {}
func (nopMetrics) PoolSize(int, int)                    {}
func (nopMetrics) ReaperAction(string, string)          {}
func (nopMetrics) RecoveryAttempt(string)               {}
func (nopMetrics) TokenReminted()                       {}
func (nopMetrics) SandboxPreempted()                    {}
func (nopMetrics) LeaseHeld(string, bool)               {}

// readyFilling splits the kept pool entries by whether they can be adopted
// now, which is the two states design 017's pool gauge carries.
func readyFilling(entries []driver.State) (ready, filling int) {
	for _, e := range entries {
		if e.Phase == driver.Running {
			ready++
		}
	}
	return ready, len(entries) - ready
}
