// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import "time"

// Metrics is what the store measures: design 017's row that design 010 owns.
// The registry of design 017 satisfies it; a test passes a fake.
type Metrics interface {
	// StoreQuery observes one operation, named by the method the controller
	// called. The label is the operation and never the statement, so the
	// vocabulary is the seven methods below and cannot grow with the schema.
	StoreQuery(op string, d time.Duration)
}

// The operations of the controller's store, which are design 017's op label.
const (
	OpLoad    = "load"
	OpSave    = "save"
	OpWrite   = "write"
	OpRemove  = "remove"
	OpRebuild = "rebuild"
	OpEvents  = "events"
	OpAcquire = "acquire"
)

// nopMetrics is what a store built with no recorder measures.
type nopMetrics struct{}

func (nopMetrics) StoreQuery(string, time.Duration) {}

// observe times one operation. It is deferred at the top of each method, so
// the observation covers the transaction and not only its first statement.
func (c *Controlled) observe(op string, started time.Time) {
	c.metrics.StoreQuery(op, time.Since(started))
}
