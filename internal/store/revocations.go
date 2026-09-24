// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"
)

// RevocationList is the revocation list as a caller outside a transaction
// reads it: the verifier of design 006 asking about one jti, the mint
// revoking one, and the reaper's tick sweeping the rows that have expired.
// Each call is its own transaction, because none of the three is part of a
// state change that has to commit with it.
type RevocationList struct{ store Store }

// NewRevocations is the list over one store.
func NewRevocations(s Store) *RevocationList { return &RevocationList{store: s} }

// Revoke refuses the jti until exp passes.
func (r *RevocationList) Revoke(ctx context.Context, jti string, exp time.Time) error {
	return r.store.Tx(ctx, func(tx Tx) error { return tx.Revocations().Revoke(ctx, jti, exp) })
}

// Revoked reports whether this jti was revoked.
func (r *RevocationList) Revoked(ctx context.Context, jti string) (bool, error) {
	var revoked bool
	err := r.store.Tx(ctx, func(tx Tx) error {
		var err error
		revoked, err = tx.Revocations().Revoked(ctx, jti)
		return err
	})
	return revoked, err
}

// Forget drops the revocations and the key records whose exp has passed and
// reports how many went. The key records are swept here because both are
// rows a credential leaves behind and both end at its exp, and this is the
// sweep the reaper's tick already runs under its lease.
func (r *RevocationList) Forget(ctx context.Context, before time.Time) (int, error) {
	var n int
	err := r.store.Tx(ctx, func(tx Tx) error {
		revoked, err := tx.Revocations().Forget(ctx, before)
		if err != nil {
			return err
		}
		expired, err := tx.Keys().Forget(ctx, before)
		n = revoked + expired
		return err
	})
	return n, err
}
