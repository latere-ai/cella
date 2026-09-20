// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package store is the state of design 010: desired state per object with a
// version for conditional writes, the observed index a driver's list rebuilds,
// the journal every mutation appends to, the leases one writer holds, and
// secret values under envelope encryption. Two adapters implement it, memory
// and postgres, and one suite runs against both.
//
// Every call runs inside a transaction: Tx(ctx, fn) is the whole contract, so
// a write and the journal row that records it commit together or not at all.
package store

import (
	"context"
	"errors"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// Kinds of design 003 the store holds. The kind is a column, so a later kind
// needs no migration.
const (
	KindSandbox     = "Sandbox"
	KindSecret      = "Secret"
	KindEnvironment = "Environment"
)

// PhaseDeleting is the one phase the count ceiling of design 007 excludes: a
// sandbox on its way out has already freed the seat it held.
const PhaseDeleting = "Deleting"

// The errors every adapter reports for the same condition, so a caller reads
// one error whichever store it opened.
var (
	// ErrNotFound is a read of a row that is absent or deleted.
	ErrNotFound = errors.New("store: object not found")
	// ErrVersionConflict is a conditional write whose row moved since it was
	// read. Design 008 answers it with 409 version_conflict.
	ErrVersionConflict = errors.New("store: object version moved")
	// ErrNameTaken is a write whose (kind, owner, name) is held by a live row.
	ErrNameTaken = errors.New("store: name is already in use")
	// ErrNoSecretKey is a secret value written to a store opened without the
	// key that wraps every data key.
	ErrNoSecretKey = errors.New("store: no secret key, so a secret value cannot be sealed")
)

// Store is the state of one control plane.
type Store interface {
	// Tx runs fn in one transaction: every write inside commits together or
	// not at all. The memory adapter holds one mutex for the duration.
	Tx(ctx context.Context, fn func(Tx) error) error
	// Durable reports whether what is written outlives this process. The
	// lost rule of design 005 recovers a sandbox where it is true and reaps
	// one after the grace where it is false.
	Durable() bool
	// Ready is the readiness check of design 002: the store answers a
	// trivial statement inside the probe's budget.
	Ready(ctx context.Context) error
	// Close releases the pool and every lease this process holds.
	Close() error
}

// Tx is every method set of the store, inside one transaction.
//
// Design 010 names three more: Queue (slice 038), Ledger (design 022) and
// Records (design 018). Queue is declared below with its table in the schema
// and has no accessor here until a caller exists.
type Tx interface {
	Desired() Desired
	Observed() Observed
	Journal() Journal
	Values() Values
	Leases() Leases
	Revocations() Revocations
	Operations() Operations
}

// Object is one desired-state row: the identity every kind is indexed by, the
// resolved object as JSON, and the status the controller writes beside it.
//
// Design 003 has no v1.Object interface yet, so the identity is columns and
// the object is bytes. Version is the row's, advanced by every write.
type Object struct {
	Kind        string
	ID          string
	Owner       string
	Name        string
	Environment string
	Phase       string
	Labels      map[string]string
	// Data is the resolved object as JSON, without its status.
	Data []byte
	// Status is what the controller writes beside the object. Rebuild never
	// touches it.
	Status []byte
	// LastApplied is the desired state the controller last applied, the left
	// side of design 005's update diff.
	LastApplied []byte
	Version     int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// DeletedAt is zero while the row is live. A deleted row keeps its id and
	// frees its name.
	DeletedAt time.Time
}

// Filter narrows a list. An empty field does not narrow. Labels match on
// every pair given.
type Filter struct {
	Owner       string
	Phase       string
	Environment string
	Labels      map[string]string
	IDs         []string
}

// Page is one page of a list: at most Limit rows from Cursor. A list returns
// the cursor of the next page, or the empty string at the end.
type Page struct {
	Limit  int
	Cursor string
}

// DefaultPageLimit is the page a caller that names none gets.
const DefaultPageLimit = 200

// Size is the page to read, bounded so a caller cannot ask for the whole
// table in one statement.
func (p Page) Size() int {
	switch {
	case p.Limit <= 0:
		return DefaultPageLimit
	case p.Limit > MaxPageLimit:
		return MaxPageLimit
	}
	return p.Limit
}

// MaxPageLimit is the most rows one list returns.
const MaxPageLimit = 1000

// PageOf cuts rows to one page and returns the cursor of the page after it,
// which is the key of the last row returned. A caller reads one row more than
// the page, so a full page with nothing behind it ends the list rather than
// handing out a cursor onto no rows.
func PageOf[T any](rows []T, p Page, key func(T) string) ([]T, string) {
	limit := p.Size()
	if len(rows) <= limit {
		return rows, ""
	}
	rows = rows[:limit]
	return rows, key(rows[len(rows)-1])
}

// Desired is the control plane's own state: what a caller applied, resolved,
// with the status the controller writes beside it.
type Desired interface {
	// Put writes the object at ifVersion and returns the version it now
	// holds. Zero creates, and a create whose id exists is ErrVersionConflict.
	// A name a live row of the same owner and kind holds is ErrNameTaken.
	Put(ctx context.Context, obj Object, ifVersion int64) (int64, error)
	// Get reads one live object by id.
	Get(ctx context.Context, kind, id string) (Object, error)
	// ByName reads one live object by the name its owner gave it.
	ByName(ctx context.Context, kind, owner, name string) (Object, error)
	// List reads live objects of one kind, ordered by id, one page at a time.
	List(ctx context.Context, kind string, f Filter, p Page) ([]Object, string, error)
	// Delete marks the row deleted, which frees its name and keeps its id.
	Delete(ctx context.Context, kind, id string) error
	// Count is the count ceiling's query of design 007: the owner's live
	// objects, excluding phase Deleting.
	Count(ctx context.Context, kind, owner string) (int, error)
	// PutStatus writes the controller's status without touching the object
	// or its version.
	PutStatus(ctx context.Context, kind, id string, status []byte) error
	// LastApplied reads the desired state the controller last applied.
	LastApplied(ctx context.Context, id string) ([]byte, error)
	// SetLastApplied records the desired state the controller just applied.
	SetLastApplied(ctx context.Context, id string, data []byte) error
}

// Observed is the index of what the driver reports. It is a cache: every row
// is rebuildable from a driver's list, and losing it costs one rebuild.
type Observed interface {
	// Put records one state on one environment.
	Put(ctx context.Context, environment string, s driver.State) error
	// Get reads one state and the environment it was observed on.
	Get(ctx context.Context, id string) (driver.State, string, error)
	// List reads observed states, ordered by id, one page at a time.
	List(ctx context.Context, f Filter, p Page) ([]driver.State, string, error)
	// Rebuild replaces the rows of one environment with states and leaves
	// every other environment's rows, and every desired object, alone.
	Rebuild(ctx context.Context, environment string, states []driver.State) error
}

// Event is one journal row: what happened to one object, in the order it
// happened to that object, and how far its delivery has got.
type Event struct {
	// ID is the event's own id (evt_). Append assigns one when it is empty.
	ID string
	// ObjectID is the object the event is about; Seq counts within it.
	ObjectID string
	Seq      int64
	Type     string
	At       time.Time
	Payload  []byte
	// Attempts is how many deliveries have failed and NextAttemptAt when the
	// next one is due. Both are zero on a row nobody has tried.
	Attempts      int
	NextAttemptAt time.Time
	// AckedAt is when the sink took the event and DroppedAt when delivery
	// gave it up. A row with either set is finished and never pending
	// again; the two are apart because design 010's retention forgets both
	// and a reader must not read one as the other.
	AckedAt   time.Time
	DroppedAt time.Time
}

// Journal is the append-only record of every mutation, ordered per object,
// and the delivery of design 009 over it.
type Journal interface {
	// Append writes one event and returns the sequence it took, which is one
	// more than the object's last. An event whose AckedAt is set is stored
	// finished, which is how a mutation that design 009 names no type for is
	// recorded without ever being delivered.
	Append(ctx context.Context, e Event) (int64, error)
	// ByObject reads one object's events, newest first, one page at a time.
	ByObject(ctx context.Context, objectID string, p Page) ([]Event, string, error)
	// Prune drops events older than before and reports how many went.
	Prune(ctx context.Context, before time.Time) (int, error)
	// Pending returns at most limit unfinished events, at most one per
	// object: each object's lowest sequence, and only where its next attempt
	// is due. The lowest sequence is chosen before the due filter, so a
	// deferred event holds the events behind it for that object and lets no
	// other object's wait.
	Pending(ctx context.Context, limit int, now time.Time) ([]Event, error)
	// Acknowledge marks one event delivered.
	Acknowledge(ctx context.Context, id string, at time.Time) error
	// Defer records one failed attempt and when the next is due.
	Defer(ctx context.Context, id string, next time.Time) error
	// Drop ends one event undelivered.
	Drop(ctx context.Context, id string, at time.Time) error
}

// Values holds secret values under the envelope of design 018: a data key
// per secret, the value sealed under it, the data key sealed under the
// store's key. Open is the only method that returns a plaintext and design
// 018 gives it one caller, Controlled.OpenValue, which TestValuesAreConfined
// holds to being the only one.
type Values interface {
	Put(ctx context.Context, secretID string, plaintext []byte) (version int, err error)
	Open(ctx context.Context, secretID string) (plaintext []byte, version int, err error)
	Delete(ctx context.Context, secretID string) error
	// Rewrap rewrites every wrapped data key from oldKEK to newKEK and
	// reports how many rows moved. No value's ciphertext is read or
	// rewritten, so no plaintext exists at any point of a rotation, and the
	// store reads under newKEK once it returns.
	Rewrap(ctx context.Context, oldKEK, newKEK []byte) (n int, err error)
}

// Leases are the single-writer seam of design 005: the loops that act on an
// environment run on the replica that holds the named lease.
//
// The names are reaper, journal, scheduler, environments and pool:<environment>,
// each with a 15 second term the holder renews at a third of it.
type Leases interface {
	// Acquire takes or renews the lease for holder and reports whether it is
	// held. A live lease of another holder is not held and not an error.
	Acquire(ctx context.Context, name, holder string, ttl time.Duration) (bool, error)
	// Release frees a lease this holder holds. Releasing a lease another
	// holder took is not an error and frees nothing.
	Release(ctx context.Context, name, holder string) error
}

// Revocations is the jti of every token cellad minted and then replaced or
// ended before it expired. The verifier asks it about every token it signed
// itself (design 006), and the reaper's tick sweeps the rows no live token
// could present any more.
type Revocations interface {
	// Revoke refuses the jti until exp passes. It is idempotent: a rotation
	// or a recovery that retried revokes a jti it already revoked.
	Revoke(ctx context.Context, jti string, exp time.Time) error
	// Revoked reports whether this jti was revoked.
	Revoked(ctx context.Context, jti string) (bool, error)
	// Forget drops the rows whose exp has passed and reports how many went.
	Forget(ctx context.Context, before time.Time) (int, error)
}

// QueueItem is one sandbox waiting for capacity on a queued environment.
type QueueItem struct {
	SandboxID   string
	Environment string
	Queue       string
	Priority    int
	Subject     string
	EnqueuedAt  time.Time
}

// Queue is the seam slice 038 fills. Design 020 owns the order Dequeue
// returns and this slice does not guess it; the queue table is in the schema.
type Queue interface {
	Enqueue(ctx context.Context, item QueueItem) error
	Dequeue(ctx context.Context, environment, queue string) (*QueueItem, error)
	Remove(ctx context.Context, sandboxID string) error
	Position(ctx context.Context, sandboxID string) (int, error)
}

// Operation is one unit of work for a data plane worker: one driver call, the
// worker that holds it, and the answer that ends it.
type Operation struct {
	ID          string
	Environment string
	SandboxID   string
	Type        string
	Payload     []byte
	State       string
	ClaimedBy   string
	ClaimedAt   time.Time
	Attempts    int
	Result      []byte
	CreatedAt   time.Time
}

// The states one operation passes through. A queued row waits for a worker, a
// claimed row is one worker's until its lease lapses, and a done row carries
// the result and is kept until the sweep forgets it.
const (
	OperationQueued  = "queued"
	OperationClaimed = "claimed"
	OperationDone    = "done"
)

// Worker is one registration of a data plane worker and its last heartbeat.
type Worker struct {
	Environment   string
	Worker        string
	Replica       string
	LastHeartbeat time.Time
}

// Operations is the operation queue a worker claims from and the
// registrations an environment's phase is computed from (design 021). The
// stream carries the frames; this table is the arbiter of who holds what, so
// a dropped connection redelivers to whichever worker and replica is live.
type Operations interface {
	// Enqueue writes one operation for an environment. An id already in the
	// table is ErrVersionConflict.
	Enqueue(ctx context.Context, op Operation) error
	// Claim takes at most n operations for one worker: rows nobody holds,
	// and rows held by a worker whose last heartbeat is before lapsed. The
	// claimed rows are returned with their attempt count advanced.
	Claim(ctx context.Context, environment, worker string, n int, lapsed time.Time) ([]Operation, error)
	// Acknowledge ends one operation with its result. An operation already
	// done keeps the result it had, so a redelivery that raced a result
	// cannot overwrite the answer the caller was given.
	Acknowledge(ctx context.Context, opID string, result []byte) error
	// Get reads one operation, done or not.
	Get(ctx context.Context, opID string) (Operation, error)
	// Register records one worker's registration, replacing any row it held
	// before, and stamps its heartbeat.
	Register(ctx context.Context, w Worker) error
	// Heartbeat stamps a registered worker. A worker with no row is
	// ErrNotFound, which is how a worker learns the control plane forgot it
	// and registers again.
	Heartbeat(ctx context.Context, environment, worker string, at time.Time) error
	// Workers is every registration of one environment with its last
	// heartbeat, ordered by worker id.
	Workers(ctx context.Context, environment string) ([]Worker, error)
	// Forget drops one worker's registration, which is what a clean
	// disconnect writes so the environment's phase moves at once rather
	// than at the end of the lease.
	Forget(ctx context.Context, environment, worker string) error
	// Prune drops done operations created before an instant and reports how
	// many went.
	Prune(ctx context.Context, before time.Time) (int, error)
}
