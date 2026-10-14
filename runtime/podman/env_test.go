// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	driver "latere.ai/x/cella/runtime"
)

// noLabelCarries fails the test where a label of any volume or container the
// engine holds carries one of the values.
func noLabelCarries(t *testing.T, f *fake, values ...string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	check := func(object string, labels map[string]string) {
		for key, label := range labels {
			for _, value := range values {
				if strings.Contains(label, value) {
					t.Errorf("the label %s of %s carries %q: %q", key, object, value, label)
				}
			}
		}
	}
	for name, labels := range f.volumes {
		check(name, labels)
	}
	for name, c := range f.containers {
		check(name, c.labels)
	}
}

// labelAsAnEarlierVersion stamps the environment on a sandbox's current
// record the way an earlier version of this driver wrote it.
func labelAsAnEarlierVersion(t *testing.T, f *fake, d *Driver, id string, env map[string]string) {
	t.Helper()
	rec, err := d.current(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes[recordVolume(id, rec.generation)][labelEnv] = string(body)
}

// keptEnv is the environment the sandbox's container keeps for its commands.
func keptEnv(t *testing.T, f *fake, id string) map[string]string {
	t.Helper()
	c := f.container(t, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := c.files[envPath]
	if !ok {
		return nil
	}
	env := map[string]string{}
	if err := json.Unmarshal(file.body, &env); err != nil {
		t.Fatalf("the environment file of %s: %v", id, err)
	}
	return env
}

// execEnv runs one command and returns what the request carried and what
// the session saw.
func execEnv(t *testing.T, f *fake, d *Driver, id string, req map[string]string) (requested, seen []string) {
	t.Helper()
	e, err := d.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"true"}, Env: req})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := read(t, e); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	last := f.execs["exec"+strconv.Itoa(len(f.execs))]
	return slices.Clone(last.requested), slices.Clone(last.env)
}

// TestNoLabelCarriesAValue: a create, an update of the environment and an
// adoption each leave every label of every object the engine holds without
// the sandbox's environment values and its gateway credential.
func TestNoLabelCarriesAValue(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Env: map[string]string{"API_KEY": "created-canary"},
		Egress: driver.Egress{ProxyAddr: "gateway:3128", Credential: "credential-canary"}, Token: []byte("tok")})
	changed := map[string]string{"API_KEY": "updated-canary"}
	if err := d.Update(t.Context(), "sbx_a", driver.Change{Env: &changed}); err != nil {
		t.Fatal(err)
	}
	if err := d.Touch(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	prewarmed(t, d, "sbx_pool")
	adoption := driver.Adoption{Owner: "alice", Name: "adopted", Env: map[string]string{"API_KEY": "adopted-canary"},
		Egress: driver.Egress{ProxyAddr: "gateway:3128", Credential: "adopted-credential"}}
	if err := d.Update(t.Context(), "sbx_pool", driver.Change{Adopt: &adoption}); err != nil {
		t.Fatal(err)
	}
	noLabelCarries(t, f, "created-canary", "credential-canary", "updated-canary", "adopted-canary", "adopted-credential")
	for _, id := range []string{"sbx_a", "sbx_pool"} {
		rec, err := d.current(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.env != nil {
			t.Errorf("the record of %s reads an environment %v", id, rec.env)
		}
	}
}

// TestACommandIsGivenWhatItsContainerLacks: a command inherits its container's
// environment, so its request carries only the request's own variables until
// an update sets one at another value, and then that one; the session sees
// the whole environment either way.
func TestACommandIsGivenWhatItsContainerLacks(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Env: map[string]string{"GREETING": "hello", "KEPT": "same"}})
	requested, seen := execEnv(t, f, d, "sbx_a", map[string]string{"TERM": "xterm"})
	if !slices.Equal(requested, []string{"TERM=xterm"}) {
		t.Errorf("the request carried %v, want the request's own variable alone", requested)
	}
	if !slices.Equal(seen, []string{"GREETING=hello", "KEPT=same", "TERM=xterm"}) {
		t.Errorf("the session saw %v", seen)
	}
	changed := map[string]string{"GREETING": "changed", "KEPT": "same"}
	if err := d.Update(t.Context(), "sbx_a", driver.Change{Env: &changed}); err != nil {
		t.Fatal(err)
	}
	if got := keptEnv(t, f, "sbx_a"); !maps.Equal(got, changed) {
		t.Errorf("the container keeps %v, want what the update set", got)
	}
	requested, seen = execEnv(t, f, d, "sbx_a", nil)
	if !slices.Equal(requested, []string{"GREETING=changed"}) {
		t.Errorf("the request carried %v, want the one variable the update changed", requested)
	}
	if !slices.Equal(seen, []string{"GREETING=changed", "KEPT=same"}) {
		t.Errorf("the session saw %v", seen)
	}
	// An update that clears the environment is kept as that.
	none := map[string]string(nil)
	if err := d.Update(t.Context(), "sbx_a", driver.Change{Env: &none}); err != nil {
		t.Fatal(err)
	}
	if got := keptEnv(t, f, "sbx_a"); got == nil || len(got) != 0 {
		t.Errorf("the container keeps %v after an update that cleared the environment", got)
	}
}

