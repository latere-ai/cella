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
	// ByObject reads one object's records, newest first, one page at a time.
	// The cursor is what the previous page returned and an empty one starts
	// at the newest; an empty next cursor is the end. It is the read half of
	// the per-object feed, and a row's columns are authoritative over its
	// payload, so a record comes back with the sequence the journal assigned.
	ByObject(ctx context.Context, objectID, cursor string, limit int) ([]Record, string, error)
	// Pending returns at most limit records, at most one per object, each
	// the lowest unacknowledged sequence of its object and due at now. A
	// deferred record holds the records behind it for that object and no
	// other. The instant is the deliverer's, not the store's, so one clock
	// decides the backoff and the due time.
	Pending(ctx context.Context, limit int, now time.Time) ([]Pending, error)
	// Undelivered counts the records the sink has not taken and that were
	// not dropped, whatever their next attempt is due. It is the backlog
	// design 017's cella_events_pending gauge reads.
	Undelivered(ctx context.Context) (int, error)
	// Acknowledge marks a record the sink took.
	Acknowledge(ctx context.Context, id string, at time.Time) error
	// Defer records one failed attempt and when the next one is due.
	Defer(ctx context.Context, id string, next time.Time) error
	// Drop ends a record: the sink refused it, or the retry window closed.
	Drop(ctx context.Context, id string, at time.Time) error
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

// Feed is the per-object feed of design 009: one page of one object's
// records, newest first. An emitter over no journal answers an empty page,
// which is the control plane that journals nothing rather than an error a
// caller can do nothing about.
func (e *Emitter) Feed(ctx context.Context, objectID, cursor string, limit int) ([]Record, string, error) {
	if e == nil || e.journal == nil {
		return nil, "", nil
	}
	return e.journal.ByObject(ctx, objectID, cursor, limit)
}
