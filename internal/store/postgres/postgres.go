// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package postgres is the durable adapter of design 010: the same contract the
// memory adapter serves, over one connection pool and the schema under
// migrations/.
//
// It is what makes desired state outlive the process, so a sandbox the data
// plane lost is recreated rather than forgotten, and what lets two control
// plane replicas agree on one writer through the leases table.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // the pgx5:// driver pgxmigrate selects by scheme
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"latere.ai/x/pkg/pgxmigrate"

	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/postgres/migrations"
)

// Pool bounds. The database a fleet of services shares has a small connection
// ceiling, and a control plane replica reads a few rows per tick, so it holds
// few connections and none idle.
const (
	// DefaultMaxConns is the pool CELLA_DB_MAX_CONNS overrides.
	DefaultMaxConns = 4
	// MaxMaxConns is the largest pool the store opens: past it one replica
	// holds the ceiling the whole fleet shares.
	MaxMaxConns       = 32
	minConns          = 0
	maxConnLifetime   = 30 * time.Minute
	maxConnIdleTime   = time.Minute
	healthCheckPeriod = time.Minute
	// readyBudget is the readiness statement's budget, inside the probe's.
	readyBudget = time.Second
	// DefaultRenewEvery is a third of the 15 second lease term of design 010,
	// so a holder renews twice before its term would lapse.
	DefaultRenewEvery = 5 * time.Second
)

// ErrClosed is every call after Close.
var ErrClosed = errors.New("store: the postgres store is closed")

// Options opens the Postgres store.
type Options struct {
	// URL is CELLA_DB_URL: a postgres:// URL on a direct endpoint or a
	// session-mode pooler. A migration holds a session-scoped lock across its
	// statements, which a transaction-mode pooler loses between transactions.
	URL string
	// MaxConns is CELLA_DB_MAX_CONNS. Zero takes DefaultMaxConns.
	MaxConns int32
	// Key is the 32 byte key secret values are sealed under. Without one the
	// store serves everything but Values.
	Key []byte
	// RenewEvery is how often a lease this process holds is renewed. Zero
	// takes DefaultRenewEvery.
	RenewEvery time.Duration
}

// Store is one control plane's state in Postgres.
type Store struct {
	pool *pgxpool.Pool
	env  store.Envelope

	mu     sync.Mutex
	held   map[string]string // lease name to the holder this process registered
	terms  map[string]time.Duration
	closed bool

	stop context.CancelFunc
	done chan struct{}
}

// Open connects, refuses a schema this binary does not know, applies the
// pending migrations, and starts the lease renewal.
func Open(ctx context.Context, o Options) (*Store, error) {
	env, err := store.NewEnvelope(o.Key)
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(o.URL)
	if err != nil {
		return nil, fmt.Errorf("store: CELLA_DB_URL is not a Postgres URL: %w", err)
	}
	cfg.MaxConns = bound(o.MaxConns)
	cfg.MinConns = minConns
	cfg.MaxConnLifetime = maxConnLifetime
	cfg.MaxConnIdleTime = maxConnIdleTime
	cfg.HealthCheckPeriod = healthCheckPeriod
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: opening the pool: %w", err)
	}
	if err := prepare(ctx, pool, o.URL); err != nil {
		pool.Close()
		return nil, err
	}
	loopCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	s := &Store{
		pool: pool, env: env,
		held: map[string]string{}, terms: map[string]time.Duration{},
		stop: stop, done: make(chan struct{}),
	}
	go s.renewLoop(loopCtx, renewEvery(o.RenewEvery))
	return s, nil
}

// bound holds the pool inside the ceiling the fleet shares.
func bound(maxConns int32) int32 {
	switch {
	case maxConns <= 0:
		return DefaultMaxConns
	case maxConns > MaxMaxConns:
		return MaxMaxConns
	}
	return maxConns
}

func renewEvery(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultRenewEvery
	}
	return d
}

// prepare reads the schema this database is at, refuses one this binary does
// not know, and applies what is pending.
func prepare(ctx context.Context, pool *pgxpool.Pool, rawURL string) error {
	highest, err := migrations.Highest()
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := guard(ctx, pool, highest); err != nil {
		return err
	}
	dsn, err := migrationURL(rawURL)
	if err != nil {
		return err
	}
	if err := pgxmigrate.Up(dsn, migrations.FS, "."); err != nil {
		return fmt.Errorf("store: applying the schema: %w", err)
	}
	return nil
}

