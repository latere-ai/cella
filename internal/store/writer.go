// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"latere.ai/x/cella/controller"
)

// WriterLease is the lease that makes a process the writer of spec 076: the
// one process that opens the controller, runs its loops, holds the gateway and
// worker hubs and answers the API. Every other replica forwards to it.
const WriterLease = "writer"

// Advertise records the address this process's public listener is reached at
// on every lease it takes from here, and returns the adapter.
func (c *Controlled) Advertise(address string) *Controlled {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.address = address
	return c
}

// Fence makes every write transaction of this adapter first find this process
// holding the named lease, and refuse with controller.ErrNotWriter where it
// does not, and returns the adapter. A writer that lost its lease without
// knowing it, across a partition or a pause longer than the term, therefore
// writes nothing after its successor read the rows.
func (c *Controlled) Fence(name string) *Controlled {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fence = name
	return c
}

// Lease reads one lease row: who holds it, where that holder is reached, and
// whether its term has not lapsed.
func (c *Controlled) Lease(ctx context.Context, name string) (Lease, error) {
	var out Lease
	err := c.store.Tx(ctx, func(tx Tx) error {
		var err error
		out, err = tx.Leases().Get(ctx, name)
		return err
	})
	return out, err
}

// ReleaseAll frees every lease this process has taken, the loops' and the
// writer's, the writer's last, so a successor that promotes finds the loops'
// leases free. A lease another holder took since is left alone by the store.
func (c *Controlled) ReleaseAll(ctx context.Context) error {
	c.mu.Lock()
	names := make([]string, 0, len(c.acquired))
	for name := range c.acquired {
		names = append(names, name)
	}
	c.acquired = map[string]struct{}{}
	c.mu.Unlock()
	// The writer's goes last: a successor that promotes on seeing it free
	// finds every loop lease free too.
	slices.Sort(names)
	if i := slices.Index(names, WriterLease); i >= 0 {
		names = append(slices.Delete(names, i, i+1), WriterLease)
	}
	var failed error
	for _, name := range names {
		err := c.store.Tx(ctx, func(tx Tx) error { return tx.Leases().Release(ctx, name, c.holder) })
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("store: releasing the lease %s: %w", name, err))
		}
	}
	return failed
}

// write runs one write transaction behind the fence, and maps the store's
// refusals onto the controller's: a name a live row holds is
// controller.ErrNameTaken and a row that moved since it was read is
// controller.ErrVersionConflict, each still matching the store's own error.
func (c *Controlled) write(ctx context.Context, fn func(Tx) error) error {
	c.mu.Lock()
	fence := c.fence
	c.mu.Unlock()
	err := c.store.Tx(ctx, func(tx Tx) error {
		if fence != "" {
			held, err := tx.Leases().Holds(ctx, fence, c.holder)
			if err != nil {
				return err
			}
			if !held {
				return controller.ErrNotWriter
			}
		}
		return fn(tx)
	})
	switch {
	case err == nil, errors.Is(err, controller.ErrNotWriter):
		return err
	case errors.Is(err, ErrNameTaken) && !errors.Is(err, controller.ErrNameTaken):
		return fmt.Errorf("%w: %w", controller.ErrNameTaken, err)
	case errors.Is(err, ErrVersionConflict) && !errors.Is(err, controller.ErrVersionConflict):
		return fmt.Errorf("%w: %w", controller.ErrVersionConflict, err)
	}
	return err
}
