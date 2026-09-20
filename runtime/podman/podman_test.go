// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"path"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// create makes one sandbox on the fake engine and fails the test if it does
// not appear.
func create(t *testing.T, d *Driver, s driver.CreateSpec) driver.State {
	t.Helper()
	if s.Image == "" {
		s.Image = "img"
	}
	ref, err := d.Create(t.Context(), s)
	if err != nil {
		t.Fatalf("Create %s: %v", s.ID, err)
	}
	if ref.ID != s.ID {
		t.Fatalf("Create returned %q, want %q", ref.ID, s.ID)
	}
	state, err := d.Inspect(t.Context(), s.ID)
	if err != nil {
		t.Fatalf("Inspect %s: %v", s.ID, err)
	}
	return state
}

func TestDeclarations(t *testing.T) {
	d := newFake(t).driver(t)
	if d.Name() != "podman" || d.Isolation() != v1.IsolationContainer {
		t.Fatalf("declarations: %q %q", d.Name(), d.Isolation())
	}
	want := driver.Capabilities{Files: true, Detach: true, Attach: true, Pool: true, Display: true, Input: true}
	if got := d.Capabilities(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Capabilities = %+v, want %+v", got, want)
	}
	// The forbidden half of the table: this driver advertises none of it.
	c := d.Capabilities()
	if c.Mesh || c.Dial || c.Volumes || c.Snapshots || c.Ingress || c.Resize || len(c.Egress) > 0 {
		t.Fatalf("a capability this driver does not implement is declared: %+v", c)
	}
}

