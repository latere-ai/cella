// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/events"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// Controlled is the store behind the narrow interfaces the controller declares
// over it: its desired state, its observed index, its journal and its lease.
//
// The controller imports nothing under internal/, so this is the one place the
// two vocabularies meet: a v1.Sandbox on one side and a row with a version on
// the other.
type Controlled struct {
	store       Store
	environment string
	holder      string

	// versions is the row version of every object this process has read or
	// written, which is what makes each write conditional. A write whose row
	// moved under another replica is ErrVersionConflict and not a silent
	// overwrite.
	mu       sync.Mutex
	versions map[string]int64
}

// ForController wraps a store for one environment's controller.
func ForController(s Store, environment string) *Controlled {
	return &Controlled{store: s, environment: environment, holder: Holder(), versions: map[string]int64{}}
}

// Store reports the store underneath, for a caller that needs the contract
// itself rather than the controller's view of it.
func (c *Controlled) Store() Store { return c.store }

// Holder is this process's lease identity.
func (c *Controlled) Holder() string { return c.holder }

// Durable reports whether desired state outlives this process, which is what
// decides between recovery and the grace of design 005.
func (c *Controlled) Durable() bool { return c.store.Durable() }

// Ready is the store's readiness check, which cellad mounts on its probe.
func (c *Controlled) Ready(ctx context.Context) error { return c.store.Ready(ctx) }

// Close closes the store, which releases every lease this process holds.
func (c *Controlled) Close() error { return c.store.Close() }

