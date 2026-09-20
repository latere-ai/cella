// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"os"
	"testing"

	driver "latere.ai/x/cella/runtime"
)

// TestNativeProjectsTheToken is the driver's half of spec 006's identity: the
// token the create carried is a file only its owner reads, in the directory
// the driver owns rather than the workspace, and the environment names where
// it went.
func TestNativeProjectsTheToken(t *testing.T) {
	root := t.TempDir()
	d, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	const id = "sbx_token"
	if _, err = d.Create(t.Context(), driver.CreateSpec{ID: id, Name: "one", Owner: "alice", Token: []byte("first")}); err != nil {
		t.Fatal(err)
	}
	path := tokenPath(d.dir(id))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the token was not projected: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o400 {
		t.Errorf("the projected token is %v, want 0400", mode)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "first" {
		t.Fatalf("the projected token reads %q (%v), want %q", body, err, "first")
	}
	r, err := d.load(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Env[driver.TokenFileEnv]; got != path {
		t.Errorf("%s is %q, want %q", driver.TokenFileEnv, got, path)
	}
	if err := d.Update(t.Context(), id, driver.Change{Token: []byte("second")}); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(path)
	if err != nil || string(body) != "second" {
		t.Fatalf("the re-projected token reads %q (%v), want %q", body, err, "second")
	}
	if info, err = os.Stat(path); err != nil || info.Mode().Perm() != 0o400 {
		t.Errorf("the re-projected token is %v (%v), want 0400", info.Mode().Perm(), err)
	}
}

// TestNativeProjectsNoTokenWhenThereIsNone is the hosted regression restated:
// a control plane that mints nothing leaves no file and no variable behind,
// so a sandbox without an identity does not look like one that lost it.
func TestNativeProjectsNoTokenWhenThereIsNone(t *testing.T) {
	root := t.TempDir()
	d, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	const id = "sbx_none"
	if _, err = d.Create(t.Context(), driver.CreateSpec{ID: id, Name: "one", Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokenPath(d.dir(id))); !os.IsNotExist(err) {
		t.Errorf("a sandbox created with no token has a token file: %v", err)
	}
	r, err := d.load(id)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Env[driver.TokenFileEnv]; ok {
		t.Errorf("%s is set to %q on a sandbox with no token", driver.TokenFileEnv, got)
	}
}
