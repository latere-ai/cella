// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
)

// racing is a querier that runs race once, before its first write: what
// another process does between a read and the write that follows it.
type racing struct {
	querier
	race func()
	once sync.Once
}

func (r *racing) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.once.Do(r.race)
	return r.querier.Exec(ctx, sql, args...)
}

// RewrapRacing is a rotation in one transaction of its own, with race run
// between the rotation's reads and its first write.
func RewrapRacing(ctx context.Context, s *Store, oldKEK, newKEK []byte, race func()) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	n, err := values{q: &racing{querier: tx, race: race}, env: s.env}.Rewrap(ctx, oldKEK, newKEK)
	if err != nil {
		return n, errors.Join(err, tx.Rollback(ctx))
	}
	return n, tx.Commit(ctx)
}
