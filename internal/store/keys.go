// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"time"

	"latere.ai/x/cella/internal/auth"
)

// KeyRegistry is the registry of environment keys as the key routes use it
// outside a transaction: the record a mint writes, the mark a revocation
// writes beside the revocation list's own row, and the page a listing reads.
// It is the auth.KeyLog the control plane's key half is built over.
type KeyRegistry struct{ store Store }

// NewKeyRegistry is the registry over one store, the one the revocation list
// is in.
func NewKeyRegistry(s Store) *KeyRegistry { return &KeyRegistry{store: s} }

// Record keeps one minted key.
func (r *KeyRegistry) Record(ctx context.Context, k auth.KeyRecord) error {
	return r.store.Tx(ctx, func(tx Tx) error {
		return tx.Keys().Record(ctx, Key{
			JTI: k.JTI, Environment: k.Environment, Subject: k.Subject,
			MintedAt: k.MintedAt, ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt,
		})
	})
}

// Revoke marks the key revoked at the instant given and refuses its jti until
// exp passes, in one transaction, so a listing never shows a key live that the
// verifier refuses, nor one revoked that it still accepts. A jti with no
// record is refused all the same.
func (r *KeyRegistry) Revoke(ctx context.Context, jti string, at, exp time.Time) error {
	return r.store.Tx(ctx, func(tx Tx) error {
		if err := tx.Revocations().Revoke(ctx, jti, exp); err != nil {
			return err
		}
		return tx.Keys().Revoke(ctx, jti, at)
	})
}

// List is one page of an environment's keys in the order they were minted,
// and the cursor of the next page.
func (r *KeyRegistry) List(ctx context.Context, environment, cursor string, limit int) ([]auth.KeyRecord, string, error) {
	var (
		rows []Key
		next string
	)
	err := r.store.Tx(ctx, func(tx Tx) error {
		var err error
		rows, next, err = tx.Keys().List(ctx, environment, Page{Limit: limit, Cursor: cursor})
		return err
	})
	if err != nil {
		return nil, "", err
	}
	out := make([]auth.KeyRecord, 0, len(rows))
	for _, k := range rows {
		out = append(out, auth.KeyRecord{
			JTI: k.JTI, Environment: k.Environment, Subject: k.Subject,
			MintedAt: k.MintedAt, ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt,
		})
	}
	return out, next, nil
}
