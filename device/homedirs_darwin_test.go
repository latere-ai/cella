// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package device

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/cella/runtime/local"
)

// TestHomeFoldersRefuseAReadOnlyHome builds on macOS alone, where no test
// runs as root, which a read-only folder would not stop.
func TestHomeFoldersRefuseAReadOnlyHome(t *testing.T) {
	root, home := aboveHome(t)
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	err := createHomeFolders(home, []local.Grant{{Path: root, Write: true}})
	if CodeOf(err) != CodeHomeFolderFailed || !errors.Is(err, fs.ErrPermission) ||
		!strings.Contains(err.Error(), filepath.Join(home, ".config")) {
		t.Fatalf("a create in a read-only home answered %v", err)
	}
	for _, name := range homeFolders {
		requireAbsent(t, filepath.Join(home, name))
	}
}

// TestSessionWritesSettingsAboveHomeUnderSeatbelt is the reason for the
// folders: under Seatbelt, a session granted a folder above the home cannot
// create ~/.docker or ~/.config itself, since each holds a path the sandbox
// always denies, and with the engine's create it keeps its settings there.
func TestSessionWritesSettingsAboveHomeUnderSeatbelt(t *testing.T) {
	root, home := aboveHome(t)
	paths := homeAbovePaths(t, home)
	keys := OpenKeys(paths.KeyFile())
	grantedAbove(t, paths, keys, root)
	p := newStubPlane(t)
	e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
	running(t, e)
	p.waitConnected(t)
	if _, err := p.driver.Create(t.Context(), deviceCreate("sbx_settings", root, sessionLabels("ses_settings"))); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(home, ".config", "tool"), filepath.Join(home, ".docker", "contexts")} {
		if code, _ := run(t, p.driver, "sbx_settings", "mkdir", dir); code != 0 {
			t.Errorf("a session could not create %s: exit %d", dir, code)
		}
	}
	// The same session, with ~/.docker gone, cannot make it again.
	if err := os.RemoveAll(filepath.Join(home, ".docker")); err != nil {
		t.Fatal(err)
	}
	if code, _ := run(t, p.driver, "sbx_settings", "mkdir", filepath.Join(home, ".docker")); code == 0 {
		t.Error("a session created ~/.docker itself, so the sandbox no longer holds the denial below it")
	}
}