// TestTheEnvironmentOutlivesTheDriver is adoption across a restart: what an
// adoption and an update set is read back by a second driver over the same
// engine, which keeps nothing in its own process.
func TestTheEnvironmentOutlivesTheDriver(t *testing.T) {
	f := newFake(t)
	first := f.driver(t)
	prewarmed(t, first, "sbx_pool")
	adoption := driver.Adoption{Owner: "alice", Name: "adopted", Env: map[string]string{"GREETING": "adopted"}, Token: []byte("tok")}
	if err := first.Update(t.Context(), "sbx_pool", driver.Change{Adopt: &adoption}); err != nil {
		t.Fatal(err)
	}
	second := f.driver(t)
	_, seen := execEnv(t, f, second, "sbx_pool", nil)
	if !slices.Contains(seen, "GREETING=adopted") || !slices.Contains(seen, driver.TokenFileEnv+"="+driver.TokenPath) {
		t.Errorf("a command after the restart saw %v, want the adopted environment", seen)
	}
	changed := map[string]string{"GREETING": "changed"}
	if err := first.Update(t.Context(), "sbx_pool", driver.Change{Env: &changed}); err != nil {
		t.Fatal(err)
	}
	if _, seen = execEnv(t, f, f.driver(t), "sbx_pool", nil); !slices.Contains(seen, "GREETING=changed") {
		t.Errorf("a command after the next restart saw %v, want the updated environment", seen)
	}
}

// TestMoveCredentialsRewritesAnOldRecord: a record an earlier version labeled
// with the environment is read as it stands, then MoveCredentials moves the
// values into the container and writes the next generation without them,
// once.
func TestMoveCredentialsRewritesAnOldRecord(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Env: map[string]string{"GREETING": "hello"}})
	create(t, d, driver.CreateSpec{ID: "sbx_b"})
	old := map[string]string{"GREETING": "labeled-canary"}
	labelAsAnEarlierVersion(t, f, d, "sbx_a", old)
	if _, seen := execEnv(t, f, d, "sbx_a", nil); !slices.Contains(seen, "GREETING=labeled-canary") {
		t.Fatalf("a command before the move saw %v, want the labeled environment", seen)
	}
	moved, err := d.MoveCredentials(t.Context())
	if err != nil || moved != 1 {
		t.Fatalf("MoveCredentials = %d, %v, want one record", moved, err)
	}
	noLabelCarries(t, f, "labeled-canary")
	if got := keptEnv(t, f, "sbx_a"); !maps.Equal(got, old) {
		t.Errorf("the container keeps %v, want the moved environment", got)
	}
	if got := keptEnv(t, f, "sbx_b"); got != nil {
		t.Errorf("a sandbox with nothing to move keeps %v", got)
	}
	if _, seen := execEnv(t, f, d, "sbx_a", nil); !slices.Contains(seen, "GREETING=labeled-canary") {
		t.Errorf("a command after the move saw %v", seen)
	}
	if moved, err = d.MoveCredentials(t.Context()); err != nil || moved != 0 {
		t.Errorf("a second MoveCredentials = %d, %v, want nothing", moved, err)
	}
}

