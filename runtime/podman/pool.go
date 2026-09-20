// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// adopt turns one prewarmed entry into the caller's sandbox, which is the pool
// of spec 020 as this driver provides it.
//
// The claim is the record generation. A mutation writes generation n+1 as a
// new volume, and the engine refuses a second volume of one name, so of two
// adopters that both read generation n exactly one creates n+1 and the other
// is told the entry is gone. This is the whole of the exclusivity: it holds
// between two driver instances over one engine, where the in-process lock
// does not.
//
// The claim is written before the identity and the gateway's authority are
// projected into the container. A loser must never have written its caller's
// token into a container the winner now owns, and a projection that fails
// after the claim deletes the sandbox rather than leaving half of one
// caller's identity in the pool.
func (d *Driver) adopt(ctx context.Context, id string, a driver.Adoption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID.MatchString(id) {
		return driver.ErrInvalid
	}
	if a.Lifecycle.TTL < 0 || a.Lifecycle.AutoStop < 0 || a.Lifecycle.AutoDelete < 0 {
		return fmt.Errorf("%w: a negative lifecycle duration", driver.ErrInvalid)
	}
	unlock := d.lock(id)
	defer unlock()
	vi, err := d.inspectVolume(ctx, workspaceVolume(id))
	if err != nil {
		return err
	}
	ident := identityOf(vi.Labels)
	if a.Workspace.Path != "" && a.Workspace.Path != ident.workspacePath {
		return fmt.Errorf("%w: the entry's workspace is at %s, not %s", driver.ErrInvalid, ident.workspacePath, a.Workspace.Path)
	}
	held, err := d.current(ctx, id)
	if err != nil {
		return err
	}
	if !held.pool {
		return driver.ErrNotFound
	}
	running, err := d.inspectContainer(ctx, id)
	if err != nil {
		return err
	}
	// The clock is cut to the engine's own resolution, as the create's is,
	// so an adopted sandbox does not read as having started before it began.
	now := time.Now().UTC().Truncate(time.Second)
	env := tokenEnv(a.Token, egressEnv(a.Env, a.Egress))
	next := record{
		labels: maps.Clone(a.Labels), env: maps.Clone(env), lastActivityAt: now,
		ttl: a.Lifecycle.TTL, autoStop: a.Lifecycle.AutoStop, autoDelete: a.Lifecycle.AutoDelete,
		owner: a.Owner, name: a.Name, adoptedAt: now,
	}
	if err := d.writeRecord(ctx, id, held.generation, next); err != nil {
		if conflict(err) {
			// Another writer took generation n+1 between the read and this
			// write. For an adopter that means the entry is no longer one.
			return driver.ErrNotFound
		}
		return err
	}
	if err := d.projectAdopted(ctx, id, running.user, a); err != nil {
		return errors.Join(err, d.deleteLocked(context.WithoutCancel(ctx), id))
	}
	return nil
}

// projectAdopted writes the two files the adopted sandbox holds inside itself:
// the authority its gateway signs with, and its own identity. Both are written
// into the running container, which is why they follow the claim rather than
// travelling with it.
func (d *Driver) projectAdopted(ctx context.Context, id, user string, a driver.Adoption) error {
	if a.Egress.CAPEM != "" {
		if err := d.putEgressCA(ctx, id, a.Egress.CAPEM); err != nil {
			return fmt.Errorf("podman: adopting %s: %w", id, err)
		}
	}
	if len(a.Token) > 0 {
		if err := d.putToken(ctx, id, user, a.Token); err != nil {
			return fmt.Errorf("podman: adopting %s: %w", id, err)
		}
	}
	return nil
}
