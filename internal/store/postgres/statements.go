// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"latere.ai/x/cella/internal/store"
	driver "latere.ai/x/cella/runtime"
)

// querier is what a statement runs on: the pool outside a transaction and the
// transaction inside one.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// txn is the method sets of design 010 over one transaction.
type txn struct {
	q     querier
	env   store.Envelope
	store *Store
	// appended is every journal row this transaction wrote, which Tx
	// publishes once the commit succeeded.
	appended []store.Event
}

func (t *txn) Desired() store.Desired   { return desired{t.q} }
func (t *txn) Observed() store.Observed { return observed{t.q} }
func (t *txn) Journal() store.Journal   { return journal{t.q, &t.appended} }
func (t *txn) Values() store.Values     { return values{t.q, t.env} }
func (t *txn) Leases() store.Leases     { return leases{t.q, t.store} }

func (t *txn) Revocations() store.Revocations { return revocations{t.q} }
func (t *txn) Keys() store.Keys               { return keys{t.q} }
func (t *txn) Ledger() store.Ledger           { return ledger{t.q} }
func (t *txn) Operations() store.Operations   { return operations{t.q} }

// The constraints a write can violate, and what each one means to a caller.
const (
	uniqueViolation  = "23505"
	objectPrimaryKey = "objects_pkey"
	objectLiveName   = "objects_live_name"
)

const objectColumns = `id, kind, owner, name, environment, phase, labels, data, status, last_applied, version, created_at, updated_at`

type desired struct{ q querier }

func (x desired) Put(ctx context.Context, obj store.Object, ifVersion int64) (int64, error) {
	labels, err := labelsText(obj.Labels)
	if err != nil {
		return 0, err
	}
	data, status := jsonText(obj.Data), jsonText(obj.Status)
	if ifVersion == 0 {
		var version int64
		err := x.q.QueryRow(ctx, `insert into objects (id, kind, owner, name, environment, phase, labels, data, status)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9) returning version`,
			obj.ID, obj.Kind, obj.Owner, obj.Name, obj.Environment, obj.Phase, labels, data, status).Scan(&version)
		if err != nil {
			return 0, writeError(err, "creating the object")
		}
		return version, nil
	}
	var version int64
	err = x.q.QueryRow(ctx, `update objects set owner = $3, name = $4, environment = $5, phase = $6,
			labels = $7, data = $8, status = $9, version = version + 1, updated_at = now(), deleted_at = null
		where id = $1 and kind = $2 and version = $10 returning version`,
		obj.ID, obj.Kind, obj.Owner, obj.Name, obj.Environment, obj.Phase, labels, data, status, ifVersion).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, x.whyNoRow(ctx, obj.Kind, obj.ID)
	}
	if err != nil {
		return 0, writeError(err, "writing the object")
	}
	return version, nil
}

// whyNoRow tells a conditional write that matched nothing from a row that is
// not there at all, which are different answers to the caller.
func (x desired) whyNoRow(ctx context.Context, kind, id string) error {
	var version int64
	err := x.q.QueryRow(ctx, `select version from objects where id = $1 and kind = $2`, id, kind).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: reading the object's version: %w", err)
	}
	return store.ErrVersionConflict
}

func (x desired) Get(ctx context.Context, kind, id string) (store.Object, error) {
	row := x.q.QueryRow(ctx, `select `+objectColumns+` from objects where id = $1 and kind = $2 and deleted_at is null`, id, kind)
	return scanObject(row)
}

func (x desired) ByName(ctx context.Context, kind, owner, name string) (store.Object, error) {
	row := x.q.QueryRow(ctx, `select `+objectColumns+` from objects
		where kind = $1 and owner = $2 and name = $3 and deleted_at is null`, kind, owner, name)
	return scanObject(row)
}

func (x desired) List(ctx context.Context, kind string, f store.Filter, p store.Page) ([]store.Object, string, error) {
	where := []string{`kind = $1`, `deleted_at is null`}
	args := []any{kind}
	where, args, err := narrow(where, args, f, p.Cursor)
	if err != nil {
		return nil, "", err
	}
	args = append(args, p.Size()+1)
	query := `select ` + objectColumns + ` from objects where ` + strings.Join(where, " and ") +
		` order by id limit $` + strconv.Itoa(len(args))
	rows, err := x.q.Query(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("store: listing objects: %w", err)
	}
	defer rows.Close()
	var out []store.Object
	for rows.Next() {
		obj, err := scanObject(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, obj)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: reading the objects: %w", err)
	}
	out, next := store.PageOf(out, p, func(o store.Object) string { return o.ID })
	return out, next, nil
}

