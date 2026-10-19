// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/hostsandbox"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/runtimetest"
)

// shim stands in for the sandbox runtime: a script that drops `--settings
// <file> --` and runs the command, with every other program the runtime needs
// resolved to one that exists. What it proves is the driver's bookkeeping;
// the isolation is the real runtime's, which the cases marked under srt run
// where it is installed.
func shim(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "srt")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nshift 3\nexec \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := newSandbox
	newSandbox = standIn(path)
	t.Cleanup(func() { newSandbox = previous })
	return path
}

// standIn builds the operating system's sandbox with the sandbox runtime
// resolved to the stand-in at path.
func standIn(path string) func(hostsandbox.Config) *hostsandbox.Driver {
	return func(c hostsandbox.Config) *hostsandbox.Driver {
		// The stand-in fakes srt, so the driver uses srt on every platform.
		c.Isolation = hostsandbox.IsolationSRT
		c.Look = func(name string) (string, error) {
			if name == "srt" {
				return path, nil
			}
			return "/bin/echo", nil
		}
		return hostsandbox.New(c)
	}
}

// tempDir is a temporary directory with its links resolved, which is how the
// operating system's sandbox names it.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// host is a person's computer in a temporary directory: a home holding the
// credentials the deny table names, a project folder granted writable, a docs
// folder granted read-only, and a file outside every grant.
type host struct {
	home, project, docs string
}

func newHost(t *testing.T) host {
	t.Helper()
	home := tempDir(t)
	h := host{home: home, project: filepath.Join(home, "project"), docs: filepath.Join(home, "docs")}
	for path, body := range map[string]string{
		filepath.Join(home, ".ssh", "id_ed25519"): "private key",
		filepath.Join(home, ".env"):               "SECRET=1",
		filepath.Join(home, "outside.txt"):        "outside",
		filepath.Join(h.project, "notes.txt"):     "notes",
		filepath.Join(h.project, "secret.txt"):    "withheld",
		filepath.Join(h.docs, "readme.txt"):       "readme",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// open builds a driver on the host with the options given, its data under a
// directory of its own.
func (h host) open(t *testing.T, o Options) *Driver {
	t.Helper()
	if o.DataDir == "" {
		o.DataDir = tempDir(t)
	}
	if o.Home == "" {
		o.Home = h.home
	}
	if o.Grants == nil {
		o.Grants = []Grant{{Path: h.project, Write: true}, {Path: h.docs}}
	}
	d, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// sandbox creates one sandbox and deletes it with the test.
func sandbox(t *testing.T, d *Driver, spec driver.CreateSpec) {
	t.Helper()
	if _, err := d.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create %s: %v", spec.ID, err)
	}
	t.Cleanup(func() { _ = d.Delete(context.Background(), spec.ID) })
}

// TestLocalConformance is spec 004's suite against the local driver with the
// sandbox runtime replaced by a stand-in, so it runs on every machine: each
// capability the driver does not declare is reported skipped.
func TestLocalConformance(t *testing.T) {
	shim(t)
	h := newHost(t)
	data := tempDir(t)
	runtimetest.Run(t, func(t *testing.T) driver.Driver {
		return h.open(t, Options{DataDir: data, Lookup: os.LookupEnv})
	}, runtimetest.Options{})
}

// TestLocalConformanceUnderSRT is the same suite under the real sandbox
// runtime srt, where it is installed: every command of every case runs
// inside srt's Seatbelt profile or bubblewrap. Where it is not installed it
// is skipped with what to install.
func TestLocalConformanceUnderSRT(t *testing.T) {
	withIsolation(t, hostsandbox.IsolationSRT)
	probe, err := New(Options{DataDir: tempDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Preflight(context.Background()); err != nil {
		t.Skipf("the sandbox runtime is not installed here, so the local driver is unchecked under it:\n%v", err)
	}
	data := tempDir(t)
	runtimetest.Run(t, func(t *testing.T) driver.Driver {
		d, err := New(Options{DataDir: data})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		return d
	}, runtimetest.Options{})
}

// TestLocalConformanceUnderSeatbelt is the same suite under macOS's own
// Seatbelt through hostsandbox's native driver, which needs nothing
// installed: every command of every case runs inside a profile the driver
// renders. It runs on macOS alone.
func TestLocalConformanceUnderSeatbelt(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is part of macOS")
	}
	withIsolation(t, hostsandbox.IsolationSeatbelt)
	probe, err := New(Options{DataDir: tempDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Preflight(context.Background()); err != nil {
		t.Fatalf("Seatbelt is not ready on this Mac: %v", err)
	}
	data := tempDir(t)
	runtimetest.Run(t, func(t *testing.T) driver.Driver {
		d, err := New(Options{DataDir: data})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		return d
	}, runtimetest.Options{})
}

// withIsolation builds every driver of the test with the isolation named,
// rather than the one hostsandbox picks for this program.
func withIsolation(t *testing.T, isolation hostsandbox.Isolation) {
	t.Helper()
	previous := newSandbox
	newSandbox = func(c hostsandbox.Config) *hostsandbox.Driver {
		c.Isolation = isolation
		return hostsandbox.New(c)
	}
	t.Cleanup(func() { newSandbox = previous })
}

// TestLocalRefusesRoot: a driver running as root refuses to start, and one
// on a platform with no sandbox it drives refuses with the platform named.
func TestLocalRefusesRoot(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Lookup: os.LookupEnv})
	d.euid = 0
	if err := d.Preflight(context.Background()); !errors.Is(err, ErrRunsAsRoot) {
		t.Errorf("a driver running as root answered %v", err)
	}
	d.euid, d.goos = 501, "windows"
	if err := d.Preflight(context.Background()); !errors.Is(err, driver.ErrUnsupported) || !strings.Contains(err.Error(), "windows") {
		t.Errorf("a driver on windows answered %v", err)
	}
	d.goos = "darwin"
	if err := d.Preflight(context.Background()); err != nil {
		t.Errorf("a driver with its runtime ready answered %v", err)
	}
}

// TestLocalRefusesSystemGrants: the root, the home directory, a system
// directory and a folder below one, a folder another user owns, a path that
// is not clean or not absolute, one that does not exist, a file, and one on
// the deny table are each refused as a grant; a home subfolder is granted.
func TestLocalRefusesSystemGrants(t *testing.T) {
	shim(t)
	h := newHost(t)
	file := filepath.Join(h.project, "notes.txt")
	for _, path := range []string{
		"/", h.home, "/usr", "/etc", "/usr/local/share", "/tmp",
		h.project + "/", "relative/path", filepath.Join(h.home, "absent"), file,
		filepath.Join(h.home, ".ssh"),
	} {
		if _, err := New(Options{DataDir: tempDir(t), Home: h.home, Grants: []Grant{{Path: path}}}); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("the grant %q answered %v", path, err)
		}
	}
	d := h.open(t, Options{})
	if host := d.Host(); len(host.Grants) != 2 || host.Grants[0].Path != h.project || !host.Grants[0].Write || host.Grants[1].Write {
		t.Errorf("the host reports %+v", host)
	}
	d.goos = "linux"
	if !d.system("/proc/1", true) || d.system("/home/dev", true) {
		t.Error("Linux's system directories read wrong")
	}
	for _, n := range []Network{{Mode: "open"}, {Mode: "everything"}} {
		if _, err := New(Options{DataDir: tempDir(t), Home: h.home, Network: n}); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("the network %+v answered %v", n, err)
		}
	}
	if _, err := New(Options{}); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a driver with no data directory answered %v", err)
	}
}

