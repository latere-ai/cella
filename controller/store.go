// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// Store is the controller's desired-state persistence seam. Save replaces the
// snapshot atomically. A store instance has exactly one controller owner;
// methods run under that controller's mutex.
type Store interface {
	Load() (map[string]v1.Sandbox, error)
	Save(map[string]v1.Sandbox) error
	Close() error
}

// Durable is the store of design 010 as the controller reads it: desired state
// that can outlive the process, the observed index a driver's list rebuilds,
// and the journal every mutation appends to.
//
// A Store that is also a Durable takes one conditional write per object rather
// than the whole snapshot, so a second replica cannot overwrite a row it did
// not read. One whose Durable reports true also recovers a sandbox the data
// plane lost; one that reports false reaps it after LostGrace, which is the
// rule of design 005.
type Durable interface {
	Store
	// Durable reports whether what is written outlives this process.
	Durable() bool
	// Write stores one object at the version this process last saw and
	// appends the mutation to the journal, in one transaction.
	Write(ctx context.Context, obj v1.Sandbox, mutation string) error
	// Remove deletes one object and appends the mutation, in one transaction.
	Remove(ctx context.Context, id, mutation string) error
	// Rebuild replaces the observed rows of one environment with what its
	// driver last listed.
	Rebuild(ctx context.Context, environment string, states []driver.State) error
}

// The mutations the controller appends to the journal, one per act it takes
// on a sandbox. Each name that design 009 has in its event vocabulary is
// delivered to the operator's sink; the three that it does not are journaled
// and never delivered, because design 010 keeps one row per mutation and
// design 009 does not make an event of every write.
//
// MutationDeleting is the intent written before the driver is asked, and
// MutationStatus the status written back after a driver read. Neither is a
// change a reader of the feed acts on: the delete is reported when it
// completes, and the status is what a read of the sandbox already says.
// MutationSaved is the snapshot store's whole-map write.
const (
	MutationCreated    = "sandbox.created"
	MutationUpdated    = "sandbox.updated"
	MutationStarted    = "sandbox.started"
	MutationStopped    = "sandbox.stopped"
	MutationFailed     = "sandbox.failed"
	MutationDeleted    = "sandbox.deleted"
	MutationLost       = "sandbox.lost"
	MutationRecovering = "sandbox.recovering"
	MutationRecovered  = "sandbox.recovered"
	MutationSaved      = "sandbox.saved"
	MutationDeleting   = "sandbox.deleting"
	MutationStatus     = "sandbox.status"
)

// Sealer is the envelope of design 010 as the snapshot store needs it: one
// data key per value, sealed under the operator's own key. The store of
// design 010 implements it, so the crypto has one implementation whichever
// store a deployment runs on.
type Sealer interface {
	// Seal returns the wrapped data key and the sealed value.
	Seal(plaintext []byte) (wrapped, sealed []byte, err error)
	// Open reverses Seal.
	Open(wrapped, sealed []byte) ([]byte, error)
}

type fileStore struct {
	dir    string
	lock   *os.File
	sealer Sealer
	// mu guards every collection below and the file they are written to.
	// The controller's callers hold different locks: a sandbox is written
	// under the controller's, an environment's phase under none, so the
	// store serializes its own document rather than trusting one of them.
	mu sync.Mutex
	// objects is this store's own copy of the sandboxes, never the map the
	// controller mutates: a write of any other collection marshals it, and
	// that write does not run under the controller's lock.
	objects map[string]v1.Sandbox
	secrets map[string]secretRow
	// ledger is the spawn count of design 022, one entry per sandbox that
	// has created a child. It is in the snapshot because the count outlives
	// the process that made it, and it commits with the objects because the
	// file is one document.
	ledger map[string]int
	// environments is the Environment kind in the snapshot, one row per
	// environment with the version an If-Match is compared against.
	environments map[string]environmentRow
}

// environmentRow is one Environment in the snapshot: the object and the row
// version, which is the same shape the table of design 010 holds.
type environmentRow struct {
	Object  v1.Environment `json:"object"`
	Version int64          `json:"version"`
}

// secretRow is one Secret in the snapshot: the object a read returns and the
// two ciphertexts of its value, which is the same shape the table of design
// 010 holds.
type secretRow struct {
	Object  v1.Secret `json:"object"`
	Version int       `json:"version"`
	Wrapped []byte    `json:"wrapped,omitempty"`
	Sealed  []byte    `json:"sealed,omitempty"`
}

