// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package memory is the in-process adapter of design 010: maps under one
// mutex, which is the transaction. It is the default store, and the one a
// laptop runs on with nothing beside it.
//
// Nothing it holds outlives the process, so Durable is false: a sandbox the
// data plane loses is reported and reaped after the grace of design 005
// rather than recreated. Secret values are held under the same envelope the
// Postgres adapter uses, so no store keeps a plaintext at rest.
package memory

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"latere.ai/x/cella/internal/store"
	driver "latere.ai/x/cella/runtime"
)

// ErrClosed is every call after Close.
var ErrClosed = errors.New("store: the memory store is closed")

// Options opens the memory store. Key is the 32 byte key secret values are
// sealed under; without one the store serves everything but Values.
type Options struct {
	Key []byte
}

// Store is one control plane's state in memory.
type Store struct {
	mu     sync.Mutex
	env    store.Envelope
	data   *data
	closed bool
}

// Open takes the options and returns a store with nothing in it.
func Open(o Options) (*Store, error) {
	env, err := store.NewEnvelope(o.Key)
	if err != nil {
		return nil, err
	}
	return &Store{env: env, data: newData()}, nil
}

// Tx runs fn under the one mutex. A failed fn restores the snapshot taken
// before it ran, so every write inside commits together or not at all.
func (s *Store) Tx(ctx context.Context, fn func(store.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	before := s.data.clone()
	if err := fn(&txn{d: s.data, env: s.env}); err != nil {
		s.data = before
		return err
	}
	return nil
}

// Durable is false: desired state lives with the process.
func (s *Store) Durable() bool { return false }

// Ready is nil while the store is open.
func (s *Store) Ready(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return nil
}

// Close drops everything the store holds.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.data = newData()
	return nil
}

// data is every map of one store, replaced wholesale when a transaction
// fails.
type data struct {
	objects  map[string]store.Object
	observed map[string]observedRow
	events   map[string][]store.Event
	seq      map[string]int64
	values   map[string]valueRow
	leases   map[string]leaseRow
}

type observedRow struct {
	environment string
	state       driver.State
}

type valueRow struct {
	version int
	wrapped []byte
	sealed  []byte
}

type leaseRow struct {
	holder  string
	expires time.Time
}

func newData() *data {
	return &data{
		objects:  map[string]store.Object{},
		observed: map[string]observedRow{},
		events:   map[string][]store.Event{},
		seq:      map[string]int64{},
		values:   map[string]valueRow{},
		leases:   map[string]leaseRow{},
	}
}

// clone is the transaction's undo: a copy deep enough that a write inside the
// transaction cannot reach the snapshot.
func (d *data) clone() *data {
	n := newData()
	for k, v := range d.objects {
		n.objects[k] = cloneObject(v)
	}
	for k, v := range d.observed {
		n.observed[k] = observedRow{environment: v.environment, state: cloneState(v.state)}
	}
	for k, v := range d.events {
		events := make([]store.Event, len(v))
		for i, e := range v {
			e.Payload = slices.Clone(e.Payload)
			events[i] = e
		}
		n.events[k] = events
	}
	maps.Copy(n.seq, d.seq)
	for k, v := range d.values {
		v.wrapped, v.sealed = slices.Clone(v.wrapped), slices.Clone(v.sealed)
		n.values[k] = v
	}
	maps.Copy(n.leases, d.leases)
	return n
}

func cloneObject(o store.Object) store.Object {
	o.Labels = maps.Clone(o.Labels)
	o.Data = slices.Clone(o.Data)
	o.Status = slices.Clone(o.Status)
	o.LastApplied = slices.Clone(o.LastApplied)
	return o
}

func cloneState(s driver.State) driver.State {
	s.Labels = maps.Clone(s.Labels)
	if s.ExitCode != nil {
		code := *s.ExitCode
		s.ExitCode = &code
	}
	return s
}

