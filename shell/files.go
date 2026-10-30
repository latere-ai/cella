// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// files is the file system as the line's programs see it: only what the
// mounts serve, read-only mounts refused before the file system is called,
// directories mkdir made kept for the rest of the line when the file
// system keeps none, and listings kept until something changes.
type files struct {
	fsys   FS
	mounts []Mount
	// now is the time of a directory the file system does not know: a
	// mount's that holds nothing yet, or one mkdir made.
	now time.Time

	mu      sync.Mutex
	made    map[string]bool
	listing map[string][]fs.FileInfo
}

func newFiles(fsys FS, mounts []Mount, now time.Time) *files {
	return &files{fsys: fsys, mounts: mounts, now: now, made: map[string]bool{}, listing: map[string][]fs.FileInfo{}}
}

// notServed is the answer for a path no mount serves: the line never
// learns anything about it.
func notServed(op, p string) error {
	return &fs.PathError{Op: op, Path: p, Err: syscall.ENOENT}
}

func (f *files) serves(p string) bool { return serves(f.mounts, p) }

func (f *files) readOnly(p string) bool {
	m, ok := mountOf(f.mounts, p)
	return !ok || m.ReadOnly
}

func (f *files) isMountDir(p string) bool {
	return slices.ContainsFunc(f.mounts, func(m Mount) bool { return m.Dir == p })
}

// changed forgets the listings kept, after a change.
func (f *files) changed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.listing)
}

// stat describes p. A mount's directory and a directory mkdir made are
// directories whatever the file system says of them.
func (f *files) stat(ctx context.Context, p string) (fs.FileInfo, error) {
	if !f.serves(p) {
		return nil, notServed("stat", p)
	}
	info, err := f.fsys.Stat(ctx, p)
	if err == nil {
		return info, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	f.mu.Lock()
	made := f.made[p]
	f.mu.Unlock()
	if made || f.isMountDir(p) {
		return NewFileInfo(path.Base(p), 0, dirMode, f.now), nil
	}
	return nil, err
}

// dirMode is the mode of a directory the file system does not know. A
// read-only mount keeps its directories' modes, as a disk mounted
// read-only does.
const dirMode = fs.ModeDir | 0o755

// dirExists reports whether p is a directory the line can enter: one a
// stat that failed for any reason is not.
func (f *files) dirExists(ctx context.Context, p string) bool {
	dir, err := f.isDir(ctx, p)
	return err == nil && dir
}

// isDir reports whether p is a directory, and the error of a stat that
// failed.
func (f *files) isDir(ctx context.Context, p string) (bool, error) {
	info, err := f.stat(ctx, p)
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

// readDir lists p: the file system's entries, the directories mkdir made
// in it, and the mounts directly in it.
func (f *files) readDir(ctx context.Context, p string) ([]fs.FileInfo, error) {
	if !f.serves(p) {
		return nil, notServed("open", p)
	}
	f.mu.Lock()
	kept, ok := f.listing[p]
	f.mu.Unlock()
	if ok {
		return kept, nil
	}
	info, err := f.stat(ctx, p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.ENOTDIR}
	}
	entries, err := f.fsys.ReadDir(ctx, p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name()] = true
	}
	f.mu.Lock()
	for d := range f.made {
		if path.Dir(d) == p && !seen[path.Base(d)] {
			entries = append(entries, NewFileInfo(path.Base(d), 0, dirMode, f.now))
			seen[path.Base(d)] = true
		}
	}
	f.mu.Unlock()
	for _, m := range f.mounts {
		if path.Dir(m.Dir) == p && !seen[path.Base(m.Dir)] {
			entries = append(entries, NewFileInfo(path.Base(m.Dir), 0, dirMode, f.now))
			seen[path.Base(m.Dir)] = true
		}
	}
	f.mu.Lock()
	f.listing[p] = entries
	f.mu.Unlock()
	return entries, nil
}

// read is a file's content.
func (f *files) read(ctx context.Context, p string) ([]byte, error) {
	if !f.serves(p) {
		return nil, notServed("open", p)
	}
	dir, err := f.isDir(ctx, p)
	if err != nil {
		return nil, err
	}
	if dir {
		return nil, &fs.PathError{Op: "read", Path: p, Err: syscall.EISDIR}
	}
	return f.fsys.ReadFile(ctx, p)
}

// writable checks that p may be written: served, in a mount that is not
// read only, not a directory, and in a directory that exists.
func (f *files) writable(ctx context.Context, op, p string) error {
	if !f.serves(p) {
		return notServed(op, p)
	}
	if info, err := f.stat(ctx, p); err == nil && info.IsDir() {
		return &fs.PathError{Op: op, Path: p, Err: syscall.EISDIR}
	}
	parent, err := f.stat(ctx, path.Dir(p))
	switch {
	case err != nil:
		return &fs.PathError{Op: op, Path: p, Err: err}
	case !parent.IsDir():
		return &fs.PathError{Op: op, Path: p, Err: syscall.ENOTDIR}
	}
	if f.readOnly(p) {
		return &fs.PathError{Op: op, Path: p, Err: syscall.EROFS}
	}
	return nil
}

// write replaces a file's content.
func (f *files) write(ctx context.Context, p string, data []byte) error {
	if err := f.writable(ctx, "open", p); err != nil {
		return err
	}
	defer f.changed()
	return f.fsys.WriteFile(ctx, p, data)
}