// TestAWriteRewritesAnOldRecord: the next change of a sandbox moves an
// environment its record still labels, and a sandbox whose container is gone
// has its record written without it.
func TestAWriteRewritesAnOldRecord(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	labelAsAnEarlierVersion(t, f, d, "sbx_a", map[string]string{"GREETING": "labeled-canary"})
	if err := d.Start(t.Context(), "sbx_a"); err != nil { // a start of a running sandbox changes nothing else
		t.Fatal(err)
	}
	noLabelCarries(t, f, "labeled-canary")
	if got := keptEnv(t, f, "sbx_a"); got["GREETING"] != "labeled-canary" {
		t.Errorf("the container keeps %v", got)
	}

	create(t, d, driver.CreateSpec{ID: "sbx_gone"})
	labelAsAnEarlierVersion(t, f, d, "sbx_gone", map[string]string{"GREETING": "orphan-canary"})
	if err := d.removeContainer(t.Context(), "sbx_gone"); err != nil {
		t.Fatal(err)
	}
	if err := d.Touch(t.Context(), "sbx_gone"); err != nil {
		t.Fatalf("a touch of a sandbox with no container: %v", err)
	}
	noLabelCarries(t, f, "orphan-canary")
}

// TestAMoveThatCannotWriteLeavesTheRecord: a move whose write into the
// container fails reports the sandbox and leaves its record, value and all,
// for the next change.
func TestAMoveThatCannotWriteLeavesTheRecord(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	labelAsAnEarlierVersion(t, f, d, "sbx_a", map[string]string{"GREETING": "labeled"})
	f.fault("PUT "+compat+"/containers/cella-sbx_a/archive", http.StatusInternalServerError)
	moved, err := d.MoveCredentials(t.Context())
	if moved != 0 || err == nil || !strings.Contains(err.Error(), "sbx_a") {
		t.Fatalf("MoveCredentials = %d, %v, want sbx_a reported", moved, err)
	}
	rec, err := d.current(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if rec.env["GREETING"] != "labeled" {
		t.Errorf("the record lost its environment: %v", rec.env)
	}
	f.fault("GET "+libpod+"/volumes/json", http.StatusInternalServerError)
	if _, err := d.MoveCredentials(t.Context()); err == nil {
		t.Error("a list that failed moved nothing and reported nothing")
	}
}

// TestUpdatingTheEnvironmentOfAGoneContainer is ErrNotFound: there is no
// container to keep it in.
func TestUpdatingTheEnvironmentOfAGoneContainer(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	if err := d.removeContainer(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GREETING": "changed"}
	if err := d.Update(t.Context(), "sbx_a", driver.Change{Env: &env}); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("Update = %v, want ErrNotFound", err)
	}
}

// TestAnUnreadableEnvironmentFailsTheCommand: a command is not started with
// less than its sandbox's environment.
func TestAnUnreadableEnvironmentFailsTheCommand(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	changed := map[string]string{"GREETING": "changed"}
	if err := d.Update(t.Context(), "sbx_a", driver.Change{Env: &changed}); err != nil {
		t.Fatal(err)
	}
	f.fault("GET "+compat+"/containers/cella-sbx_a/archive", http.StatusInternalServerError)
	if _, err := d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"true"}}); err == nil {
		t.Error("a command started without the environment it could not read")
	}
	c := f.container(t, "sbx_a")
	f.mu.Lock()
	c.files[envPath] = fakeFile{mode: 0o400, body: []byte("{")}
	f.mu.Unlock()
	if _, err := d.Exec(t.Context(), "sbx_a", driver.ExecRequest{Command: []string{"true"}}); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a command over an environment that is not JSON: %v, want ErrInvalid", err)
	}
}