type snapshot struct {
	Version      int                       `json:"version"`
	Objects      map[string]v1.Sandbox     `json:"objects"`
	Secrets      map[string]secretRow      `json:"secrets,omitempty"`
	Ledger       map[string]int            `json:"ledger,omitempty"`
	Environments map[string]environmentRow `json:"environments,omitempty"`
}

// OpenFileStore opens a provisional local desired-state snapshot, taking an
// exclusive process lock. It is not a distributed database or operation
// journal. It holds no secret value, because it was given no key to seal one
// with; OpenSealedFileStore is the same store with one.
func OpenFileStore(dir string) (Store, error) { return OpenSealedFileStore(dir, nil) }

// OpenSealedFileStore is OpenFileStore for a deployment that stores secret
// values: the sealer is the operator's key, and without one every write of a
// value is ErrNoSecretKey and the rest of the store serves as before.
func OpenSealedFileStore(dir string, sealer Sealer) (Store, error) {
	if dir == "" {
		return nil, errors.New("store directory is required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("controller directory is already in use: %w", err)
	}
	return &fileStore{dir: dir, lock: lock, sealer: sealer,
		objects: map[string]v1.Sandbox{}, secrets: map[string]secretRow{},
		ledger: map[string]int{}, environments: map[string]environmentRow{}}, nil
}
func (s *fileStore) Load() (map[string]v1.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(filepath.Join(s.dir, "objects.json"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]v1.Sandbox{}, nil
	}
	if err != nil {
		return nil, err
	}
	var data snapshot
	if err = json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	if data.Version != 1 || data.Objects == nil {
		return nil, errors.New("unsupported or invalid controller snapshot")
	}
	s.objects = data.Objects
	s.secrets = data.Secrets
	if s.secrets == nil {
		s.secrets = map[string]secretRow{}
	}
	s.ledger = data.Ledger
	if s.ledger == nil {
		s.ledger = map[string]int{}
	}
	s.environments = data.Environments
	if s.environments == nil {
		s.environments = map[string]environmentRow{}
	}
	return maps.Clone(data.Objects), nil
}

// LoadSecrets is the Secret half of Load. The snapshot was read by Load,
// which the controller calls first.
func (s *fileStore) LoadSecrets() (map[string]v1.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]v1.Secret, len(s.secrets))
	for id, row := range s.secrets {
		obj := row.Object
		obj.Status.Version = row.Version
		out[id] = obj
	}
	return out, nil
}

// WriteSecret seals the plaintext where one is given and rewrites the
// snapshot. The journal is not this store's: it keeps desired state and
// nothing else, and the controller's emitter takes the record.
func (s *fileStore) WriteSecret(_ context.Context, obj v1.Secret, plaintext []byte, _ string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.secrets[obj.Status.ID]
	row.Object = obj
	row.Object.Spec.Value = ""
	if len(plaintext) > 0 {
		if s.sealer == nil {
			return 0, ErrNoSecretKey
		}
		wrapped, sealed, err := s.sealer.Seal(plaintext)
		if err != nil {
			return 0, err
		}
		row.Wrapped, row.Sealed = wrapped, sealed
		row.Version++
	}
	row.Object.Status.Version = row.Version
	previous, held := s.secrets[obj.Status.ID]
	s.secrets[obj.Status.ID] = row
	if err := s.write(); err != nil {
		s.restoreSecret(obj.Status.ID, previous, held)
		return 0, err
	}
	return row.Version, nil
}

// RemoveSecret drops one Secret and its ciphertext from the snapshot.
func (s *fileStore) RemoveSecret(_ context.Context, id, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, held := s.secrets[id]
	delete(s.secrets, id)
	if err := s.write(); err != nil {
		s.restoreSecret(id, previous, held)
		return err
	}
	return nil
}

// OpenValue unseals one value. It is the snapshot store's half of the one
// decrypting call, and it has the same single caller the durable store's has.
func (s *fileStore) OpenValue(_ context.Context, secretID string) ([]byte, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, held := s.secrets[secretID]
	if !held || len(row.Sealed) == 0 {
		return nil, 0, ErrNotFound
	}
	if s.sealer == nil {
		return nil, 0, ErrNoSecretKey
	}
	plaintext, err := s.sealer.Open(row.Wrapped, row.Sealed)
	if err != nil {
		return nil, 0, err
	}
	return plaintext, row.Version, nil
}

func (s *fileStore) restoreSecret(id string, previous secretRow, held bool) {
	if held {
		s.secrets[id] = previous
		return
	}
	delete(s.secrets, id)
}

// Save replaces the sandboxes with a copy of the controller's map, taken
// under this store's lock, and rewrites the snapshot.
func (s *fileStore) Save(objects map[string]v1.Sandbox) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects = maps.Clone(objects)
	return s.write()
}

