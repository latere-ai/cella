// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package fill is the program the Kubernetes driver runs to fill a volume
// from an image (spec 019): `cellad fill`, staged into the fill Pod from the
// fill image and run inside a container of the source image, so an image
// with no shell and no tar is filled as well as one with both.
//
// Copy walks the container's own root file system and writes it into the
// volume's mount. Every path on another device than the root is left out,
// which drops /proc, /sys, /dev, the files the kubelet binds over the image
// (/etc/hosts and its siblings) and the mounts themselves, the volume's
// included, so what lands is the image's content and nothing the runtime
// added. The package reaches the standard library alone.
package fill

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// The two modes of `cellad fill` and the flag of the second, which the
// driver writes into the fill Pod and the command reads, named once here.
const (
	ModeStage    = "stage"
	ModeCopy     = "copy"
	FlagMaxBytes = "max-bytes"
)

// ErrTooLarge is a copy whose regular files hold more bytes than the volume
// may: the cap is the volume's size, so content past it would not fit.
var ErrTooLarge = errors.New("the source is larger than the volume")

// Stage copies the running executable to dst with mode 0755. It is the fill
// Pod's init container, run from the fill image, which leaves the program in
// a directory the source image's container then runs it from.
func Stage(dst string) error { return stage(osSystem{}, dst) }

func stage(sys system, dst string) error {
	self, err := sys.Executable()
	if err != nil {
		return fmt.Errorf("finding this executable: %w", err)
	}
	src, err := sys.Open(self)
	if err != nil {
		return fmt.Errorf("opening this executable: %w", err)
	}
	defer func() { _ = src.Close() }()
	out, err := sys.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", dst, err)
	}
	// The mode is set again after the write, since the process's umask
	// narrows the one OpenFile was given.
	return sys.Chmod(dst, 0o755)
}

// Options are what one copy is bounded by. Device reads the device a path
// lives on; nil reads it from the file's own status, which a test replaces
// to stand for a mount.
type Options struct {
	MaxBytes int64
	Device   func(path string, info fs.FileInfo) (uint64, error)
}

// Result is what one copy did: the files and bytes it wrote, the links it
// made, and the entries it left out because this process could not read
// them, which the sandbox's user could not read either.
type Result struct {
	Files      int
	Bytes      int64
	Links      int
	Dirs       int
	Unreadable int
	Skipped    int
}

// Copy writes the tree at src into dst. Directories keep their mode,
// regular files their mode and content, symbolic links their target and are
// never followed; a hard link is copied as a file of its own; a socket, a
// device and a named pipe are left out. An entry on another device than src
// is left out with everything below it, and so is dst itself where it lies
// inside src. A regular file that would take the total past MaxBytes ends
// the copy with ErrTooLarge.
func Copy(src, dst string, o Options) (Result, error) { return copyTree(osSystem{}, src, dst, o) }

func copyTree(sys system, src, dst string, o Options) (Result, error) {
	device := o.Device
	if device == nil {
		device = statDevice
	}
	src, err := sys.Abs(src)
	if err != nil {
		return Result{}, err
	}
	dst, err = sys.Abs(dst)
	if err != nil {
		return Result{}, err
	}
	rootInfo, err := sys.Lstat(src)
	if err != nil {
		return Result{}, fmt.Errorf("reading the source root: %w", err)
	}
	if !rootInfo.IsDir() {
		return Result{}, fmt.Errorf("the source root %s is not a directory", src)
	}
	rootDevice, err := device(src, rootInfo)
	if err != nil {
		return Result{}, fmt.Errorf("reading the source root's device: %w", err)
	}
	if err := sys.MkdirAll(dst, 0o755); err != nil {
		return Result{}, fmt.Errorf("creating the destination: %w", err)
	}
	c := &copier{sys: sys, src: src, dst: dst, max: o.MaxBytes, device: device, root: rootDevice}
	if err := c.walk(src); err != nil {
		return c.result, err
	}
	// A directory's own mode and time are set once everything below it is
	// written, deepest first, so a directory a sandbox may not write is
	// still written by the copy and the copy does not move its time.
	for _, d := range slices.Backward(c.dirs) {
		if err := c.sys.Chmod(d.path, d.mode); err != nil {
			return c.result, fmt.Errorf("setting the mode of %s: %w", d.path, err)
		}
		if err := c.sys.Chtimes(d.path, d.mtime); err != nil {
			return c.result, fmt.Errorf("setting the time of %s: %w", d.path, err)
		}
	}
	return c.result, nil
}

