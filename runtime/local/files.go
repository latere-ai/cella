// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package local

import (
	"archive/tar"
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

// MaxArchiveBytes bounds what one archive written into a sandbox holds.
const MaxArchiveBytes int64 = 256 << 20

// area is one directory the file operations reach: the guest path a caller
// names it by, the host directory it is, and whether it may be written.
type area struct {
	guest string
	host  string
	write bool
	// workspace says this is the sandbox's workspace rather than one of its
	// scratch directories.
	workspace bool
}

// areas are the directories the file operations of one sandbox reach: its
// workspace by the path the manifest names, and its home and temporary
// directory by their host paths, which status.paths reports.
func (d *Driver) areas(r record) []area {
	p := d.paths(r.State.ID)
	return []area{
		{guest: r.Guest, host: r.Root, write: r.Write, workspace: true},
		{guest: p.home, host: p.home, write: true},
		{guest: p.tmp, host: p.tmp, write: true},
	}
}

// resolution says whether the last component of a path is followed. An
// operation that reads through the name follows it; one that replaces or
// drops the name itself does not.
type resolution bool

const (
	followLink  resolution = true
	replaceName resolution = false
)

// entry opens the area a path is in and returns the path relative to it,
// refused unless the path stays inside the area, off the deny table, and
// writable where the operation writes. The operating system's sandbox does
// not hold here, since this runs in the driver's own process, so this is
// where the host's grants hold for the file operations.
func (d *Driver) entry(ctx context.Context, id, p string, how resolution, write bool) (*os.Root, area, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, area{}, "", err
	}
	d.mu.Lock()
	r, err := d.load(id)
	d.mu.Unlock()
	if err != nil {
		return nil, area{}, "", err
	}
	a, rel, err := d.locate(r, p)
	if err != nil {
		return nil, area{}, "", err
	}
	if err = d.contain(a, p, rel, how); err != nil {
		return nil, area{}, "", err
	}
	if write && !a.write {
		return nil, area{}, "", fmt.Errorf("%w: %s is in a folder this host grants read-only", driver.ErrWorkspaceNotGranted, p)
	}
	root, err := os.OpenRoot(a.host)
	if err != nil {
		return nil, area{}, "", err
	}
	return root, a, rel, nil
}

// locate is the area a path is in, the longest that holds it, and the path
// relative to it.
func (d *Driver) locate(r record, p string) (area, string, error) {
	if !path.IsAbs(p) || strings.ContainsRune(p, 0) || slices.Contains(strings.Split(p, "/"), "..") {
		return area{}, "", fmt.Errorf("%w: %s is not a clean absolute path in the sandbox", driver.ErrInvalid, p)
	}
	p = path.Clean(p)
	var found area
	held := false
	for _, a := range d.areas(r) {
		if (p == a.guest || strings.HasPrefix(p, a.guest+"/")) && (!held || len(a.guest) > len(found.guest)) {
			found, held = a, true
		}
	}
	if !held {
		return area{}, "", fmt.Errorf("%w: %s is outside the workspace", driver.ErrInvalid, p)
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(p, found.guest), "/")
	if rel == "" {
		rel = "."
	}
	return found, rel, nil
}

// contain is the containment rule: the deepest ancestor of the path that
// exists resolves, with every link followed, inside the area, and neither the
// path nor what it resolves to is on the deny table.
func (d *Driver) contain(a area, p, rel string, how resolution) error {
	base, err := filepath.EvalSymlinks(a.host)
	if err != nil {
		return err
	}
	target := filepath.Join(base, filepath.FromSlash(rel))
	if d.denied(target) || d.denied(filepath.Join(a.host, filepath.FromSlash(rel))) {
		return fmt.Errorf("%w: %s is on the deny table", driver.ErrInvalid, p)
	}
	probe := target
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
	real, err := filepath.EvalSymlinks(probe)
	if err != nil || !under(real, base) || d.denied(real) {
		return fmt.Errorf("%w: %s leaves the workspace", driver.ErrInvalid, p)
	}
	return nil
}

