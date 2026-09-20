// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"testing"

	driver "latere.ai/x/cella/runtime"
)

// TestPodmanProjectsTheToken is the driver's half of spec 006's identity: the
// token is in the container at the reserved path before it starts, its
// environment names that path, and a re-projection is what the next read
// returns.
func TestPodmanProjectsTheToken(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Name: "one", Owner: "alice", Image: "img", Token: []byte("first")})
	c := f.container(t, "sbx_a")
	if c.env[driver.TokenFileEnv] != driver.TokenPath {
		t.Errorf("%s = %q, want %q", driver.TokenFileEnv, c.env[driver.TokenFileEnv], driver.TokenPath)
	}
	file, ok := c.files[driver.TokenPath]
	if !ok {
		t.Fatalf("the token is not at %s; the container holds %v", driver.TokenPath, c.files)
	}
	if string(file.body) != "first" {
		t.Errorf("the projected token = %q, want %q", file.body, "first")
	}
	if file.mode != 0o400 {
		t.Errorf("the projected token is mode %o, want 0400", file.mode)
	}
	if err := d.Update(t.Context(), "sbx_a", driver.Change{Token: []byte("second")}); err != nil {
		t.Fatal(err)
	}
	if body := f.container(t, "sbx_a").files[driver.TokenPath].body; string(body) != "second" {
		t.Errorf("the re-projected token = %q, want %q", body, "second")
	}
}

// TestPodmanProjectsTheTokenForItsUser stamps the sandbox's own uid on the
// file, because a token only root can read is one a sandbox that dropped
// privileges cannot use.
func TestPodmanProjectsTheTokenForItsUser(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Name: "one", Owner: "alice", Image: "img", User: "1001:2002", Token: []byte("first")})
	file := f.container(t, "sbx_a").files[driver.TokenPath]
	if file.uid != 1001 || file.gid != 2002 {
		t.Errorf("the projected token is owned by %d:%d, want 1001:2002", file.uid, file.gid)
	}
	if err := d.Update(t.Context(), "sbx_a", driver.Change{Token: []byte("second")}); err != nil {
		t.Fatal(err)
	}
	file = f.container(t, "sbx_a").files[driver.TokenPath]
	if string(file.body) != "second" || file.uid != 1001 || file.gid != 2002 {
		t.Errorf("the re-projected token is %q owned by %d:%d, want %q owned by 1001:2002", file.body, file.uid, file.gid, "second")
	}
}

// TestPodmanProjectsTheTokenForANamedUser cannot resolve a name from outside
// the image, so the file is readable inside the container instead of owned by
// a uid the driver guessed.
func TestPodmanProjectsTheTokenForANamedUser(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Name: "one", Owner: "alice", Image: "img", User: "appuser", Token: []byte("first")})
	if mode := f.container(t, "sbx_a").files[driver.TokenPath].mode; mode != 0o444 {
		t.Errorf("the projected token is mode %o, want 0444 for a user given by name", mode)
	}
}

// TestPodmanWithoutATokenProjectsNothing is the hosted regression restated: a
// control plane that mints none leaves no file and no variable behind.
func TestPodmanWithoutATokenProjectsNothing(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Name: "one", Owner: "alice", Image: "img"})
	c := f.container(t, "sbx_a")
	if _, ok := c.files[driver.TokenPath]; ok {
		t.Error("a token was projected into a sandbox that carries none")
	}
	if _, ok := c.env[driver.TokenFileEnv]; ok {
		t.Errorf("%s is set on a sandbox with no token", driver.TokenFileEnv)
	}
}

// TestPodmanRefusesACreateWhoseTokenCannotBeProjected leaves no container
// behind: a sandbox that cannot reach the control plane with its own identity
// is not the sandbox that was asked for.
func TestPodmanRefusesACreateWhoseTokenCannotBeProjected(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	f.fault("PUT "+compat+"/containers/"+containerName("sbx_a")+"/archive", 500)
	_, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_a", Name: "one", Owner: "alice", Image: "img", Token: []byte("first")})
	if err == nil {
		t.Fatal("a create whose token could not be projected was reported as a success")
	}
	if f.containerCount() != 0 {
		t.Fatal("the refused create left a container behind")
	}
}

// TestPodmanRefusesToReprojectIntoAMissingSandbox answers a rotation against
// a container the engine no longer has with the error the controller reads as
// lost, rather than with a silent success.
func TestPodmanRefusesToReprojectIntoAMissingSandbox(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	if err := d.Update(t.Context(), "sbx_gone", driver.Change{Token: []byte("first")}); err == nil {
		t.Fatal("a re-projection into a sandbox that does not exist was reported as a success")
	}
}