// statDevice is the device a file's own status names.
func statDevice(path string, info fs.FileInfo) (uint64, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%s carries no device in its status", path)
	}
	return uint64(st.Dev), nil //nolint:unconvert // the field is int32 on some systems
}

type copier struct {
	sys      system
	src, dst string
	max      int64
	device   func(string, fs.FileInfo) (uint64, error)
	root     uint64
	result   Result
	dirs     []dirMode
}

// dirMode is a directory whose mode and time are set at the end.
type dirMode struct {
	path  string
	mode  fs.FileMode
	mtime time.Time
}

// walk copies every entry below dir, which is a directory of src on the root
// device.
func (c *copier) walk(dir string) error {
	entries, err := c.sys.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			c.result.Unreadable++
			return nil
		}
		return fmt.Errorf("reading %s: %w", dir, err)
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if within(path, c.dst) {
			c.result.Skipped++
			continue
		}
		info, err := c.sys.Lstat(path)
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				c.result.Unreadable++
				continue
			}
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("reading %s: %w", path, err)
		}
		dev, err := c.device(path, info)
		if err != nil {
			return fmt.Errorf("reading the device of %s: %w", path, err)
		}
		if dev != c.root {
			c.result.Skipped++
			continue
		}
		if err := c.one(path, info); err != nil {
			return err
		}
	}
	return nil
}

// one copies a single entry, and for a directory everything below it.
func (c *copier) one(path string, info fs.FileInfo) error {
	rel, err := filepath.Rel(c.src, path)
	if err != nil {
		return err
	}
	target := filepath.Join(c.dst, rel)
	mode := info.Mode()
	// A fill the cluster ran again over a volume a previous one left half
	// written replaces what it finds: the volume is this fill's alone.
	if err := c.clearPath(target, mode.IsDir()); err != nil {
		return err
	}
	switch {
	case mode.IsDir():
		if err := c.sys.Mkdir(target, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("creating %s: %w", target, err)
		}
		c.result.Dirs++
		c.dirs = append(c.dirs, dirMode{path: target, mode: permissions(mode), mtime: info.ModTime()})
		return c.walk(path)
	case mode&fs.ModeSymlink != 0:
		link, err := c.sys.Readlink(path)
		if err != nil {
			return fmt.Errorf("reading the link %s: %w", path, err)
		}
		if err := c.sys.Symlink(link, target); err != nil {
			return fmt.Errorf("creating the link %s: %w", target, err)
		}
		c.result.Links++
		return nil
	case mode.IsRegular():
		return c.file(path, target, info)
	default:
		// A socket, a device or a named pipe is the runtime's and not
		// content a volume holds.
		c.result.Skipped++
		return nil
	}
}

// file copies one regular file, held to the byte cap.
func (c *copier) file(path, target string, info fs.FileInfo) error {
	if c.max > 0 && c.result.Bytes+info.Size() > c.max {
		return fmt.Errorf("%w: %s takes the content past %d bytes", ErrTooLarge, path, c.max)
	}
	in, err := c.sys.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			c.result.Unreadable++
			return nil
		}
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = in.Close() }()
	out, err := c.sys.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating %s: %w", target, err)
	}
	// A file that grew since it was read is held to the cap as well: one
	// byte past what remains is enough to know.
	var source io.Reader = in
	if c.max > 0 {
		source = io.LimitReader(in, c.max-c.result.Bytes+1)
	}
	n, err := io.Copy(out, source)
	if err != nil {
		_ = out.Close()
		if errors.Is(err, syscall.ENOSPC) {
			return fmt.Errorf("%w: the volume filled up writing %s", ErrTooLarge, target)
		}
		return fmt.Errorf("writing %s: %w", target, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", target, err)
	}
	c.result.Bytes += n
	if c.max > 0 && c.result.Bytes > c.max {
		return fmt.Errorf("%w: %s takes the content past %d bytes", ErrTooLarge, path, c.max)
	}
	if err := c.sys.Chmod(target, permissions(info.Mode())); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", target, err)
	}
	if err := c.sys.Chtimes(target, info.ModTime()); err != nil {
		return fmt.Errorf("setting the time of %s: %w", target, err)
	}
	c.result.Files++
	return nil
}