// txn is the transaction handed to fn. It assumes the store's mutex is held
// for its whole life, which is why the store has no accessor outside Tx: a
// second lock from inside fn would deadlock.
type txn struct {
	d   *data
	env store.Envelope
}

func (t *txn) Desired() store.Desired   { return desired{t.d} }
func (t *txn) Observed() store.Observed { return observed{t.d} }
func (t *txn) Journal() store.Journal   { return journal{t.d} }
func (t *txn) Values() store.Values     { return values{t.d, t.env} }
func (t *txn) Leases() store.Leases     { return leases{t.d} }

type desired struct{ d *data }

func (x desired) Put(ctx context.Context, obj store.Object, ifVersion int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	current, held := x.d.objects[obj.ID]
	switch {
	case ifVersion == 0 && held:
		return 0, store.ErrVersionConflict
	case ifVersion != 0 && !held:
		return 0, store.ErrNotFound
	case ifVersion != 0 && current.Version != ifVersion:
		return 0, store.ErrVersionConflict
	}
	for id, other := range x.d.objects {
		if id != obj.ID && other.DeletedAt.IsZero() &&
			other.Kind == obj.Kind && other.Owner == obj.Owner && other.Name == obj.Name {
			return 0, store.ErrNameTaken
		}
	}
	now := time.Now().UTC()
	row := cloneObject(obj)
	row.Version = ifVersion + 1
	row.UpdatedAt = now
	row.DeletedAt = time.Time{}
	if held {
		row.CreatedAt = current.CreatedAt
		row.LastApplied = slices.Clone(current.LastApplied)
	} else {
		row.CreatedAt = now
	}
	x.d.objects[obj.ID] = row
	return row.Version, nil
}

func (x desired) Get(ctx context.Context, kind, id string) (store.Object, error) {
	if err := ctx.Err(); err != nil {
		return store.Object{}, err
	}
	row, held := x.d.objects[id]
	if !held || row.Kind != kind || !row.DeletedAt.IsZero() {
		return store.Object{}, store.ErrNotFound
	}
	return cloneObject(row), nil
}

func (x desired) ByName(ctx context.Context, kind, owner, name string) (store.Object, error) {
	if err := ctx.Err(); err != nil {
		return store.Object{}, err
	}
	for _, row := range x.d.objects {
		if row.Kind == kind && row.Owner == owner && row.Name == name && row.DeletedAt.IsZero() {
			return cloneObject(row), nil
		}
	}
	return store.Object{}, store.ErrNotFound
}

