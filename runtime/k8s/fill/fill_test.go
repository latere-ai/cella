// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package fill

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// tree writes a source tree: nested directories with their own modes, files
// with theirs, a symbolic link, a hard link and a named pipe.
func tree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "usr", "bin"), 0o755))
	must(os.WriteFile(filepath.Join(src, "usr", "bin", "grader"), []byte("#!/bin/sh\necho ok\n"), 0o755))
	must(os.WriteFile(filepath.Join(src, "usr", "bin", "data.txt"), []byte("data"), 0o444))
	must(os.Mkdir(filepath.Join(src, "locked"), 0o755))
	must(os.WriteFile(filepath.Join(src, "locked", "inside"), []byte("x"), 0o644))
	must(os.Chmod(filepath.Join(src, "locked"), 0o555))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(src, "locked"), 0o755) })
	must(os.Symlink("usr/bin/grader", filepath.Join(src, "run")))
	must(os.Symlink("/etc/passwd", filepath.Join(src, "outside")))
	must(os.Link(filepath.Join(src, "usr", "bin", "grader"), filepath.Join(src, "usr", "bin", "grader-again")))
	must(syscall.Mkfifo(filepath.Join(src, "pipe"), 0o644))
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	must(os.Chtimes(filepath.Join(src, "usr", "bin", "grader"), old, old))
	return src
}

// TestCopyKeepsWhatAnImageHolds: directories and files keep their modes and
// times, a symbolic link keeps its target and is not followed, a hard link
// is a file of its own, and a named pipe is left out.
func TestCopyKeepsWhatAnImageHolds(t *testing.T) {
	src := tree(t)
	dst := filepath.Join(t.TempDir(), "volume")
	result, err := Copy(src, dst, Options{MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]fs.FileMode{
		"usr/bin/grader":       0o755,
		"usr/bin/grader-again": 0o755,
		"usr/bin/data.txt":     0o444,
		"locked/inside":        0o644,
	} {
		info, err := os.Lstat(filepath.Join(dst, path))
		if err != nil {
			t.Fatalf("%s was not copied: %v", path, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != want {
			t.Errorf("%s is %v, want a regular file of mode %v", path, info.Mode(), want)
		}
	}
	if info, err := os.Lstat(filepath.Join(dst, "locked")); err != nil || info.Mode().Perm() != 0o555 {
		t.Errorf("a directory a sandbox may not write lost its mode: %v %v", info, err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dst, "locked"), 0o755) })
	if info, err := os.Lstat(filepath.Join(dst, "usr", "bin", "grader")); err != nil ||
		!info.ModTime().Equal(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("a file's time was not kept: %v %v", info, err)
	}
	for link, target := range map[string]string{"run": "usr/bin/grader", "outside": "/etc/passwd"} {
		got, err := os.Readlink(filepath.Join(dst, link))
		if err != nil || got != target {
			t.Errorf("the link %s reads %q %v, want %q", link, got, err, target)
		}
	}
	a, _ := os.Stat(filepath.Join(dst, "usr", "bin", "grader"))
	b, _ := os.Stat(filepath.Join(dst, "usr", "bin", "grader-again"))
	if os.SameFile(a, b) {
		t.Error("a hard link was carried as a link; it is a file of its own")
	}
	if _, err := os.Lstat(filepath.Join(dst, "pipe")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the named pipe was copied: %v", err)
	}
	if result.Files != 4 || result.Links != 2 || result.Skipped != 1 || result.Unreadable != 0 {
		t.Errorf("the result is %+v", result)
	}
	if !strings.Contains(result.String(), "copied 4 files") {
		t.Errorf("the summary line is %q", result.String())
	}
}

// TestCopyStopsAtTheCap: content past the volume's size ends the copy with
// ErrTooLarge, and content at it does not.
func TestCopyStopsAtTheCap(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a"), make([]byte, 600), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "b"), make([]byte, 600), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(src, filepath.Join(t.TempDir(), "v"), Options{MaxBytes: 1000}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a copy past the cap answered %v, want ErrTooLarge", err)
	}
	result, err := Copy(src, filepath.Join(t.TempDir(), "v"), Options{MaxBytes: 1200})
	if err != nil || result.Bytes != 1200 {
		t.Fatalf("a copy at the cap answered %+v %v", result, err)
	}
}