// guard reads schema_migrations before the migrator runs.
//
// A dirty flag means a migration failed halfway and the schema is in neither
// version; an operator repairs it, and a binary that ran Up over it would
// report the same failure with less information. A version above the newest
// migration this binary carries means an older binary met a newer schema: Up
// reports no change and every statement afterwards runs against columns this
// code does not know.
func guard(ctx context.Context, pool *pgxpool.Pool, highest uint) error {
	var (
		version int64
		dirty   bool
	)
	err := pool.QueryRow(ctx, "select version, dirty from schema_migrations").Scan(&version, &dirty)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil // migrated to nothing yet
	case isUndefinedTable(err):
		return nil // a database with no schema at all
	case err != nil:
		return fmt.Errorf("store: reading the schema version: %w", err)
	case dirty:
		return fmt.Errorf("store: the schema is dirty at version %d: a migration failed halfway and an operator repairs it before this process starts", version)
	case version > int64(highest):
		return fmt.Errorf("store: the schema is at version %d and this binary carries %d: run the version of cellad that wrote the schema, or migrate down to %d", version, highest, highest)
	}
	return nil
}

// isUndefinedTable reports the one Postgres code that means a fresh database.
func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// migrationURL is the store's URL under the scheme golang-migrate's pgx/v5
// driver registers. pgxmigrate imports no driver, so the scheme is what
// selects one.
func migrationURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("store: CELLA_DB_URL is not a URL: %w", err)
	}
	if !strings.HasPrefix(u.Scheme, "postgres") {
		return "", fmt.Errorf("store: CELLA_DB_URL has the scheme %q, not postgres", u.Scheme)
	}
	u.Scheme = "pgx5"
	return u.String(), nil
}

// Tx runs fn in one transaction and commits it when fn returns nil.
func (s *Store) Tx(ctx context.Context, fn func(store.Tx) error) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: beginning a transaction: %w", err)
	}
	if err := fn(&txn{q: tx, env: s.env, store: s}); err != nil {
		if rollback := tx.Rollback(ctx); rollback != nil && !errors.Is(rollback, pgx.ErrTxClosed) {
			return errors.Join(err, fmt.Errorf("store: rolling back: %w", rollback))
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: committing: %w", err)
	}
	return nil
}

// Durable is true: this is the store recovery of design 005 needs.
func (s *Store) Durable() bool { return true }

// Ready is the readiness check of design 002, inside its own budget so a
// database that stopped answering fails the probe rather than holding it.
func (s *Store) Ready(ctx context.Context) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	ctx, cancel := context.WithTimeout(ctx, readyBudget)
	defer cancel()
	var one int
	if err := s.pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// Close stops the renewal, releases every lease this process holds so another
// replica takes over at once rather than after the term, and closes the pool.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	held := make(map[string]string, len(s.held))
	maps.Copy(held, s.held)
	s.mu.Unlock()

	s.stop()
	<-s.done

	ctx, cancel := context.WithTimeout(context.Background(), readyBudget)
	defer cancel()
	var failed error
	for name, holder := range held {
		if _, err := s.pool.Exec(ctx, releaseLease, name, holder); err != nil {
			failed = errors.Join(failed, fmt.Errorf("store: releasing the lease %s: %w", name, err))
		}
	}
	s.pool.Close()
	return failed
}

// renewLoop renews every lease this process holds. A lease another replica
// took is forgotten rather than retried: the next Acquire reports it is not
// held, which is what stops the loop that asked for it.
func (s *Store) renewLoop(ctx context.Context, every time.Duration) {
	defer close(s.done)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.renewHeld(ctx)
		}
	}
}

func (s *Store) renewHeld(ctx context.Context) {
	s.mu.Lock()
	held := make(map[string]string, len(s.held))
	terms := make(map[string]time.Duration, len(s.terms))
	for name, holder := range s.held {
		held[name] = holder
		terms[name] = s.terms[name]
	}
	s.mu.Unlock()
	for name, holder := range held {
		tag, err := s.pool.Exec(ctx, acquireLease, name, holder, terms[name].Seconds())
		if err != nil || tag.RowsAffected() == 0 {
			s.forget(name)
		}
	}
}

// remember registers a lease for renewal, and forget drops one.
func (s *Store) remember(name, holder string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[name] = holder
	s.terms[name] = ttl
}

func (s *Store) forget(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.held, name)
	delete(s.terms, name)
}