// TestLocalReadPathsRespectTheDenyTable: a read path under ~/.ssh, the home
// directory, a system directory and a relative path are refused at New; a
// toolchain folder is admitted; and a Deny entry inside a granted folder is
// unreadable through the file operations.
func TestLocalReadPathsRespectTheDenyTable(t *testing.T) {
	shim(t)
	h := newHost(t)
	for _, path := range []string{filepath.Join(h.home, ".ssh", "config"), h.home, "/usr", "relative"} {
		if _, err := New(Options{DataDir: tempDir(t), Home: h.home, ReadPaths: []string{path}}); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("the read path %q answered %v", path, err)
		}
	}
	toolchain := filepath.Join(h.home, ".rustup")
	if err := os.MkdirAll(toolchain, 0o700); err != nil {
		t.Fatal(err)
	}
	d := h.open(t, Options{ReadPaths: []string{toolchain}, Deny: []string{"~/project/secret.txt"}, Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_deny", Workspace: driver.Workspace{Path: h.project, Host: h.project}})
	if _, _, err := d.Open(context.Background(), "sbx_deny", filepath.Join(h.project, "secret.txt")); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a Deny entry inside the grant was read: %v", err)
	}
	if body, _, err := d.Open(context.Background(), "sbx_deny", filepath.Join(h.project, "notes.txt")); err != nil {
		t.Errorf("a granted file was refused: %v", err)
	} else {
		_ = body.Close()
	}
	if spec := d.stage(mustLoad(t, d, "sbx_deny"), "probe", []string{"true"}, nil, h.project); !slices.Contains(spec.Denied, filepath.Join(h.project, "secret.txt")) {
		t.Errorf("the stage does not deny the Deny entry: %v", spec.Denied)
	}
}

func mustLoad(t *testing.T, d *Driver, id string) record {
	t.Helper()
	r, err := d.load(id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