// TestCopyLeavesOutAnotherDevice: an entry the device lookup puts on another
// device, a mount, is left out with everything below it, and so is the
// destination where it lies inside the source.
func TestCopyLeavesOutAnotherDevice(t *testing.T) {
	src := t.TempDir()
	for _, dir := range []string{"proc/1", "etc", "cella-volume"} {
		if err := os.MkdirAll(filepath.Join(src, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"proc/1/status", "etc/hosts", "etc/os-release"} {
		if err := os.WriteFile(filepath.Join(src, file), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mounts := map[string]bool{filepath.Join(src, "proc"): true, filepath.Join(src, "etc", "hosts"): true}
	device := func(path string, _ fs.FileInfo) (uint64, error) {
		if mounts[path] {
			return 2, nil
		}
		return 1, nil
	}
	dst := filepath.Join(src, "cella-volume")
	result, err := Copy(src, dst, Options{Device: device})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{"etc/os-release": true, "etc/hosts": false, "proc": false, "cella-volume": false} {
		_, err := os.Lstat(filepath.Join(dst, path))
		if got := err == nil; got != want {
			t.Errorf("%s copied %v, want %v", path, got, want)
		}
	}
	if result.Skipped != 3 {
		t.Errorf("left out %d entries, want the two mounts and the destination: %+v", result.Skipped, result)
	}
}

// TestCopySkipsWhatItCannotRead: a file and a directory this process cannot
// read are counted and left out, and the copy goes on.
func TestCopySkipsWhatItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file, so nothing here is unreadable")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "shadow"), []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "root"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(src, "root"), 0o755) })
	if err := os.WriteFile(filepath.Join(src, "readable"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "v")
	result, err := Copy(src, dst, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Unreadable != 2 || result.Files != 1 {
		t.Fatalf("the result is %+v, want two unreadable entries and one file", result)
	}
	// A directory that lists and cannot be searched names entries that
	// cannot be read.
	listed := t.TempDir()
	if err := os.Mkdir(filepath.Join(listed, "names"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(listed, "names", "hidden"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(listed, "names"), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(listed, "names"), 0o755) })
	listedDst := filepath.Join(t.TempDir(), "v")
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(listedDst, "names"), 0o755) })
	if result, err := Copy(listed, listedDst, Options{}); err != nil || result.Unreadable != 1 {
		t.Fatalf("a listed directory that cannot be searched = %+v %v, want one unreadable entry", result, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "shadow")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an unreadable file was written: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dst, "root"), 0o755) })
}

// TestCopyRefusesARootThatIsNoDirectory: a source that is a file, or absent,
// is an error before anything is written.
func TestCopyRefusesARootThatIsNoDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{file, filepath.Join(t.TempDir(), "absent")} {
		if _, err := Copy(src, filepath.Join(t.TempDir(), "v"), Options{}); err == nil {
			t.Errorf("Copy of %s answered nil", src)
		}
	}
	failing := func(string, fs.FileInfo) (uint64, error) { return 0, errors.New("no device") }
	if _, err := Copy(t.TempDir(), filepath.Join(t.TempDir(), "v"), Options{Device: failing}); err == nil {
		t.Error("a root whose device cannot be read was copied")
	}
}

// TestStageWritesAnExecutable: the staged program is this executable, and
// it runs.
func TestStageWritesAnExecutable(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "cellad")
	if err := Stage(dst); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dst)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("the staged program is %v %v, want mode 0755", info, err)
	}
	// The test binary lists its tests and exits zero, which proves the copy
	// is a whole executable and not a truncated file.
	if out, err := exec.Command(dst, "-test.list", "TestStageWritesAnExecutable").CombinedOutput(); err != nil ||
		!strings.Contains(string(out), "TestStageWritesAnExecutable") {
		t.Fatalf("the staged program did not run: %v %s", err, out)
	}
	if err := Stage(filepath.Join(t.TempDir(), "absent", "cellad")); err == nil {
		t.Error("a stage into a directory that does not exist answered nil")
	}
}

// TestCopyReportsWhatItCannotWrite: a destination that cannot be made and
// an entry whose device cannot be read each end the copy with an error, and
// what a previous fill left half written is replaced.
func TestCopyReportsWhatItCannotWrite(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(src, filepath.Join(blocker, "v"), Options{}); err == nil {
		t.Error("a destination below a file was made")
	}
	// A previous fill left a read-only file where the file goes, a
	// directory where a link goes, and a file where a directory goes.
	if err := os.Symlink("file", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(src, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	taken := t.TempDir()
	if err := os.WriteFile(filepath.Join(taken, "file"), []byte("stale"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(taken, "link", "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taken, "dir"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Copy(src, taken, Options{}); err != nil {
		t.Fatalf("a copy over a half-written volume = %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(taken, "file")); err != nil || string(body) != "x" {
		t.Errorf("the stale file reads %q %v", body, err)
	}
	if target, err := os.Readlink(filepath.Join(taken, "link")); err != nil || target != "file" {
		t.Errorf("the link reads %q %v", target, err)
	}
	if info, err := os.Lstat(filepath.Join(taken, "dir")); err != nil || !info.IsDir() {
		t.Errorf("the directory is %v %v", info, err)
	}
	calls := 0
	device := func(string, fs.FileInfo) (uint64, error) {
		calls++
		if calls > 1 {
			return 0, errors.New("no device")
		}
		return 1, nil
	}
	if _, err := Copy(src, filepath.Join(t.TempDir(), "v"), Options{Device: device}); err == nil {
		t.Error("an entry whose device cannot be read was copied")
	}
}

// TestStatDeviceNeedsAStatus: a file information with no system status
// names no device.
func TestStatDeviceNeedsAStatus(t *testing.T) {
	if _, err := statDevice("/x", bareInfo{}); err == nil {
		t.Fatal("a status with no device answered one")
	}
	info, err := os.Lstat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := statDevice("/x", info); err != nil {
		t.Fatalf("a real status = %v", err)
	}
}

// bareInfo is a file information whose system status is nil.
type bareInfo struct{}

func (bareInfo) Name() string       { return "x" }
func (bareInfo) Size() int64        { return 0 }
func (bareInfo) Mode() fs.FileMode  { return 0 }
func (bareInfo) ModTime() time.Time { return time.Time{} }
func (bareInfo) IsDir() bool        { return false }
func (bareInfo) Sys() any           { return nil }