func (x desired) List(ctx context.Context, kind string, f store.Filter, p store.Page) ([]store.Object, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	var rows []store.Object
	for _, row := range x.d.objects {
		if row.Kind != kind || !row.DeletedAt.IsZero() || row.ID <= p.Cursor {
			continue
		}
		if !matches(f, row.Owner, row.Phase, row.Environment, row.ID, row.Labels) {
			continue
		}
		rows = append(rows, cloneObject(row))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	rows, next := store.PageOf(rows, p, func(o store.Object) string { return o.ID })
	return rows, next, nil
}

func (x desired) Delete(ctx context.Context, kind, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	row, held := x.d.objects[id]
	if !held || row.Kind != kind || !row.DeletedAt.IsZero() {
		return store.ErrNotFound
	}
	row.DeletedAt = time.Now().UTC()
	row.UpdatedAt = row.DeletedAt
	row.Version++
	x.d.objects[id] = row
	return nil
}

func (x desired) Count(ctx context.Context, kind, owner string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, row := range x.d.objects {
		if row.Kind == kind && row.Owner == owner && row.DeletedAt.IsZero() && row.Phase != store.PhaseDeleting {
			n++
		}
	}
	return n, nil
}

func (x desired) PutStatus(ctx context.Context, kind, id string, status []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	row, held := x.d.objects[id]
	if !held || row.Kind != kind || !row.DeletedAt.IsZero() {
		return store.ErrNotFound
	}
	row.Status = slices.Clone(status)
	row.UpdatedAt = time.Now().UTC()
	x.d.objects[id] = row
	return nil
}

func (x desired) LastApplied(ctx context.Context, id string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	row, held := x.d.objects[id]
	if !held {
		return nil, store.ErrNotFound
	}
	return slices.Clone(row.LastApplied), nil
}

func (x desired) SetLastApplied(ctx context.Context, id string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	row, held := x.d.objects[id]
	if !held {
		return store.ErrNotFound
	}
	row.LastApplied = slices.Clone(data)
	row.UpdatedAt = time.Now().UTC()
	x.d.objects[id] = row
	return nil
}

type observed struct{ d *data }

func (x observed) Put(ctx context.Context, environment string, s driver.State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	x.d.observed[s.ID] = observedRow{environment: environment, state: cloneState(s)}
	return nil
}

func (x observed) Get(ctx context.Context, id string) (driver.State, string, error) {
	if err := ctx.Err(); err != nil {
		return driver.State{}, "", err
	}
	row, held := x.d.observed[id]
	if !held {
		return driver.State{}, "", store.ErrNotFound
	}
	return cloneState(row.state), row.environment, nil
}

func (x observed) List(ctx context.Context, f store.Filter, p store.Page) ([]driver.State, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	var rows []driver.State
	for id, row := range x.d.observed {
		if id <= p.Cursor {
			continue
		}
		if !matches(f, row.state.Owner, row.state.Phase, row.environment, id, row.state.Labels) {
			continue
		}
		rows = append(rows, cloneState(row.state))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	rows, next := store.PageOf(rows, p, func(s driver.State) string { return s.ID })
	return rows, next, nil
}

func (x observed) Rebuild(ctx context.Context, environment string, states []driver.State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for id, row := range x.d.observed {
		if row.environment == environment {
			delete(x.d.observed, id)
		}
	}
	for _, s := range states {
		x.d.observed[s.ID] = observedRow{environment: environment, state: cloneState(s)}
	}
	return nil
}

type journal struct{ d *data }

func (x journal) Append(ctx context.Context, e store.Event) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if e.ObjectID == "" || e.Type == "" {
		return 0, errors.New("store: an event carries an object and a type")
	}
	x.d.seq[e.ObjectID]++
	e.Seq = x.d.seq[e.ObjectID]
	if e.ID == "" {
		e.ID = store.EventID()
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	e.Payload = slices.Clone(e.Payload)
	x.d.events[e.ObjectID] = append(x.d.events[e.ObjectID], e)
	return e.Seq, nil
}

// Pending reads each object's lowest unfinished sequence first and holds it
// to the due time after, so a deferred event blocks its own object's later
// events and no other object's.
func (x journal) Pending(ctx context.Context, limit int, now time.Time) ([]store.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = store.DefaultPageLimit
	}
	var heads []store.Event
	for _, events := range x.d.events {
		head, found := store.Event{}, false
		for _, e := range events {
			if !e.AckedAt.IsZero() || !e.DroppedAt.IsZero() {
				continue
			}
			if !found || e.Seq < head.Seq {
				head, found = e, true
			}
		}
		if !found || head.NextAttemptAt.After(now) {
			continue
		}
		head.Payload = slices.Clone(head.Payload)
		heads = append(heads, head)
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i].ID < heads[j].ID })
	if len(heads) > limit {
		heads = heads[:limit]
	}
	return heads, nil
}

func (x journal) Acknowledge(ctx context.Context, id string, at time.Time) error {
	return x.finish(ctx, id, func(e *store.Event) { e.AckedAt = at.UTC() })
}

func (x journal) Drop(ctx context.Context, id string, at time.Time) error {
	return x.finish(ctx, id, func(e *store.Event) { e.DroppedAt = at.UTC() })
}

func (x journal) Defer(ctx context.Context, id string, next time.Time) error {
	return x.finish(ctx, id, func(e *store.Event) {
		e.Attempts++
		e.NextAttemptAt = next.UTC()
	})
}

