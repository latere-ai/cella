// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	v1 "latere.ai/x/cella/manifest/v1"
)

// Store is the direct controller's desired-state persistence seam. Save replaces
// the snapshot atomically. A store instance has exactly one controller owner;
// methods run under that controller's mutex. The full transactional, observed,
// journal and operation stores remain the responsibility of design 010.
type Store interface {
	Load() (map[string]v1.Sandbox, error)
	Save(map[string]v1.Sandbox) error
	Close() error
}
type fileStore struct {
	dir  string
	lock *os.File
}
type snapshot struct {
	Version int                   `json:"version"`
	Objects map[string]v1.Sandbox `json:"objects"`
}

// OpenFileStore opens a provisional local desired-state snapshot, taking an
// exclusive process lock. It is not a distributed database or operation journal.
func OpenFileStore(dir string) (Store, error) {
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
	return &fileStore{dir: dir, lock: lock}, nil
}
func (s *fileStore) Load() (map[string]v1.Sandbox, error) {
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
	return data.Objects, nil
}
func (s *fileStore) Save(objects map[string]v1.Sandbox) error {
	b, err := json.Marshal(snapshot{1, objects})
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
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}