// Stat describes one entry.
func (d *Driver) Stat(ctx context.Context, id, p string) (driver.FileInfo, error) {
	root, _, rel, err := d.entry(ctx, id, p, followLink, false)
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

// ReadDir lists the immediate entries of a directory, sorted by name. An
// entry on the deny table is left out of the listing.
func (d *Driver) ReadDir(ctx context.Context, id, p string) ([]driver.FileInfo, error) {
	root, a, rel, err := d.entry(ctx, id, p, followLink, false)
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
		if d.denied(filepath.Join(a.host, filepath.FromSlash(rel), entry.Name())) {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fileError(err)
		}
		out = append(out, describe(path.Join(p, entry.Name()), info))
	}
	slices.SortFunc(out, func(a, b driver.FileInfo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Open streams one file. The reader holds its area open until it is closed.
func (d *Driver) Open(ctx context.Context, id, p string) (io.ReadCloser, driver.FileInfo, error) {
	root, _, rel, err := d.entry(ctx, id, p, followLink, false)
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

// rootedFile closes its area's handle with the file.
type rootedFile struct {
	*os.File
	root *os.Root
}

func (f *rootedFile) Close() error { return errors.Join(f.File.Close(), f.root.Close()) }

// Write stages the body beside its destination and renames it onto the name,
// so a body that fails or passes the bound leaves the previous file whole. A
// destination that is a link is replaced, never followed.
func (d *Driver) Write(ctx context.Context, id string, req driver.WriteRequest) (int64, error) {
	root, _, rel, err := d.entry(ctx, id, req.Path, replaceName, true)
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()
	if rel == "." {
		return 0, fmt.Errorf("%w: the workspace itself is not a file", driver.ErrInvalid)
	}
	if err = parents(root, rel); err != nil {
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
	if err = root.Rename(temp, rel); err != nil {
		return 0, fileError(err)
	}
	committed = true
	return n, nil
}

// Mkdir creates a directory and the missing parents.
func (d *Driver) Mkdir(ctx context.Context, id, p string) error {
	root, _, rel, err := d.entry(ctx, id, p, replaceName, true)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if rel == "." {
		return fmt.Errorf("%w: the workspace itself is not the caller's to make", driver.ErrInvalid)
	}
	if err = root.MkdirAll(rel, 0o755); err != nil {
		if name, yes := notADirectory(root, rel); yes {
			return fmt.Errorf("%w: %s is not a directory", driver.ErrInvalid, name)
		}
		return fileError(err)
	}
	return nil
}

// Remove deletes a file or a directory tree. A path that names nothing is not
// an error. The workspace itself is never removed, which keeps a host folder
// where it is.
func (d *Driver) Remove(ctx context.Context, id, p string) error {
	root, _, rel, err := d.entry(ctx, id, p, replaceName, true)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if rel == "." {
		return fmt.Errorf("%w: the workspace itself is not removable", driver.ErrInvalid)
	}
	return fileError(root.RemoveAll(rel))
}

// Move renames from onto the exact path to, within one area. A destination
// that is an existing directory is refused rather than nesting the source.
func (d *Driver) Move(ctx context.Context, id, from, to string) error {
	root, a, source, err := d.entry(ctx, id, from, replaceName, true)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	d.mu.Lock()
	r, err := d.load(id)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	b, destination, err := d.locate(r, to)
	if err == nil && b.host != a.host {
		err = fmt.Errorf("%w: a move stays inside one directory of the sandbox", driver.ErrInvalid)
	}
	if err == nil {
		err = d.contain(b, to, destination, replaceName)
	}
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

// parents creates the directories above rel.
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
// says whether it is something other than a directory.
func notADirectory(root *os.Root, rel string) (string, bool) {
	for above := rel; above != "." && above != "/"; above = path.Dir(above) {
		if info, err := root.Lstat(above); err == nil {
			return above, !info.IsDir()
		}
	}
	return "", false
}

// describe renders one entry as the contract's FileInfo.
func describe(p string, info fs.FileInfo) driver.FileInfo {
	size := info.Size()
	if info.IsDir() {
		size = 0
	}
	return driver.FileInfo{
		Name: path.Base(p), Path: p, Size: size,
		Mode: info.Mode() & (fs.ModePerm | fs.ModeDir), ModTime: info.ModTime().UTC(), IsDir: info.IsDir(),
	}
}

// fileError turns the filesystem's answers into the contract's.
func fileError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return missing(err)
	case errors.Is(err, driver.ErrTooLarge), errors.Is(err, driver.ErrInvalid):
		return err
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%w: %w", driver.ErrInvalid, err)
	}
	return err
}

// missingFile is a path a file operation did not find, worded by the path
// alone, "not found: AGENTS.md", which is what the host's own record of the
// operation shows. It is both the contract's ErrNotFound and the
// filesystem's own error.
type missingFile struct {
	path string
	err  error
}

func (e *missingFile) Error() string   { return driver.ErrNotFound.Error() + ": " + e.path }
func (e *missingFile) Unwrap() []error { return []error{driver.ErrNotFound, e.err} }

// missing words a path the filesystem did not find by the path it names,
// relative to the area it was looked for in, and keeps the filesystem's text
// where the error names no path.
func missing(err error) error {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	switch {
	case errors.As(err, &pathErr):
		return &missingFile{path: pathErr.Path, err: err}
	case errors.As(err, &linkErr):
		return &missingFile{path: linkErr.Old, err: err}
	}
	return fmt.Errorf("%w: %w", driver.ErrNotFound, err)
}

// contextReader ends a copy when its context does.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// ImportTar extracts an archive below a directory of the sandbox's own. It
// never writes into a host folder: an archive does not overwrite a person's
// folder in one call.
func (d *Driver) ImportTar(ctx context.Context, id, dest string, src io.Reader) error {
	base, a, rel, err := d.entry(ctx, id, dest, followLink, true)
	if err != nil {
		return err
	}
	defer func() { _ = base.Close() }()
	d.mu.Lock()
	r, err := d.load(id)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	if a.workspace && r.Host != "" {
		return fmt.Errorf("%w: an archive is not extracted into a host folder", driver.ErrUnsupported)
	}
	if err = base.MkdirAll(rel, 0o700); err != nil {
		return fileError(err)
	}
	root, err := base.OpenRoot(rel)
	if err != nil {
		return fileError(err)
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(src)
	var total int64
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if !fs.ValidPath(name) || strings.Contains(name, "\\") || strings.ContainsRune(name, 0) {
			return fmt.Errorf("%w: archive path", driver.ErrInvalid)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err = root.MkdirAll(name, 0o755); err != nil {
				return fileError(err)
			}
		case tar.TypeReg:
			if h.Size < 0 || h.Size > MaxArchiveBytes-total {
				return fmt.Errorf("%w: archive size", driver.ErrInvalid)
			}
			total += h.Size
			if err = root.MkdirAll(path.Dir(name), 0o700); err != nil {
				return fileError(err)
			}
			if err = importFile(ctx, root, name, os.FileMode(h.Mode)&0o777, tr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: archive entry type", driver.ErrInvalid)
		}
	}
}

func importFile(ctx context.Context, root *os.Root, name string, mode os.FileMode, src io.Reader) error {
	temp := ".cella-import-" + rand.Text()
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fileError(err)
	}
	defer func() { _ = root.Remove(temp) }()
	_, err = io.Copy(f, &contextReader{ctx: ctx, r: src})
	if err == nil {
		err = f.Chmod(mode)
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	return fileError(root.Rename(temp, name))
}

// ExportTar writes the named paths of the sandbox as one archive, each
// entry named relative to the directory it is in. Every path is held to the
// same grants and deny table as a read.
func (d *Driver) ExportTar(ctx context.Context, id string, paths []string, dst io.Writer) error {
	d.mu.Lock()
	r, err := d.load(id)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		paths = []string{r.Guest}
	}
	tw := tar.NewWriter(dst)
	for _, p := range paths {
		root, a, rel, err := d.entry(ctx, id, p, followLink, false)
		if err != nil {
			return err
		}
		err = fs.WalkDir(root.FS(), rel, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.denied(filepath.Join(a.host, filepath.FromSlash(name))) {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return nil
			}
			h, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			h.Name = name
			if err = tw.WriteHeader(h); err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			f, err := root.Open(name)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, &contextReader{ctx: ctx, r: f})
			return errors.Join(err, f.Close())
		})
		_ = root.Close()
		if err != nil {
			return fileError(err)
		}
	}
	return tw.Close()
}
