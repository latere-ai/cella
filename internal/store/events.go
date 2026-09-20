// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"latere.ai/x/cella/internal/events"
)

// Delivery is what happens to a record this process journals.
//
// Delivered waits for the sink to take it. Journaled stores it acknowledged,
// which is a record with no sink configured: design 008's per-object feed
// still reads it, and design 010's retention forgets it on schedule rather
// than holding a row nothing will ever post.
type Delivery bool

// The two settings, named so a call site says which it means.
const (
	Journaled Delivery = false
	Delivered Delivery = true
)

// EventJournal is design 009's journal over one store. Each call opens its
// own transaction, which is what an operation needs: an exec changes no
// desired state, so there is no mutation to commit with.
//
// A mutation's record does not come through here. Controlled.Write and
// Controlled.Remove append it inside the transaction that writes the state,
// so a record and the change it explains commit together or not at all.
func EventJournal(s Store, d Delivery) events.Journal { return eventJournal{s, d} }

type eventJournal struct {
	store    Store
	delivery Delivery
}

// Append writes one record. The journal assigns the sequence, so the record
// the caller built carries none and the row's column is authoritative.
func (j eventJournal) Append(ctx context.Context, r events.Record) error {
	row, err := journalRow(r, j.delivery)
	if err != nil {
		return err
	}
	return j.store.Tx(ctx, func(tx Tx) error {
		_, err := tx.Journal().Append(ctx, row)
		return err
	})
}

func (j eventJournal) Pending(ctx context.Context, limit int, now time.Time) ([]events.Pending, error) {
	var out []events.Pending
	err := j.store.Tx(ctx, func(tx Tx) error {
		rows, err := tx.Journal().Pending(ctx, limit, now)
		if err != nil {
			return err
		}
		out = make([]events.Pending, 0, len(rows))
		for _, row := range rows {
			record, err := events.Rebuild(row.Payload, row.ID, row.Seq, row.Type, row.At)
			if err != nil {
				return fmt.Errorf("store: rebuilding the event %s: %w", row.ID, err)
			}
			out = append(out, events.Pending{Record: record, Attempts: row.Attempts})
		}
		return nil
	})
	return out, err
}

func (j eventJournal) Undelivered(ctx context.Context) (int, error) {
	var n int
	err := j.store.Tx(ctx, func(tx Tx) error {
		var err error
		n, err = tx.Journal().Undelivered(ctx)
		return err
	})
	return n, err
}

func (j eventJournal) Acknowledge(ctx context.Context, id string, at time.Time) error {
	return j.store.Tx(ctx, func(tx Tx) error { return tx.Journal().Acknowledge(ctx, id, at) })
}

func (j eventJournal) Defer(ctx context.Context, id string, next time.Time) error {
	return j.store.Tx(ctx, func(tx Tx) error { return tx.Journal().Defer(ctx, id, next) })
}

func (j eventJournal) Drop(ctx context.Context, id string, at time.Time) error {
	return j.store.Tx(ctx, func(tx Tx) error { return tx.Journal().Drop(ctx, id, at) })
}

// journalRow splits one record into the columns and the payload. A record of
// a type design 009 does not deliver, and every record where no sink is
// configured, is stored acknowledged: design 010 keeps one row per mutation,
// and a row nothing will ever post is finished the moment it is written
// rather than pending forever.
func journalRow(r events.Record, d Delivery) (Event, error) {
	if err := r.Valid(); err != nil {
		return Event{}, err
	}
	payload, err := events.Payload(r)
	if err != nil {
		return Event{}, fmt.Errorf("store: encoding the event %s: %w", r.ID, err)
	}
	row := Event{
		ID: r.ID, ObjectID: r.Object.ID, Type: string(r.Type), At: r.Time, Payload: payload,
	}
	if d == Journaled || !events.Deliverable(r.Type) {
		row.AckedAt = r.Time
	}
	return row, nil
}
