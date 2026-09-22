// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
)

// workspace is a resolved manifest, which is what Create takes: the boundary
// carries the mode the resolver inferred, because a manifest with none never
// reaches the controller.
func workspace() v1.Sandbox {
	return v1.Sandbox{APIVersion: v1.APIVersion, Kind: "Sandbox", Metadata: v1.Metadata{Name: "work", Labels: map[string]string{"team": "a"}}, Spec: v1.SandboxSpec{
		Environment: "default", Workdir: "/workspace",
		Network: v1.Network{Egress: v1.Egress{Mode: v1.EgressOpen}},
	}}
}
func newController(t *testing.T) (*Controller, Options) {
	t.Helper()
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	o := Options{DataDir: t.TempDir(), Driver: d, Environment: "default"}
	c, err := Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, o
}
func TestDurableLifecycle(t *testing.T) {
	c, o := newController(t)
	ctx := t.Context()
	obj, err := c.Create(ctx, workspace(), "alice", 2)
	if err != nil {
		t.Fatal(err)
	}
	if obj.Status.Phase != driver.Running || obj.Status.Owner != "alice" || c.Environment() != "default" || c.Isolation() != "none" {
		t.Fatal(obj)
	}
	if _, err = Open(t.Context(), o); err == nil {
		t.Fatal("second process opened live store")
	}
	obj.Metadata.Labels["team"] = "mutated"
	got, err := c.Get(ctx, "work", "alice")
	if err != nil || got.Metadata.Labels["team"] != "a" {
		t.Fatal(got, err)
	}
	if _, err = c.Get(ctx, "work", "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err = c.Act(ctx, obj.Status.ID, "start"); !errors.Is(err, ErrPhase) {
		t.Fatal(err)
	}
	if _, err = c.Act(ctx, obj.Status.ID, "unknown"); !errors.Is(err, ErrPhase) {
		t.Fatal(err)
	}
	if _, err = c.Act(ctx, "missing", "delete"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	stopped, err := c.Act(ctx, obj.Status.ID, "stop")
	if err != nil || stopped.Status.Phase != driver.Stopped {
		t.Fatal(stopped, err)
	}
	if _, err = c.Act(ctx, obj.Status.ID, "stop"); !errors.Is(err, ErrPhase) {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err = c.Get(ctx, obj.Status.ID, "")
	if err != nil || got.Status.Owner != "alice" {
		t.Fatal(got, err)
	}
	started, err := c.Act(ctx, obj.Status.ID, "start")
	if err != nil || started.Status.Phase != driver.Running {
		t.Fatal(started, err)
	}
	list := c.List()
	list[0].Metadata.Labels["team"] = "wrong"
	if c.List()[0].Metadata.Labels["team"] != "a" {
		t.Fatal("list aliases store")
	}
	if _, err = c.Act(ctx, obj.Status.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	if len(c.List()) != 0 {
		t.Fatal("deleted row remains")
	}
	if _, err = c.Create(ctx, workspace(), "alice", 1); err != nil {
		t.Fatal("deleted name not reusable", err)
	}
}
func TestQuotaAndNameAtomic(t *testing.T) {
	c, _ := newController(t)
	var success atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			obj := workspace()
			obj.Metadata.Name = ""
			if _, err := c.Create(t.Context(), obj, "alice", 1); err == nil {
				success.Add(1)
			} else if !errors.Is(err, ErrQuota) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if success.Load() != 1 {
		t.Fatal(success.Load())
	}
	if _, err := c.Create(t.Context(), workspace(), "bob", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create(t.Context(), workspace(), "bob", 0); !errors.Is(err, ErrNameTaken) {
		t.Fatal(err)
	}
	if _, err := c.Create(t.Context(), workspace(), "", 0); err == nil {
		t.Fatal("empty owner")
	}
}

type failureDriver struct {
	driver.Driver
	createErr, inspectErr, deleteErr, errorAct error
}

func (d failureDriver) Create(context.Context, driver.CreateSpec) (driver.Ref, error) {
	return driver.Ref{}, d.createErr
}
func (d failureDriver) Inspect(context.Context, string) (driver.State, error) {
	return driver.State{}, d.inspectErr
}
func (d failureDriver) Delete(context.Context, string) error { return d.deleteErr }
func (d failureDriver) Start(context.Context, string) error  { return d.errorAct }
func (d failureDriver) Stop(context.Context, string) error   { return d.errorAct }
func TestFailuresRemainRecoverable(t *testing.T) {
	c, _ := newController(t)
	real, _ := c.driverFor(c.environment)
	c.setDriver(c.environment, failureDriver{Driver: real, createErr: errors.New("create failed"), inspectErr: driver.ErrNotFound, deleteErr: errors.New("delete failed")})
	obj, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err == nil || obj.Status.Phase != "Failed" {
		t.Fatal(obj, err)
	}
	if _, err = c.Act(t.Context(), obj.Status.ID, "delete"); err == nil || len(c.List()) != 1 {
		t.Fatal("delete failure lost state")
	}
	got, err := c.Refresh(t.Context(), obj)
	if err != nil || got.Status.Phase != "Failed" {
		t.Fatal(got, err)
	}
	obj.Status.Phase = driver.Running
	got, err = c.Refresh(t.Context(), obj)
	if err != nil || got.Status.Phase != "Lost" {
		t.Fatal(got, err)
	}
	c.setDriver(c.environment, failureDriver{Driver: real, inspectErr: errors.New("inspection failed"), deleteErr: driver.ErrNotFound})
	if _, err = c.Refresh(t.Context(), obj); err == nil {
		t.Fatal("inspection error lost")
	}
	if _, err = c.Act(t.Context(), obj.Status.ID, "start"); err == nil {
		t.Fatal("inspection error lost")
	}
	if _, err = c.Act(t.Context(), obj.Status.ID, "stop"); err == nil {
		t.Fatal("inspection error lost")
	}
	if _, err = c.Act(t.Context(), obj.Status.ID, "delete"); err != nil || len(c.List()) != 0 {
		t.Fatal(err)
	}
}
func TestCorruptStateRefused(t *testing.T) {
	_, o := newController(t)
	o.DataDir = t.TempDir()
	for _, data := range []string{`broken`, `{"version":9,"objects":{}}`, `{"version":1,"objects":{"bad":{}}}`} {
		if err := os.WriteFile(filepath.Join(o.DataDir, "objects.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if c, err := Open(t.Context(), o); err == nil {
			c.Close()
			t.Fatal("corrupt state accepted")
		}
	}
	if _, err := Open(t.Context(), Options{}); err == nil {
		t.Fatal("missing options")
	}
	bad := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(bad, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	o.DataDir = bad
	if _, err := Open(t.Context(), o); err == nil {
		t.Fatal("file as dir")
	}
}

type memoryStore struct {
	objects          map[string]v1.Sandbox
	loadErr, saveErr error
	calls            int
	failOn           int
}

func (s *memoryStore) Load() (map[string]v1.Sandbox, error) { return s.objects, s.loadErr }
func (s *memoryStore) Save(_ map[string]v1.Sandbox) error {
	s.calls++
	if s.failOn == 0 || s.calls == s.failOn {
		return s.saveErr
	}
	return nil
}
func (s *memoryStore) Close() error { return nil }
func TestStoreFailuresNeverLoseDesiredState(t *testing.T) {
	c, o := newController(t)
	m := &memoryStore{loadErr: errors.New("unavailable")}
	o.Store = m
	if _, err := Open(t.Context(), o); err == nil {
		t.Fatal("load failure ignored")
	}
	m = &memoryStore{saveErr: errors.New("write failed")}
	o.Store = m
	other, err := Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Create(t.Context(), workspace(), "alice", 0); err == nil || len(other.List()) != 0 {
		t.Fatal("failed reservation persisted")
	}
	obj, err := c.Create(t.Context(), workspace(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	c.store = m
	if _, err = c.Act(t.Context(), obj.Status.ID, "delete"); err == nil {
		t.Fatal("intent error ignored")
	}
	if c.List()[0].Status.Phase != "Running" {
		t.Fatal("failed intent changed state")
	}
	m.calls = 0
	m.failOn = 2
	if _, err = c.Act(t.Context(), obj.Status.ID, "delete"); err == nil {
		t.Fatal("deletion save error ignored")
	}
	if len(c.List()) != 1 || c.List()[0].Status.Phase != "Deleting" {
		t.Fatal("desired deletion lost")
	}
	got, err := c.Refresh(t.Context(), c.List()[0])
	if err != nil || got.Status.Phase != "Deleting" {
		t.Fatal(got, err)
	}
}
func TestFileStoreFailures(t *testing.T) {
	if _, err := OpenFileStore(""); err == nil {
		t.Fatal("empty store path")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileStore(dir); err == nil {
		t.Fatal("lock directory")
	}
	dir = t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = os.Mkdir(filepath.Join(dir, "objects.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Load(); err == nil {
		t.Fatal("snapshot directory")
	}
	if err = s.Save(map[string]v1.Sandbox{}); err == nil {
		t.Fatal("rename to directory")
	}
	if err = os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err = s.Save(map[string]v1.Sandbox{}); err == nil {
		t.Fatal("save missing directory")
	}
}

type deleteFailureDriver struct{ driver.Driver }

func (d deleteFailureDriver) Delete(context.Context, string) error {
	return errors.New("temporary runtime outage")
}
func TestDeletingObjectsDoNotConsumeCountQuota(t *testing.T) {
	c, _ := newController(t)
	obj, err := c.Create(t.Context(), workspace(), "alice", 1)
	if err != nil {
		t.Fatal(err)
	}
	c.setDriver(c.environment, deleteFailureDriver{openDriver(c)})
	if _, err = c.Act(t.Context(), obj.Status.ID, "delete"); err == nil {
		t.Fatal("expected cleanup failure")
	}
	next := workspace()
	next.Metadata.Name = "replacement"
	if _, err = c.Create(t.Context(), next, "alice", 1); err != nil {
		t.Fatal("Deleting still consumed count quota", err)
	}
}

// recordingDriver keeps the spec it was created with, so a test can see what
// the manifest turned into.
type recordingDriver struct {
	driver.Driver
	spec driver.CreateSpec
}

func (d *recordingDriver) Create(ctx context.Context, spec driver.CreateSpec) (driver.Ref, error) {
	d.spec = spec
	return d.Driver.Create(ctx, spec)
}

// openDriver is the driver of the environment the controller under test
// drives itself, which is the one a case swaps to make a driver fail.
func openDriver(c *Controller) driver.Driver {
	d, err := c.driverFor(c.environment)
	if err != nil {
		panic(err)
	}
	return d
}

func TestCreateSpecCarriesManifestFields(t *testing.T) {
	c, _ := newController(t)
	recorder := &recordingDriver{Driver: openDriver(c)}
	c.setDriver(c.environment, recorder)
	obj := workspace()
	obj.Spec.User = "1000:1000"
	obj.Spec.Resources = v1.Resources{CPU: "500m", Memory: "2Gi", Disk: "10Gi"}
	obj.Spec.Workspace = v1.Workspace{Path: "/workspace", Source: v1.WorkspaceSourceEmpty}
	obj.Spec.Lifecycle = v1.Lifecycle{AutoStop: "15m", TTL: "1h", AutoDelete: v1.DurationNever}
	obj.Status.Warnings = []string{"The native environment does not limit cpu, memory or disk."}
	got, err := c.Create(t.Context(), obj, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if recorder.spec.User != "1000:1000" || recorder.spec.Workspace.Path != "/workspace" {
		t.Fatalf("spec = %+v", recorder.spec)
	}
	if recorder.spec.Resources != (driver.Resources{CPU: "500m", Memory: "2Gi", Disk: "10Gi"}) {
		t.Fatalf("resources = %+v", recorder.spec.Resources)
	}
	// never is the zero duration the drivers already read as no bound.
	if recorder.spec.Lifecycle != (driver.Lifecycle{AutoStop: 15 * time.Minute, TTL: time.Hour}) {
		t.Fatalf("lifecycle = %+v", recorder.spec.Lifecycle)
	}
	// The resolver's warnings survive the status the controller writes, and
	// the expiry the driver computed is read back.
	if !slices.Equal(got.Status.Warnings, obj.Status.Warnings) {
		t.Fatalf("warnings = %v", got.Status.Warnings)
	}
	if want := got.Status.CreatedAt.Add(time.Hour); got.Status.ExpiresAt.Sub(want).Abs() > time.Minute {
		t.Fatalf("expiresAt = %v, want about %v", got.Status.ExpiresAt, want)
	}
	stored, err := c.Get(t.Context(), got.Status.ID, "alice")
	if err != nil || !slices.Equal(stored.Status.Warnings, obj.Status.Warnings) {
		t.Fatal(stored.Status.Warnings, err)
	}
	stored.Status.Warnings[0] = "mutated"
	if again, _ := c.Get(t.Context(), got.Status.ID, "alice"); again.Status.Warnings[0] == "mutated" {
		t.Fatal("the store aliases its warnings")
	}
	// A lifecycle the resolver would have refused never reaches the driver.
	obj = workspace()
	obj.Metadata.Name = "broken"
	obj.Spec.Lifecycle.TTL = "soon"
	if _, err = c.Create(t.Context(), obj, "alice", 0); err == nil {
		t.Fatal("an unparsable lifecycle reached the driver")
	}
}
