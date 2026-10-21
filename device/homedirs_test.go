// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"latere.ai/x/pkg/hostsandbox"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime/local"
)

// aboveHome is a root folder the test's user owns with the home directory
// below it, root/home, so a grant of root covers the home as a grant of a
// folder above a person's home does.
func aboveHome(t *testing.T) (root, home string) {
	t.Helper()
	root = resolvedTemp(t)
	home = filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return root, home
}

// requireCreated fails unless dir is a directory, not a link, that this
// process's user owns with mode 0700.
func requireCreated(t *testing.T, dir string) {
	t.Helper()
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("%s was not created: %v", dir, err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("%s is %v, want a directory of mode 0700", dir, info.Mode())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		t.Errorf("%s belongs to uid %d, not this process's %d", dir, stat.Uid, os.Geteuid())
	}
}

// requireAbsent fails unless nothing at all is at path.
func requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (%v), and nothing should have been created there", path, err)
	}
}

func TestHomeFoldersCreatedForAGrantAboveHome(t *testing.T) {
	root, home := aboveHome(t)
	if err := createHomeFolders(home, []local.Grant{{Path: root, Write: true}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range homeFolders {
		requireCreated(t, filepath.Join(home, name))
	}
	// Created again, nothing changes and nothing is refused.
	if err := createHomeFolders(home, []local.Grant{{Path: root, Write: true}}); err != nil {
		t.Errorf("a second start refused the folders it created: %v", err)
	}
}

func TestHomeFoldersCoverTheHomeAsWrittenOrResolved(t *testing.T) {
	t.Run("the home itself", func(t *testing.T) {
		_, home := aboveHome(t)
		if err := createHomeFolders(home, []local.Grant{{Path: home, Write: true}}); err != nil {
			t.Fatal(err)
		}
		requireCreated(t, filepath.Join(home, ".config"))
	})
	t.Run("a link to a folder above it", func(t *testing.T) {
		root, home := aboveHome(t)
		link := filepath.Join(resolvedTemp(t), "root")
		if err := os.Symlink(root, link); err != nil {
			t.Fatal(err)
		}
		if err := createHomeFolders(home, []local.Grant{{Path: link, Write: true}}); err != nil {
			t.Fatal(err)
		}
		requireCreated(t, filepath.Join(home, ".docker"))
	})
	t.Run("the root", func(t *testing.T) {
		_, home := aboveHome(t)
		if !coversHome(home, []local.Grant{{Path: "/", Write: true}}) {
			t.Error("a grant of / does not cover the home")
		}
	})
}

func TestHomeFoldersLeaveWhatIsThere(t *testing.T) {
	t.Run("a directory", func(t *testing.T) {
		root, home := aboveHome(t)
		config := filepath.Join(home, ".config")
		if err := os.Mkdir(config, 0o755); err != nil {
			t.Fatal(err)
		}
		// Mkdir's mode passes through the umask; the test sets it outright.
		if err := os.Chmod(config, 0o755); err != nil {
			t.Fatal(err)
		}
		kept := filepath.Join(config, "tool.toml")
		if err := os.WriteFile(kept, []byte("kept\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := createHomeFolders(home, []local.Grant{{Path: root, Write: true}}); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(config)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o755 {
			t.Errorf("the present ~/.config changed: %v, %v", info, err)
		}
		if raw, err := os.ReadFile(kept); err != nil || string(raw) != "kept\n" {
			t.Errorf("a file in the present ~/.config reads %q, %v", raw, err)
		}
		requireCreated(t, filepath.Join(home, ".docker"))
	})
	t.Run("a link", func(t *testing.T) {
		root, home := aboveHome(t)
		// ~/.config leads to a directory elsewhere and ~/.docker to nowhere.
		elsewhere := filepath.Join(resolvedTemp(t), "dotconfig")
		if err := os.Mkdir(elsewhere, 0o700); err != nil {
			t.Fatal(err)
		}
		nowhere := filepath.Join(resolvedTemp(t), "dotdocker")
		links := map[string]string{".config": elsewhere, ".docker": nowhere}
		for name, target := range links {
			if err := os.Symlink(target, filepath.Join(home, name)); err != nil {
				t.Fatal(err)
			}
		}
		if err := createHomeFolders(home, []local.Grant{{Path: root, Write: true}}); err != nil {
			t.Fatalf("a link at the name refused the grant: %v", err)
		}
		for name, target := range links {
			if got, err := os.Readlink(filepath.Join(home, name)); err != nil || got != target {
				t.Errorf("~/%s is no longer the link to %s: %q, %v", name, target, got, err)
			}
		}
		if entries, err := os.ReadDir(elsewhere); err != nil || len(entries) != 0 {
			t.Errorf("something was created through the link, in %s: %v, %v", elsewhere, entries, err)
		}
		requireAbsent(t, nowhere)
	})
	t.Run("a file", func(t *testing.T) {
		root, home := aboveHome(t)
		file := filepath.Join(home, ".config")
		if err := os.WriteFile(file, []byte("not a folder\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := createHomeFolders(home, []local.Grant{{Path: root, Write: true}}); err != nil {
			t.Fatalf("a file at the name refused the grant: %v", err)
		}
		info, err := os.Lstat(file)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("the file at ~/.config is now %v, %v", info, err)
		}
		if raw, err := os.ReadFile(file); err != nil || string(raw) != "not a folder\n" {
			t.Errorf("the file at ~/.config reads %q, %v", raw, err)
		}
		requireCreated(t, filepath.Join(home, ".docker"))
	})
}

func TestHomeFoldersRefuseAFailedCreate(t *testing.T) {
	// A home directory that is not there makes the first create fail, for
	// any user, root included.
	root := resolvedTemp(t)
	home := filepath.Join(root, "missing")
	err := createHomeFolders(home, []local.Grant{{Path: root, Write: true}})
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != CodeHomeFolderFailed {
		t.Fatalf("a failed create answered %v", err)
	}
	folder := filepath.Join(home, ".config")
	if !strings.Contains(refusal.Detail, folder) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refusal does not name %s and its cause: %q", folder, refusal.Detail)
	}
	if !strings.Contains(refusal.Message, "~/.config or ~/.docker") || refusal.Hint == "" {
		t.Errorf("the refusal reads %+v", refusal)
	}
	requireAbsent(t, home)
}

func TestHomeFoldersNotCreatedForAGrantInsideHome(t *testing.T) {
	root, home := aboveHome(t)
	project := filepath.Join(home, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, grants := range [][]local.Grant{
		nil,
		{{Path: project, Write: true}},
		// Read-only above the home: a session creates nothing there.
		{{Path: root}},
		{{Path: resolvedTemp(t), Write: true}},
		// A grant that does not resolve; the driver refuses it.
		{{Path: filepath.Join(root, "gone"), Write: true}},
	} {
		if err := createHomeFolders(home, grants); err != nil {
			t.Fatalf("grants %v: %v", grants, err)
		}
		for _, name := range homeFolders {
			requireAbsent(t, filepath.Join(home, name))
		}
	}
}

// TestHomeFoldersAreTheDenyTablesParents holds the list to the sandbox's own
// table: every always-denied path below a folder of the home directory,
// rather than directly in it, is below one the engine creates.
func TestHomeFoldersAreTheDenyTablesParents(t *testing.T) {
	for _, entry := range hostsandbox.AlwaysDenyRead {
		rest, ok := strings.CutPrefix(entry, "~/")
		if !ok {
			continue
		}
		first, below, nested := strings.Cut(rest, "/")
		if nested && below != "" && !slices.Contains(homeFolders, first) {
			t.Errorf("%s is always denied below ~/%s, which the engine does not create", entry, first)
		}
	}
}

// homeAbovePaths are the engine's paths with the home directory below a
// folder the test grants, and the configuration and cache outside the home,
// so that nothing but the engine's own create puts ~/.config or ~/.docker
// there.
func homeAbovePaths(t *testing.T, home string) Paths {
	t.Helper()
	state, err := os.MkdirTemp("/tmp", "ld")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	outside := resolvedTemp(t)
	config := filepath.Join(outside, "config", "latere")
	return Paths{
		Home: home, Config: config, Token: filepath.Join(config, "auth-token.json"),
		State: filepath.Join(state, "latere"), Cache: filepath.Join(outside, "cache", "latere"),
	}
}

// grantedAbove writes a host policy that grants folder writable and keeps a
// fresh key for it.
func grantedAbove(t *testing.T, paths Paths, keys KeyStore, folder string) {
	t.Helper()
	if err := SavePolicy(paths.PolicyFile(), Policy{
		Device: testEnv, Name: "Test computer", SessionStart: SessionStartAllow,
		Folders: []Folder{{Path: folder, Write: true}},
		Network: Network{Mode: NetworkNone},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := keys.Put(Key{Environment: testEnv, Token: "key-1", JTI: "jti-1", MintedAt: now, ExpiresAt: now.Add(KeyTTL)}); err != nil {
		t.Fatal(err)
	}
}

func TestEngineCreatesHomeFoldersForAGrantAboveHome(t *testing.T) {
	sandboxRuntime(t)
	root, home := aboveHome(t)
	paths := homeAbovePaths(t, home)
	keys := OpenKeys(paths.KeyFile())
	grantedAbove(t, paths, keys, root)
	p := newStubPlane(t)
	e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
	running(t, e)
	p.waitConnected(t)
	for _, name := range homeFolders {
		requireCreated(t, filepath.Join(home, name))
	}
	host, _ := p.live()
	if host == nil || !slices.ContainsFunc(host.Grants, func(g v1.HostGrant) bool { return g.Path == root && g.Write }) {
		t.Errorf("the worker registered without the grant of %s: %+v", root, host)
	}
}

// bounded is a context for a Run that should refuse at once, ended after a
// while, so a Run that serves instead returns and fails the test rather than
// hanging it.
func bounded(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestEngineRefusesAGrantWhoseHomeFoldersFail(t *testing.T) {
	root := resolvedTemp(t)
	home := filepath.Join(root, "missing")
	paths := homeAbovePaths(t, home)
	keys := OpenKeys(paths.KeyFile())
	grantedAbove(t, paths, keys, root)
	p := newStubPlane(t)
	e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
	err := e.Run(bounded(t))
	if CodeOf(err) != CodeHomeFolderFailed || !strings.Contains(err.Error(), filepath.Join(home, ".config")) {
		t.Fatalf("Run returned %v", err)
	}
	if _, ok := p.live(); ok {
		t.Error("the worker connected with a grant whose folders could not be created")
	}
}

func TestEngineRefusesTheHomeItselfAndCreatesNothing(t *testing.T) {
	_, home := aboveHome(t)
	paths := homeAbovePaths(t, home)
	keys := OpenKeys(paths.KeyFile())
	grantedAbove(t, paths, keys, "~")
	e := engineOn(paths, newStubPlane(t), &fakeAccount{exists: true}, keys)
	if err := e.Run(bounded(t)); CodeOf(err) != CodePolicyInvalid || !strings.Contains(err.Error(), "is the home directory") {
		t.Fatalf("Run returned %v", err)
	}
	for _, name := range homeFolders {
		requireAbsent(t, filepath.Join(home, name))
	}
}
