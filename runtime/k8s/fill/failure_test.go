// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package fill

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// errDrawn is the failure a faulty system draws.
var errDrawn = errors.New("the node refused")

// faulty is the operating system's file system with one operation failing on
// the paths that end in at, or on every path where at is empty. The
// operation Read fails a file's content, Grow adds bytes to it, Write and
// Close fail the file a copy writes, and Full fails it with ENOSPC.
type faulty struct {
	osSystem
	op, at string
	err    error
}

func (f faulty) fails(op, name string) bool {
	return f.op == op && (f.at == "" || strings.HasSuffix(name, string(filepath.Separator)+f.at))
}

func (f faulty) failure() error {
	if f.err != nil {
		return f.err
	}
	return errDrawn
}

func (f faulty) Executable() (string, error) {
	if f.fails("Executable", "") {
		return "", f.failure()
	}
	return f.osSystem.Executable()
}

func (f faulty) Abs(path string) (string, error) {
	if f.fails("Abs", path) {
		return "", f.failure()
	}
	return f.osSystem.Abs(path)
}

func (f faulty) Lstat(name string) (fs.FileInfo, error) {
	if f.fails("Lstat", name) {
		return nil, f.failure()
	}
	return f.osSystem.Lstat(name)
}

func (f faulty) ReadDir(name string) ([]fs.DirEntry, error) {
	if f.fails("ReadDir", name) {
		return nil, f.failure()
	}
	return f.osSystem.ReadDir(name)
}

func (f faulty) Mkdir(name string, perm fs.FileMode) error {
	if f.fails("Mkdir", name) {
		return f.failure()
	}
	return f.osSystem.Mkdir(name, perm)
}

func (f faulty) Readlink(name string) (string, error) {
	if f.fails("Readlink", name) {
		return "", f.failure()
	}
	return f.osSystem.Readlink(name)
}

func (f faulty) Symlink(oldname, newname string) error {
	if f.fails("Symlink", newname) {
		return f.failure()
	}
	return f.osSystem.Symlink(oldname, newname)
}

func (f faulty) Open(name string) (io.ReadCloser, error) {
	switch {
	case f.fails("Open", name):
		return nil, f.failure()
	case f.fails("Read", name):
		return io.NopCloser(failingReader{f.failure()}), nil
	case f.fails("Grow", name):
		body, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(strings.NewReader(string(body) + "grown")), nil
	}
	return f.osSystem.Open(name)
}

func (f faulty) OpenFile(name string, flag int, perm fs.FileMode) (io.WriteCloser, error) {
	if f.fails("OpenFile", name) {
		return nil, f.failure()
	}
	out, err := f.osSystem.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	switch {
	case f.fails("Write", name):
		return failingWriter{out, f.failure(), nil}, nil
	case f.fails("Full", name):
		return failingWriter{out, syscall.ENOSPC, nil}, nil
	case f.fails("Close", name):
		return failingWriter{out, nil, f.failure()}, nil
	}
	return out, nil
}

func (f faulty) Chmod(name string, mode fs.FileMode) error {
	if f.fails("Chmod", name) {
		return f.failure()
	}
	return f.osSystem.Chmod(name, mode)
}

func (f faulty) Chtimes(name string, mtime time.Time) error {
	if f.fails("Chtimes", name) {
		return f.failure()
	}
	return f.osSystem.Chtimes(name, mtime)
}

func (f faulty) RemoveAll(name string) error {
	if f.fails("RemoveAll", name) {
		return f.failure()
	}
	return f.osSystem.RemoveAll(name)
}

// failingReader fails every read.
type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// failingWriter fails its writes with write and its close with close, where
// each is set, and closes the file beneath either way.
type failingWriter struct {
	file         io.WriteCloser
	write, close error
}

func (w failingWriter) Write(p []byte) (int, error) {
	if w.write != nil {
		return 0, w.write
	}
	return w.file.Write(p)
}

func (w failingWriter) Close() error {
	err := w.file.Close()
	if w.close != nil {
		return w.close
	}
	return err
}