// finish applies one delivery outcome to one row. An id no row holds is
// ErrNotFound: the caller read it from Pending, so its absence is a fact
// worth reporting rather than a write that quietly did nothing.
func (x journal) finish(ctx context.Context, id string, apply func(*store.Event)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for object, events := range x.d.events {
		for i := range events {
			if events[i].ID != id {
				continue
			}
			apply(&events[i])
			x.d.events[object] = events
			return nil
		}
	}
	return store.ErrNotFound
}

func (x journal) ByObject(ctx context.Context, objectID string, p store.Page) ([]store.Event, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	cursor := int64(0)
	if p.Cursor != "" {
		parsed, err := strconv.ParseInt(p.Cursor, 10, 64)
		if err != nil {
			return nil, "", errors.New("store: the page cursor is not a sequence")
		}
		cursor = parsed
	}
	var rows []store.Event
	for _, e := range x.d.events[objectID] {
		if cursor > 0 && e.Seq >= cursor {
			continue
		}
		e.Payload = slices.Clone(e.Payload)
		rows = append(rows, e)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq > rows[j].Seq })
	rows, next := store.PageOf(rows, p, func(e store.Event) string { return strconv.FormatInt(e.Seq, 10) })
	return rows, next, nil
}

// Prune forgets finished rows only. An event still waiting for the sink is
// older than the retention long before it is undeliverable, and design 009
// decides when it is given up, not the retention.
func (x journal) Prune(ctx context.Context, before time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n := 0
	for id, events := range x.d.events {
		kept := events[:0]
		for _, e := range events {
			finished := !e.AckedAt.IsZero() || !e.DroppedAt.IsZero()
			if e.At.Before(before) && finished {
				n++
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(x.d.events, id)
			continue
		}
		x.d.events[id] = kept
	}
	return n, nil
}

type values struct {
	d   *data
	env store.Envelope
}

func (x values) Put(ctx context.Context, secretID string, plaintext []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	wrapped, sealed, err := x.env.Seal(plaintext)
	if err != nil {
		return 0, err
	}
	row := x.d.values[secretID]
	row.version++
	row.wrapped, row.sealed = wrapped, sealed
	x.d.values[secretID] = row
	return row.version, nil
}

func (x values) Open(ctx context.Context, secretID string) ([]byte, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	row, held := x.d.values[secretID]
	if !held {
		return nil, 0, store.ErrNotFound
	}
	plaintext, err := x.env.Open(row.wrapped, row.sealed)
	if err != nil {
		return nil, 0, err
	}
	return plaintext, row.version, nil
}

func (x values) Delete(ctx context.Context, secretID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, held := x.d.values[secretID]; !held {
		return store.ErrNotFound
	}
	delete(x.d.values, secretID)
	return nil
}

type leases struct{ d *data }

func (x leases) Acquire(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	now := time.Now().UTC()
	current, held := x.d.leases[name]
	if held && current.holder != holder && current.expires.After(now) {
		return false, nil
	}
	x.d.leases[name] = leaseRow{holder: holder, expires: now.Add(ttl)}
	return true, nil
}

func (x leases) Release(ctx context.Context, name, holder string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if current, held := x.d.leases[name]; held && current.holder == holder {
		delete(x.d.leases, name)
	}
	return nil
}

// matches is the store filter over one row's indexed columns.
func matches(f store.Filter, owner, phase, environment, id string, labels map[string]string) bool {
	switch {
	case f.Owner != "" && f.Owner != owner:
		return false
	case f.Phase != "" && f.Phase != phase:
		return false
	case f.Environment != "" && f.Environment != environment:
		return false
	case len(f.IDs) > 0 && !slices.Contains(f.IDs, id):
		return false
	}
	for k, v := range f.Labels {
		if labels[k] != v {
			return false
		}
	}
	return true
}
