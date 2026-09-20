// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// Tokens is the identity every sandbox carries (design 006): the mint the
// create order reaches at its fifth step, and the revocation that ends a
// token the control plane replaced or a sandbox that is gone.
//
// A control plane with no Tokens mints nothing. Every sandbox is then created
// without one, no driver projects a file, and the rotation rule never fires.
type Tokens interface {
	// Mint signs one sandbox's identity. The expiry is the sandbox's own or
	// the mint's cap, whichever is sooner, so a token never outlives what it
	// speaks for.
	Mint(ctx context.Context, obj v1.Sandbox) (token, jti string, exp time.Time, err error)
	// Revoke refuses the jti from now until exp passes.
	Revoke(ctx context.Context, jti string, exp time.Time) error
}

// TokenSweeper is the optional half of Tokens: the list that forgets a
// revocation once no token could still present it. The reaper's tick runs it
// where the implementation has one.
type TokenSweeper interface {
	Forget(ctx context.Context, before time.Time) (int, error)
}

// rotateAfter is the fraction of a token's life that passes before it is
// re-minted, from design 005's table: two thirds, which leaves a third of the
// lifetime for the re-projection to reach the sandbox and be read.
const (
	rotateNumerator   = 2
	rotateDenominator = 3
)

// dueForRotation reports whether the sandbox's token has passed two thirds of
// its lifetime and a re-mint would give it a longer one.
//
// A token whose expiry is already the sandbox's own is never rotated: the cap
// of design 006 would mint the same expiry again, so the rule would fire on
// every tick until the sandbox is reaped, and the sandbox and the identity it
// carries are meant to end together. The sandbox's expiry is read from the
// state the driver just reported rather than from the status, because an
// update may have moved it since the status was last written.
func dueForRotation(state *v1.TokenState, expiresAt, now time.Time) bool {
	if state == nil || state.ExpiresAt.IsZero() || !state.IssuedAt.Before(state.ExpiresAt) {
		return false
	}
	if !expiresAt.IsZero() && !state.ExpiresAt.Before(expiresAt) {
		return false
	}
	life := state.ExpiresAt.Sub(state.IssuedAt)
	return !now.Before(state.IssuedAt.Add(life * rotateNumerator / rotateDenominator))
}

// mintToken signs one sandbox's identity, which is step 5 of design 005's
// create order. A control plane with no mint returns nothing, and every step
// after this one reads that as a sandbox with no identity.
func (c *Controller) mintToken(ctx context.Context, obj v1.Sandbox) (token string, state *v1.TokenState, err error) {
	if c.tokens == nil {
		return "", nil, nil
	}
	value, jti, exp, err := c.tokens.Mint(ctx, obj)
	if err != nil {
		return "", nil, fmt.Errorf("minting the sandbox's identity: %w", err)
	}
	return value, &v1.TokenState{JTI: jti, IssuedAt: c.clock.Now().UTC(), ExpiresAt: exp.UTC()}, nil
}

// revokeToken ends one token. It is called where design 005 says the identity
// goes: the undo of a failed create, the delete, and the replaced half of a
// rotation. A revocation that cannot be written is reported to the caller,
// which logs it; the act it belongs to has already happened.
func (c *Controller) revokeToken(ctx context.Context, state *v1.TokenState) error {
	if c.tokens == nil || state == nil || state.JTI == "" {
		return nil
	}
	if err := c.tokens.Revoke(ctx, state.JTI, state.ExpiresAt); err != nil {
		return fmt.Errorf("revoking the token %s: %w", state.JTI, err)
	}
	return nil
}

// rotateLocked is design 005's token rule in one act: mint, re-project
// through the driver, revoke the token that was replaced, and record what the
// sandbox now holds.
//
// The order is what keeps a sandbox from ever holding a revoked token. The
// new one reaches the sandbox before the old one is ended, so the window in
// which both verify is the projection, and the window in which neither does
// is empty. A projection that fails revokes the token it just minted and
// leaves the sandbox with the one it had.
func (c *Controller) rotateLocked(ctx context.Context, obj v1.Sandbox) error {
	id := obj.Status.ID
	previous := obj.Status.TokenState
	token, state, err := c.mintToken(ctx, obj)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	if err := c.driver.Update(ctx, id, driver.Change{Token: []byte(token)}); err != nil {
		return errors.Join(fmt.Errorf("re-projecting the token into %s: %w", id, err),
			c.revokeToken(ctx, state))
	}
	obj.Status.TokenState = state
	// The status carries the record and no manifest path changed, so the
	// mutation is the status write of design 009, which is journaled and
	// never delivered: a sandbox's identity being replaced is the control
	// plane keeping its own promise, not an act a reader of the feed takes.
	if err := c.persist(ctx, obj, MutationStatus); err != nil {
		return err
	}
	c.log.InfoContext(ctx, "the sandbox's identity was re-minted", "sandbox", id,
		"jti", state.JTI, "expires", state.ExpiresAt)
	c.metrics.TokenReminted()
	return c.revokeToken(ctx, previous)
}

// sweepRevocations drops the revocations whose tokens have expired, which is
// design 010's Forget on the reaper's tick. A list without the sweep keeps
// every jti forever, and the rows are bounded by the longest lifetime a live
// token has rather than by the sandboxes that ever ran.
func (c *Controller) sweepRevocations(ctx context.Context, now time.Time) error {
	sweeper, ok := c.tokens.(TokenSweeper)
	if !ok {
		return nil
	}
	if _, err := sweeper.Forget(ctx, now); err != nil {
		return fmt.Errorf("sweeping the revocation list: %w", err)
	}
	return nil
}