// failureTree is a source with a file, a link and a directory holding a
// file, and a destination where a previous fill left a file named stale
// where the source's directory goes.
func failureTree(t *testing.T) (src, dst string) {
	t.Helper()
	src = t.TempDir()
	for name, body := range map[string]string{"file": "abc", "dir/inner": "x"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(src, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("file", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "stale"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst = filepath.Join(t.TempDir(), "volume")
	if err := os.MkdirAll(filepath.Join(dst, "stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	return src, dst
}

// TestEveryFailureEndsTheCopy: each operation of a copy that fails ends it
// with an error naming the step, a file that grew past the cap or a volume
// that filled up is ErrTooLarge, and the failures that are not failures are
// counted and passed: an entry gone before it was read, and a directory or a
// file this process may not read.
func TestEveryFailureEndsTheCopy(t *testing.T) {
	for _, tc := range []struct {
		op, at string
		err    error
		want   string
	}{
		{"Abs", "", nil, "the node refused"},
		{"ReadDir", "dir", nil, "reading"},
		{"Lstat", "file", nil, "reading"},
		{"Lstat", "inner", nil, "reading"},
		{"Mkdir", "dir", nil, "creating"},
		{"Readlink", "link", nil, "reading the link"},
		{"Symlink", "link", nil, "creating the link"},
		{"Open", "file", nil, "opening"},
		{"OpenFile", "file", nil, "creating"},
		{"Read", "file", nil, "writing"},
		{"Write", "file", nil, "writing"},
		{"Close", "file", nil, "closing"},
		{"Chmod", "file", nil, "setting the mode"},
		{"Chtimes", "file", nil, "setting the time"},
		{"Chmod", "dir", nil, "setting the mode"},
		{"Chtimes", "dir", nil, "setting the time"},
		{"RemoveAll", "volume/stale", nil, "replacing"},
	} {
		t.Run(tc.op+" "+tc.at, func(t *testing.T) {
			src, dst := failureTree(t)
			_, err := copyTree(faulty{op: tc.op, at: tc.at, err: tc.err}, src, dst, Options{})
			if err == nil || !strings.Contains(err.Error(), tc.want) || errors.Is(err, ErrTooLarge) {
				t.Fatalf("a failed %s answered %v, want an error naming %q", tc.op, err, tc.want)
			}
		})
	}
	// A clearPath that cannot read what is there fails too: the Lstat of a
	// target, which the copy asks of the destination before it writes.
	src, dst := failureTree(t)
	if _, err := copyTree(faulty{op: "Lstat", at: "volume/stale"}, src, dst, Options{}); err == nil ||
		!strings.Contains(err.Error(), "reading") {
		t.Fatalf("a target that cannot be read was written over: %v", err)
	}

	for _, op := range []string{"Grow", "Full"} {
		src, dst := failureTree(t)
		_, err := copyTree(faulty{op: op, at: "file"}, src, dst, Options{MaxBytes: 1 << 20})
		if op == "Grow" {
			_, err = copyTree(faulty{op: op, at: "file"}, src, filepath.Join(t.TempDir(), "v"), Options{MaxBytes: 4})
		}
		if !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s answered %v, want ErrTooLarge", op, err)
		}
	}

	for _, tc := range []struct {
		op, at     string
		unreadable int
	}{
		{"Lstat", "file", 0},
		{"ReadDir", "dir", 1},
		{"Lstat", "file", 1},
		{"Open", "file", 1},
	} {
		src, dst := failureTree(t)
		drawn := fs.ErrPermission
		if tc.unreadable == 0 {
			drawn = fs.ErrNotExist
		}
		result, err := copyTree(faulty{op: tc.op, at: tc.at, err: drawn}, src, dst, Options{})
		if err != nil || result.Unreadable != tc.unreadable {
			t.Errorf("%s %s with %v answered %+v %v, want %d unreadable and no error", tc.op, tc.at, drawn, result, err, tc.unreadable)
		}
	}
}

// TestEveryFailureEndsTheStage: each step of a stage that fails ends it with
// an error naming the step.
func TestEveryFailureEndsTheStage(t *testing.T) {
	for _, tc := range []struct{ op, want string }{
		{"Executable", "finding this executable"},
		{"Open", "opening this executable"},
		{"OpenFile", "creating"},
		{"Write", "writing"},
		{"Close", "closing"},
		{"Chmod", "the node refused"},
	} {
		dst := filepath.Join(t.TempDir(), "cellad")
		if err := stage(faulty{op: tc.op}, dst); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("a failed %s answered %v, want an error naming %q", tc.op, err, tc.want)
		}
	}
}
