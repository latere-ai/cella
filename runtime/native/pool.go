// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// adopt turns one prewarmed entry into the caller's sandbox, which is the pool
// of spec 020 as this driver provides it. One process owns the root directory,
// so the driver's own lock is the whole of the exclusivity: the record is read
// and rewritten inside it, and a second adopter that arrives after the rewrite
// reads an entry that is no longer one.
//
// The claim is written before the identity and the authority are projected. A
// projection that fails afterwards deletes the sandbox rather than leaving a
// half-adopted entry in the pool, and the controller creates for real.
func (d *Driver) adopt(ctx context.Context, id string, a driver.Adoption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.Lifecycle.TTL < 0 || a.Lifecycle.AutoStop < 0 || a.Lifecycle.AutoDelete < 0 {
		return fmt.Errorf("%w: a negative lifecycle duration", driver.ErrInvalid)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.load(id)
	if err != nil {
		return err
	}
	if !r.State.Pool {
		// Either this id was never an entry or another adopter has already
		// taken it. Both are the same answer to this caller: what it asked
		// to adopt is not there.
		return driver.ErrNotFound
	}
	if a.Workspace.Path != "" && a.Workspace.Path != r.Workdir {
		return fmt.Errorf("%w: the entry's workspace is at %s, not %s", driver.ErrInvalid, r.Workdir, a.Workspace.Path)
	}
	now := time.Now().UTC()
	r.State.Pool = false
	r.State.Owner, r.State.Name = a.Owner, a.Name
	r.State.Labels = maps.Clone(a.Labels)
	// The deadlines count from the adoption and not from the prewarm, so an
	// entry that sat warm for hours is not expired on the first tick after a
	// caller took it.
	r.State.CreatedAt, r.State.LastActivityAt = now, now
	r.State.AutoStop, r.State.AutoDelete = a.Lifecycle.AutoStop, a.Lifecycle.AutoDelete
	r.State.ExpiresAt = time.Time{}
	if a.Lifecycle.TTL > 0 {
		r.State.ExpiresAt = now.Add(a.Lifecycle.TTL)
	}
	if err := d.save(id, r); err != nil {
		return err
	}
	env, err := projectEgress(d.dir(id), a.Env, a.Egress)
	if err == nil {
		env, err = projectToken(d.dir(id), a.Token, env)
	}
	if err == nil {
		r.Env = env
		err = d.save(id, r)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("native: adopting %s: %w", id, err), d.deleteLocked(id))
	}
	return nil
}
