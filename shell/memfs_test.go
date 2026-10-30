// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"context"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// memFS is a file system in memory. With dirs set it keeps directories,
// as a disk does; without, a directory is there while a file is under it,
// as in a files service that keeps files by path.
type memFS struct {
	mu    sync.Mutex
	files map[string]memFile
	// dirs is the directories kept, with their times; nil for a file
	// system that keeps none.
	dirs map[string]time.Time
	now  time.Time
	// fail answers an operation's error before it runs, keyed by the
	// operation and the path, such as "write /m/a".
	fail map[string]error
	// ops records the writes and removes, in order.
	ops []string
}

type memFile struct {
	data []byte
	mod  time.Time
	mode fs.FileMode
}

func newMemFS(keepDirs bool) *memFS {
	m := &memFS{files: map[string]memFile{}, now: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
	if keepDirs {
		m.dirs = map[string]time.Time{}
	}
	return m
}

// put adds a file, and with dirs kept the directories above it up to the
// root.
func (m *memFS) put(name, data string, mod time.Time) {
	m.files[name] = memFile{data: []byte(data), mod: mod, mode: 0o644}
	if m.dirs != nil {
		for d := path.Dir(name); d != "/"; d = path.Dir(d) {
			if _, ok := m.dirs[d]; !ok {
				m.dirs[d] = mod
			}
		}
	}
}

func (m *memFS) dir(name string, mod time.Time) {
	for d := name; d != "/"; d = path.Dir(d) {
		if _, ok := m.dirs[d]; !ok || d == name {
			m.dirs[d] = mod
		}
	}
}

func (m *memFS) failed(op, name string) error {
	if err, ok := m.fail[op+" "+name]; ok {
		return err
	}
	return nil
}

func (m *memFS) ReadFile(_ context.Context, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failed("read", name); err != nil {
		return nil, err
	}
	f, ok := m.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return slices.Clone(f.data), nil
}

func (m *memFS) WriteFile(_ context.Context, name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failed("write", name); err != nil {
		return err
	}
	m.ops = append(m.ops, "write "+name)
	m.files[name] = memFile{data: slices.Clone(data), mod: m.now, mode: 0o644}
	return nil
}

func (m *memFS) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failed("stat", name); err != nil {
		return nil, err
	}
	if f, ok := m.files[name]; ok {
		return NewFileInfo(path.Base(name), int64(len(f.data)), f.mode, f.mod), nil
	}
	if m.dirs != nil {
		if mod, ok := m.dirs[name]; ok {
			return NewFileInfo(path.Base(name), 0, fs.ModeDir|0o755, mod), nil
		}
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	var latest time.Time
	found := false
	for p, f := range m.files {
		if strings.HasPrefix(p, name+"/") {
			found = true
			if f.mod.After(latest) {
				latest = f.mod
			}
		}
	}
	if !found {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return NewFileInfo(path.Base(name), 0, fs.ModeDir|0o755, latest), nil
}

func (m *memFS) ReadDir(ctx context.Context, name string) ([]fs.FileInfo, error) {
	if err := m.failed("readdir", name); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	m.mu.Lock()
	for p := range m.files {
		if rest, ok := strings.CutPrefix(p, name+"/"); ok {
			first, _, _ := strings.Cut(rest, "/")
			names[first] = true
		}
	}
	for p := range m.dirs {
		if rest, ok := strings.CutPrefix(p, name+"/"); ok {
			first, _, _ := strings.Cut(rest, "/")
			names[first] = true
		}
	}
	m.mu.Unlock()
	var out []fs.FileInfo
	for _, n := range slices.Sorted(maps.Keys(names)) {
		info, err := m.Stat(ctx, path.Join(name, n))
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

func (m *memFS) Remove(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failed("remove", name); err != nil {
		return err
	}
	if _, ok := m.files[name]; ok {
		m.ops = append(m.ops, "remove "+name)
		delete(m.files, name)
		return nil
	}
	if _, ok := m.dirs[name]; ok {
		m.ops = append(m.ops, "rmdir "+name)
		delete(m.dirs, name)
		return nil
	}
	return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
}

// memDirFS is a memFS that keeps directories and makes them.
type memDirFS struct{ *memFS }

func (m memDirFS) Mkdir(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failed("mkdir", name); err != nil {
		return err
	}
	if _, ok := m.dirs[name]; ok {
		return &fs.PathError{Op: "mkdir", Path: name, Err: syscall.EEXIST}
	}
	m.ops = append(m.ops, "mkdir "+name)
	m.dirs[name] = m.now
	return nil
}

// memRenameFS is a memFS that moves a file, or a directory and what is
// under it, in one step.
type memRenameFS struct{ *memFS }

func (m memRenameFS) Rename(_ context.Context, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failed("rename", from); err != nil {
		return err
	}
	m.ops = append(m.ops, "rename "+from+" "+to)
	if f, ok := m.files[from]; ok {
		delete(m.files, from)
		m.files[to] = f
		return nil
	}
	moved := false
	for p, f := range m.files {
		if rest, ok := strings.CutPrefix(p, from+"/"); ok {
			delete(m.files, p)
			m.files[to+"/"+rest] = f
			moved = true
		}
	}
	if !moved {
		return &fs.PathError{Op: "rename", Path: from, Err: fs.ErrNotExist}
	}
	return nil
}