func TestPreflightSockets(t *testing.T) {
	f := newFake(t)
	absent := path.Join(t.TempDir(), "x")

	if _, err := New(Options{}); err != nil {
		t.Fatal(err)
	}
	if got := DefaultSockets(); len(got) == 0 || !strings.HasSuffix(got[len(got)-1], "/run/podman/podman.sock") {
		t.Fatalf("DefaultSockets = %v", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if got := DefaultSockets(); len(got) != 2 {
		t.Fatalf("DefaultSockets with XDG_RUNTIME_DIR = %v", got)
	}

	d, err := New(Options{Socket: absent})
	if err != nil {
		t.Fatal(err)
	}
	err = d.Preflight(t.Context())
	if err == nil || !strings.Contains(err.Error(), absent) || !strings.Contains(err.Error(), "CELLA_PODMAN_SOCKET") {
		t.Fatalf("Preflight with no engine: %v", err)
	}
	if err := d.Ready(t.Context()); err == nil {
		t.Fatal("Ready is nil with no engine")
	}

	// A driver with several candidates keeps the first that answers.
	d = &Driver{candidates: []string{absent, f.socket}, pullTimeout: time.Second, locks: map[string]*sync.Mutex{}}
	d.conn.Store(newClient(absent))
	if err := d.Preflight(t.Context()); err != nil {
		t.Fatalf("Preflight over two candidates: %v", err)
	}
	if d.Socket() != f.socket {
		t.Fatalf("Socket = %q, want %q", d.Socket(), f.socket)
	}
	if err := d.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNewRefusesNoCandidate(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := New(Options{}); err != nil {
		t.Fatalf("New with the system socket alone: %v", err)
	}
}

func TestCreateStampsIdentityAndRecord(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	labels := map[string]string{"team": "a", "tier/level": "1"}
	state := create(t, d, driver.CreateSpec{
		ID: "sbx_a", Name: "one", Owner: "alice@example.com", Image: "img",
		Env: map[string]string{"GREETING": "hello"}, Labels: labels,
		User: "1000:1000", Workspace: driver.Workspace{Path: "/space"},
		Resources: driver.Resources{CPU: "500m", Memory: "256Mi", Disk: "1Gi"},
		Lifecycle: driver.Lifecycle{TTL: time.Hour, AutoStop: time.Minute, AutoDelete: 2 * time.Minute},
	})
	if state.Phase != driver.Running || state.Name != "one" || state.Owner != "alice@example.com" {
		t.Fatalf("state = %+v", state)
	}
	if !maps.Equal(state.Labels, labels) {
		t.Fatalf("labels = %v, want %v", state.Labels, labels)
	}
	if state.Isolation != v1.IsolationContainer || state.CreatedAt.IsZero() || state.LastActivityAt.IsZero() {
		t.Fatalf("state = %+v", state)
	}
	if !state.ExpiresAt.Equal(state.CreatedAt.Add(time.Hour)) || state.AutoStop != time.Minute || state.AutoDelete != 2*time.Minute {
		t.Fatalf("lifecycle in state = %+v", state)
	}

	ident := f.labelsOf(t, workspaceVolume("sbx_a"))
	for key, want := range map[string]string{
		labelID: "sbx_a", labelKind: kindWorkspace, labelName: "one", labelOwner: "alice@example.com",
		labelImage: "img", labelDigest: "sha256:d1", labelWorkspace: "/space", labelDisk: "1Gi",
	} {
		if ident[key] != want {
			t.Errorf("workspace label %s = %q, want %q", key, ident[key], want)
		}
	}
	rec := f.labelsOf(t, recordVolume("sbx_a", 1))
	if rec[labelKind] != kindRecord || rec[labelGeneration] != "1" || rec[labelTTL] != "1h0m0s" {
		t.Errorf("record labels = %v", rec)
	}
	if rec[labelUser+"tier/level"] != "1" {
		t.Errorf("a user label with a slash did not survive: %v", rec)
	}
	if _, set := rec[labelStopped]; set {
		t.Errorf("a fresh record carries the stop flag: %v", rec)
	}

	c := f.container(t, "sbx_a")
	if c.workdir != "/space" || c.user != "1000:1000" || c.state != "running" {
		t.Errorf("container = %+v", c)
	}
	if len(c.volumes) != 1 || c.volumes[0].Name != workspaceVolume("sbx_a") || c.volumes[0].Dest != "/space" {
		t.Errorf("container volumes = %+v", c.volumes)
	}
	if !slices.Equal(c.command, idleCommand) {
		t.Errorf("a spec with no command got %v, want the idle command", c.command)
	}
	if c.limits == nil || c.limits.CPU.Quota != 50000 || c.limits.CPU.Period != cpuPeriod || c.limits.Memory.Limit != 256<<20 {
		t.Errorf("limits = %+v", c.limits)
	}
	if c.labels[labelID] != "sbx_a" || c.labels[labelKind] != kindSandbox {
		t.Errorf("container labels = %v", c.labels)
	}
}

func TestCreateCommandAndArgs(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_c", Command: []string{"sh", "-c"}, Args: []string{"true"}})
	if got := f.container(t, "sbx_c").command; !slices.Equal(got, []string{"sh", "-c", "true"}) {
		t.Fatalf("command = %v", got)
	}
}

func TestCreateRefusals(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	for name, s := range map[string]driver.CreateSpec{
		"badID":         {ID: "no spaces", Image: "img"},
		"emptyID":       {Image: "img"},
		"noImage":       {ID: "sbx_x"},
		"argsOnly":      {ID: "sbx_x", Image: "img", Args: []string{"x"}},
		"negativeTTL":   {ID: "sbx_x", Image: "img", Lifecycle: driver.Lifecycle{TTL: -1}},
		"badWorkspace":  {ID: "sbx_x", Image: "img", Workspace: driver.Workspace{Path: "relative"}},
		"rootWorkspace": {ID: "sbx_x", Image: "img", Workspace: driver.Workspace{Path: "/"}},
		"badWorkdir":    {ID: "sbx_x", Image: "img", Workdir: "/elsewhere"},
		"badCPU":        {ID: "sbx_x", Image: "img", Resources: driver.Resources{CPU: "many"}},
		"badMemory":     {ID: "sbx_x", Image: "img", Resources: driver.Resources{Memory: "lots"}},
		"badDisk":       {ID: "sbx_x", Image: "img", Resources: driver.Resources{Disk: "huge"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := d.Create(t.Context(), s); !errors.Is(err, driver.ErrInvalid) {
				t.Fatalf("Create %+v: %v, want ErrInvalid", s, err)
			}
		})
	}
	if len(f.volumeNames()) != 0 {
		t.Fatalf("a refused create left %v behind", f.volumeNames())
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := d.Create(cancelled, driver.CreateSpec{ID: "sbx_x", Image: "img"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create with a cancelled context: %v", err)
	}
}

func TestCreateIsExclusive(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	if _, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_a", Image: "img"}); !errors.Is(err, driver.ErrAlreadyExists) {
		t.Fatalf("second Create: %v", err)
	}
}

func TestCreatePullsTheImage(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_p", Image: "elsewhere/base:v1"})
	if f.pulls != 1 {
		t.Fatalf("pulls = %d, want 1", f.pulls)
	}
	if got := f.labelsOf(t, workspaceVolume("sbx_p"))[labelDigest]; got != "sha256:pulled" {
		t.Fatalf("digest = %q", got)
	}
	// An image the registry does not answer for stops the create.
	if _, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_q", Image: "unpullable"}); err == nil || !strings.Contains(err.Error(), "absent after the pull") {
		t.Fatalf("unpullable image: %v", err)
	}
	f.fault("POST "+libpod+"/images/pull", http.StatusInternalServerError)
	if _, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_r", Image: "other/base:v1"}); err == nil || !strings.Contains(err.Error(), "pulling") {
		t.Fatalf("refused pull: %v", err)
	}
}

func TestCreateRollsBackEveryStep(t *testing.T) {
	for name, fault := range map[string]string{
		"record":    "POST " + libpod + "/volumes/create",
		"container": "POST " + libpod + "/containers/create",
		"start":     "POST " + libpod + "/containers/cella-sbx_a/start",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			d := f.driver(t)
			// The workspace volume is the first create, so the record is the second.
			f.faultAfter(fault, http.StatusInternalServerError, map[string]int{"record": 1}[name])
			if _, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_a", Image: "img"}); err == nil {
				t.Fatal("Create did not report the arranged fault")
			}
			if names := f.volumeNames(); len(names) != 0 {
				t.Fatalf("rollback left %v", names)
			}
			if f.containerCount() != 0 {
				t.Fatalf("rollback left a container")
			}
		})
	}
}