// Load reads this environment's desired state and remembers every row's
// version, so the first write of each object is conditional on the row it was
// read from.
func (c *Controlled) Load() (map[string]v1.Sandbox, error) {
	ctx := context.Background()
	objects := map[string]v1.Sandbox{}
	versions := map[string]int64{}
	cursor := ""
	for {
		var (
			rows []Object
			next string
		)
		err := c.store.Tx(ctx, func(tx Tx) error {
			var err error
			rows, next, err = tx.Desired().List(ctx, KindSandbox,
				Filter{Environment: c.environment}, Page{Cursor: cursor})
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			obj, err := decode(row)
			if err != nil {
				return nil, err
			}
			objects[row.ID] = obj
			versions[row.ID] = row.Version
		}
		if next == "" {
			break
		}
		cursor = next
	}
	c.mu.Lock()
	c.versions = versions
	c.mu.Unlock()
	return objects, nil
}

// Save writes the whole map, which is the snapshot store's contract. A
// controller that found a Durable takes Write and Remove per object instead,
// so this runs only for a caller that holds the store as a plain Store.
func (c *Controlled) Save(objects map[string]v1.Sandbox) error {
	ctx := context.Background()
	for id, obj := range objects {
		if err := c.Write(ctx, obj, controller.MutationSaved); err != nil {
			return fmt.Errorf("store: saving %s: %w", id, err)
		}
	}
	held, err := c.Load()
	if err != nil {
		return err
	}
	for id := range held {
		if _, kept := objects[id]; kept {
			continue
		}
		if err := c.Remove(ctx, id, controller.MutationDeleted); err != nil {
			return err
		}
	}
	return nil
}

// Write stores one object at the version this process last saw and appends the
// mutation to the journal, in one transaction. One call is one mutation and
// one journal row, so a journal that misses a write is not reachable from
// here.
func (c *Controlled) Write(ctx context.Context, obj v1.Sandbox, mutation string) error {
	row, err := encode(obj)
	if err != nil {
		return err
	}
	if row.Environment == "" {
		row.Environment = c.environment
	}
	c.mu.Lock()
	version := c.versions[row.ID]
	c.mu.Unlock()
	event, err := c.record(ctx, mutation, obj)
	if err != nil {
		return err
	}
	var written int64
	err = c.store.Tx(ctx, func(tx Tx) error {
		next, err := tx.Desired().Put(ctx, row, version)
		if err != nil {
			return err
		}
		if _, err := tx.Journal().Append(ctx, event); err != nil {
			return err
		}
		written = next
		return nil
	})
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.versions[row.ID] = written
	c.mu.Unlock()
	return nil
}

// Remove deletes one object and appends the mutation, in one transaction. A
// row another replica already deleted is not an error: the intent is gone
// either way, and the journal still records that this replica ended it.
func (c *Controlled) Remove(ctx context.Context, id, mutation string) error {
	err := c.store.Tx(ctx, func(tx Tx) error {
		// The row is read before it goes, so the record carries the labels,
		// name, owner and reason of an object that no longer exists once the
		// transaction ends. A row another replica already removed leaves the
		// record with the id alone, which is still a true statement that
		// this replica ended the object.
		obj := v1.Sandbox{Status: v1.SandboxStatus{ID: id}}
		if row, err := tx.Desired().Get(ctx, KindSandbox, id); err == nil {
			if decoded, err := decode(row); err == nil {
				obj = decoded
			}
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		event, err := c.record(ctx, mutation, obj)
		if err != nil {
			return err
		}
		if err := tx.Desired().Delete(ctx, KindSandbox, id); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		_, err = tx.Journal().Append(ctx, event)
		return err
	})
	if err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.versions, id)
	c.mu.Unlock()
	return nil
}

// record is the journal row for one mutation: design 009's record, built from
// the object being written, the act's name, and the actor the context
// carries. It is built here rather than in the controller so the row and the
// state it explains commit in one transaction.
func (c *Controlled) record(ctx context.Context, mutation string, obj v1.Sandbox) (Event, error) {
	kind := events.Type(mutation)
	rec, err := events.Mutation(kind, events.ReasonOf(obj.Status.Reason, kind),
		events.OfSandbox(obj), events.MutationData(kind, obj), events.ActorFrom(ctx), time.Now().UTC())
	if err != nil {
		return Event{}, err
	}
	return journalRow(rec)
}

// Rebuild replaces the observed rows of one environment with what its driver
// last listed.
func (c *Controlled) Rebuild(ctx context.Context, environment string, states []driver.State) error {
	return c.store.Tx(ctx, func(tx Tx) error {
		return tx.Observed().Rebuild(ctx, environment, states)
	})
}

// Events reads one object's journal, newest first. It is what a test and an
// operator read to see the order a sandbox went through.
func (c *Controlled) Events(ctx context.Context, id string, limit int) ([]Event, error) {
	var events []Event
	err := c.store.Tx(ctx, func(tx Tx) error {
		var err error
		events, _, err = tx.Journal().ByObject(ctx, id, Page{Limit: limit})
		return err
	})
	return events, err
}

// Acquire is the controller's lease seam: this process takes or renews the
// named lease, and the store renews it underneath until it is released.
func (c *Controlled) Acquire(ctx context.Context, name string, ttl time.Duration) (bool, error) {
	var held bool
	err := c.store.Tx(ctx, func(tx Tx) error {
		var err error
		held, err = tx.Leases().Acquire(ctx, name, c.holder, ttl)
		return err
	})
	return held, err
}

// encode turns one sandbox into a row: the resolved object in data, the
// controller's status beside it, and the identity as columns.
func encode(obj v1.Sandbox) (Object, error) {
	status := obj.Status
	obj.Status = v1.SandboxStatus{}
	data, err := json.Marshal(obj)
	if err != nil {
		return Object{}, fmt.Errorf("store: encoding the sandbox: %w", err)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		return Object{}, fmt.Errorf("store: encoding the status: %w", err)
	}
	return Object{
		Kind: KindSandbox, ID: status.ID, Owner: status.Owner, Name: obj.Metadata.Name,
		Environment: status.Environment, Phase: status.Phase, Labels: obj.Metadata.Labels,
		Data: data, Status: encoded,
	}, nil
}

// decode reverses encode.
func decode(row Object) (v1.Sandbox, error) {
	var obj v1.Sandbox
	if err := json.Unmarshal(row.Data, &obj); err != nil {
		return v1.Sandbox{}, fmt.Errorf("store: decoding the sandbox %s: %w", row.ID, err)
	}
	if len(row.Status) > 0 {
		if err := json.Unmarshal(row.Status, &obj.Status); err != nil {
			return v1.Sandbox{}, fmt.Errorf("store: decoding the status of %s: %w", row.ID, err)
		}
	}
	return obj, nil
}

// The controller's interfaces this type satisfies. A change to either side
// that breaks the other is a build failure here rather than a nil store at
// start-up.
var (
	_ controller.Store   = (*Controlled)(nil)
	_ controller.Durable = (*Controlled)(nil)
	_ controller.Lease   = (*Controlled)(nil)
)