// remove removes a file.
func (f *files) remove(ctx context.Context, p string) error {
	if !f.serves(p) {
		return notServed("remove", p)
	}
	if f.readOnly(p) {
		if _, err := f.stat(ctx, p); err != nil {
			return err
		}
		return &fs.PathError{Op: "remove", Path: p, Err: syscall.EROFS}
	}
	defer f.changed()
	return f.fsys.Remove(ctx, p)
}

// mkdir makes a directory whose parent exists.
func (f *files) mkdir(ctx context.Context, p string) error {
	if !f.serves(p) {
		return notServed("mkdir", p)
	}
	if _, err := f.stat(ctx, p); err == nil {
		return &fs.PathError{Op: "mkdir", Path: p, Err: syscall.EEXIST}
	}
	parent, err := f.stat(ctx, path.Dir(p))
	switch {
	case err != nil:
		return &fs.PathError{Op: "mkdir", Path: p, Err: err}
	case !parent.IsDir():
		return &fs.PathError{Op: "mkdir", Path: p, Err: syscall.ENOTDIR}
	}
	if f.readOnly(p) {
		return &fs.PathError{Op: "mkdir", Path: p, Err: syscall.EROFS}
	}
	defer f.changed()
	if dm, ok := f.fsys.(DirMaker); ok {
		return dm.Mkdir(ctx, p)
	}
	f.mu.Lock()
	f.made[p] = true
	f.mu.Unlock()
	return nil
}

// rmdir removes an empty directory: one mkdir made, or one the file
// system keeps no longer once its files are gone.
func (f *files) rmdir(ctx context.Context, p string) error {
	if f.isMountDir(p) {
		return &fs.PathError{Op: "rmdir", Path: p, Err: syscall.EBUSY}
	}
	if f.readOnly(p) {
		return &fs.PathError{Op: "rmdir", Path: p, Err: syscall.EROFS}
	}
	entries, err := f.readDir(ctx, p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A directory the file system holds only by its files is gone
		// once they are.
		return nil
	case err != nil:
		return err
	}
	if len(entries) > 0 {
		return &fs.PathError{Op: "rmdir", Path: p, Err: syscall.ENOTEMPTY}
	}
	defer f.changed()
	f.mu.Lock()
	delete(f.made, p)
	f.mu.Unlock()
	if _, ok := f.fsys.(DirMaker); ok {
		// A file system that keeps directories removes one as a file.
		return f.fsys.Remove(ctx, p)
	}
	return nil
}

// rename moves a file in one step when the file system can, and otherwise
// writes it at its new path and removes the old one.
func (f *files) rename(ctx context.Context, from, to string) error {
	if f.readOnly(from) {
		return &fs.PathError{Op: "rename", Path: from, Err: syscall.EROFS}
	}
	if err := f.writable(ctx, "rename", to); err != nil {
		return err
	}
	defer f.changed()
	if r, ok := f.fsys.(Renamer); ok {
		return r.Rename(ctx, from, to)
	}
	data, err := f.fsys.ReadFile(ctx, from)
	if err != nil {
		return err
	}
	if err := f.fsys.WriteFile(ctx, to, data); err != nil {
		return err
	}
	return f.fsys.Remove(ctx, from)
}

// renameTree moves a directory: in one step when the file system can,
// and otherwise file by file, each written at its new path and then
// removed.
func (f *files) renameTree(ctx context.Context, from, to string) error {
	switch {
	case f.holdsMount(from):
		return &fs.PathError{Op: "rename", Path: from, Err: syscall.EBUSY}
	case f.readOnly(from):
		return &fs.PathError{Op: "rename", Path: from, Err: syscall.EROFS}
	}
	if r, ok := f.fsys.(Renamer); ok {
		if err := f.writable(ctx, "rename", to); err != nil {
			return err
		}
		defer f.changed()
		f.mu.Lock()
		for d := range f.made {
			if within(d, from) {
				delete(f.made, d)
				f.made[to+strings.TrimPrefix(d, from)] = true
			}
		}
		f.mu.Unlock()
		return r.Rename(ctx, from, to)
	}
	return f.moveTree(ctx, from, to)
}

func (f *files) moveTree(ctx context.Context, from, to string) error {
	entries, err := f.readDir(ctx, from)
	if err != nil {
		return err
	}
	if _, err := f.stat(ctx, to); err != nil {
		if err := f.mkdir(ctx, to); err != nil {
			return err
		}
	}
	for _, e := range entries {
		src, dst := path.Join(from, e.Name()), path.Join(to, e.Name())
		if e.IsDir() {
			if err := f.moveTree(ctx, src, dst); err != nil {
				return err
			}
			continue
		}
		if err := f.rename(ctx, src, dst); err != nil {
			return err
		}
	}
	return f.rmdir(ctx, from)
}

// mountOf is the mount that holds p.
func (f *files) mountOf(p string) Mount {
	m, _ := mountOf(f.mounts, p)
	return m
}

// holdsMount reports whether a served folder is mounted at p or under it.
func (f *files) holdsMount(p string) bool {
	return slices.ContainsFunc(f.mounts, func(m Mount) bool { return within(m.Dir, p) })
}

// fileInfo is a FileInfo a file system builds from what it knows.
type fileInfo struct {
	name string
	size int64
	mode fs.FileMode
	mod  time.Time
}

// NewFileInfo is an fs.FileInfo of a file or, with fs.ModeDir in mode, a
// directory, for a file system to answer Stat and ReadDir with.
func NewFileInfo(name string, size int64, mode fs.FileMode, modTime time.Time) fs.FileInfo {
	return fileInfo{name: name, size: size, mode: mode, mod: modTime}
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return i.mode }
func (i fileInfo) ModTime() time.Time { return i.mod }
func (i fileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fileInfo) Sys() any           { return nil }
