// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/events"
	v1 "latere.ai/x/cella/manifest/v1"
)

// The Environment kind as design 010 holds it: one row per environment, keyed
// by the name that is also its id, with the phase loop's status beside the
// object and one journal row per act, in the transaction that act commits in.

// LoadEnvironments reads every live Environment with the version of the row
// it came from, which is what an If-Match is compared against.
func (c *Controlled) LoadEnvironments() (map[string]v1.Environment, error) {
	ctx := context.Background()
	out := map[string]v1.Environment{}
	cursor := ""
	for {
		var (
			rows []Object
			next string
		)
		err := c.store.Tx(ctx, func(tx Tx) error {
			var err error
			rows, next, err = tx.Desired().List(ctx, KindEnvironment, Filter{}, Page{Cursor: cursor})
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			obj, err := decodeEnvironment(row)
			if err != nil {
				return nil, err
			}
			out[row.ID] = obj
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// WriteEnvironment stores one Environment at the version given and appends
// the mutation to the journal, in one transaction. A row that moved since it
// was read is controller.ErrVersionConflict and nothing is written.
func (c *Controlled) WriteEnvironment(ctx context.Context, obj v1.Environment, ifVersion int64, mutation string) (int64, error) {
	row, err := encodeEnvironment(obj)
	if err != nil {
		return 0, err
	}
	event, err := c.environmentRecord(ctx, mutation, obj)
	if err != nil {
		return 0, err
	}
	var written int64
	err = c.store.Tx(ctx, func(tx Tx) error {
		next, err := tx.Desired().Put(ctx, row, ifVersion)
		if err != nil {
			return err
		}
		if _, err := tx.Journal().Append(ctx, event); err != nil {
			return err
		}
		written = next
		return nil
	})
	if errors.Is(err, ErrVersionConflict) {
		return 0, controller.ErrVersionConflict
	}
	return written, err
}

// WriteEnvironmentStatus writes what the phase loop computed without touching
// the object or its version, and appends the record where the transition has
// one. A status write design 009 names no type for is state and no event.
func (c *Controlled) WriteEnvironmentStatus(ctx context.Context, obj v1.Environment, mutation string) error {
	status, err := json.Marshal(obj.Status)
	if err != nil {
		return fmt.Errorf("store: encoding the environment status: %w", err)
	}
	var event Event
	if mutation != "" {
		if event, err = c.environmentRecord(ctx, mutation, obj); err != nil {
			return err
		}
	}
	return c.store.Tx(ctx, func(tx Tx) error {
		if err := tx.Desired().PutStatus(ctx, KindEnvironment, obj.Metadata.Name, status); err != nil {
			return err
		}
		if mutation == "" {
			return nil
		}
		_, err := tx.Journal().Append(ctx, event)
		return err
	})
}

// RemoveEnvironment deletes one Environment and appends the mutation, in one
// transaction.
func (c *Controlled) RemoveEnvironment(ctx context.Context, name, mutation string) error {
	return c.store.Tx(ctx, func(tx Tx) error {
		obj := v1.Environment{Metadata: v1.Metadata{Name: name}, Status: v1.EnvironmentStatus{ID: name}}
		if row, err := tx.Desired().Get(ctx, KindEnvironment, name); err == nil {
			if decoded, err := decodeEnvironment(row); err == nil {
				obj = decoded
			}
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		event, err := c.environmentRecord(ctx, mutation, obj)
		if err != nil {
			return err
		}
		if err := tx.Desired().Delete(ctx, KindEnvironment, name); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		_, err = tx.Journal().Append(ctx, event)
		return err
	})
}

// environmentRecord is the journal row for one act on an environment: design
// 009's record, built here so the row and the state it explains commit in one
// transaction.
func (c *Controlled) environmentRecord(ctx context.Context, mutation string, obj v1.Environment) (Event, error) {
	rec, err := events.EnvironmentMutation(events.Type(mutation), obj,
		events.Environment{Workers: obj.Status.Workers, Phase: obj.Status.Phase, Reason: obj.Status.Reason},
		events.ActorFrom(ctx), time.Now().UTC())
	if err != nil {
		return Event{}, err
	}
	return journalRow(rec, c.delivery)
}

// encodeEnvironment turns one environment into a row: the object in data, the
// phase loop's status beside it, and the identity as columns. The id is the
// name, which spec 021 fixes at create and no update moves.
func encodeEnvironment(obj v1.Environment) (Object, error) {
	status := obj.Status
	obj.Status = v1.EnvironmentStatus{}
	data, err := json.Marshal(obj)
	if err != nil {
		return Object{}, fmt.Errorf("store: encoding the environment: %w", err)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		return Object{}, fmt.Errorf("store: encoding the environment status: %w", err)
	}
	return Object{
		Kind: KindEnvironment, ID: obj.Metadata.Name, Owner: status.Owner, Name: obj.Metadata.Name,
		Environment: obj.Metadata.Name, Phase: status.Phase, Labels: obj.Metadata.Labels,
		Data: data, Status: encoded,
	}, nil
}

// decodeEnvironment reverses encodeEnvironment and carries the row's version
// into the status, which is what design 008's ETag returns.
func decodeEnvironment(row Object) (v1.Environment, error) {
	var obj v1.Environment
	if err := json.Unmarshal(row.Data, &obj); err != nil {
		return v1.Environment{}, fmt.Errorf("store: decoding the environment %s: %w", row.ID, err)
	}
	if len(row.Status) > 0 {
		if err := json.Unmarshal(row.Status, &obj.Status); err != nil {
			return v1.Environment{}, fmt.Errorf("store: decoding the status of %s: %w", row.ID, err)
		}
	}
	obj.Status.Version = row.Version
	return obj, nil
}

// The controller's seam this type satisfies for the Environment kind.
var _ controller.Environments = (*Controlled)(nil)