func (x desired) Delete(ctx context.Context, kind, id string) error {
	tag, err := x.q.Exec(ctx, `update objects set deleted_at = now(), updated_at = now(), version = version + 1
		where id = $1 and kind = $2 and deleted_at is null`, id, kind)
	if err != nil {
		return fmt.Errorf("store: deleting the object: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (x desired) Count(ctx context.Context, kind, owner string) (int, error) {
	var n int
	err := x.q.QueryRow(ctx, `select count(*) from objects
		where kind = $1 and owner = $2 and deleted_at is null and phase <> $3`, kind, owner, store.PhaseDeleting).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting the owner's objects: %w", err)
	}
	return n, nil
}

func (x desired) PutStatus(ctx context.Context, kind, id string, status []byte) error {
	tag, err := x.q.Exec(ctx, `update objects set status = $3, updated_at = now()
		where id = $1 and kind = $2 and deleted_at is null`, id, kind, jsonText(status))
	if err != nil {
		return fmt.Errorf("store: writing the status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (x desired) LastApplied(ctx context.Context, id string) ([]byte, error) {
	var applied []byte
	err := x.q.QueryRow(ctx, `select last_applied from objects where id = $1`, id).Scan(&applied)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading the last applied state: %w", err)
	}
	return applied, nil
}

func (x desired) SetLastApplied(ctx context.Context, id string, data []byte) error {
	tag, err := x.q.Exec(ctx, `update objects set last_applied = $2, updated_at = now() where id = $1`, id, jsonText(data))
	if err != nil {
		return fmt.Errorf("store: writing the last applied state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

type observed struct{ q querier }

const upsertObserved = `insert into observed (id, environment, owner, phase, labels, state, updated_at)
	values ($1, $2, $3, $4, $5, $6, now())
	on conflict (id) do update set environment = excluded.environment, owner = excluded.owner,
		phase = excluded.phase, labels = excluded.labels, state = excluded.state, updated_at = now()`

func (x observed) Put(ctx context.Context, environment string, s driver.State) error {
	body, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("store: encoding the observed state: %w", err)
	}
	labels, err := labelsText(s.Labels)
	if err != nil {
		return err
	}
	if _, err := x.q.Exec(ctx, upsertObserved, s.ID, environment, s.Owner, s.Phase, labels, string(body)); err != nil {
		return fmt.Errorf("store: writing the observed state: %w", err)
	}
	return nil
}

func (x observed) Get(ctx context.Context, id string) (driver.State, string, error) {
	var (
		environment string
		body        []byte
	)
	err := x.q.QueryRow(ctx, `select environment, state from observed where id = $1`, id).Scan(&environment, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return driver.State{}, "", store.ErrNotFound
	}
	if err != nil {
		return driver.State{}, "", fmt.Errorf("store: reading the observed state: %w", err)
	}
	var state driver.State
	if err := json.Unmarshal(body, &state); err != nil {
		return driver.State{}, "", fmt.Errorf("store: decoding the observed state: %w", err)
	}
	return state, environment, nil
}

func (x observed) List(ctx context.Context, f store.Filter, p store.Page) ([]driver.State, string, error) {
	where := []string{`true`}
	where, args, err := narrow(where, nil, f, p.Cursor)
	if err != nil {
		return nil, "", err
	}
	args = append(args, p.Size()+1)
	query := `select state from observed where ` + strings.Join(where, " and ") +
		` order by id limit $` + strconv.Itoa(len(args))
	rows, err := x.q.Query(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("store: listing the observed states: %w", err)
	}
	defer rows.Close()
	var out []driver.State
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, "", fmt.Errorf("store: reading an observed state: %w", err)
		}
		var state driver.State
		if err := json.Unmarshal(body, &state); err != nil {
			return nil, "", fmt.Errorf("store: decoding an observed state: %w", err)
		}
		out = append(out, state)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: reading the observed states: %w", err)
	}
	out, next := store.PageOf(out, p, func(s driver.State) string { return s.ID })
	return out, next, nil
}

func (x observed) Rebuild(ctx context.Context, environment string, states []driver.State) error {
	if _, err := x.q.Exec(ctx, `delete from observed where environment = $1`, environment); err != nil {
		return fmt.Errorf("store: clearing the environment's observed rows: %w", err)
	}
	for _, s := range states {
		if err := x.Put(ctx, environment, s); err != nil {
			return err
		}
	}
	return nil
}

type journal struct {
	q        querier
	appended *[]store.Event
}

func (x journal) Append(ctx context.Context, e store.Event) (int64, error) {
	if e.ObjectID == "" || e.Type == "" {
		return 0, errors.New("store: an event carries an object and a type")
	}
	if e.ID == "" {
		e.ID = store.EventID()
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	var seq int64
	err := x.q.QueryRow(ctx, `insert into events (id, object_id, seq, type, at, payload, acked_at)
		select $1, $2, coalesce(max(seq), 0) + 1, $3, $4, $5, $6 from events where object_id = $2
		returning seq`, e.ID, e.ObjectID, e.Type, e.At, jsonText(e.Payload), nullTime(e.AckedAt)).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("store: appending to the journal: %w", err)
	}
	e.Seq = seq
	*x.appended = append(*x.appended, e)
	return seq, nil
}

// Pending takes each object's lowest unfinished sequence in a subquery and
// filters by the due time outside it. Filtering first would let sequence two
// overtake a deferred sequence one, which is the one ordering the contract
// promises.
func (x journal) Pending(ctx context.Context, limit int, now time.Time) ([]store.Event, error) {
	if limit <= 0 {
		limit = store.DefaultPageLimit
	}
	rows, err := x.q.Query(ctx, `select id, object_id, seq, type, at, payload, attempts,
			coalesce(next_attempt_at, to_timestamp(0))
		from (
			select distinct on (object_id) id, object_id, seq, type, at, payload, attempts, next_attempt_at
			from events where acked_at is null and dropped_at is null
			order by object_id, seq
		) head
		where next_attempt_at is null or next_attempt_at <= $1
		order by id limit $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("store: reading what the journal has pending: %w", err)
	}
	defer rows.Close()
	var out []store.Event
	for rows.Next() {
		var e store.Event
		if err := rows.Scan(&e.ID, &e.ObjectID, &e.Seq, &e.Type, &e.At, &e.Payload,
			&e.Attempts, &e.NextAttemptAt); err != nil {
			return nil, fmt.Errorf("store: reading a pending journal row: %w", err)
		}
		if e.NextAttemptAt.Unix() == 0 {
			e.NextAttemptAt = time.Time{}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading what the journal has pending: %w", err)
	}
	return out, nil
}

func (x journal) Undelivered(ctx context.Context) (int, error) {
	var n int
	err := x.q.QueryRow(ctx,
		`select count(*) from events where acked_at is null and dropped_at is null`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting what the journal has undelivered: %w", err)
	}
	return n, nil
}

func (x journal) Acknowledge(ctx context.Context, id string, at time.Time) error {
	return x.finish(ctx, `update events set acked_at = $2 where id = $1`, id, at)
}

func (x journal) Drop(ctx context.Context, id string, at time.Time) error {
	return x.finish(ctx, `update events set dropped_at = $2 where id = $1`, id, at)
}

func (x journal) Defer(ctx context.Context, id string, next time.Time) error {
	return x.finish(ctx, `update events set attempts = attempts + 1, next_attempt_at = $2 where id = $1`, id, next)
}

// finish applies one delivery outcome. An id no row holds is ErrNotFound: the
// caller read it from Pending, so its absence is a fact worth reporting
// rather than a write that quietly did nothing.
func (x journal) finish(ctx context.Context, statement, id string, at time.Time) error {
	tag, err := x.q.Exec(ctx, statement, id, at.UTC())
	if err != nil {
		return fmt.Errorf("store: recording a delivery outcome: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// nullTime is a zero time as SQL null, so an unset column stays unset.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func (x journal) ByObject(ctx context.Context, objectID string, p store.Page) ([]store.Event, string, error) {
	cursor := int64(0)
	if p.Cursor != "" {
		parsed, err := strconv.ParseInt(p.Cursor, 10, 64)
		if err != nil {
			return nil, "", errors.New("store: the page cursor is not a sequence")
		}
		cursor = parsed
	}
	rows, err := x.q.Query(ctx, `select id, object_id, seq, type, at, payload from events
		where object_id = $1 and ($2 = 0 or seq < $2) order by seq desc limit $3`,
		objectID, cursor, p.Size()+1)
	if err != nil {
		return nil, "", fmt.Errorf("store: reading the journal: %w", err)
	}
	defer rows.Close()
	var out []store.Event
	for rows.Next() {
		var e store.Event
		if err := rows.Scan(&e.ID, &e.ObjectID, &e.Seq, &e.Type, &e.At, &e.Payload); err != nil {
			return nil, "", fmt.Errorf("store: reading a journal row: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: reading the journal: %w", err)
	}
	out, next := store.PageOf(out, p, func(e store.Event) string {
		return strconv.FormatInt(e.Seq, 10)
	})
	return out, next, nil
}

// After reads one object's rows above seq, oldest first, over the unique
// index on (object_id, seq).
func (x journal) After(ctx context.Context, objectID string, seq int64, limit int) ([]store.Event, error) {
	rows, err := x.q.Query(ctx, `select id, object_id, seq, type, at, payload from events
		where object_id = $1 and seq > $2 order by seq limit $3`,
		objectID, seq, (store.Page{Limit: limit}).Size())
	if err != nil {
		return nil, fmt.Errorf("store: reading the journal: %w", err)
	}
	defer rows.Close()
	var out []store.Event
	for rows.Next() {
		var e store.Event
		if err := rows.Scan(&e.ID, &e.ObjectID, &e.Seq, &e.Type, &e.At, &e.Payload); err != nil {
			return nil, fmt.Errorf("store: reading a journal row: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading the journal: %w", err)
	}
	return out, nil
}

// Prune forgets finished rows only. An event still waiting for the sink is
// older than the retention long before it is undeliverable, and design 009
// decides when it is given up, not the retention.
//
// Each object's newest row stays whatever its age. Append takes an object's
// next sequence from the rows this table holds, so a prune that took every
// row of an object would start it again at one and hand a reader a number it
// has already seen.
func (x journal) Prune(ctx context.Context, before time.Time) (int, error) {
	tag, err := x.q.Exec(ctx, `delete from events e
		where e.at < $1 and (e.acked_at is not null or e.dropped_at is not null)
			and e.seq < (select max(m.seq) from events m where m.object_id = e.object_id)`, before)
	if err != nil {
		return 0, fmt.Errorf("store: pruning the journal: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

type values struct {
	q   querier
	env store.Envelope
}

func (x values) Put(ctx context.Context, secretID string, plaintext []byte) (int, error) {
	wrapped, sealed, err := x.env.Seal(plaintext)
	if err != nil {
		return 0, err
	}
	var version int
	err = x.q.QueryRow(ctx, `insert into secret_values (secret_id, version, wrapped_key, ciphertext)
		values ($1, 1, $2, $3)
		on conflict (secret_id) do update set version = secret_values.version + 1,
			wrapped_key = excluded.wrapped_key, ciphertext = excluded.ciphertext, updated_at = now()
		returning version`, secretID, wrapped, sealed).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("store: writing the secret value: %w", err)
	}
	return version, nil
}

func (x values) Open(ctx context.Context, secretID string) ([]byte, int, error) {
	var (
		version         int
		wrapped, sealed []byte
	)
	err := x.q.QueryRow(ctx, `select version, wrapped_key, ciphertext from secret_values where secret_id = $1`,
		secretID).Scan(&version, &wrapped, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, store.ErrNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("store: reading the secret value: %w", err)
	}
	plaintext, err := x.env.Open(wrapped, sealed)
	if err != nil {
		return nil, 0, err
	}
	return plaintext, version, nil
}

func (x values) Delete(ctx context.Context, secretID string) error {
	tag, err := x.q.Exec(ctx, `delete from secret_values where secret_id = $1`, secretID)
	if err != nil {
		return fmt.Errorf("store: deleting the secret value: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// Rewrap reads every row's wrapped data key, moves it to the new key, and
// writes the one column back. The ciphertext column is neither read nor
// written, so a rotation moves no value and produces no plaintext beyond the
// data key it is rewrapping.
func (x values) Rewrap(ctx context.Context, oldKEK, newKEK []byte) (int, error) {
	old, next, err := store.RewrapKeys(oldKEK, newKEK)
	if err != nil {
		return 0, err
	}
	rows, err := x.q.Query(ctx, `select secret_id, wrapped_key from secret_values order by secret_id`)
	if err != nil {
		return 0, fmt.Errorf("store: reading the secret values to rewrap: %w", err)
	}
	type rewrapped struct {
		id      string
		wrapped []byte
	}
	var out []rewrapped
	for rows.Next() {
		var row rewrapped
		if err = rows.Scan(&row.id, &row.wrapped); err != nil {
			rows.Close()
			return 0, err
		}
		if row.wrapped, err = store.RewrapKey(old, next, row.wrapped); err != nil {
			rows.Close()
			return 0, err
		}
		out = append(out, row)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	for _, row := range out {
		if _, err = x.q.Exec(ctx, `update secret_values set wrapped_key = $2 where secret_id = $1`, row.id, row.wrapped); err != nil {
			return 0, fmt.Errorf("store: rewrapping the secret value: %w", err)
		}
	}
	x.env.Adopt(next)
	return len(out), nil
}

// The lease statements, which the renewal loop runs as well as this method
// set, so the conditional upsert has one statement in one place.
const (
	acquireLease = `insert into leases (name, holder, expires_at)
		values ($1, $2, now() + make_interval(secs => $3))
		on conflict (name) do update set holder = excluded.holder, expires_at = excluded.expires_at
		where leases.holder = excluded.holder or leases.expires_at < now()`
	releaseLease = `delete from leases where name = $1 and holder = $2`
)

type leases struct {
	q     querier
	store *Store
}

func (x leases) Acquire(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	tag, err := x.q.Exec(ctx, acquireLease, name, holder, ttl.Seconds())
	if err != nil {
		return false, fmt.Errorf("store: acquiring the lease %s: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	x.store.remember(name, holder, ttl)
	return true, nil
}

func (x leases) Release(ctx context.Context, name, holder string) error {
	x.store.forget(name)
	if _, err := x.q.Exec(ctx, releaseLease, name, holder); err != nil {
		return fmt.Errorf("store: releasing the lease %s: %w", name, err)
	}
	return nil
}

// narrow turns a filter and a page cursor into conditions over the columns
// both objects and observed carry under the same names.
func narrow(where []string, args []any, f store.Filter, cursor string) ([]string, []any, error) {
	add := func(condition string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(condition, len(args)))
	}
	if f.Owner != "" {
		add("owner = $%d", f.Owner)
	}
	if f.Phase != "" {
		add("phase = $%d", f.Phase)
	}
	if f.Environment != "" {
		add("environment = $%d", f.Environment)
	}
	if len(f.Labels) > 0 {
		labels, err := labelsText(f.Labels)
		if err != nil {
			return nil, nil, err
		}
		add("labels @> $%d", labels)
	}
	if len(f.IDs) > 0 {
		add("id = any($%d)", f.IDs)
	}
	if cursor != "" {
		add("id > $%d", cursor)
	}
	return where, args, nil
}

// scanObject reads one objects row.
func scanObject(row pgx.Row) (store.Object, error) {
	var obj store.Object
	err := row.Scan(&obj.ID, &obj.Kind, &obj.Owner, &obj.Name, &obj.Environment, &obj.Phase,
		&obj.Labels, &obj.Data, &obj.Status, &obj.LastApplied, &obj.Version, &obj.CreatedAt, &obj.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Object{}, store.ErrNotFound
	}
	if err != nil {
		return store.Object{}, fmt.Errorf("store: reading the object: %w", err)
	}
	return obj, nil
}

// writeError turns the constraints of the objects table into the answers
// design 008 gives a caller: a name another live row holds, and a row that
// moved since it was read.
func writeError(err error, what string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		switch pgErr.ConstraintName {
		case objectLiveName:
			return store.ErrNameTaken
		case objectPrimaryKey:
			return store.ErrVersionConflict
		}
	}
	return fmt.Errorf("store: %s: %w", what, err)
}

// revocations is the list a verifier asks before it trusts a token cellad
// minted: one row per jti, against the exp the token carried.
type revocations struct{ q querier }

func (x revocations) Revoke(ctx context.Context, jti string, exp time.Time) error {
	if jti == "" {
		return errors.New("store: a revocation names a jti")
	}
	// A revocation is idempotent, because a rotation or a recovery that
	// retried revokes a jti it already revoked. The later exp wins, so the
	// row outlives every token that could present it.
	_, err := x.q.Exec(ctx, `insert into revocations (jti, exp) values ($1, $2)
		on conflict (jti) do update set exp = greatest(revocations.exp, excluded.exp)`, jti, exp.UTC())
	if err != nil {
		return fmt.Errorf("store: revoking %s: %w", jti, err)
	}
	return nil
}

func (x revocations) Revoked(ctx context.Context, jti string) (bool, error) {
	var found bool
	err := x.q.QueryRow(ctx, `select exists (select 1 from revocations where jti = $1)`, jti).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("store: reading the revocation of %s: %w", jti, err)
	}
	return found, nil
}

func (x revocations) Forget(ctx context.Context, before time.Time) (int, error) {
	tag, err := x.q.Exec(ctx, `delete from revocations where exp < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("store: forgetting expired revocations: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// keys is the registry of environment keys: one row per minted key, by jti,
// in the table environment_keys.
type keys struct{ q querier }

func (x keys) Record(ctx context.Context, k store.Key) error {
	if k.JTI == "" || k.Environment == "" {
		return errors.New("store: a key record names its jti and its environment")
	}
	var revoked *time.Time
	if !k.RevokedAt.IsZero() {
		at := k.RevokedAt.UTC()
		revoked = &at
	}
	_, err := x.q.Exec(ctx, `insert into environment_keys (jti, environment, subject, minted_at, expires_at, revoked_at)
		values ($1, $2, $3, $4, $5, $6)`,
		k.JTI, k.Environment, k.Subject, k.MintedAt.UTC(), k.ExpiresAt.UTC(), revoked)
	if err != nil {
		return fmt.Errorf("store: recording the key %s: %w", k.JTI, err)
	}
	return nil
}

func (x keys) Revoke(ctx context.Context, jti string, at time.Time) error {
	// The earliest mark is kept, so a revocation that retried does not move
	// the instant a console shows.
	_, err := x.q.Exec(ctx, `update environment_keys set revoked_at = $2
		where jti = $1 and (revoked_at is null or revoked_at > $2)`, jti, at.UTC())
	if err != nil {
		return fmt.Errorf("store: marking the key %s revoked: %w", jti, err)
	}
	return nil
}

func (x keys) List(ctx context.Context, environment string, p store.Page) ([]store.Key, string, error) {
	rows, err := x.q.Query(ctx, `select jti, environment, subject, minted_at, expires_at, revoked_at
		from environment_keys where environment = $1 and jti > $2 order by jti limit $3`,
		environment, p.Cursor, p.Size()+1)
	if err != nil {
		return nil, "", fmt.Errorf("store: listing the keys of %s: %w", environment, err)
	}
	defer rows.Close()
	var out []store.Key
	for rows.Next() {
		var (
			k       store.Key
			revoked *time.Time
		)
		if err := rows.Scan(&k.JTI, &k.Environment, &k.Subject, &k.MintedAt, &k.ExpiresAt, &revoked); err != nil {
			return nil, "", fmt.Errorf("store: reading a key of %s: %w", environment, err)
		}
		if revoked != nil {
			k.RevokedAt = revoked.UTC()
		}
		k.MintedAt, k.ExpiresAt = k.MintedAt.UTC(), k.ExpiresAt.UTC()
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("store: listing the keys of %s: %w", environment, err)
	}
	page, next := store.PageOf(out, p, func(k store.Key) string { return k.JTI })
	return page, next, nil
}

func (x keys) Forget(ctx context.Context, before time.Time) (int, error) {
	tag, err := x.q.Exec(ctx, `delete from environment_keys where expires_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("store: forgetting expired keys: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ledger is the spawn budget of design 022: one row per sandbox holding how
// many children it has created in total. The budget is not a column, so the
// debit carries the number it is conditional on.
type ledger struct{ q querier }

// Debit inserts the first child's row and raises every later one, both under
// the same condition, so two concurrent debits at one remaining unit yield
// one success. The insert conflicts on the primary key and the update's where
// clause decides; a statement that changed no row found the budget spent.
func (x ledger) Debit(ctx context.Context, parentID string, budget int) error {
	if parentID == "" {
		return errors.New("store: a debit names a sandbox")
	}
	if budget <= 0 {
		return store.ErrBudgetExhausted
	}
	tag, err := x.q.Exec(ctx, `insert into ledger (sandbox_id, used) values ($1, 1)
		on conflict (sandbox_id) do update set used = ledger.used + 1, updated_at = now()
		where ledger.used < $2`, parentID, budget)
	if err != nil {
		return fmt.Errorf("store: debiting the spawn budget of %s: %w", parentID, err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrBudgetExhausted
	}
	return nil
}

func (x ledger) Credit(ctx context.Context, parentID string) error {
	_, err := x.q.Exec(ctx, `update ledger set used = used - 1, updated_at = now()
		where sandbox_id = $1 and used > 0`, parentID)
	if err != nil {
		return fmt.Errorf("store: crediting the spawn budget of %s: %w", parentID, err)
	}
	return nil
}

func (x ledger) Used(ctx context.Context, parentID string) (int, error) {
	var used int
	err := x.q.QueryRow(ctx, `select coalesce(max(used), 0) from ledger where sandbox_id = $1`, parentID).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("store: reading the spawn budget of %s: %w", parentID, err)
	}
	return used, nil
}

func (x ledger) Forget(ctx context.Context, parentID string) error {
	if _, err := x.q.Exec(ctx, `delete from ledger where sandbox_id = $1`, parentID); err != nil {
		return fmt.Errorf("store: forgetting the spawn ledger of %s: %w", parentID, err)
	}
	return nil
}

const operationColumns = `id, environment, sandbox_id, type, payload, state, claimed_by, claimed_at, attempts, result, created_at`

// operations is the worker queue of design 021: the rows a worker claims
// from, and the registrations an environment's phase is computed from. The
// claim is one statement, so two replicas handing work to two workers cannot
// hand the same row to both.
type operations struct{ q querier }

func (x operations) Enqueue(ctx context.Context, op store.Operation) error {
	if op.ID == "" || op.Environment == "" || op.Type == "" {
		return errors.New("store: an operation names an id, an environment and a type")
	}
	if op.State == "" {
		op.State = store.OperationQueued
	}
	if op.CreatedAt.IsZero() {
		op.CreatedAt = time.Now().UTC()
	}
	_, err := x.q.Exec(ctx, `insert into operations
		(id, environment, sandbox_id, type, payload, state, attempts, created_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8)`,
		op.ID, op.Environment, op.SandboxID, op.Type, jsonText(op.Payload), op.State, op.Attempts, op.CreatedAt.UTC())
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == uniqueViolation {
			return store.ErrVersionConflict
		}
		return fmt.Errorf("store: enqueueing operation %s: %w", op.ID, err)
	}
	return nil
}

// Claim takes rows nobody holds and rows whose holder stopped heartbeating,
// in one statement with the rows locked, so a row is claimed once however
// many replicas claim at the same instant.
func (x operations) Claim(ctx context.Context, environment, worker string, n int, lapsed time.Time) ([]store.Operation, error) {
	if environment == "" || worker == "" {
		return nil, errors.New("store: a claim names an environment and a worker")
	}
	if n <= 0 {
		return nil, nil
	}
	rows, err := x.q.Query(ctx, `update operations set
			state = 'claimed', claimed_by = $2, claimed_at = now(), attempts = attempts + 1
		where id in (
			select o.id from operations o
			left join workers w on w.environment = o.environment and w.worker = o.claimed_by
			where o.environment = $1 and (
				o.state = 'queued'
				or (o.state = 'claimed' and o.claimed_by <> $2
					and (w.worker is null or w.last_heartbeat < $3)))
			order by o.created_at, o.id
			limit $4
			for update of o skip locked)
		returning `+operationColumns, environment, worker, lapsed.UTC(), n)
	if err != nil {
		return nil, fmt.Errorf("store: claiming operations of %s: %w", environment, err)
	}
	defer rows.Close()
	out := []store.Operation{}
	for rows.Next() {
		op, scanErr := scanOperation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, op)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("store: claiming operations of %s: %w", environment, err)
	}
	// returning has no order of its own, and a worker executes what it
	// claimed in the order the queue holds it.
	slices.SortFunc(out, func(a, b store.Operation) int {
		if a.CreatedAt.Equal(b.CreatedAt) {
			return strings.Compare(a.ID, b.ID)
		}
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	return out, nil
}

func (x operations) Acknowledge(ctx context.Context, opID string, result []byte) error {
	tag, err := x.q.Exec(ctx, `update operations set state = 'done', result = $2
		where id = $1 and state <> 'done'`, opID, jsonText(result))
	if err != nil {
		return fmt.Errorf("store: acknowledging operation %s: %w", opID, err)
	}
	if tag.RowsAffected() == 0 {
		// Either the row is gone, or it is already done and the answer the
		// caller was given stands.
		var found bool
		if err = x.q.QueryRow(ctx, `select exists (select 1 from operations where id = $1)`, opID).Scan(&found); err != nil {
			return fmt.Errorf("store: acknowledging operation %s: %w", opID, err)
		}
		if !found {
			return store.ErrNotFound
		}
	}
	return nil
}

func (x operations) Get(ctx context.Context, opID string) (store.Operation, error) {
	op, err := scanOperation(x.q.QueryRow(ctx, `select `+operationColumns+` from operations where id = $1`, opID))
	if err != nil {
		return store.Operation{}, err
	}
	return op, nil
}

func (x operations) Register(ctx context.Context, w store.Worker) error {
	if w.Environment == "" || w.Worker == "" {
		return errors.New("store: a registration names an environment and a worker")
	}
	if w.LastHeartbeat.IsZero() {
		w.LastHeartbeat = time.Now()
	}
	_, err := x.q.Exec(ctx, `insert into workers (environment, worker, replica, last_heartbeat)
		values ($1, $2, $3, $4)
		on conflict (environment, worker) do update
		set replica = excluded.replica, last_heartbeat = excluded.last_heartbeat`,
		w.Environment, w.Worker, w.Replica, w.LastHeartbeat.UTC())
	if err != nil {
		return fmt.Errorf("store: registering worker %s: %w", w.Worker, err)
	}
	return nil
}

func (x operations) Heartbeat(ctx context.Context, environment, worker string, at time.Time) error {
	tag, err := x.q.Exec(ctx, `update workers set last_heartbeat = $3
		where environment = $1 and worker = $2`, environment, worker, at.UTC())
	if err != nil {
		return fmt.Errorf("store: stamping worker %s: %w", worker, err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (x operations) Workers(ctx context.Context, environment string) ([]store.Worker, error) {
	rows, err := x.q.Query(ctx, `select environment, worker, replica, last_heartbeat
		from workers where environment = $1 order by worker`, environment)
	if err != nil {
		return nil, fmt.Errorf("store: reading the workers of %s: %w", environment, err)
	}
	defer rows.Close()
	out := []store.Worker{}
	for rows.Next() {
		var w store.Worker
		if err = rows.Scan(&w.Environment, &w.Worker, &w.Replica, &w.LastHeartbeat); err != nil {
			return nil, fmt.Errorf("store: reading the workers of %s: %w", environment, err)
		}
		w.LastHeartbeat = w.LastHeartbeat.UTC()
		out = append(out, w)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading the workers of %s: %w", environment, err)
	}
	return out, nil
}

func (x operations) Forget(ctx context.Context, environment, worker string) error {
	_, err := x.q.Exec(ctx, `delete from workers where environment = $1 and worker = $2`, environment, worker)
	if err != nil {
		return fmt.Errorf("store: forgetting worker %s: %w", worker, err)
	}
	return nil
}

func (x operations) Prune(ctx context.Context, before time.Time) (int, error) {
	tag, err := x.q.Exec(ctx, `delete from operations where state = 'done' and created_at < $1`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("store: forgetting finished operations: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func scanOperation(row pgx.Row) (store.Operation, error) {
	var (
		op        store.Operation
		claimedBy *string
		claimedAt *time.Time
	)
	err := row.Scan(&op.ID, &op.Environment, &op.SandboxID, &op.Type, &op.Payload,
		&op.State, &claimedBy, &claimedAt, &op.Attempts, &op.Result, &op.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Operation{}, store.ErrNotFound
	}
	if err != nil {
		return store.Operation{}, fmt.Errorf("store: reading an operation: %w", err)
	}
	if claimedBy != nil {
		op.ClaimedBy = *claimedBy
	}
	if claimedAt != nil {
		op.ClaimedAt = claimedAt.UTC()
	}
	op.CreatedAt = op.CreatedAt.UTC()
	return op, nil
}

// The json and jsonb columns take their values as text. A pooled endpoint
// runs in exec mode, where the server describes no parameter and the driver
// encodes each value from its Go type alone and sends it as text: a byte slice
// goes as bytea and reaches the column as a hex literal it refuses, and a Go
// map has no encoding at all. A string is sent as it is in every mode, and the
// server reads it as the column's type.

// jsonText is a JSON document as the text a jsonb parameter takes, and nil,
// SQL null, for an empty one, so a row with no document reads back as none
// rather than as the JSON literal null.
func jsonText(raw []byte) *string {
	if len(raw) == 0 {
		return nil
	}
	text := string(raw)
	return &text
}

// labelsText is a label set as the text of a jsonb object: the empty object
// for none, which is what a labels column holds for a row without labels.
func labelsText(labels map[string]string) (string, error) {
	if labels == nil {
		return "{}", nil
	}
	b, err := json.Marshal(labels)
	if err != nil {
		return "", fmt.Errorf("store: encoding the labels: %w", err)
	}
	return string(b), nil
}
