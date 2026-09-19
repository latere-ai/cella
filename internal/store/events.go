// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"latere.ai/x/cella/internal/events"
)

// EventJournal is design 009's journal over one store. Each call opens its
// own transaction, which is what an operation needs: an exec changes no
// desired state, so there is no mutation to commit with.
//
// A mutation's record does not come through here. Controlled.Write and
// Controlled.Remove append it inside the transaction that writes the state,
// so a record and the change it explains commit together or not at all.
func EventJournal(s Store) events.Journal { return eventJournal{s} }

type eventJournal struct{ store Store }

// Append writes one record. The journal assigns the sequence, so the record
// the caller built carries none and the row's column is authoritative.
func (j eventJournal) Append(ctx context.Context, r events.Record) error {
	row, err := journalRow(r)
	if err != nil {
		return err
	}
	return j.store.Tx(ctx, func(tx Tx) error {
		_, err := tx.Journal().Append(ctx, row)
		return err
	})
}

func (j eventJournal) Pending(ctx context.Context, limit int) ([]events.Pending, error) {
	var out []events.Pending
	err := j.store.Tx(ctx, func(tx Tx) error {
		rows, err := tx.Journal().Pending(ctx, limit, time.Now().UTC())
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

func (j eventJournal) Acknowledge(ctx context.Context, id string) error {
	return j.store.Tx(ctx, func(tx Tx) error {
		return tx.Journal().Acknowledge(ctx, id, time.Now().UTC())
	})
}

func (j eventJournal) Defer(ctx context.Context, id string, next time.Time) error {
	return j.store.Tx(ctx, func(tx Tx) error { return tx.Journal().Defer(ctx, id, next) })
}

func (j eventJournal) Drop(ctx context.Context, id string) error {
	return j.store.Tx(ctx, func(tx Tx) error {
		return tx.Journal().Drop(ctx, id, time.Now().UTC())
	})
}

// journalRow splits one record into the columns and the payload. A record of
// a type design 009 does not deliver is stored acknowledged: design 010 keeps
// one row per mutation, and a row nothing will ever post is finished the
// moment it is written rather than pending forever.
func journalRow(r events.Record) (Event, error) {
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
	if !events.Deliverable(r.Type) {
		row.AckedAt = r.Time
	}
	return row, nil
}
