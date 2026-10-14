// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/postgres/migrations"
)

// The two maintenance runs an operator starts against a database with no
// replica of cellad serve running: Unseal, which takes a database back to a
// release that does not seal, and Rotate, which moves what the database holds
// sealed to another key. Each holds the writer lease for its whole run, so a
// replica started meanwhile waits rather than writing rows the run is moving.

// ErrWriterRunning is a maintenance run refused because a replica of cellad
// serve holds the writer lease.
var ErrWriterRunning = errors.New("store: a replica of cellad serve holds the writer lease")

// UnsealResult is what Unseal did: how many rows it rewrote and the schema
// version the database is at when it returns.
type UnsealResult struct {
	Unsealed int
	Schema   uint
}

// Unseal takes a database back to a release that does not seal: it puts every
// sealed sandbox value back into its row in the clear and migrates the schema
// down to the version before SealedCredentials, which that release starts on.
// It needs the key the rows were sealed under and the direct URL, since a
// migration holds a session lock a transaction pooler loses. A database
// already below SealedCredentials holds nothing sealed, and is left as it is,
// so a second run does nothing.
func Unseal(ctx context.Context, o Options) (UnsealResult, error) {
	if len(o.Key) == 0 {
		return UnsealResult{}, fmt.Errorf("store: unsealing needs the key the rows were sealed under: %w", store.ErrNoSecretKey)
	}
	version, err := schemaVersion(ctx, o.URL)
	if err != nil {
		return UnsealResult{}, err
	}
	if version < migrations.SealedCredentials {
		return UnsealResult{Schema: version}, nil
	}
	o.PoolURL = ""
	s, err := Open(ctx, o)
	if err != nil {
		return UnsealResult{Schema: version}, err
	}
	n, err := s.whileWriter(ctx, "unsealing", s.UnsealCredentials)
	closed := s.Close()
	if err != nil {
		return UnsealResult{Unsealed: n, Schema: version}, errors.Join(err, closed)
	}
	if closed != nil {
		return UnsealResult{Unsealed: n, Schema: version}, closed
	}
	below := migrations.SealedCredentials - 1
	if err := downTo(o.URL, below); err != nil {
		return UnsealResult{Unsealed: n, Schema: version}, err
	}
	return UnsealResult{Unsealed: n, Schema: below}, nil
}

// Rotate moves every data key the database holds wrapped, each secret value's
// and each sealed sandbox row's, from o.Key to next, and reports how many it
// moved. o.Key must open every one: Open refuses a key that does not. No
// value's ciphertext is read or written. Every replica runs under the old key
// until it restarts, so Rotate is run with none serving, and each starts again
// under next.
func Rotate(ctx context.Context, o Options, next []byte) (int, error) {
	if len(o.Key) == 0 {
		return 0, fmt.Errorf("store: a rotation needs the key the database is sealed under now: %w", store.ErrNoSecretKey)
	}
	if _, _, err := store.RewrapKeys(o.Key, next); err != nil {
		return 0, fmt.Errorf("store: the rotation's keys: %w", err)
	}
	s, err := Open(ctx, o)
	if err != nil {
		return 0, err
	}
	n, err := s.whileWriter(ctx, "rotating the key", func(ctx context.Context) (int, error) {
		var moved int
		err := s.Tx(ctx, func(tx store.Tx) error {
			var err error
			moved, err = tx.Values().Rewrap(ctx, o.Key, next)
			return err
		})
		return moved, err
	})
	return n, errors.Join(err, s.Close())
}

// whileWriter runs fn holding the writer lease, which it takes for the run and
// releases after it, and refuses where a replica holds it.
func (s *Store) whileWriter(ctx context.Context, what string, fn func(context.Context) (int, error)) (int, error) {
	holder := "maintenance-" + store.Holder()
	var held bool
	err := s.Tx(ctx, func(tx store.Tx) error {
		var err error
		held, err = tx.Leases().Acquire(ctx, store.WriterLease, holder, "", controller.LeaseTTL)
		return err
	})
	if err != nil {
		return 0, err
	}
	if !held {
		return 0, fmt.Errorf("%w: stop every replica before %s", ErrWriterRunning, what)
	}
	n, err := fn(ctx)
	detached := context.WithoutCancel(ctx)
	released := s.Tx(detached, func(tx store.Tx) error {
		return tx.Leases().Release(detached, store.WriterLease, holder)
	})
	return n, errors.Join(err, released)
}

// schemaVersion reads the version the database's schema is at, zero for one
// with no schema, under the same refusals Open applies.
func schemaVersion(ctx context.Context, rawURL string) (uint, error) {
	highest, err := migrations.Highest()
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	conn, err := pgx.Connect(ctx, rawURL)
	if err != nil {
		return 0, fmt.Errorf("store: connecting to CELLA_DB_URL: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var (
		version int64
		dirty   bool
	)
	err = conn.QueryRow(ctx, "select version, dirty from schema_migrations").Scan(&version, &dirty)
	switch {
	case errors.Is(err, pgx.ErrNoRows), isUndefinedTable(err):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("store: reading the schema version: %w", err)
	case dirty:
		return 0, fmt.Errorf("store: the schema is dirty at version %d: a migration failed halfway and an operator repairs it first", version)
	case version > int64(highest):
		return 0, fmt.Errorf("store: the schema is at version %d and this binary carries %d: run the version of cellad that wrote the schema", version, highest)
	}
	return uint(version), nil
}

// downTo migrates the schema down to version, running every down migration
// above it in order. A down migration that refuses leaves the schema where it
// stopped and reports why.
func downTo(rawURL string, version uint) error {
	dsn, err := migrationURL(rawURL)
	if err != nil {
		return err
	}
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("store: the embedded migrations: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		return fmt.Errorf("store: opening the migrator: %w", err)
	}
	err = m.Migrate(version)
	if errors.Is(err, migrate.ErrNoChange) {
		err = nil
	}
	if err != nil {
		err = fmt.Errorf("store: migrating the schema down to %d: %w", version, err)
	}
	sourceErr, dbErr := m.Close()
	return errors.Join(err, sourceErr, dbErr)
}