func TestStartStopAreIdempotent(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	s0 := create(t, d, driver.CreateSpec{ID: "sbx_a"})
	if err := d.Stop(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	s1, err := d.Inspect(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if s1.Phase != driver.Stopped || s1.StoppedAt.IsZero() || s1.ExitCode != nil {
		t.Fatalf("after Stop: %+v", s1)
	}
	if err := d.Stop(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	s2, err := d.Inspect(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if !s2.StoppedAt.Equal(s1.StoppedAt) {
		t.Fatalf("a second Stop moved the clock: %v then %v", s1.StoppedAt, s2.StoppedAt)
	}
	if got := f.labelsOf(t, recordVolume("sbx_a", 2))[labelStopped]; got != "true" {
		t.Fatalf("the stop flag is %q", got)
	}
	if err := d.Start(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	s3, err := d.Inspect(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if s3.Phase != driver.Running || !s3.StoppedAt.IsZero() || s3.StartedAt.Before(s0.StartedAt) {
		t.Fatalf("after Start: %+v", s3)
	}
	if err := d.Start(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	s4, err := d.Inspect(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if !s4.StartedAt.Equal(s3.StartedAt) || !s4.CreatedAt.Equal(s0.CreatedAt) {
		t.Fatalf("a second Start moved a clock: %+v then %+v", s3, s4)
	}
}

func TestLifecycleOfAnUnknownSandbox(t *testing.T) {
	d := newFake(t).driver(t)
	labels := map[string]string{"a": "1"}
	for name, call := range map[string]func() error{
		"Start":  func() error { return d.Start(t.Context(), "sbx_absent") },
		"Stop":   func() error { return d.Stop(t.Context(), "sbx_absent") },
		"Update": func() error { return d.Update(t.Context(), "sbx_absent", driver.Change{Labels: &labels}) },
		"Touch":  func() error { return d.Touch(t.Context(), "sbx_absent") },
		"Inspect": func() error {
			_, err := d.Inspect(t.Context(), "sbx_absent")
			return err
		},
		"Logs": func() error {
			_, err := d.Logs(t.Context(), "sbx_absent", driver.LogsRequest{})
			return err
		},
		"Exec": func() error {
			_, err := d.Exec(t.Context(), "sbx_absent", driver.ExecRequest{Command: []string{"true"}})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, driver.ErrNotFound) {
				t.Fatalf("%s of an unknown id: %v, want ErrNotFound", name, err)
			}
		})
	}
	if err := d.Delete(t.Context(), "sbx_absent"); err != nil {
		t.Fatalf("Delete of an unknown id: %v", err)
	}
}

func TestEveryCallRefusesABadID(t *testing.T) {
	d := newFake(t).driver(t)
	bad := "not a name"
	for name, call := range map[string]func() error{
		"Start":  func() error { return d.Start(t.Context(), bad) },
		"Stop":   func() error { return d.Stop(t.Context(), bad) },
		"Delete": func() error { return d.Delete(t.Context(), bad) },
		"Touch":  func() error { return d.Touch(t.Context(), bad) },
		"Inspect": func() error {
			_, err := d.Inspect(t.Context(), bad)
			return err
		},
		"ExportTar": func() error { return d.ExportTar(t.Context(), bad, nil, nopWriter{}) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, driver.ErrInvalid) {
				t.Fatalf("%s of %q: %v, want ErrInvalid", name, bad, err)
			}
		})
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestDeleteRemovesEveryObject(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	if err := d.Touch(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	if names := f.volumeNames(); len(names) != 0 {
		t.Fatalf("Delete left %v", names)
	}
	if f.containerCount() != 0 {
		t.Fatalf("Delete left a container")
	}
	if _, err := d.Inspect(t.Context(), "sbx_a"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("Inspect after Delete: %v", err)
	}
	if err := d.Delete(t.Context(), "sbx_a"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestUpdateWritesARecordAndLeavesTheContainer(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	s0 := create(t, d, driver.CreateSpec{ID: "sbx_a", Labels: map[string]string{"a": "1"},
		Env: map[string]string{"GREETING": "hello"}, Lifecycle: driver.Lifecycle{TTL: time.Hour}})
	before := f.container(t, "sbx_a")
	labels := map[string]string{"b": "2"}
	env := map[string]string{"GREETING": "changed"}
	if err := d.Update(t.Context(), "sbx_a", driver.Change{Labels: &labels, Env: &env,
		Lifecycle: &driver.Lifecycle{TTL: 2 * time.Hour, AutoStop: 3 * time.Minute, AutoDelete: 4 * time.Minute}}); err != nil {
		t.Fatal(err)
	}
	s1, err := d.Inspect(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(s1.Labels, labels) || s1.AutoStop != 3*time.Minute || s1.AutoDelete != 4*time.Minute {
		t.Fatalf("after Update: %+v", s1)
	}
	if !s1.ExpiresAt.Equal(s0.CreatedAt.Add(2 * time.Hour)) {
		t.Fatalf("ExpiresAt = %v, want %v", s1.ExpiresAt, s0.CreatedAt.Add(2*time.Hour))
	}
	if after := f.container(t, "sbx_a"); after != before || !maps.Equal(after.env, map[string]string{"GREETING": "hello"}) {
		t.Fatalf("Update touched the container: %+v", after)
	}
	// A zero lifecycle clears the bounds rather than leaving the old ones.
	if err := d.Update(t.Context(), "sbx_a", driver.Change{Lifecycle: &driver.Lifecycle{}}); err != nil {
		t.Fatal(err)
	}
	s2, err := d.Inspect(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if !s2.ExpiresAt.IsZero() || s2.AutoStop != 0 || s2.AutoDelete != 0 {
		t.Fatalf("a zero lifecycle did not clear: %+v", s2)
	}
	if err := d.Update(t.Context(), "sbx_a", driver.Change{Lifecycle: &driver.Lifecycle{AutoStop: -1}}); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("a negative bound: %v", err)
	}
	if err := d.Update(t.Context(), "sbx_a", driver.Change{}); err != nil {
		t.Fatal(err)
	}
	s3, err := d.Inspect(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(s3.Labels, labels) {
		t.Fatalf("an empty Update changed the labels to %v", s3.Labels)
	}
}

func TestTouchAdvancesActivity(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	s0 := create(t, d, driver.CreateSpec{ID: "sbx_a"})
	if err := d.Touch(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	s1, err := d.Inspect(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if !s1.LastActivityAt.After(s0.LastActivityAt) {
		t.Fatalf("LastActivityAt %v did not advance past %v", s1.LastActivityAt, s0.LastActivityAt)
	}
	if got := f.labelsOf(t, recordVolume("sbx_a", 2))[labelActivity]; got == "" {
		t.Fatal("the new record carries no activity")
	}
}

func TestRecordGenerations(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	for range 3 {
		if err := d.Touch(t.Context(), "sbx_a"); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{recordVolume("sbx_a", 4), workspaceVolume("sbx_a")}
	slices.Sort(want)
	if got := f.volumeNames(); !slices.Equal(got, want) {
		t.Fatalf("volumes = %v, want %v", got, want)
	}

	// A crash between the write and the removal leaves two generations. The
	// higher is the record, and the read sweeps the lower.
	stale := f.labelsOf(t, recordVolume("sbx_a", 4))
	stale[labelGeneration] = "3"
	stale[labelActivity] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	f.mu.Lock()
	f.volumes[recordVolume("sbx_a", 3)] = stale
	f.mu.Unlock()
	state, err := d.Inspect(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if state.LastActivityAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("the read took the stale generation: %v", state.LastActivityAt)
	}
	if got := f.volumeNames(); slices.Contains(got, recordVolume("sbx_a", 3)) {
		t.Fatalf("the read did not sweep the stale generation: %v", got)
	}
}

func TestRecordSurvivesANewDriver(t *testing.T) {
	f := newFake(t)
	first := f.driver(t)
	labels := map[string]string{"team": "a"}
	s0 := create(t, first, driver.CreateSpec{ID: "sbx_a", Name: "one", Owner: "alice@example.com",
		Labels: labels, Lifecycle: driver.Lifecycle{TTL: time.Hour, AutoStop: time.Minute, AutoDelete: time.Hour}})
	second := f.driver(t)
	s1, err := second.Inspect(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s0, s1) {
		t.Fatalf("a second driver reads %+v, the first wrote %+v", s1, s0)
	}
}

func TestListAndFilters(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Name: "a", Owner: "alice", Labels: map[string]string{"k": "1"}})
	create(t, d, driver.CreateSpec{ID: "sbx_b", Name: "b", Owner: "alice"})
	create(t, d, driver.CreateSpec{ID: "sbx_c", Name: "c", Owner: "bob"})
	if err := d.Stop(t.Context(), "sbx_b"); err != nil {
		t.Fatal(err)
	}
	all, err := d.List(t.Context(), driver.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ID != "sbx_a" || all[2].ID != "sbx_c" {
		t.Fatalf("List = %+v", all)
	}
	if !maps.Equal(all[0].Labels, map[string]string{"k": "1"}) || all[0].Owner != "alice" || all[0].Name != "a" {
		t.Fatalf("List does not read the identity back: %+v", all[0])
	}
	for name, tc := range map[string]struct {
		filter driver.Filter
		want   []string
	}{
		"Owner": {driver.Filter{Owner: "alice"}, []string{"sbx_a", "sbx_b"}},
		"Phase": {driver.Filter{Phase: driver.Stopped}, []string{"sbx_b"}},
		"IDs":   {driver.Filter{IDs: []string{"sbx_a", "sbx_c"}}, []string{"sbx_a", "sbx_c"}},
		"All":   {driver.Filter{Owner: "alice", Phase: driver.Running, IDs: []string{"sbx_a", "sbx_b"}}, []string{"sbx_a"}},
		"None":  {driver.Filter{Owner: "nobody"}, []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			states, err := d.List(t.Context(), tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			got := []string{}
			for _, s := range states {
				got = append(got, s.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("filter %+v selected %v, want %v", tc.filter, got, tc.want)
			}
		})
	}
	// A volume of another tool, and one of this driver with no id, are skipped.
	f.mu.Lock()
	f.volumes["someone-elses"] = map[string]string{"other": "1"}
	f.volumes["cella-orphan"] = map[string]string{labelID: "", labelKind: kindWorkspace}
	f.mu.Unlock()
	if states, err := d.List(t.Context(), driver.Filter{}); err != nil || len(states) != 3 {
		t.Fatalf("List over a shared engine: %d %v", len(states), err)
	}
}

func TestPhaseTable(t *testing.T) {
	exit := func(code int) *int { return &code }
	for name, tc := range map[string]struct {
		st      status
		stopped bool
		phase   string
		code    *int
	}{
		"absent":       {status{}, false, driver.Stopped, nil},
		"created":      {status{present: true, state: "created"}, false, driver.Pending, nil},
		"initialized":  {status{present: true, state: "initialized"}, false, driver.Pending, nil},
		"running":      {status{present: true, state: "running"}, false, driver.Running, nil},
		"paused":       {status{present: true, state: "paused"}, false, driver.Running, nil},
		"stopping":     {status{present: true, state: "stopping"}, false, "Stopping", nil},
		"removing":     {status{present: true, state: "removing"}, false, "Deleting", nil},
		"killedByStop": {status{present: true, state: "exited", exitCode: 137}, true, driver.Stopped, nil},
		"exitedClean":  {status{present: true, state: "exited"}, false, driver.Stopped, exit(0)},
		"exitedDirty":  {status{present: true, state: "exited", exitCode: 4}, false, "Failed", exit(4)},
		"dead":         {status{present: true, state: "dead", exitCode: 1}, false, "Failed", exit(1)},
		"unknown":      {status{present: true, state: "something-new"}, false, driver.Pending, nil},
	} {
		t.Run(name, func(t *testing.T) {
			phase, code := phaseOf(tc.st, tc.stopped)
			if phase != tc.phase {
				t.Fatalf("phase = %q, want %q", phase, tc.phase)
			}
			if (code == nil) != (tc.code == nil) || code != nil && *code != *tc.code {
				t.Fatalf("exit code = %v, want %v", code, tc.code)
			}
		})
	}
}

func TestAWorkloadThatDiedReadsFailed(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Command: []string{"sh", "-c", "exit 4"}})
	f.exit(t, "sbx_a", 4)
	state, err := d.Inspect(t.Context(), "sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "Failed" || state.ExitCode == nil || *state.ExitCode != 4 || state.StoppedAt.IsZero() {
		t.Fatalf("after the workload exited: %+v", state)
	}
	// The same shape reaches a caller through List, which reads other fields.
	states, err := d.List(t.Context(), driver.Filter{IDs: []string{"sbx_a"}})
	if err != nil || len(states) != 1 {
		t.Fatalf("List: %d %v", len(states), err)
	}
	if states[0].Phase != "Failed" || states[0].StoppedAt.IsZero() || states[0].StartedAt.IsZero() {
		t.Fatalf("List after the workload exited: %+v", states[0])
	}
}

func TestEngineFaultsReachTheCaller(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	for name, tc := range map[string]struct {
		fault string
		call  func() error
	}{
		"inspectContainer": {"GET " + libpod + "/containers/cella-sbx_a/json", func() error {
			_, err := d.Inspect(t.Context(), "sbx_a")
			return err
		}},
		"listVolumes": {"GET " + libpod + "/volumes/json", func() error {
			_, err := d.List(t.Context(), driver.Filter{})
			return err
		}},
		"listContainers": {"GET " + libpod + "/containers/json", func() error {
			_, err := d.List(t.Context(), driver.Filter{})
			return err
		}},
		"start":  {"POST " + libpod + "/containers/cella-sbx_a/start", func() error { return d.Start(t.Context(), "sbx_a") }},
		"stop":   {"POST " + libpod + "/containers/cella-sbx_a/stop", func() error { return d.Stop(t.Context(), "sbx_a") }},
		"delete": {"DELETE " + libpod + "/containers/cella-sbx_a", func() error { return d.Delete(t.Context(), "sbx_a") }},
		"record": {"POST " + libpod + "/volumes/create", func() error { return d.Touch(t.Context(), "sbx_a") }},
	} {
		t.Run(name, func(t *testing.T) {
			f.fault(tc.fault, http.StatusInternalServerError)
			if err := tc.call(); err == nil {
				t.Fatalf("%s did not report the arranged fault", name)
			}
		})
	}
}

func TestCancelledContexts(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for name, call := range map[string]func() error{
		"Delete": func() error { return d.Delete(ctx, "sbx_a") },
		"Touch":  func() error { return d.Touch(ctx, "sbx_a") },
		"Start":  func() error { return d.Start(ctx, "sbx_a") },
		"Inspect": func() error {
			_, err := d.Inspect(ctx, "sbx_a")
			return err
		},
		"List": func() error {
			_, err := d.List(ctx, driver.Filter{})
			return err
		},
		"Logs": func() error {
			_, err := d.Logs(ctx, "sbx_a", driver.LogsRequest{})
			return err
		},
		"Exec": func() error {
			_, err := d.Exec(ctx, "sbx_a", driver.ExecRequest{Command: []string{"true"}})
			return err
		},
		"ExportTar": func() error { return d.ExportTar(ctx, "sbx_a", nil, nopWriter{}) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s: %v, want context.Canceled", name, err)
			}
		})
	}
}

func TestResourceLimits(t *testing.T) {
	for name, tc := range map[string]struct {
		in   driver.Resources
		want *resourceLimits
	}{
		"none":     {driver.Resources{}, nil},
		"zero":     {driver.Resources{CPU: "0", Memory: "0"}, nil},
		"cpu":      {driver.Resources{CPU: "2"}, &resourceLimits{CPU: &cpuLimit{Period: cpuPeriod, Quota: 200000}}},
		"milli":    {driver.Resources{CPU: "250m"}, &resourceLimits{CPU: &cpuLimit{Period: cpuPeriod, Quota: 25000}}},
		"memory":   {driver.Resources{Memory: "1Gi"}, &resourceLimits{Memory: &memoryLimit{Limit: 1 << 30}}},
		"diskOnly": {driver.Resources{Disk: "10Gi"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := resourcesOf(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("resourcesOf(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestWorkspacePathRules(t *testing.T) {
	if got, err := workspaceRoot(""); err != nil || got != driver.DefaultWorkdir {
		t.Fatalf("an empty path = %q %v", got, err)
	}
	for _, bad := range []string{"relative", "/", "/a/../b", "/trailing/", "/nul\x00"} {
		if _, err := workspaceRoot(bad); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("workspaceRoot(%q) = %v, want ErrInvalid", bad, err)
		}
	}
	if got, err := relative("/workspace", "/workspace"); err != nil || got != "." {
		t.Fatalf("the root = %q %v", got, err)
	}
	if got, err := relative("/workspace", "/workspace/a/b"); err != nil || got != "a/b" {
		t.Fatalf("a path below the root = %q %v", got, err)
	}
	for _, bad := range []string{"relative", "/etc", "/workspace/../etc", "/workspacex", "/nul\x00"} {
		if _, err := relative("/workspace", bad); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("relative(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestExportNames(t *testing.T) {
	for _, tc := range []struct{ rel, entry, want string }{
		{".", "workspace/", ""},
		{".", "workspace/tree/hello.txt", "tree/hello.txt"},
		{"tree", "tree/", "tree"},
		{"tree", "tree/hello.txt", "tree/hello.txt"},
		{"tree", "tree/bin/run.sh", "tree/bin/run.sh"},
		{"a/b", "b/c.txt", "a/b/c.txt"},
	} {
		if got := exportName(tc.rel, tc.entry); got != tc.want {
			t.Errorf("exportName(%q, %q) = %q, want %q", tc.rel, tc.entry, got, tc.want)
		}
	}
}

func TestLabelRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Nanosecond)
	i := identity{id: "sbx_a", name: "n", owner: "o", image: "img", digest: "d",
		workspacePath: "/space", disk: "1Gi", createdAt: now,
		display: &driver.Geometry{Width: 1280, Height: 800},
		ports:   []driver.Port{{Name: "web", Port: 8080, Expose: driver.ExposeNone}}}
	if got := identityOf(i.labels()); !reflect.DeepEqual(got, i) {
		t.Fatalf("identity round trip = %+v, want %+v", got, i)
	}
	// A sandbox that asked for neither reads neither back.
	plain := identity{id: "sbx_b", workspacePath: "/space", createdAt: now}
	if got := identityOf(plain.labels()); !reflect.DeepEqual(got, plain) {
		t.Fatalf("a sandbox with no desktop round trips to %+v", got)
	}
	// A workspace volume written before the path was stamped reads the default.
	bare := identityOf(map[string]string{labelID: "sbx_a"})
	if bare.workspacePath != driver.DefaultWorkdir {
		t.Fatalf("a record with no path reads %q", bare.workspacePath)
	}
	r := record{generation: 7, labels: map[string]string{"a": "1"}, env: map[string]string{"K": "v"},
		stopped: true, lastActivityAt: now, ttl: time.Hour, autoStop: time.Minute, autoDelete: time.Second}
	got := recordOf(r.volumeLabels("sbx_a"))
	if !reflect.DeepEqual(got, r) {
		t.Fatalf("record round trip = %+v, want %+v", got, r)
	}
	// Unreadable values fall back rather than failing a list of every sandbox.
	broken := recordOf(map[string]string{labelGeneration: "x", labelActivity: "x", labelTTL: "x", labelEnv: "{"})
	if broken.generation != 0 || !broken.lastActivityAt.IsZero() || broken.ttl != 0 || broken.env != nil {
		t.Fatalf("a broken record = %+v", broken)
	}
}

func TestClientHelpers(t *testing.T) {
	if got := query("a", "1", "b", "", "c", "3"); got != "a=1&c=3" {
		t.Fatalf("query = %q", got)
	}
	if got := labelKeyFilter(labelID); !strings.Contains(got, labelID) {
		t.Fatalf("labelKeyFilter = %q", got)
	}
	if notFound(errors.New("x")) || conflict(errors.New("x")) {
		t.Fatal("a plain error read as a podman status")
	}
	if !conflict(&apiError{Status: http.StatusConflict}) || !conflict(&apiError{Status: 500, Message: "already exists"}) {
		t.Fatal("conflict does not read podman's two shapes")
	}
	if got := (&apiError{Status: 404, Message: "gone"}).Error(); !strings.Contains(got, "404") || !strings.Contains(got, "gone") {
		t.Fatalf("apiError = %q", got)
	}
}

// TestListAndInspectAgree pins the one clock resolution: the list endpoint
// carries a container's instants as unix seconds and the inspect endpoint to
// the nanosecond, so a state read one way must equal the same state read the
// other or a caller comparing an observed state against a stored one sees
// drift that is not there.
func TestListAndInspectAgree(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Name: "one", Owner: "alice",
		Labels: map[string]string{"k": "1"}, Lifecycle: driver.Lifecycle{TTL: time.Hour, AutoStop: time.Minute}})
	// The engine reports sub-second precision on inspect and whole seconds on
	// the list, which is what podman does.
	c := f.container(t, "sbx_a")
	f.mu.Lock()
	c.startedAt = time.Now().UTC().Add(-time.Minute).Add(637 * time.Millisecond)
	f.mu.Unlock()
	for _, stop := range []bool{false, true} {
		if stop {
			if err := d.Stop(t.Context(), "sbx_a"); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			c.finishedAt = time.Now().UTC().Add(412 * time.Millisecond)
			f.mu.Unlock()
		}
		one, err := d.Inspect(t.Context(), "sbx_a")
		if err != nil {
			t.Fatal(err)
		}
		many, err := d.List(t.Context(), driver.Filter{IDs: []string{"sbx_a"}})
		if err != nil || len(many) != 1 {
			t.Fatalf("List: %d %v", len(many), err)
		}
		if !reflect.DeepEqual(one, many[0]) {
			t.Fatalf("stopped=%v: Inspect reads %+v, List reads %+v", stop, one, many[0])
		}
	}
	if !engineInstant(time.Time{}).IsZero() {
		t.Fatal("a container that never ran reports an instant")
	}
}

// TestCreatedAtPrecedesTheStart holds the ordering a caller reads off one
// sandbox: it was created before it started, whatever second either fell in.
func TestCreatedAtPrecedesTheStart(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	for range 20 {
		state := create(t, d, driver.CreateSpec{ID: "sbx_a"})
		if state.StartedAt.Before(state.CreatedAt) {
			t.Fatalf("StartedAt %v is before CreatedAt %v", state.StartedAt, state.CreatedAt)
		}
		if err := d.Delete(t.Context(), "sbx_a"); err != nil {
			t.Fatal(err)
		}
	}
}
