// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"log/slog"
	"time"
)

// Journal is the delivery half of design 010's journal, as this package
// needs it. The store adapters implement it; nothing here knows a row from a
// map.
//
// Append is for a record with no mutation of its own, which is every
// operation. A mutation's record is appended inside the mutation's own
// transaction by the store bridge, so the state and the record it explains
// commit together or not at all.
type Journal interface {
	Append(ctx context.Context, r Record) error
	// Pending returns at most limit records, at most one per object, each
	// the lowest unacknowledged sequence of its object and due now. A
	// deferred record holds the records behind it for that object and no
	// other.
	Pending(ctx context.Context, limit int) ([]Pending, error)
	// Acknowledge marks a record the sink took.
	Acknowledge(ctx context.Context, id string) error
	// Defer records one failed attempt and when the next one is due.
	Defer(ctx context.Context, id string, next time.Time) error
	// Drop ends a record: the sink refused it, or the retry window closed.
	Drop(ctx context.Context, id string) error
}

// Pending is one record waiting for the sink and the delivery state the
// backoff reads.
type Pending struct {
	Record   Record
	Attempts int
}

// Emitter writes records to the journal. A failure to journal is logged and
// never returned to the caller: a record is a note about work, not the work,
// and a sandbox that started does not un-start because its record did not
// land.
type Emitter struct {
	journal Journal
	log     *slog.Logger
}

// NewEmitter builds the emitter over one journal.
func NewEmitter(j Journal, log *slog.Logger) *Emitter {
	if log == nil {
		log = slog.Default()
	}
	return &Emitter{journal: j, log: log}
}

// Write appends one record. It is the operation path: the caller has already
// built the record and holds no transaction.
func (e *Emitter) Write(ctx context.Context, r Record) {
	if e == nil || e.journal == nil {
		return
	}
	if err := e.journal.Append(ctx, r); err != nil {
		e.log.WarnContext(ctx, "the event was not journaled",
			"type", r.Type, "object", r.Object.ID, "error", err)
	}
}