// write replaces the snapshot atomically: both collections, every time,
// because the file is one document and a half-written one is no state at all.
// The caller holds s.mu.
func (s *fileStore) write() error {
	b, err := json.Marshal(snapshot{Version: 1, Objects: s.objects, Secrets: s.secrets,
		Ledger: s.ledger, Environments: s.environments})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".objects-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, filepath.Join(s.dir, "objects.json")); err != nil {
		return err
	}
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
func (s *fileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}

// WriteSpawn raises the parent's count and writes the child in one snapshot
// write, which is what makes the debit and the child's own row one act on
// this store: the file is replaced by one rename, so a reader sees both or
// neither.
func (s *fileStore) WriteSpawn(_ context.Context, obj v1.Sandbox, _, parentID string, budget int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if parentID == "" {
		return errors.New("a debit names a sandbox")
	}
	if s.ledger[parentID] >= budget {
		return ErrBudgetExhausted
	}
	previous, held := s.objects[obj.Status.ID]
	s.ledger[parentID]++
	s.objects[obj.Status.ID] = obj
	if err := s.write(); err != nil {
		s.ledger[parentID]--
		if held {
			s.objects[obj.Status.ID] = previous
		} else {
			delete(s.objects, obj.Status.ID)
		}
		return err
	}
	return nil
}

// CreditSpawn returns one unit to a parent whose child never started.
func (s *fileStore) CreditSpawn(_ context.Context, parentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.ledger[parentID]; n > 0 {
		s.ledger[parentID] = n - 1
		return s.write()
	}
	return nil
}

// SpawnsUsed is how many children one sandbox has created in total.
func (s *fileStore) SpawnsUsed(_ context.Context, parentID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ledger[parentID], nil
}

// ForgetSpawns drops one sandbox's count at the delete that ends it.
func (s *fileStore) ForgetSpawns(_ context.Context, parentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, held := s.ledger[parentID]; !held {
		return nil
	}
	delete(s.ledger, parentID)
	return s.write()
}

// The seams the snapshot store satisfies. A change to either side that breaks
// the other is a build failure here rather than a nil store at start-up.
var (
	_ Store        = (*fileStore)(nil)
	_ Secrets      = (*fileStore)(nil)
	_ Spawner      = (*fileStore)(nil)
	_ Environments = (*fileStore)(nil)
)

// LoadEnvironments is the Environment half of Load. The snapshot was read by
// Load, which the controller calls first.
func (s *fileStore) LoadEnvironments() (map[string]v1.Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]v1.Environment, len(s.environments))
	for name, row := range s.environments {
		obj := row.Object
		obj.Status.Version = row.Version
		out[name] = obj
	}
	return out, nil
}

// WriteEnvironment stores one Environment at the version given and rewrites
// the snapshot. The journal is not this store's: it keeps desired state and
// nothing else, and the controller's emitter takes the record.
func (s *fileStore) WriteEnvironment(_ context.Context, obj v1.Environment, ifVersion int64, _ string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := obj.Metadata.Name
	previous, held := s.environments[name]
	if held && previous.Version != ifVersion {
		return 0, ErrVersionConflict
	}
	if !held && ifVersion != 0 {
		return 0, ErrVersionConflict
	}
	row := environmentRow{Object: obj, Version: previous.Version + 1}
	row.Object.Status.Version = row.Version
	s.environments[name] = row
	if err := s.write(); err != nil {
		s.restoreEnvironment(name, previous, held)
		return 0, err
	}
	return row.Version, nil
}

// WriteEnvironmentStatus writes what the phase loop computed, which is a
// write of the whole row on a store that keeps one document.
func (s *fileStore) WriteEnvironmentStatus(_ context.Context, obj v1.Environment, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := obj.Metadata.Name
	previous, held := s.environments[name]
	if !held {
		return ErrNotFound
	}
	row := previous
	row.Object.Status = obj.Status
	row.Object.Status.Version = row.Version
	s.environments[name] = row
	if err := s.write(); err != nil {
		s.restoreEnvironment(name, previous, held)
		return err
	}
	return nil
}

// RemoveEnvironment drops one Environment from the snapshot.
func (s *fileStore) RemoveEnvironment(_ context.Context, name, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, held := s.environments[name]
	delete(s.environments, name)
	if err := s.write(); err != nil {
		s.restoreEnvironment(name, previous, held)
		return err
	}
	return nil
}

func (s *fileStore) restoreEnvironment(name string, previous environmentRow, held bool) {
	if held {
		s.environments[name] = previous
		return
	}
	delete(s.environments, name)
}
