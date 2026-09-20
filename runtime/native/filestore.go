// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	driver "latere.ai/x/cella/runtime"
)

var _ driver.FileStore = (*Driver)(nil)

// Stat describes one entry of the workspace.
func (d *Driver) Stat(ctx context.Context, id, p string) (driver.FileInfo, error) {
	root, rel, err := d.entry(ctx, id, p, followLink)
	if err != nil {
		return driver.FileInfo{}, err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(rel)
	if err != nil {
		return driver.FileInfo{}, fileError(err)
	}
	return describe(p, info), nil
}

// ReadDir lists the immediate entries of a directory, sorted by name.
func (d *Driver) ReadDir(ctx context.Context, id, p string) ([]driver.FileInfo, error) {
	root, rel, err := d.entry(ctx, id, p, followLink)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	dir, err := root.Open(rel)
	if err != nil {
		return nil, fileError(err)
	}
	defer func() { _ = dir.Close() }()
	if info, err := dir.Stat(); err != nil {
		return nil, fileError(err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", driver.ErrInvalid, p)
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fileError(err)
	}
	out := make([]driver.FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			// The entry went between the listing and the stat; it is not part
			// of the directory the caller is being shown.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fileError(err)
		}
		out = append(out, describe(path.Join(p, entry.Name()), info))
	}
	slices.SortFunc(out, func(a, b driver.FileInfo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Open streams one file. The reader holds the workspace open until it is
// closed, which is what the caller ends the transfer with.
func (d *Driver) Open(ctx context.Context, id, p string) (io.ReadCloser, driver.FileInfo, error) {
	root, rel, err := d.entry(ctx, id, p, followLink)
	if err != nil {
		return nil, driver.FileInfo{}, err
	}
	f, err := root.Open(rel)
	if err != nil {
		_ = root.Close()
		return nil, driver.FileInfo{}, fileError(err)
	}
	info, err := f.Stat()
	if err == nil && info.IsDir() {
		err = fmt.Errorf("%w: %s is a directory", driver.ErrInvalid, p)
	}
	if err != nil {
		_ = f.Close()
		_ = root.Close()
		return nil, driver.FileInfo{}, fileError(err)
	}
	return &rootedFile{File: f, root: root}, describe(p, info), nil
}

// rootedFile closes the workspace handle with the file it came from.
type rootedFile struct {
	*os.File
	root *os.Root
}

func (f *rootedFile) Close() error { return errors.Join(f.File.Close(), f.root.Close()) }

// Write stages the body beside its destination and renames it onto the name,
// so a body that fails or passes the bound leaves the previous file whole. A
// destination that is a symbolic link is replaced, never followed.
func (d *Driver) Write(ctx context.Context, id string, req driver.WriteRequest) (int64, error) {
	root, rel, err := d.entry(ctx, id, req.Path, replaceName)
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()
	if err := parents(root, rel); err != nil {
		return 0, err
	}
	mode := req.Mode.Perm()
	if mode == 0 {
		mode = driver.DefaultFileMode
	}
	temp := path.Join(path.Dir(rel), ".cella-write-"+rand.Text())
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fileError(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = root.Remove(temp)
		}
	}()
	source := req.Body
	if source == nil {
		source = strings.NewReader("")
	}
	body := io.Reader(&contextReader{ctx: ctx, r: source})
	if req.MaxBytes > 0 {
		// One byte past the bound separates a body at the bound from one over
		// it without reading the rest of it.
		body = io.LimitReader(body, req.MaxBytes+1)
	}
	n, err := io.Copy(f, body)
	if err == nil && req.MaxBytes > 0 && n > req.MaxBytes {
		err = fmt.Errorf("%w: %d bytes, the write allows %d", driver.ErrTooLarge, n, req.MaxBytes)
	}
	if err == nil {
		err = f.Chmod(mode)
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return 0, fileError(err)
	}
	if err := root.Rename(temp, rel); err != nil {
		return 0, fileError(err)
	}
	committed = true
	return n, nil
}

// Mkdir creates a directory and the missing parents.
func (d *Driver) Mkdir(ctx context.Context, id, p string) error {
	root, rel, err := d.entry(ctx, id, p, replaceName)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(rel, 0o755); err != nil {
		if name, yes := notADirectory(root, rel); yes {
			return fmt.Errorf("%w: %s is not a directory", driver.ErrInvalid, name)
		}
		return fileError(err)
	}
	return nil
}

// Remove deletes a file or a directory tree. A path that names nothing is not
// an error: what the caller asked to be gone is gone.
func (d *Driver) Remove(ctx context.Context, id, p string) error {
	root, rel, err := d.entry(ctx, id, p, replaceName)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if rel == "." {
		return fmt.Errorf("%w: the workspace itself is not removable", driver.ErrInvalid)
	}
	return fileError(root.RemoveAll(rel))
}

// Move renames from onto the exact path to. A destination that is an existing
// directory is refused rather than nesting the source inside it.
func (d *Driver) Move(ctx context.Context, id, from, to string) error {
	root, source, err := d.entry(ctx, id, from, replaceName)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	destination, err := d.contain(root.Name(), to, replaceName)
	if err != nil {
		return err
	}
	if source == "." || destination == "." {
		return fmt.Errorf("%w: the workspace itself is not movable", driver.ErrInvalid)
	}
	if _, err := root.Lstat(source); err != nil {
		return fileError(err)
	}
	if info, err := root.Stat(destination); err == nil && info.IsDir() {
		return fmt.Errorf("%w: %s is a directory, and a move never nests the source inside one", driver.ErrInvalid, to)
	}
	if err := parents(root, destination); err != nil {
		return err
	}
	return fileError(root.Rename(source, destination))
}

// parents creates the directories above rel. A parent that exists and is not
// a directory is the caller's mistake, not the filesystem's failure, and the
// portable error for it says only that the operation could not be done.
func parents(root *os.Root, rel string) error {
	dir := path.Dir(rel)
	if dir == "." {
		return nil
	}
	if err := root.MkdirAll(dir, 0o700); err != nil {
		if name, yes := notADirectory(root, dir); yes {
			return fmt.Errorf("%w: %s is not a directory", driver.ErrInvalid, name)
		}
		return fileError(err)
	}
	return nil
}

// notADirectory names the first component at or above rel that exists, and
// says whether it is something other than a directory. The portable error for
// a path below a file says only that the operation could not be done, so the
// reason is read back here.
func notADirectory(root *os.Root, rel string) (string, bool) {
	for above := rel; above != "." && above != "/"; above = path.Dir(above) {
		if info, err := root.Lstat(above); err == nil {
			return above, !info.IsDir()
		}
	}
	return "", false
}

// resolution says whether the last component of a path is followed. An
// operation that reads through the name follows it; one that replaces or
// drops the name itself does not, because the name is the caller's own and
// what it points at is not what the operation touches.
type resolution bool

const (
	followLink  resolution = true
	replaceName resolution = false
)

// entry opens the sandbox's workspace and returns the path relative to it,
// refused unless the containment rule of design 033 holds.
func (d *Driver) entry(ctx context.Context, id, p string, how resolution) (*os.Root, string, error) {
	root, err := d.workspace(ctx, id)
	if err != nil {
		return nil, "", err
	}
	rel, err := d.contain(root.Name(), p, how)
	if err != nil {
		_ = root.Close()
		return nil, "", err
	}
	return root, rel, nil
}

// contain is the containment rule: the path is lexically inside the
// workspace, and the deepest ancestor that exists resolves, with every
// symbolic link followed, inside it as well. os.Root refuses an escape too,
// and does it without a reason a caller can read; this is where the reason
// comes from.
func (d *Driver) contain(dir, p string, how resolution) (string, error) {
	rel, err := workspacePath(p)
	if err != nil {
		return "", fmt.Errorf("%w: %s is outside the workspace", driver.ErrInvalid, p)
	}
	base, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	probe := filepath.Join(base, filepath.FromSlash(rel))
	if how == replaceName {
		probe = filepath.Dir(probe)
	}
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	resolved, err := filepath.EvalSymlinks(probe)
	if err != nil || !under(resolved, base) {
		return "", fmt.Errorf("%w: %s leaves the workspace", driver.ErrInvalid, p)
	}
	return rel, nil
}

// under reports whether p is base or below it.
func under(p, base string) bool {
	return p == base || strings.HasPrefix(p, base+string(filepath.Separator))
}

// describe renders one entry as the contract's FileInfo.
func describe(p string, info fs.FileInfo) driver.FileInfo {
	size := info.Size()
	if info.IsDir() {
		size = 0
	}
	return driver.FileInfo{
		Name:    path.Base(p),
		Path:    p,
		Size:    size,
		Mode:    info.Mode() & (fs.ModePerm | fs.ModeDir),
		ModTime: info.ModTime().UTC(),
		IsDir:   info.IsDir(),
	}
}

// fileError turns the filesystem's answers into the contract's.
func fileError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %w", driver.ErrNotFound, err)
	case errors.Is(err, driver.ErrTooLarge), errors.Is(err, driver.ErrInvalid):
		return err
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%w: %w", driver.ErrInvalid, err)
	}
	return err
}