// clearPath removes what a previous fill left at target, unless it is a
// directory and a directory is what goes there.
func (c *copier) clearPath(target string, dir bool) error {
	info, err := c.sys.Lstat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("reading %s: %w", target, err)
	case dir && info.IsDir():
		return nil
	}
	if err := c.sys.RemoveAll(target); err != nil {
		return fmt.Errorf("replacing %s: %w", target, err)
	}
	return nil
}

// permissions is the part of a mode a copy keeps: the permission bits and
// the setuid, setgid and sticky bits.
func permissions(mode fs.FileMode) fs.FileMode {
	return mode.Perm() | mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)
}

// within reports whether path is dir or below it.
func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// String is the one line a copy prints when it ends.
func (r Result) String() string {
	return fmt.Sprintf("copied %d files (%d bytes), %d links and %d directories; left out %d unreadable and %d on another device or of another type",
		r.Files, r.Bytes, r.Links, r.Dirs, r.Unreadable, r.Skipped)
}

// system is the file system a stage and a copy read and write: the
// operating system's, which a test replaces with one that fails a named
// operation, so the answer to each failure a node can produce is held by a
// test whoever runs it, root included.
type system interface {
	Executable() (string, error)
	Abs(path string) (string, error)
	Lstat(name string) (fs.FileInfo, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	Mkdir(name string, perm fs.FileMode) error
	MkdirAll(name string, perm fs.FileMode) error
	Readlink(name string) (string, error)
	Symlink(oldname, newname string) error
	Open(name string) (io.ReadCloser, error)
	OpenFile(name string, flag int, perm fs.FileMode) (io.WriteCloser, error)
	Chmod(name string, mode fs.FileMode) error
	Chtimes(name string, mtime time.Time) error
	RemoveAll(name string) error
}

// osSystem is the operating system's file system.
type osSystem struct{}

func (osSystem) Executable() (string, error)                { return os.Executable() }
func (osSystem) Abs(path string) (string, error)            { return filepath.Abs(path) }
func (osSystem) Lstat(name string) (fs.FileInfo, error)     { return os.Lstat(name) }
func (osSystem) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (osSystem) Mkdir(name string, perm fs.FileMode) error  { return os.Mkdir(name, perm) }
func (osSystem) MkdirAll(name string, perm fs.FileMode) error {
	return os.MkdirAll(name, perm)
}
func (osSystem) Readlink(name string) (string, error)      { return os.Readlink(name) }
func (osSystem) Symlink(oldname, newname string) error     { return os.Symlink(oldname, newname) }
func (osSystem) Open(name string) (io.ReadCloser, error)   { return os.Open(name) }
func (osSystem) Chmod(name string, mode fs.FileMode) error { return os.Chmod(name, mode) }
func (osSystem) RemoveAll(name string) error               { return os.RemoveAll(name) }
func (osSystem) OpenFile(name string, flag int, perm fs.FileMode) (io.WriteCloser, error) {
	return os.OpenFile(name, flag, perm)
}

// Chtimes sets a file's access and modification times to one instant: a
// copy keeps the source's modification time, and an image carries no access
// time worth keeping.
func (osSystem) Chtimes(name string, mtime time.Time) error { return os.Chtimes(name, mtime, mtime) }
