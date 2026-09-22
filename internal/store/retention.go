// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"
)

// Retention forgets what a store keeps only for a window: the journal's
// finished records and the worker operations that were answered, each older
// than the window (design 010). A record still waiting for the sink and an
// operation not yet answered are kept whatever their age, because delivery
// and redelivery decide when those end, not the retention.
type Retention struct {
	store  Store
	window time.Duration
}

// NewRetention is the retention of one store over one window.
func NewRetention(s Store, window time.Duration) *Retention {
	return &Retention{store: s, window: window}
}

// Prune runs one transaction over both tables and reports how many rows went.
func (r *Retention) Prune(ctx context.Context, now time.Time) (int, error) {
	before := now.Add(-r.window)
	var pruned int
	err := r.store.Tx(ctx, func(tx Tx) error {
		records, err := tx.Journal().Prune(ctx, before)
		if err != nil {
			return fmt.Errorf("pruning the journal: %w", err)
		}
		operations, err := tx.Operations().Prune(ctx, before)
		if err != nil {
			return fmt.Errorf("pruning the operations: %w", err)
		}
		pruned = records + operations
		return nil
	})
	return pruned, err
}
