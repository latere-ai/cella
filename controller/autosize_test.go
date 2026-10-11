// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/autosize"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

const (
	mi = int64(1) << 20
	gi = int64(1) << 30
)

// sizeDriver is a driver that sizes a sandbox, over the fake every other
// loop is tested against. It keeps the size each sandbox was asked to hold
// beside the size it holds, answers a change of size with the verdict a test
// set, starts a sandbox at the size it was last asked for, and reads back
// the working set and the count of kills a test wrote.
type sizeDriver struct {
	*fakeDriver
	flags v1.Autosize

	smu sync.Mutex
	// asked is the size each sandbox was last asked to hold, which is what a
	// start gives it, and changes every change of size the driver took.
	asked   map[string]driver.Resources
	changes []driver.Resources
	// verdict is what a running sandbox makes of a change: empty applies it,
	// and any other value leaves the size held and reports that reason.
	// refuse is the error a change returns after it was recorded, as a
	// cluster that refuses the patch does; drop is one returned before it.
	verdict string
	refuse  error
	drop    error
	// noRoom fails every start at a memory above it, which is a cluster
	// with no node that large.
	noRoom int64
	usage  map[string]driver.MemoryUsage
	kills  map[string]int64
	reads  []string
	// usageErr and killsErr fail the two readings.
	usageErr, killsErr error
	instance           int
	// exit is what the next command ends with, and execErr what its stream
	// fails with.
	exit    int
	execErr error
	// inspects counts the reads of one sandbox's state.
	inspects int
	// onStart runs as a start begins, before the sandbox is running again,
	// which is where a test holds the restart open while readers read.
	onStart func(id string)
	// killedAtCreate ends every created workload at once, killed for
	// memory, as a main process that needs more than it was given ends.
	killedAtCreate bool
}

func (d *sizeDriver) Inspect(ctx context.Context, id string) (driver.State, error) {
	d.smu.Lock()
	d.inspects++
	d.smu.Unlock()
	return d.fakeDriver.Inspect(ctx, id)
}

func (d *sizeDriver) inspected() int {
	d.smu.Lock()
	defer d.smu.Unlock()
	return d.inspects
}

func newSizeDriver(clock *fakeClock, flags v1.Autosize) *sizeDriver {
	return &sizeDriver{
		fakeDriver: newDriver(clock), flags: flags,
		asked: map[string]driver.Resources{}, usage: map[string]driver.MemoryUsage{}, kills: map[string]int64{},
	}
}

func (d *sizeDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{Autosize: d.flags, Attach: true, Dial: true}
}

// held is a size as the driver reports it: the CPU limit always named.
func held(r driver.Resources) driver.Resources {
	if r.CPULimit == "" {
		r.CPULimit = r.CPU
	}
	return r
}

func (d *sizeDriver) Create(ctx context.Context, s driver.CreateSpec) (driver.Ref, error) {
	ref, err := d.fakeDriver.Create(ctx, s)
	if err != nil {
		return ref, err
	}
	d.smu.Lock()
	d.asked[s.ID] = held(s.Resources)
	d.instance++
	d.smu.Unlock()
	d.smu.Lock()
	killed := d.killedAtCreate
	d.smu.Unlock()
	d.set(func(f *fakeDriver) {
		state := f.states[s.ID]
		state.Resources = held(s.Resources)
		if killed {
			code := killedExit
			state.Phase, state.Reason, state.ExitCode = PhaseFailed, reasonOOMKilled, &code
		}
		f.states[s.ID] = state
	})
	return ref, nil
}

func (d *sizeDriver) Update(ctx context.Context, id string, c driver.Change) error {
	if c.Resources == nil {
		return d.fakeDriver.Update(ctx, id, c)
	}
	d.smu.Lock()
	defer d.smu.Unlock()
	if d.drop != nil {
		return d.drop
	}
	next := held(*c.Resources)
	next.Disk = d.asked[id].Disk
	found := false
	d.set(func(f *fakeDriver) {
		state, ok := f.states[id]
		if !ok {
			return
		}
		found = true
		d.asked[id] = next
		d.changes = append(d.changes, next)
		verdict := d.verdict
		if d.refuse != nil {
			verdict = driver.ResizeAtRestart
		}
		switch {
		case state.Phase != driver.Running, verdict == "":
			if state.Phase == driver.Running || state.Phase == driver.Stopped {
				state.Resources = next
			}
			state.Resize = nil
		default:
			state.Resize = &driver.Resize{Resources: next, Reason: verdict, Message: "the node has no room"}
		}
		f.states[id] = state
	})
	if !found {
		return driver.ErrNotFound
	}
	return d.refuse
}

func (d *sizeDriver) Start(ctx context.Context, id string) error {
	d.smu.Lock()
	hook := d.onStart
	d.smu.Unlock()
	if hook != nil {
		hook(id)
	}
	d.smu.Lock()
	asked := d.asked[id]
	if d.noRoom > 0 && bytesOf(asked.Memory) > d.noRoom {
		d.smu.Unlock()
		// A Pod that never became ready is removed, which leaves the
		// sandbox stopped at the size it was asked for.
		d.set(func(f *fakeDriver) {
			state := f.states[id]
			state.Phase, state.Reason, state.Resize = driver.Stopped, "", nil
			f.states[id] = state
		})
		return errors.New("the pod was not ready within the timeout: pod not scheduled")
	}
	d.instance++
	d.smu.Unlock()
	if err := d.fakeDriver.Start(ctx, id); err != nil {
		return err
	}
	d.set(func(f *fakeDriver) {
		state := f.states[id]
		state.Resources, state.Resize, state.Reason, state.ExitCode = asked, nil, "", nil
		f.states[id] = state
	})
	return nil
}

func (d *sizeDriver) MemoryUsage(_ context.Context, id string) (driver.MemoryUsage, error) {
	d.smu.Lock()
	defer d.smu.Unlock()
	d.reads = append(d.reads, "usage")
	if d.usageErr != nil {
		return driver.MemoryUsage{}, d.usageErr
	}
	usage, ok := d.usage[id]
	if !ok {
		return driver.MemoryUsage{}, driver.ErrNotRunning
	}
	return usage, nil
}

func (d *sizeDriver) MemoryKills(_ context.Context, id string) (driver.MemoryKills, error) {
	d.smu.Lock()
	defer d.smu.Unlock()
	d.reads = append(d.reads, "kills")
	if d.killsErr != nil {
		return driver.MemoryKills{}, d.killsErr
	}
	return driver.MemoryKills{Count: d.kills[id], Instance: strconv.Itoa(d.instance)}, nil
}

// Exec is a command that ends at once with what the test set.
func (d *sizeDriver) Exec(context.Context, string, driver.ExecRequest) (driver.Exec, error) {
	d.smu.Lock()
	defer d.smu.Unlock()
	return &endedExec{code: d.exit, err: d.execErr}, nil
}

type endedExec struct {
	code   int
	err    error
	closed int
}

func (e *endedExec) Stdout() io.Reader { return strings.NewReader("") }
func (e *endedExec) Stderr() io.Reader { return strings.NewReader("") }
func (e *endedExec) Wait(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return e.code, e.err
}
func (e *endedExec) Close() error { e.closed++; return nil }

type openSession struct{ exit chan int }

func (s *openSession) Read([]byte) (int, error)    { return 0, io.EOF }
func (s *openSession) Write(p []byte) (int, error) { return len(p), nil }
func (s *openSession) Close() error                { return nil }
func (s *openSession) Resize(int, int) error       { return nil }
func (s *openSession) Wait(ctx context.Context) (int, error) {
	select {
	case code := <-s.exit:
		return code, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (d *sizeDriver) Attach(context.Context, string, driver.AttachRequest) (driver.Session, error) {
	return &openSession{exit: make(chan int, 1)}, nil
}

func (d *sizeDriver) Dial(context.Context, string, int) (net.Conn, error) {
	here, there := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, there) }()
	return here, nil
}

func (d *sizeDriver) set2(fn func(*sizeDriver)) {
	d.smu.Lock()
	defer d.smu.Unlock()
	fn(d)
}

func (d *sizeDriver) changed() []driver.Resources {
	d.smu.Lock()
	defer d.smu.Unlock()
	return slices.Clone(d.changes)
}

func (d *sizeDriver) readings() []string {
	d.smu.Lock()
	defer d.smu.Unlock()
	return slices.Clone(d.reads)
}

// sample writes the working set a reading will find, at a new instant each
// time, as the metrics API takes a new sample per window.
func (d *sizeDriver) sample(id string, bytes int64) {
	d.smu.Lock()
	defer d.smu.Unlock()
	at := d.usage[id].ReadAt
	if at.IsZero() {
		at = epoch
	}
	d.usage[id] = driver.MemoryUsage{WorkingSet: bytes, ReadAt: at.Add(15 * time.Second)}
}

// recorded is every record a controller on the snapshot store emitted.
type recorded struct {
	mu   sync.Mutex
	acts []Act
}

func (r *recorded) Emit(_ context.Context, a Act) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acts = append(r.acts, a)
}
func (r *recorded) EmitSecret(context.Context, SecretAct)           {}
func (r *recorded) EmitEnvironment(context.Context, EnvironmentAct) {}

// of is the payload of every record of one type, in order.
func (r *recorded) of(kind string) []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []any
	for _, a := range r.acts {
		if a.Type == kind {
			out = append(out, a.Data)
		}
	}
	return out
}

func (r *recorded) resized() []ResizedData {
	var out []ResizedData
	for _, data := range r.of(MutationResized) {
		out = append(out, data.(ResizedData))
	}
	return out
}

// sizedSandbox is a resolved manifest that starts small under a ceiling.
func sizedSandbox(max v1.ResourceMax) v1.Sandbox {
	obj := workspace()
	obj.Spec.Resources = v1.Resources{CPU: "500m", Memory: "256Mi", Disk: "5Gi", Max: max}
	return obj
}

// sizedController is a controller over a driver that sizes memory and cpu,
// with one running sandbox under the ceiling a case names.
func sizedController(t *testing.T, max v1.ResourceMax) (*Controller, *sizeDriver, *recorded, string) {
	t.Helper()
	clock := newClock()
	d := newSizeDriver(clock, v1.Autosize{CPU: true, Memory: true})
	events := &recorded{}
	c := openController(t, Options{Driver: d, Clock: clock, Events: events})
	obj, err := realized(t.Context(), c, sizedSandbox(max), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	return c, d, events, obj.Status.ID
}

// readSizes runs one pass of the loop and waits for what it left running.
func readSizes(t *testing.T, c *Controller) {
	t.Helper()
	if _, err := c.Autosize(t.Context()); err != nil {
		t.Fatalf("Autosize: %v", err)
	}
	c.sizeWork.Wait()
}

func stored(t *testing.T, c *Controller, id string) v1.Sandbox {
	t.Helper()
	obj, err := c.Get(t.Context(), id, "alice")
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

// TestASandboxWithACeilingRunsAtItAndReportsItsSize: the create asks the
// driver for the cpu ceiling as the limit over the cpu the sandbox was
// granted, and status.resources is what it holds from the first read. A
// manifest with no ceiling asks for what it always did and reports none of
// the three status members.
func TestASandboxWithACeilingRunsAtItAndReportsItsSize(t *testing.T) {
	c, d, _, id := sizedController(t, v1.ResourceMax{CPU: "2", Memory: "1Gi"})
	want := driver.Resources{CPU: "500m", CPULimit: "2", Memory: "256Mi", Disk: "5Gi"}
	if got := d.specs[id].Resources; got != want {
		t.Fatalf("the create asked for %+v, want %+v", got, want)
	}
	obj := stored(t, c, id)
	if got, want := obj.Status.Resources, (v1.Size{CPU: "2", Memory: "256Mi", Disk: "5Gi"}); got != want {
		t.Fatalf("status.resources = %+v, want %+v", got, want)
	}
	if obj.Status.Usage != nil || obj.Status.Moves != 0 {
		t.Fatalf("a sandbox never read reports usage %+v and %d moves", obj.Status.Usage, obj.Status.Moves)
	}

	fixed := workspace()
	fixed.Metadata.Name = "fixed"
	fixed.Spec.Resources = v1.Resources{CPU: "500m", Memory: "256Mi", Disk: "5Gi"}
	plain, err := realized(t.Context(), c, fixed, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := d.specs[plain.Status.ID].Resources, (driver.Resources{CPU: "500m", Memory: "256Mi", Disk: "5Gi"}); got != want {
		t.Fatalf("a sandbox with no ceiling was created at %+v, want %+v", got, want)
	}
	readSizes(t, c)
	encoded, err := json.Marshal(stored(t, c, plain.Status.ID).Status)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{`"resources"`, `"usage"`, `"moves"`, v1.ConditionMemoryAtCeiling} {
		if strings.Contains(string(encoded), member) {
			t.Errorf("the status of a sandbox with no ceiling carries %s: %s", member, encoded)
		}
	}
	if n := len(d.readings()); n != 2 {
		t.Errorf("one pass took %d readings, want the two of the one sandbox with a memory ceiling", n)
	}
}

// TestMemoryIsRaisedAfterReadingsInARow is the first signal: a working set at
// the grow line for autosize.MemoryGrowReadings readings in a row raises
// memory by one step, in place, with the record that says so; one reading,
// or one sample read twice, raises nothing; and the steps end at the ceiling,
// where the driver is asked for nothing more.
func TestMemoryIsRaisedAfterReadingsInARow(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "1Gi"})
	d.sample(id, 240*mi)
	readSizes(t, c)
	readSizes(t, c) // the same sample again
	if n := len(d.changed()); n != 0 {
		t.Fatalf("one sample at the line raised memory: %d changes", n)
	}
	if usage := stored(t, c, id).Status.Usage; usage == nil || usage.Memory != 240*mi || usage.ReadAt.IsZero() {
		t.Fatalf("status.usage = %+v, want the reading", usage)
	}
	d.sample(id, 250*mi)
	readSizes(t, c)
	if got := d.changed(); len(got) != 1 || got[0].Memory != "768Mi" || got[0].CPU != "500m" {
		t.Fatalf("the driver was asked for %+v, want 768Mi with the cpu unchanged", got)
	}
	obj := stored(t, c, id)
	if obj.Status.Resources.Memory != "768Mi" || obj.Status.Moves != 0 {
		t.Fatalf("status = %+v with %d moves, want 768Mi in place", obj.Status.Resources, obj.Status.Moves)
	}
	want := []ResizedData{{Resource: autosize.ResourceMemory, From: "256Mi", To: "768Mi", Reason: autosize.ReasonUsage, Moved: false}}
	if got := events.resized(); !slices.Equal(got, want) {
		t.Fatalf("records = %+v, want %+v", got, want)
	}
	// The raise starts the count again: two more readings at the new line.
	d.sample(id, 700*mi)
	readSizes(t, c)
	if n := len(d.changed()); n != 1 {
		t.Fatalf("the first reading after a raise raised again: %d changes", n)
	}
	d.sample(id, 700*mi)
	readSizes(t, c)
	if got := d.changed(); len(got) != 2 || got[1].Memory != "1Gi" {
		t.Fatalf("the second step asked for %+v, want the ceiling 1Gi", got)
	}
	// At the ceiling nothing is asked of the driver, whatever is read.
	for range 3 {
		d.sample(id, 1000*mi)
		readSizes(t, c)
	}
	if n := len(d.changed()); n != 2 {
		t.Fatalf("a sandbox at its ceiling was resized: %d changes", n)
	}
	if got := events.resized(); len(got) != 2 || got[1] != (ResizedData{Resource: "memory", From: "768Mi", To: "1Gi", Reason: "usage"}) {
		t.Fatalf("records = %+v", got)
	}
}

// TestACeilingEqualToTheSizeChangesNothing: a memory ceiling at the amount
// the sandbox starts at is a size that never changes, with no change asked
// of the driver, no record, and no loop of either.
func TestACeilingEqualToTheSizeChangesNothing(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "256Mi"})
	for range 4 {
		d.sample(id, 200*mi)
		readSizes(t, c)
	}
	if n := len(d.changed()); n != 0 {
		t.Fatalf("a sandbox at its ceiling was asked to change size %d times", n)
	}
	if got := events.resized(); len(got) != 0 {
		t.Fatalf("records = %+v, want none", got)
	}
	if got := stored(t, c, id).Status.Resources.Memory; got != "256Mi" {
		t.Fatalf("status.resources.memory = %s", got)
	}
}

// TestAChildKilledForMemoryRaisesOneStep is the third signal at the
// controller: the first count of a workload instance is its baseline and
// raises nothing, a rise raises memory by one step with reason oom however
// many kills it counted, and a new instance begins a new baseline.
func TestAChildKilledForMemoryRaisesOneStep(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "4Gi"})
	d.set2(func(d *sizeDriver) { d.kills[id] = 3 })
	readSizes(t, c)
	if n := len(d.changed()); n != 0 {
		t.Fatalf("a count that was already there raised memory: %d changes", n)
	}
	d.set2(func(d *sizeDriver) { d.kills[id] = 8 })
	readSizes(t, c)
	if got := d.changed(); len(got) != 1 || got[0].Memory != "768Mi" {
		t.Fatalf("five kills in one reading asked for %+v, want one step to 768Mi", got)
	}
	if got := events.resized(); len(got) != 1 || got[0] != (ResizedData{Resource: "memory", From: "256Mi", To: "768Mi", Reason: autosize.ReasonOOM}) {
		t.Fatalf("records = %+v", got)
	}
	readSizes(t, c)
	if n := len(d.changed()); n != 1 {
		t.Fatalf("one rise raised memory twice: %d changes", n)
	}
	// A stop and a start is a new instance, whose count is a new baseline
	// even where it is above the last one read.
	if _, err := c.Act(t.Context(), id, "stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Start(t.Context(), id, 0); err != nil {
		t.Fatal(err)
	}
	d.set2(func(d *sizeDriver) { d.kills[id] = 20 })
	readSizes(t, c)
	if n := len(d.changed()); n != 1 {
		t.Fatalf("the first count of a new instance raised memory: %d changes", n)
	}
	if got := d.state(id).Resources.Memory; got != "768Mi" {
		t.Fatalf("the start gave the sandbox %s, want the 768Mi it had been raised to", got)
	}
	// A reading the driver cannot take is no signal and no failure.
	d.set2(func(d *sizeDriver) { d.killsErr, d.usageErr = errors.New("exec failed"), errors.New("metrics failed") })
	readSizes(t, c)
	d.set2(func(d *sizeDriver) { d.killsErr, d.usageErr = driver.ErrUnsupported, driver.ErrNotRunning })
	readSizes(t, c)
	if n := len(d.changed()); n != 1 {
		t.Fatalf("a failed reading raised memory: %d changes", n)
	}
}

// TestAFailedCommandIsReadBeforeItsExitIsReported: a command that a signal
// ended, or whose stream broke, has the sandbox read inside its Wait, so the
// raise a kill asks for is given to the driver before the client reads the
// exit; a command that failed any other way is handed to the loop and its
// answer does not wait; and a command that succeeded costs no reading.
func TestAFailedCommandIsReadBeforeItsExitIsReported(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "4Gi"})
	readSizes(t, c) // the baseline
	run := func(code int, streamErr error) (int, error) {
		t.Helper()
		d.set2(func(d *sizeDriver) { d.exit, d.execErr = code, streamErr })
		e, err := c.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"install"}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = e.Close() }()
		if open := c.sessionsOpen(id); open != 1 {
			t.Fatalf("a running command counts as %d sessions, want 1", open)
		}
		return e.Wait(t.Context())
	}
	before := len(d.readings())
	if code, err := run(0, nil); code != 0 || err != nil {
		t.Fatalf("exit = %d, %v", code, err)
	}
	if n := len(d.readings()); n != before {
		t.Fatalf("a command that succeeded cost %d readings", n-before)
	}
	if open := c.sessionsOpen(id); open != 0 {
		t.Fatalf("an ended command still counts as %d sessions", open)
	}

	// A child was killed, and the command reports the signal.
	d.set2(func(d *sizeDriver) { d.kills[id] = 1 })
	if code, err := run(killedExit, nil); code != killedExit || err != nil {
		t.Fatalf("exit = %d, %v", code, err)
	}
	if got := d.changed(); len(got) != 1 || got[0].Memory != "768Mi" {
		t.Fatalf("when the killed command's Wait returned the driver had been asked for %+v, want 768Mi", got)
	}
	c.sizeWork.Wait()
	if got := events.resized(); len(got) != 1 || got[0].Reason != autosize.ReasonOOM || got[0].Moved {
		t.Fatalf("records = %+v, want one raise in place for oom", got)
	}
	// The command reading takes the kill count alone: the working set is
	// the interval's.
	if got := d.readings(); got[len(got)-1] != "kills" || slices.Contains(got[before:], "usage") {
		t.Fatalf("a command reading read %v", got[before:])
	}

	// A command a signal ended for another reason costs the one reading of
	// the count and nothing else: the sandbox's state is not read.
	before = len(d.readings())
	inspected := d.inspected()
	if code, err := run(killedExit, nil); code != killedExit || err != nil {
		t.Fatalf("exit = %d, %v", code, err)
	}
	if got := d.readings()[before:]; len(got) != 1 || got[0] != "kills" || d.inspected() != inspected {
		t.Fatalf("a command killed for another reason read %v and the state %d times", got, d.inspected()-inspected)
	}
	if n := len(d.changed()); n != 1 {
		t.Fatalf("a command killed for another reason raised memory: %d changes", n)
	}

	// A broken stream is read the same way.
	d.set2(func(d *sizeDriver) { d.kills[id] = 2 })
	if _, err := run(0, errors.New("the stream broke")); err == nil {
		t.Fatal("the stream error was swallowed")
	}
	c.sizeWork.Wait()
	if got := d.changed(); len(got) != 2 || got[1].Memory != "1280Mi" {
		t.Fatalf("a broken stream asked for %+v, want the second step", got)
	}

	// Any other failure is the loop's: nothing is read here, and the
	// sandbox is queued for a reading.
	d.set2(func(d *sizeDriver) { d.kills[id] = 3 })
	before = len(d.readings())
	if code, _ := run(1, nil); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	if n := len(d.readings()); n != before {
		t.Fatalf("an ordinary failure was read inside Wait: %d readings", n-before)
	}
	select {
	case woken := <-c.sizeWake:
		if woken != id {
			t.Fatalf("the loop was asked to read %s, want %s", woken, id)
		}
	default:
		t.Fatal("an ordinary failure did not ask the loop for a reading")
	}

	// A command whose caller went away is not read at all.
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	d.set2(func(d *sizeDriver) { d.exit = killedExit })
	e, err := c.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"install"}})
	if err != nil {
		t.Fatal(err)
	}
	before = len(d.readings())
	if _, err := e.Wait(gone); err == nil {
		t.Fatal("a cancelled wait reported no error")
	}
	if n := len(d.readings()); n != before {
		t.Fatalf("a cancelled command was read: %d readings", n-before)
	}

	// A sandbox with no memory ceiling is never read for a failed command.
	fixed := workspace()
	fixed.Metadata.Name = "fixed"
	plain, err := realized(t.Context(), c, fixed, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	before, wakes := len(d.readings()), len(c.sizeWake)
	d.set2(func(d *sizeDriver) { d.exit, d.execErr = killedExit, nil })
	e, err = c.Exec(t.Context(), plain.Status.ID, driver.ExecRequest{Command: []string{"install"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(d.readings()); n != before || len(c.sizeWake) != wakes {
		t.Fatalf("a sandbox with no ceiling was read after a killed command: %d readings, %d wakes", n-before, len(c.sizeWake)-wakes)
	}
}

// warnings is a log a test reads: every record the controller wrote, as
// text, safe to write from the goroutines sizing runs on.
type warnings struct {
	mu   sync.Mutex
	text strings.Builder
}

func (w *warnings) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.text.Write(p)
}

// count is how many records carry the text.
func (w *warnings) count(text string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Count(w.text.String(), text)
}

// TestAKillCountThatCannotBeReadWarnsAfterReadingsInARow: one reading asks
// for the kill count once, so a command that ended killed in a container the
// runtime has not yet reported as ended, which is what a kill of the whole
// container looks like at that instant, is one count that could not be read
// and no warning. The warning is for a count that cannot be read reading
// after reading, is written once, and is written again only after a count
// was read in between.
func TestAKillCountThatCannotBeReadWarnsAfterReadingsInARow(t *testing.T) {
	const warning = "the count of kills for memory cannot be read"
	clock := newClock()
	d := newSizeDriver(clock, v1.Autosize{CPU: true, Memory: true})
	log := &warnings{}
	c := openController(t, Options{Driver: d, Clock: clock, Log: slog.New(slog.NewTextHandler(log, nil))})
	obj, err := realized(t.Context(), c, sizedSandbox(v1.ResourceMax{Memory: "4Gi"}), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	id := obj.Status.ID
	readSizes(t, c) // the baseline
	killed := func() {
		t.Helper()
		e, err := c.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"install"}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = e.Close() }()
		if code, err := e.Wait(t.Context()); code != killedExit || err != nil {
			t.Fatalf("exit = %d, %v", code, err)
		}
		c.sizeWork.Wait()
	}
	d.set2(func(d *sizeDriver) { d.exit, d.killsErr = killedExit, errors.New("container not found") })
	before := len(d.readings())
	killed()
	if got := d.readings()[before:]; len(got) != 1 || got[0] != "kills" {
		t.Fatalf("the reading of a killed command read %v, want the kill count once", got)
	}
	if n := log.count(warning); n != 0 {
		t.Fatalf("one reading that could not count kills warned %d times", n)
	}
	// The next reading cannot count them either: that is the warning.
	readSizes(t, c)
	if n := log.count(warning); n != 1 {
		t.Fatalf("two readings in a row that could not count kills warned %d times, want once", n)
	}
	readSizes(t, c)
	killed()
	if n := log.count(warning); n != 1 {
		t.Fatalf("a count that still cannot be read warned %d times, want once", n)
	}
	// A count that was read ends the run of failures, and one failure
	// after it is one again.
	d.set2(func(d *sizeDriver) { d.killsErr = nil })
	readSizes(t, c)
	d.set2(func(d *sizeDriver) { d.killsErr = errors.New("container not found") })
	killed()
	if n := log.count(warning); n != 1 {
		t.Fatalf("one failure after a count that was read warned: %d warnings", n)
	}
	readSizes(t, c)
	if n := log.count(warning); n != 2 {
		t.Fatalf("a second run of failures warned %d times in all, want 2", n)
	}
	if n := len(d.changed()); n != 0 {
		t.Fatalf("a count that could not be read raised memory: %d changes", n)
	}
}

// TestADeferredRaiseMovesOnceNoSessionIsOpen is the move at the controller: a
// raise the node cannot hold leaves the sandbox running at its size while a
// session is open in it, and restarts it at the larger size, with moved true
// and status.moves counted, at the first reading that finds none.
func TestADeferredRaiseMovesOnceNoSessionIsOpen(t *testing.T) {
	for _, verdict := range []string{driver.ResizeDeferred, driver.ResizeInfeasible} {
		t.Run(verdict, func(t *testing.T) {
			c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "4Gi"})
			d.set2(func(d *sizeDriver) { d.verdict = verdict })
			terminal, err := c.Attach(t.Context(), id, driver.AttachRequest{})
			if err != nil {
				t.Fatal(err)
			}
			d.sample(id, 250*mi)
			readSizes(t, c)
			d.sample(id, 250*mi)
			readSizes(t, c)
			if got := d.changed(); len(got) != 1 || got[0].Memory != "768Mi" {
				t.Fatalf("the driver was asked for %+v", got)
			}
			for range 2 {
				readSizes(t, c)
			}
			if stops, _, _ := d.acted(); len(stops) != 0 {
				t.Fatalf("a sandbox with a terminal open was stopped: %v", stops)
			}
			obj := stored(t, c, id)
			if obj.Status.Resources.Memory != "256Mi" || obj.Status.Moves != 0 || len(events.resized()) != 0 {
				t.Fatalf("a raise still outstanding is reported as %+v, %d moves, %+v", obj.Status.Resources, obj.Status.Moves, events.resized())
			}
			if n := len(d.changed()); n != 1 {
				t.Fatalf("an outstanding raise was asked for again: %d changes", n)
			}
			if err := terminal.Close(); err != nil {
				t.Fatal(err)
			}
			readSizes(t, c)
			stops, _, _ := d.acted()
			if len(stops) != 1 || len(d.started()) != 1 {
				t.Fatalf("the move stopped %v and started %v, want one of each", stops, d.started())
			}
			obj = stored(t, c, id)
			if obj.Status.Resources.Memory != "768Mi" || obj.Status.Moves != 1 || obj.Status.Phase != driver.Running {
				t.Fatalf("after the move: %+v, %d moves, %s", obj.Status.Resources, obj.Status.Moves, obj.Status.Phase)
			}
			want := []ResizedData{{Resource: "memory", From: "256Mi", To: "768Mi", Reason: autosize.ReasonUsage, Moved: true}}
			if got := events.resized(); !slices.Equal(got, want) {
				t.Fatalf("records = %+v, want %+v", got, want)
			}
			// The move is one restart: later readings start nothing more.
			readSizes(t, c)
			if len(d.started()) != 1 {
				t.Fatalf("a moved sandbox was started again: %v", d.started())
			}
		})
	}
}

// TestEverySessionKindHoldsAMove: an exec, a terminal, a dialed connection
// and an archive transfer each count as a session until they end, and each
// ends its count once.
func TestEverySessionKindHoldsAMove(t *testing.T) {
	c, d, _, id := sizedController(t, v1.ResourceMax{Memory: "4Gi"})
	e, err := c.Exec(t.Context(), id, driver.ExecRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := c.Attach(t.Context(), id, driver.AttachRequest{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Attach(t.Context(), id, driver.AttachRequest{})
	if err != nil {
		t.Fatal(err)
	}
	dialer, err := c.Dialer(id)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialer.Dial(t.Context(), id, 8080)
	if err != nil {
		t.Fatal(err)
	}
	if open := c.sessionsOpen(id); open != 4 {
		t.Fatalf("%d sessions are counted, want 4", open)
	}
	// Each end is counted once, however many times it is reached.
	for range 2 {
		_ = e.Close()
		_ = terminal.Close()
		_ = conn.Close()
	}
	if open := c.sessionsOpen(id); open != 1 {
		t.Fatalf("%d sessions are counted after three closed, want 1", open)
	}
	// A terminal whose command ended is over without being closed, and one
	// whose wait was abandoned is not.
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := second.Wait(gone); err == nil {
		t.Fatal("an abandoned wait reported no error")
	}
	if open := c.sessionsOpen(id); open != 1 {
		t.Fatalf("an abandoned wait ended the session: %d counted", open)
	}
	second.(*trackedSession).Session.(*openSession).exit <- 0
	if _, err := second.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if open := c.sessionsOpen(id); open != 0 {
		t.Fatalf("%d sessions are counted after every one ended", open)
	}
	// A transfer counts while it runs and not after. The fake takes
	// neither archive, which is its answer and not the count's.
	_ = c.ExportTar(t.Context(), id, []string{"."}, io.Discard)
	_ = c.ImportTar(t.Context(), id, "/workspace", strings.NewReader(""))
	if open := c.sessionsOpen(id); open != 0 {
		t.Fatalf("%d sessions are counted after two transfers", open)
	}
	// A driver that refuses the session opens no count.
	c.setDriver(c.environment, noDialer{d})
	if _, err := c.Dialer(id); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatal(err)
	}
}

// TestAKillThatTookTheContainerMovesAtOnce is the second signal: a sandbox
// that ended Failed with OOMKilled below its ceiling is started again at the
// next size with a session open or not, because its processes are already
// gone; and at its ceiling it stays Failed, with one at_ceiling record and no
// start however often it is read.
func TestAKillThatTookTheContainerMovesAtOnce(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "768Mi"})
	killed := func() {
		code := killedExit
		d.set(func(f *fakeDriver) {
			state := f.states[id]
			state.Phase, state.Reason, state.ExitCode = PhaseFailed, reasonOOMKilled, &code
			f.states[id] = state
		})
	}
	terminal, err := c.Attach(t.Context(), id, driver.AttachRequest{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = terminal.Close() }()
	killed()
	readSizes(t, c)
	if len(d.started()) != 1 {
		t.Fatalf("a sandbox killed for memory was started %d times, want once and at once", len(d.started()))
	}
	if stops, _, _ := d.acted(); len(stops) != 0 {
		t.Fatalf("a sandbox that had already ended was stopped: %v", stops)
	}
	obj := stored(t, c, id)
	if obj.Status.Phase != driver.Running || obj.Status.Resources.Memory != "768Mi" || obj.Status.Moves != 1 {
		t.Fatalf("after the kill: %s at %+v with %d moves", obj.Status.Phase, obj.Status.Resources, obj.Status.Moves)
	}
	want := []ResizedData{{Resource: "memory", From: "256Mi", To: "768Mi", Reason: autosize.ReasonOOM, Moved: true}}
	if got := events.resized(); !slices.Equal(got, want) {
		t.Fatalf("records = %+v, want %+v", got, want)
	}

	// Killed again, now at its ceiling: nothing starts it, however often
	// it is read, and the crossing is recorded once.
	killed()
	for range 4 {
		readSizes(t, c)
	}
	if len(d.started()) != 1 || len(d.changed()) != 1 {
		t.Fatalf("a sandbox killed at its ceiling was started %d times and resized %d times", len(d.started()), len(d.changed()))
	}
	if got := events.of(MutationAtCeiling); len(got) != 1 || got[0] != (AtCeilingData{Resource: "memory", Used: 768 * mi, Max: "768Mi"}) {
		t.Fatalf("at_ceiling records = %+v, want one for the kill", got)
	}
	if got := conditionOf(stored(t, c, id), v1.ConditionMemoryAtCeiling); got.Status != v1.ConditionTrue || got.Reason != v1.ReasonOOM {
		t.Fatalf("condition = %+v", got)
	}
	// A sandbox that failed for another reason is not sizing's to start.
	d.set(func(f *fakeDriver) {
		state := f.states[id]
		state.Reason = "Error"
		f.states[id] = state
	})
	readSizes(t, c)
	if len(d.started()) != 1 {
		t.Fatal("a sandbox that failed without a kill for memory was started")
	}
}

// TestAMoveThatFindsNoRoomIsUndone: where no node holds the larger size the
// start fails, the sandbox is put back at its previous size and started
// there, nothing is recorded as resized, and its memory is left alone for
// autosize.MoveBackoff before it is asked for again.
func TestAMoveThatFindsNoRoomIsUndone(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "4Gi"})
	d.set2(func(d *sizeDriver) { d.verdict, d.noRoom = driver.ResizeDeferred, 512*mi })
	d.sample(id, 250*mi)
	readSizes(t, c)
	d.sample(id, 250*mi)
	readSizes(t, c)
	state := d.state(id)
	if state.Phase != driver.Running || state.Resources.Memory != "256Mi" || state.Resize != nil {
		t.Fatalf("after a move with no room the sandbox is %s at %+v with %+v outstanding", state.Phase, state.Resources, state.Resize)
	}
	if got := d.changed(); len(got) != 3 || got[2].Memory != "256Mi" {
		t.Fatalf("the driver's sizes were %+v, want the previous size put back last", got)
	}
	obj := stored(t, c, id)
	if obj.Status.Resources.Memory != "256Mi" || obj.Status.Moves != 0 || len(events.resized()) != 0 {
		t.Fatalf("an undone move is reported as %+v, %d moves, %+v", obj.Status.Resources, obj.Status.Moves, events.resized())
	}
	// Inside the backoff the same pressure asks for nothing.
	for range 3 {
		d.sample(id, 250*mi)
		readSizes(t, c)
	}
	if n := len(d.changed()); n != 3 {
		t.Fatalf("memory was asked for again inside the backoff: %d changes", n)
	}
	// And a kill that takes the container inside it leaves it Failed.
	code := killedExit
	d.set(func(f *fakeDriver) {
		state := f.states[id]
		state.Phase, state.Reason, state.ExitCode = PhaseFailed, reasonOOMKilled, &code
		f.states[id] = state
	})
	starts := len(d.started())
	readSizes(t, c)
	if len(d.started()) != starts {
		t.Fatal("a sandbox on hold was started after a kill")
	}
	d.set(func(f *fakeDriver) {
		state := f.states[id]
		state.Phase, state.Reason, state.ExitCode = driver.Running, "", nil
		f.states[id] = state
	})
	// Past it, with room now, the raise is taken.
	c.clock.(*fakeClock).Advance(autosize.MoveBackoff)
	d.set2(func(d *sizeDriver) { d.noRoom = 0 })
	d.sample(id, 250*mi)
	readSizes(t, c)
	d.sample(id, 250*mi)
	readSizes(t, c)
	if got := stored(t, c, id); got.Status.Resources.Memory != "768Mi" || got.Status.Moves != 1 {
		t.Fatalf("after the backoff: %+v with %d moves", got.Status.Resources, got.Status.Moves)
	}
}

// TestARaiseTheClusterRefusesIsGivenAgainAndThenMoved: a change the driver
// recorded and could not give in place is given again at the next reading,
// and where it is refused again the sandbox is moved, as for a deferred one.
// A change that never reached the record restarts nothing.
func TestARaiseTheClusterRefusesIsGivenAgainAndThenMoved(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "4Gi"})
	d.set2(func(d *sizeDriver) { d.refuse = errors.New("the cluster refused the resize") })
	terminal, err := c.Attach(t.Context(), id, driver.AttachRequest{})
	if err != nil {
		t.Fatal(err)
	}
	d.sample(id, 250*mi)
	readSizes(t, c)
	d.sample(id, 250*mi)
	readSizes(t, c)
	if got := d.state(id).Resize; got == nil || got.Reason != driver.ResizeAtRestart {
		t.Fatalf("outstanding = %+v, want the raise waiting for a restart", got)
	}
	// Given again and taken this time: nothing is moved.
	d.set2(func(d *sizeDriver) { d.refuse = nil })
	readSizes(t, c)
	if got := stored(t, c, id); got.Status.Resources.Memory != "256Mi" {
		t.Fatalf("the size was recorded before the reading that finds it applied: %+v", got.Status.Resources)
	}
	readSizes(t, c)
	if got := stored(t, c, id); got.Status.Resources.Memory != "768Mi" || got.Status.Moves != 0 {
		t.Fatalf("a raise taken on the second try: %+v with %d moves", got.Status.Resources, got.Status.Moves)
	}
	if got := events.resized(); len(got) != 1 || got[0].Moved {
		t.Fatalf("records = %+v, want one raise in place", got)
	}
	// Refused every time: moved once the terminal closes.
	d.set2(func(d *sizeDriver) { d.refuse = errors.New("the cluster refused the resize") })
	d.sample(id, 700*mi)
	readSizes(t, c)
	d.sample(id, 700*mi)
	readSizes(t, c)
	readSizes(t, c)
	if len(d.started()) != 0 {
		t.Fatal("a sandbox with a terminal open was moved")
	}
	_ = terminal.Close()
	readSizes(t, c)
	if got := stored(t, c, id); got.Status.Resources.Memory != "1280Mi" || got.Status.Moves != 1 {
		t.Fatalf("a raise refused twice: %+v with %d moves, want a move", got.Status.Resources, got.Status.Moves)
	}

	// A change the driver never recorded ends the move before a restart.
	code := killedExit
	d.set(func(f *fakeDriver) {
		state := f.states[id]
		state.Phase, state.Reason, state.ExitCode = PhaseFailed, reasonOOMKilled, &code
		f.states[id] = state
	})
	d.set2(func(d *sizeDriver) { d.refuse, d.drop = nil, errors.New("the record could not be written") })
	starts := len(d.started())
	readSizes(t, c)
	if len(d.started()) != starts {
		t.Fatal("a sandbox was restarted for a size its record does not hold")
	}
}

// TestAtCeilingOncePerCrossing: with memory at its ceiling, a working set at
// the warning line sets MemoryAtCeiling and emits one sandbox.at_ceiling; the
// readings that stay above it emit nothing more, a reading below it clears
// the condition, and the next crossing emits again. The condition is stored,
// so a control plane that restarts above the line does not repeat the event.
func TestAtCeilingOncePerCrossing(t *testing.T) {
	clock := newClock()
	d := newSizeDriver(clock, v1.Autosize{CPU: true, Memory: true})
	events := &recorded{}
	dir := t.TempDir()
	c := openController(t, Options{Driver: d, Clock: clock, Events: events, DataDir: dir})
	obj, err := realized(t.Context(), c, sizedSandbox(v1.ResourceMax{Memory: "256Mi"}), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	id := obj.Status.ID
	line := 250 * mi
	if !autosize.AtCeiling(line, 256*mi) || autosize.AtCeiling(200*mi, 256*mi) {
		t.Fatal("the fixture's readings are not on the two sides of the warning line")
	}
	d.sample(id, 200*mi)
	readSizes(t, c)
	if got := conditionOf(stored(t, c, id), v1.ConditionMemoryAtCeiling); got.Type != "" {
		t.Fatalf("a sandbox below the line carries %+v", got)
	}
	for range 3 {
		d.sample(id, line)
		readSizes(t, c)
	}
	want := AtCeilingData{Resource: autosize.ResourceMemory, Used: line, Max: "256Mi"}
	if got := events.of(MutationAtCeiling); len(got) != 1 || got[0] != want {
		t.Fatalf("records = %+v, want one %+v", got, want)
	}
	if got := conditionOf(stored(t, c, id), v1.ConditionMemoryAtCeiling); got.Status != v1.ConditionTrue || got.Reason != v1.ReasonUsage || got.Message == "" {
		t.Fatalf("condition = %+v", got)
	}

	// A control plane that restarts above the line reads the stored
	// condition and emits nothing.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = openController(t, Options{Driver: d, Clock: clock, Events: events, DataDir: dir})
	for range 2 {
		d.sample(id, line)
		readSizes(t, c)
	}
	if got := events.of(MutationAtCeiling); len(got) != 1 {
		t.Fatalf("a restart above the line emitted again: %d records", len(got))
	}

	d.sample(id, 100*mi)
	readSizes(t, c)
	if got := conditionOf(stored(t, c, id), v1.ConditionMemoryAtCeiling); got.Status != v1.ConditionFalse || got.Reason != v1.ReasonBelowCeiling {
		t.Fatalf("condition below the line = %+v", got)
	}
	d.sample(id, line)
	readSizes(t, c)
	if got := events.of(MutationAtCeiling); len(got) != 2 {
		t.Fatalf("the second crossing emitted %d records in all, want 2", len(got))
	}

	// A kill at the ceiling is a crossing too, with the last reading as
	// what was in use, and one with no reading at all reports the ceiling.
	d.sample(id, 100*mi)
	readSizes(t, c)
	d.set2(func(d *sizeDriver) { d.kills[id] = 1; d.usageErr = driver.ErrNotRunning })
	readSizes(t, c)
	got := events.of(MutationAtCeiling)
	if len(got) != 3 || got[2] != (AtCeilingData{Resource: "memory", Used: 100 * mi, Max: "256Mi"}) {
		t.Fatalf("a kill at the ceiling recorded %+v", got)
	}
	if cond := conditionOf(stored(t, c, id), v1.ConditionMemoryAtCeiling); cond.Reason != v1.ReasonOOM {
		t.Fatalf("condition after a kill = %+v", cond)
	}
}

// TestRaisingTheCeilingMakesRoomAgain: an update that raises max.memory is
// read at the next reading, where the sandbox at its old ceiling grows and
// its MemoryAtCeiling condition clears with the raise.
func TestRaisingTheCeilingMakesRoomAgain(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "256Mi"})
	d.sample(id, 250*mi)
	readSizes(t, c)
	if len(events.of(MutationAtCeiling)) != 1 {
		t.Fatal("the sandbox is not at its ceiling")
	}
	next := stored(t, c, id)
	next.Spec.Resources.Max.Memory = "2Gi"
	if _, err := c.Update(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if n := len(d.changed()); n != 0 {
		t.Fatalf("raising a memory ceiling asked the driver for %d changes", n)
	}
	d.sample(id, 250*mi)
	readSizes(t, c)
	d.sample(id, 250*mi)
	readSizes(t, c)
	obj := stored(t, c, id)
	if obj.Status.Resources.Memory != "768Mi" {
		t.Fatalf("under the raised ceiling the sandbox holds %s", obj.Status.Resources.Memory)
	}
	if got := conditionOf(obj, v1.ConditionMemoryAtCeiling); got.Status != v1.ConditionFalse {
		t.Fatalf("condition after the raise = %+v", got)
	}
}

// TestAnUpdateOfTheCPUCeilingReachesTheDriver: max.cpu moved by an update is
// the limit the driver is asked for at once, with the memory the sandbox
// holds now and not the amount it started at; the status follows what the
// driver reports; and an update that changes no size calls no driver.
func TestAnUpdateOfTheCPUCeilingReachesTheDriver(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{CPU: "2", Memory: "4Gi"})
	d.set2(func(d *sizeDriver) { d.kills[id] = 0 })
	readSizes(t, c)
	d.set2(func(d *sizeDriver) { d.kills[id] = 1 })
	readSizes(t, c)
	next := stored(t, c, id)
	next.Metadata.Labels = map[string]string{"team": "b"}
	if _, err := c.Update(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if n := len(d.changed()); n != 1 {
		t.Fatalf("an update of the labels asked the driver for a size: %d changes", n)
	}
	next.Spec.Resources.Max.CPU = "4"
	if _, err := c.Update(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	got := d.changed()
	if len(got) != 2 || got[1] != (driver.Resources{CPU: "500m", CPULimit: "4", Memory: "768Mi", Disk: "5Gi"}) {
		t.Fatalf("the driver was asked for %+v, want the new cpu limit with the memory held", got)
	}
	select {
	case woken := <-c.sizeWake:
		if woken != id {
			t.Fatalf("the update asked for a reading of %s", woken)
		}
	default:
		t.Fatal("the update asked for no reading")
	}
	readSizes(t, c)
	if obj := stored(t, c, id); obj.Status.Resources != (v1.Size{CPU: "4", Memory: "768Mi", Disk: "5Gi"}) {
		t.Fatalf("status.resources = %+v", obj.Status.Resources)
	}
	if n := len(events.resized()); n != 1 {
		t.Fatalf("a change of the cpu ceiling wrote %d resized records, want the one of the memory raise", n)
	}
	// The cpu ceiling removed: the limit is the cpu again.
	next = stored(t, c, id)
	next.Spec.Resources.Max.CPU = ""
	if _, err := c.Update(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if got := d.changed(); got[len(got)-1].CPULimit != "500m" {
		t.Fatalf("removing the cpu ceiling asked for %+v", got[len(got)-1])
	}
	// A driver that recorded the size and could not give it in place has
	// taken the update: the sandbox holds it at its next start.
	d.set2(func(d *sizeDriver) { d.refuse = errors.New("the cluster refused the resize") })
	next.Spec.Resources.Max.CPU = "3"
	if _, err := c.Update(t.Context(), next); err != nil {
		t.Fatalf("an update whose size the driver recorded was refused: %v", err)
	}
	if got := d.state(id).Resize; got == nil || got.Resources.CPULimit != "3" || got.Reason != driver.ResizeAtRestart {
		t.Fatalf("outstanding after the update = %+v, want the cpu limit waiting for a restart", got)
	}
	if got := stored(t, c, id).Spec.Resources.Max.CPU; got != "3" {
		t.Fatalf("the update stored the cpu ceiling %q", got)
	}
	d.set2(func(d *sizeDriver) { d.refuse = nil })
	next = stored(t, c, id)
	// A driver that refuses the change refuses the update, and nothing is
	// stored.
	d.set2(func(d *sizeDriver) { d.drop = errors.New("the record could not be written") })
	next.Spec.Resources.Max.CPU = "8"
	if _, err := c.Update(t.Context(), next); err == nil {
		t.Fatal("an update whose size the driver refused was accepted")
	}
	if got := stored(t, c, id).Spec.Resources.Max.CPU; got != "3" {
		t.Fatalf("a refused update stored the cpu ceiling %q", got)
	}
}

// TestAnUpdateOfTheAmountResetsTheSize: where a driver declares Resize, an
// update of spec.resources.memory is the size from then on, replacing what
// the sandbox had been raised to, and its record says update; a stopped
// sandbox takes it in its record and starts at it; and a sandbox whose last
// ceiling was removed reports no size of its own.
func TestAnUpdateOfTheAmountResetsTheSize(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "8Gi"})
	readSizes(t, c)
	d.set2(func(d *sizeDriver) { d.kills[id] = 1 })
	readSizes(t, c)
	if got := stored(t, c, id).Status.Resources.Memory; got != "768Mi" {
		t.Fatalf("the fixture was raised to %s", got)
	}
	next := stored(t, c, id)
	next.Spec.Resources.Memory = "2Gi"
	if _, err := c.Update(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	got := d.changed()
	if got[len(got)-1].Memory != "2Gi" {
		t.Fatalf("the driver was asked for %+v, want the explicit 2Gi", got[len(got)-1])
	}
	readSizes(t, c)
	if obj := stored(t, c, id); obj.Status.Resources.Memory != "2Gi" {
		t.Fatalf("status.resources.memory = %s", obj.Status.Resources.Memory)
	}
	records := events.resized()
	if last := records[len(records)-1]; last != (ResizedData{Resource: "memory", From: "768Mi", To: "2Gi", Reason: autosize.ReasonUpdate}) {
		t.Fatalf("the record of an explicit size is %+v", last)
	}

	// Lowered while stopped: recorded, and the start takes it.
	if _, err := c.Act(t.Context(), id, "stop"); err != nil {
		t.Fatal(err)
	}
	next = stored(t, c, id)
	next.Spec.Resources.Memory = "512Mi"
	if _, err := c.Update(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	readSizes(t, c)
	if obj := stored(t, c, id); obj.Status.Resources.Memory != "512Mi" {
		t.Fatalf("a stopped sandbox reports %s after an explicit size", obj.Status.Resources.Memory)
	}
	if _, err := c.Start(t.Context(), id, 0); err != nil {
		t.Fatal(err)
	}
	if got := d.state(id).Resources.Memory; got != "512Mi" {
		t.Fatalf("the start gave the sandbox %s, want the explicit 512Mi", got)
	}

	// The ceiling removed: no size of its own, and no condition.
	next = stored(t, c, id)
	next.Spec.Resources.Max = v1.ResourceMax{}
	updated, err := c.Update(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status.Resources != (v1.Size{}) || updated.Status.Usage != nil {
		t.Fatalf("a sandbox with no ceiling left reports %+v and %+v", updated.Status.Resources, updated.Status.Usage)
	}
	if c.watched(id) {
		t.Fatal("a sandbox with no memory ceiling is still watched")
	}
	// A ceiling added back gives it one again, from what the manifest asks.
	next = stored(t, c, id)
	next.Spec.Resources.Max = v1.ResourceMax{CPU: "1"}
	if updated, err = c.Update(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Resources != (v1.Size{CPU: "1", Memory: "512Mi", Disk: "5Gi"}) {
		t.Fatalf("a ceiling added by an update reports %+v", updated.Status.Resources)
	}
}

// TestRecoveryAndCapacityReadTheSizeHeld: a sandbox raised inside its ceiling
// is recreated at the size it holds, not the amount it started at, and holds
// that much of its environment's declared memory.
func TestRecoveryAndCapacityReadTheSizeHeld(t *testing.T) {
	c, d, _, id := sizedController(t, v1.ResourceMax{CPU: "2", Memory: "4Gi"})
	readSizes(t, c)
	d.set2(func(d *sizeDriver) { d.kills[id] = 1 })
	readSizes(t, c)
	obj := stored(t, c, id)
	if got := sizeFor(obj); got != (driver.Resources{CPU: "500m", CPULimit: "2", Memory: "768Mi", Disk: "5Gi"}) {
		t.Fatalf("the size a recreation asks for is %+v", got)
	}
	c.mu.Lock()
	used, err := c.usageOn(c.environment)
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if used.memory != 768*mi*1000 || used.cpu != 500 {
		t.Fatalf("the sandbox holds %d of memory and %d of cpu, want 768Mi and the 500m it is guaranteed", used.memory/1000, used.cpu)
	}
	// A status the control plane cannot read bounds nothing.
	obj.Status.Resources.Memory = "plenty"
	if got := sizeFor(obj).Memory; got != "256Mi" {
		t.Fatalf("an unreadable size asked for %s", got)
	}
	if bytesOf("") != 0 || bytesOf("plenty") != 0 || bytesOf("1Gi") != gi {
		t.Fatal("bytesOf misreads a quantity")
	}
}

// TestSizingLeavesOtherSandboxesAndPhasesAlone: a sandbox with no ceiling,
// one on a driver that reports no size, one being created, queued or
// deleted, and one already being read are each left alone.
func TestSizingLeavesOtherSandboxesAndPhasesAlone(t *testing.T) {
	c, d, _, id := sizedController(t, v1.ResourceMax{Memory: "4Gi"})
	if err := c.sizeOne(t.Context(), "sbx_absent", nil, readInterval); err != nil {
		t.Fatal(err)
	}
	// Already claimed: a second reading is dropped, not run beside it.
	if _, ok := c.claim(id); !ok {
		t.Fatal("a readable sandbox was not claimed")
	}
	before := len(d.readings())
	readSizes(t, c)
	if n := len(d.readings()); n != before {
		t.Fatalf("a sandbox already being read was read again: %d readings", n-before)
	}
	c.release(id)
	// A driver whose state names no size changes none.
	d.set(func(f *fakeDriver) {
		state := f.states[id]
		state.Resources = driver.Resources{}
		f.states[id] = state
	})
	readSizes(t, c)
	if n := len(d.readings()); n != before {
		t.Fatalf("a driver that reports no size was read for usage: %d readings", n-before)
	}
	d.set(func(f *fakeDriver) {
		state := f.states[id]
		state.Resources = held(driver.Resources{CPU: "500m", Memory: "256Mi", Disk: "5Gi"})
		f.states[id] = state
	})
	for _, phase := range []string{PhaseDeleting, PhaseQueued, PhaseLost, PhaseRecovering} {
		c.mu.Lock()
		obj := c.objects[id]
		obj.Status.Phase = phase
		c.objects[id] = obj
		c.mu.Unlock()
		if _, ok := c.claim(id); ok {
			t.Fatalf("a %s sandbox was claimed for a reading", phase)
		}
	}
	c.mu.Lock()
	obj := c.objects[id]
	obj.Status.Phase = driver.Running
	c.objects[id] = obj
	c.mu.Unlock()
	// A sandbox the driver reports Pending is read for nothing yet.
	d.set(func(f *fakeDriver) {
		state := f.states[id]
		state.Phase = driver.Pending
		f.states[id] = state
	})
	readSizes(t, c)
	if n := len(d.readings()); n != before {
		t.Fatalf("a pending sandbox was read: %d readings", n-before)
	}
	// A sandbox the driver no longer has, and a driver that cannot be
	// read, end the reading without a change.
	d.set(func(f *fakeDriver) { delete(f.states, id) })
	if err := c.sizeOne(t.Context(), id, nil, readWake); err != nil {
		t.Fatalf("a sandbox the driver lost: %v", err)
	}
	d.set(func(f *fakeDriver) { f.inspectErr = errors.New("the cluster did not answer") })
	if err := c.sizeOne(t.Context(), id, nil, readWake); err == nil {
		t.Fatal("a driver that did not answer was read as fine")
	}
	d.set(func(f *fakeDriver) { f.inspectErr, f.listErr = nil, errors.New("the cluster did not answer") })
	if _, err := c.Autosize(t.Context()); err == nil {
		t.Fatal("a list that failed was read as an empty environment")
	}
}

// TestADeleteDuringAMoveRemovesWhatTheMoveMade: a sandbox deleted while it is
// being moved is not brought back by the move, and a start, a stop and the
// deadline rules leave a moving sandbox alone.
func TestADeleteDuringAMoveRemovesWhatTheMoveMade(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "4Gi"})
	setMoving(c, id, true)
	c.mu.Lock()
	obj := c.objects[id]
	c.mu.Unlock()
	if _, err := c.Start(t.Context(), id, 0); !errors.Is(err, ErrPhase) {
		t.Fatalf("a start of a moving sandbox: %v, want ErrPhase", err)
	}
	if _, err := c.Act(t.Context(), id, "stop"); !errors.Is(err, ErrPhase) {
		t.Fatalf("a stop of a moving sandbox: %v, want ErrPhase", err)
	}
	if done, err := c.enforce(t.Context(), c.environment, id, ReasonAutoStop, c.clock.Now()); done || err != nil {
		t.Fatalf("a deadline rule acted on a moving sandbox: %v, %v", done, err)
	}
	if _, ok := c.claim(id); ok {
		t.Fatal("a moving sandbox was claimed for a reading")
	}
	setMoving(c, id, false)

	// The row is gone by the time the move ends: what the driver still
	// holds is removed and nothing is recorded.
	state := d.state(id)
	c.mu.Lock()
	delete(c.objects, id)
	c.mu.Unlock()
	if err := c.afterMove(t.Context(), id, d, true); err != nil {
		t.Fatal(err)
	}
	if _, deletes, _ := d.acted(); !slices.Contains(deletes, id) {
		t.Fatal("what a move made for a deleted sandbox was left on the driver")
	}
	if len(events.resized()) != 0 {
		t.Fatal("a move of a deleted sandbox was recorded")
	}
	// A move of a sandbox that is gone, or being deleted, does not begin.
	s := sizing{obj: obj, driver: d, memory: true, max: 4 * gi}
	if err := c.move(t.Context(), s, state, 768*mi, autosize.ReasonOOM); err != nil {
		t.Fatal(err)
	}
	if len(d.changed()) != 0 {
		t.Fatal("a move of a sandbox that is gone changed a size")
	}
	// And one asked for no more than is held is no move.
	c.mu.Lock()
	c.objects[id] = obj
	c.mu.Unlock()
	d.put(state)
	if err := c.move(t.Context(), s, state, 256*mi, autosize.ReasonOOM); err != nil {
		t.Fatal(err)
	}
	if len(d.started()) != 0 {
		t.Fatal("a move to the size already held restarted the sandbox")
	}
}

// setMoving marks a sandbox as being restarted at a larger size, or not, as
// a move does around its stop and its start.
func setMoving(c *Controller, id string, moving bool) {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	if moving {
		c.size.moving[id] = struct{}{}
		return
	}
	delete(c.size.moving, id)
}

// claimed reports whether a reading or a move holds the sandbox.
func claimed(c *Controller, id string) bool {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	_, busy := c.size.busy[id]
	return busy
}

// TestTheLoopReadsOnItsTickAndOnAWake: RunAutosize reads at once, on every
// tick, and when a failed command or a start asks, and only while it holds
// the autosize lease.
func TestTheLoopReadsOnItsTickAndOnAWake(t *testing.T) {
	clock := newClock()
	d := newSizeDriver(clock, v1.Autosize{CPU: true, Memory: true})
	lease := &fakeLease{held: true}
	c := openController(t, Options{Driver: d, Clock: clock, Lease: lease, Events: &recorded{}})
	obj, err := realized(t.Context(), c, sizedSandbox(v1.ResourceMax{Memory: "4Gi"}), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	id := obj.Status.ID
	// The start asked for the baseline of the new instance.
	if len(c.sizeWake) != 1 {
		t.Fatalf("a started sandbox asked for %d readings, want 1", len(c.sizeWake))
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.RunAutosize(ctx) }()
	wait := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	// The pass at start and the wake each read the kill count.
	wait("the first readings", func() bool { return len(d.readings()) >= 3 })
	d.set2(func(d *sizeDriver) { d.kills[id] = 1 })
	c.wakeSizing(id)
	wait("the raise a wake found", func() bool { return len(d.changed()) == 1 })
	// A tick is one pass, which reads the count and the working set; each
	// is waited for, so the next sample is the next pass's.
	for range 2 {
		wait("the raise to be recorded", func() bool {
			return d.state(id).Resources.Memory == "768Mi" && c.sessionsOpen(id) == 0 && !claimed(c, id)
		})
		taken := len(d.readings())
		d.sample(id, 700*mi)
		clock.ticks <- clock.Now()
		wait("a tick's readings", func() bool { return len(d.readings()) >= taken+2 })
	}
	wait("the raise two ticks found", func() bool { return len(d.changed()) == 2 })
	// Without the lease nothing is read.
	lease.answer(false, nil)
	clock.ticks <- clock.Now()
	c.wakeSizing(id)
	clock.ticks <- clock.Now()
	before := len(d.readings())
	lease.answer(false, errors.New("the store is unavailable"))
	clock.ticks <- clock.Now()
	clock.ticks <- clock.Now()
	if n := len(d.readings()); n != before {
		t.Fatalf("a replica without the lease took %d readings", n-before)
	}
	cancel()
	<-done
	if !clock.tickerStopped() {
		t.Fatal("the loop left its ticker running")
	}
}

// dataStore is the durable store with design 010's write that carries the
// record's payload, and journalStore the one that appends the record apart.
type dataStore struct {
	*durableStore
	payloads []any
	fail     error
}

func (s *dataStore) WriteData(ctx context.Context, obj v1.Sandbox, mutation string, data any) error {
	if s.fail != nil {
		return s.fail
	}
	if err := s.Write(ctx, obj, mutation); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payloads = append(s.payloads, data)
	return nil
}

type journalStore struct {
	*durableStore
	records []string
}

func (s *journalStore) WriteRecord(_ context.Context, obj v1.Sandbox, mutation string, _ any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, obj.Status.ID+" "+mutation)
	return nil
}

// TestAResizeCommitsWithItsRecord: on a store that writes an object with its
// record's payload, the new size and sandbox.resized are one write; on one
// that cannot, the status is written and the record appended; and a write
// that fails leaves the controller holding the size the store holds.
func TestAResizeCommitsWithItsRecord(t *testing.T) {
	raise := func(t *testing.T, store Store) (*Controller, *sizeDriver, string) {
		t.Helper()
		clock := newClock()
		d := newSizeDriver(clock, v1.Autosize{CPU: true, Memory: true})
		c := openController(t, Options{Driver: d, Clock: clock, Store: store})
		obj, err := realized(t.Context(), c, sizedSandbox(v1.ResourceMax{Memory: "4Gi"}), "alice", 0)
		if err != nil {
			t.Fatal(err)
		}
		id := obj.Status.ID
		readSizes(t, c)
		d.set2(func(d *sizeDriver) { d.kills[id] = 1 })
		return c, d, id
	}
	t.Run("in one write", func(t *testing.T) {
		store := &dataStore{durableStore: newDurable(true)}
		c, _, id := raise(t, store)
		readSizes(t, c)
		if got := store.mutations(id); got[len(got)-1] != MutationResized {
			t.Fatalf("the journal of the sandbox ends %v, want sandbox.resized", got)
		}
		want := ResizedData{Resource: "memory", From: "256Mi", To: "768Mi", Reason: autosize.ReasonOOM}
		if len(store.payloads) != 1 || store.payloads[0] != want {
			t.Fatalf("payloads = %+v, want %+v", store.payloads, want)
		}
		if got := store.objects[id].Status.Resources.Memory; got != "768Mi" {
			t.Fatalf("the stored size is %s", got)
		}
	})
	t.Run("a failed write changes nothing held", func(t *testing.T) {
		store := &dataStore{durableStore: newDurable(true), fail: errors.New("the store is unavailable")}
		c, _, id := raise(t, store)
		readSizes(t, c)
		if got := stored(t, c, id).Status.Resources.Memory; got != "256Mi" {
			t.Fatalf("after a failed write the controller reports %s", got)
		}
		// The next reading finds the size the driver holds and writes it.
		store.fail = nil
		readSizes(t, c)
		if got := stored(t, c, id).Status.Resources.Memory; got != "768Mi" {
			t.Fatalf("the retried write left %s", got)
		}
	})
	t.Run("status and then the record", func(t *testing.T) {
		store := &journalStore{durableStore: newDurable(true)}
		c, _, id := raise(t, store)
		readSizes(t, c)
		if got := store.mutations(id); got[len(got)-1] != MutationStatus {
			t.Fatalf("the journal of the sandbox ends %v, want the status write", got)
		}
		if want := []string{id + " " + MutationResized}; !slices.Equal(store.records, want) {
			t.Fatalf("records = %v, want %v", store.records, want)
		}
	})
	t.Run("a store with no journal seam", func(t *testing.T) {
		store := newDurable(true)
		c, _, id := raise(t, store)
		readSizes(t, c)
		if got := stored(t, c, id).Status.Resources.Memory; got != "768Mi" {
			t.Fatalf("the size is %s", got)
		}
		store.mu.Lock()
		store.writeErr = errors.New("the store is unavailable")
		store.mu.Unlock()
		c.mu.Lock()
		err := c.persistWith(t.Context(), c.objects[id], MutationResized, ResizedData{})
		c.mu.Unlock()
		if err == nil {
			t.Fatal("a failed status write was reported as written")
		}
	})
}

// TestARaiseStillBeingAppliedIsRecordedWhenItLands: a raise the substrate has
// taken and not yet applied is waited for; when it lands inside the wait the
// size is recorded at once, and when the wait ends first it is recorded by
// the next reading. A sandbox that stopped meanwhile ends the wait.
func TestARaiseStillBeingAppliedIsRecordedWhenItLands(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "4Gi"})
	c.settleWait, c.settlePoll = 2*time.Second, time.Millisecond
	apply := func() {
		d.set(func(f *fakeDriver) {
			state := f.states[id]
			if state.Resize != nil {
				state.Resources, state.Resize = state.Resize.Resources, nil
				state.Resources.Disk = "5Gi"
			}
			f.states[id] = state
		})
	}
	d.set2(func(d *sizeDriver) { d.verdict = driver.ResizeInProgress })
	readSizes(t, c)
	d.set2(func(d *sizeDriver) { d.kills[id] = 1 })
	if _, err := c.Autosize(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The kubelet applies it a moment later, while the raise is waited on.
	time.AfterFunc(20*time.Millisecond, apply)
	c.sizeWork.Wait()
	if got := events.resized(); len(got) != 1 || got[0].To != "768Mi" || got[0].Moved {
		t.Fatalf("records = %+v, want the raise recorded when it landed", got)
	}

	// The wait ends first: nothing is recorded until a reading finds it.
	c.settleWait = 5 * time.Millisecond
	d.set2(func(d *sizeDriver) { d.kills[id] = 2 })
	readSizes(t, c)
	if n := len(events.resized()); n != 1 {
		t.Fatalf("a raise not yet applied was recorded: %d records", n)
	}
	readSizes(t, c) // still in progress: left alone
	if n := len(d.changed()); n != 2 {
		t.Fatalf("a raise in progress was asked for again: %d changes", n)
	}
	apply()
	readSizes(t, c)
	if got := events.resized(); len(got) != 2 || got[1].To != "1280Mi" || got[1].Reason != autosize.ReasonOOM {
		t.Fatalf("records = %+v, want the second raise recorded by the reading that found it", got)
	}

	// A sandbox that stopped while its raise was waited on ends the wait,
	// and the stopped sandbox reports the size it will start at.
	c.settleWait = 2 * time.Second
	d.set2(func(d *sizeDriver) { d.kills[id] = 3 })
	if _, err := c.Autosize(t.Context()); err != nil {
		t.Fatal(err)
	}
	d.set(func(f *fakeDriver) {
		state := f.states[id]
		state.Phase, state.Resources, state.Resize = driver.Stopped, state.Resize.Resources, nil
		state.Resources.Disk = "5Gi"
		f.states[id] = state
	})
	c.sizeWork.Wait()
	readSizes(t, c)
	if got := stored(t, c, id).Status.Resources.Memory; got != "2Gi" {
		t.Fatalf("a stopped sandbox reports %s, want the size its next start takes", got)
	}
	// A controller that is closing leaves no work behind a reading.
	c.stopSizing()
	c.follow(id, func(context.Context) error { t.Error("work began after the controller closed"); return nil })
}

// kill ends a sandbox's workload the way a kill that takes the container
// does: the driver reports it Failed with OOMKilled and the code of a signal.
func (d *sizeDriver) kill(id string) {
	code := killedExit
	d.set(func(f *fakeDriver) {
		state := f.states[id]
		state.Phase, state.Reason, state.ExitCode = PhaseFailed, reasonOOMKilled, &code
		f.states[id] = state
	})
}

// readNow is the API's read of one sandbox: the stored object, and then the
// driver's state over it.
func readNow(t *testing.T, c *Controller, id string) v1.Sandbox {
	t.Helper()
	obj, err := c.Get(t.Context(), id, "alice")
	if err != nil {
		t.Fatalf("the sandbox was not found: %v", err)
	}
	fresh, err := c.Refresh(t.Context(), obj)
	if err != nil {
		t.Fatalf("the sandbox could not be read: %v", err)
	}
	return fresh
}

// TestAKilledSandboxWithRoomReadsStarting: a sandbox whose workload was
// killed for memory with room under its ceiling reads Starting with reason
// MemoryRaised from the first read after the kill, before the loop has read
// it at all, and that read asks the loop for its reading once however many
// readers find it. A sandbox killed at its ceiling, with no memory ceiling,
// on a driver that sizes no memory, inside the backoff of a move that found
// no room, or that failed for another reason reads Failed, as it did.
func TestAKilledSandboxWithRoomReadsStarting(t *testing.T) {
	c, d, _, id := sizedController(t, v1.ResourceMax{Memory: "768Mi"})
	for len(c.sizeWake) > 0 {
		c.woken(<-c.sizeWake)
	}
	d.kill(id)
	for range 5 {
		got := readNow(t, c, id)
		if got.Status.Phase != PhaseStarting || got.Status.Reason != v1.ReasonMemoryRaised || got.Status.ExitCode != nil {
			t.Fatalf("a sandbox killed with room reads %s %q with exit %v, want Starting MemoryRaised", got.Status.Phase, got.Status.Reason, got.Status.ExitCode)
		}
	}
	if len(c.sizeWake) != 1 {
		t.Fatalf("five reads of one kill asked the loop for %d readings, want 1", len(c.sizeWake))
	}
	// It is not a sandbox a client starts or stops: the control plane is
	// starting it.
	if _, err := c.Start(t.Context(), id, 0); !errors.Is(err, ErrPhase) {
		t.Fatalf("a start of a sandbox being raised: %v, want ErrPhase", err)
	}
	if _, err := c.Act(t.Context(), id, "stop"); !errors.Is(err, ErrPhase) {
		t.Fatalf("a stop of a sandbox being raised: %v, want ErrPhase", err)
	}
	// Nothing was written as failed, and nothing is until the loop moves it.
	c.mu.Lock()
	written := c.objects[id].Status.Phase
	c.mu.Unlock()
	if written == PhaseFailed {
		t.Fatal("a read recorded the sandbox as Failed")
	}
	readSizes(t, c)
	if got := readNow(t, c, id); got.Status.Phase != driver.Running || got.Status.Reason != "" || got.Status.Resources.Memory != "768Mi" {
		t.Fatalf("after the restart the sandbox reads %s %q at %s", got.Status.Phase, got.Status.Reason, got.Status.Resources.Memory)
	}

	// At its ceiling the same kill is terminal: Failed, with the driver's
	// reason and the exit code, until a client acts.
	d.kill(id)
	if got := readNow(t, c, id); got.Status.Phase != PhaseFailed || got.Status.Reason != reasonOOMKilled || got.Status.ExitCode == nil {
		t.Fatalf("a sandbox killed at its ceiling reads %s %q, want Failed OOMKilled", got.Status.Phase, got.Status.Reason)
	}
	if _, err := c.Start(t.Context(), id, 0); !errors.Is(err, ErrPhase) {
		t.Fatalf("a start of a failed sandbox: %v, want ErrPhase", err)
	}

	for _, tc := range []struct {
		name   string
		max    v1.ResourceMax
		flags  v1.Autosize
		mutate func(c *Controller, d *sizeDriver, id string)
	}{
		{name: "no memory ceiling", max: v1.ResourceMax{CPU: "2"}, flags: v1.Autosize{CPU: true, Memory: true}},
		{name: "a driver that sizes no memory", max: v1.ResourceMax{CPU: "2"}, flags: v1.Autosize{CPU: true},
			mutate: func(c *Controller, _ *sizeDriver, id string) {
				// The ceiling was accepted while the driver sized memory.
				c.mu.Lock()
				obj := c.objects[id]
				obj.Spec.Resources.Max.Memory = "4Gi"
				c.objects[id] = obj
				c.mu.Unlock()
			}},
		{name: "inside the backoff of a move that found no room", max: v1.ResourceMax{Memory: "4Gi"}, flags: v1.Autosize{CPU: true, Memory: true},
			mutate: func(c *Controller, _ *sizeDriver, id string) {
				c.sizeMu.Lock()
				c.size.hold[id] = c.clock.Now().Add(autosize.MoveBackoff)
				c.sizeMu.Unlock()
			}},
		{name: "failed for another reason", max: v1.ResourceMax{Memory: "4Gi"}, flags: v1.Autosize{CPU: true, Memory: true},
			mutate: func(_ *Controller, d *sizeDriver, id string) {
				d.set(func(f *fakeDriver) {
					state := f.states[id]
					state.Reason = "Error"
					f.states[id] = state
				})
			}},
		{name: "a driver that reports no size", max: v1.ResourceMax{Memory: "4Gi"}, flags: v1.Autosize{CPU: true, Memory: true},
			mutate: func(_ *Controller, d *sizeDriver, id string) {
				d.set(func(f *fakeDriver) {
					state := f.states[id]
					state.Resources = driver.Resources{}
					f.states[id] = state
				})
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newClock()
			d := newSizeDriver(clock, tc.flags)
			c := openController(t, Options{Driver: d, Clock: clock, Events: &recorded{}})
			obj, err := realized(t.Context(), c, sizedSandbox(tc.max), "alice", 0)
			if err != nil {
				t.Fatal(err)
			}
			d.kill(obj.Status.ID)
			if tc.mutate != nil {
				tc.mutate(c, d, obj.Status.ID)
			}
			if got := readNow(t, c, obj.Status.ID); got.Status.Phase != PhaseFailed {
				t.Fatalf("the sandbox reads %s %q, want Failed", got.Status.Phase, got.Status.Reason)
			}
		})
	}
}

// phases is every phase a set of readers saw, counted.
type phases struct {
	mu   sync.Mutex
	seen map[string]int
	n    int
}

func (p *phases) saw(phase, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen == nil {
		p.seen = map[string]int{}
	}
	key := phase
	if reason != "" {
		key += " " + reason
	}
	p.seen[key]++
	p.n++
}

func (p *phases) reads() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

func (p *phases) all() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]int{}
	maps.Copy(out, p.seen)
	return out
}

// readers reads one sandbox as the API does, over and over from several
// goroutines, until the test ends them, and returns what they saw. A read
// that fails, or that does not find the sandbox, fails the test.
func readers(t *testing.T, c *Controller, id string) (*phases, func()) {
	t.Helper()
	seen := &phases{}
	stop := make(chan struct{})
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				obj, err := c.Get(context.Background(), id, "alice")
				if err != nil {
					seen.saw("not found", err.Error())
					continue
				}
				fresh, err := c.Refresh(context.Background(), obj)
				if err != nil {
					seen.saw("unread", err.Error())
					continue
				}
				seen.saw(fresh.Status.Phase, fresh.Status.Reason)
			}
		})
	}
	return seen, func() { close(stop); group.Wait() }
}

// await blocks until the readers have read this many more times, so a test
// holds a window open for long enough that every reader crosses it.
func (p *phases) await(t *testing.T, more int) {
	t.Helper()
	target := p.reads() + more
	deadline := time.Now().Add(10 * time.Second)
	for p.reads() < target {
		if time.Now().After(deadline) {
			t.Fatal("the readers stopped reading")
		}
		time.Sleep(time.Millisecond)
	}
}

// onlyRunningAndStarting fails the test where the readers saw any phase but
// Running and Starting with reason MemoryRaised, and where they never saw
// the second, which is a window the test did not hold open.
func onlyRunningAndStarting(t *testing.T, seen *phases) {
	t.Helper()
	starting := PhaseStarting + " " + v1.ReasonMemoryRaised
	saw := seen.all()
	for phase, n := range saw {
		if phase != driver.Running && phase != starting {
			t.Errorf("readers saw %q %d times; a sandbox the control plane is starting again reads Running or Starting only", phase, n)
		}
	}
	if saw[starting] == 0 || saw[driver.Running] == 0 {
		t.Fatalf("readers saw %v, want both Running and Starting", saw)
	}
}

// noLifecycleRecord holds the feed to the same promise as a read: a restart
// for size writes its sandbox.resized and no record that the sandbox failed,
// stopped or started, beyond the started records there were before it.
func noLifecycleRecord(t *testing.T, events *recorded, startedBefore int) {
	t.Helper()
	if failed, stopped := len(events.of(MutationFailed)), len(events.of(MutationStopped)); failed != 0 || stopped != 0 {
		t.Errorf("a restart for size recorded %d sandbox.failed and %d sandbox.stopped, want none", failed, stopped)
	}
	if started := len(events.of(MutationStarted)); started != startedBefore {
		t.Errorf("a restart for size recorded %d sandbox.started, want the %d there were before it", started, startedBefore)
	}
}

// TestReadersNeverSeeAKilledSandboxFail is the guarantee a client relies on:
// while a kill that took the container is found, the sandbox is restarted at
// the next size and comes back, readers that read it continuously see
// Running, then Starting with reason MemoryRaised, then Running, and never
// Failed, Stopped, Lost or nothing at all. The windows are held open while
// every reader crosses them: after the kill and before the loop has read
// anything, and inside the restart, where the driver holds no workload.
func TestReadersNeverSeeAKilledSandboxFail(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "4Gi"})
	started := len(events.of(MutationStarted))
	seen, stop := readers(t, c, id)
	seen.await(t, 50)
	d.set2(func(d *sizeDriver) {
		d.onStart = func(id string) {
			// The driver removed the workload that ended and has not made
			// the next one: what it reports is a sandbox with none.
			d.set(func(f *fakeDriver) {
				state := f.states[id]
				state.Phase, state.Reason, state.ExitCode, state.StoppedAt = driver.Stopped, "", nil, f.clock.Now()
				f.states[id] = state
			})
			seen.await(t, 200)
			// What is stored while the restart runs is that it is starting.
			c.mu.Lock()
			recorded := c.objects[id].Status
			c.mu.Unlock()
			if recorded.Phase != PhaseStarting || recorded.Reason != v1.ReasonMemoryRaised {
				t.Errorf("during the restart the stored status is %s %q, want Starting MemoryRaised", recorded.Phase, recorded.Reason)
			}
		}
	})
	d.kill(id)
	// Nobody has read the sandbox for its size yet: the readers alone meet
	// the kill.
	seen.await(t, 200)
	readSizes(t, c)
	seen.await(t, 50)
	stop()
	onlyRunningAndStarting(t, seen)
	obj := stored(t, c, id)
	if obj.Status.Phase != driver.Running || obj.Status.Reason != "" || obj.Status.Resources.Memory != "768Mi" || obj.Status.Moves != 1 {
		t.Fatalf("after the restart: %s %q at %s with %d moves", obj.Status.Phase, obj.Status.Reason, obj.Status.Resources.Memory, obj.Status.Moves)
	}
	if got := events.resized(); len(got) != 1 || !got[0].Moved || got[0].Reason != autosize.ReasonOOM {
		t.Fatalf("records = %+v", got)
	}
	noLifecycleRecord(t, events, started)
}

// TestReadersNeverSeeAMoveStop: a sandbox moved because its node could not
// hold a raise is stopped and started by the control plane, and readers that
// read it continuously across the stop, the instant it has no workload and
// the start see Running and Starting only, never Stopping or Stopped.
func TestReadersNeverSeeAMoveStop(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "4Gi"})
	started := len(events.of(MutationStarted))
	d.set2(func(d *sizeDriver) { d.verdict = driver.ResizeDeferred })
	seen, stop := readers(t, c, id)
	d.set2(func(d *sizeDriver) {
		d.onStart = func(string) {
			if got := d.state(id).Phase; got != driver.Stopped {
				t.Errorf("the move starts a sandbox its driver reports %s, want it stopped first", got)
			}
			seen.await(t, 200)
		}
	})
	d.sample(id, 250*mi)
	readSizes(t, c)
	d.sample(id, 250*mi)
	readSizes(t, c)
	seen.await(t, 50)
	stop()
	onlyRunningAndStarting(t, seen)
	if obj := stored(t, c, id); obj.Status.Phase != driver.Running || obj.Status.Moves != 1 || obj.Status.Resources.Memory != "768Mi" {
		t.Fatalf("after the move: %s at %s with %d moves", obj.Status.Phase, obj.Status.Resources.Memory, obj.Status.Moves)
	}
	noLifecycleRecord(t, events, started)
}

// TestAMoveThatWasUndoneReadsWhatTheDriverReports: a move that found no room
// leaves no trace of having been starting: the stored status and every read
// say the sandbox runs at the size it had, and a kill inside the backoff is
// terminal, because the control plane is not going to start it again.
func TestAMoveThatWasUndoneReadsWhatTheDriverReports(t *testing.T) {
	c, d, _, id := sizedController(t, v1.ResourceMax{Memory: "4Gi"})
	d.set2(func(d *sizeDriver) { d.noRoom = 512 * mi })
	seen, stop := readers(t, c, id)
	d.kill(id)
	seen.await(t, 100)
	readSizes(t, c)
	seen.await(t, 50)
	stop()
	onlyRunningAndStarting(t, seen)
	c.mu.Lock()
	recorded := c.objects[id].Status
	c.mu.Unlock()
	if recorded.Phase != driver.Running || recorded.Reason != "" || recorded.Resources.Memory != "256Mi" || recorded.Moves != 0 {
		t.Fatalf("after an undone move the stored status is %s %q at %s with %d moves", recorded.Phase, recorded.Reason, recorded.Resources.Memory, recorded.Moves)
	}
	d.kill(id)
	if got := readNow(t, c, id); got.Status.Phase != PhaseFailed || got.Status.Reason != reasonOOMKilled {
		t.Fatalf("a kill inside the backoff reads %s %q, want Failed OOMKilled", got.Status.Phase, got.Status.Reason)
	}
	// Past the backoff, with room now, the same sandbox reads as starting
	// again and is started.
	c.clock.(*fakeClock).Advance(autosize.MoveBackoff)
	d.set2(func(d *sizeDriver) { d.noRoom = 0 })
	if got := readNow(t, c, id); got.Status.Phase != PhaseStarting || got.Status.Reason != v1.ReasonMemoryRaised {
		t.Fatalf("past the backoff the killed sandbox reads %s %q", got.Status.Phase, got.Status.Reason)
	}
	readSizes(t, c)
	if got := readNow(t, c, id); got.Status.Phase != driver.Running || got.Status.Resources.Memory != "768Mi" {
		t.Fatalf("past the backoff the sandbox is %s at %s", got.Status.Phase, got.Status.Resources.Memory)
	}
}

// TestRaisingTheCeilingStartsAKilledSandboxAgain: a sandbox killed at its
// ceiling is Failed until a client acts, and one act that keeps its files is
// to raise max.memory: the update gives it room, every read from then on says
// it is starting, and the loop starts it at the next size.
func TestRaisingTheCeilingStartsAKilledSandboxAgain(t *testing.T) {
	c, d, events, id := sizedController(t, v1.ResourceMax{Memory: "256Mi"})
	d.kill(id)
	readSizes(t, c)
	if got := readNow(t, c, id); got.Status.Phase != PhaseFailed {
		t.Fatalf("a sandbox killed at its ceiling reads %s", got.Status.Phase)
	}
	if len(d.started()) != 0 || len(events.of(MutationAtCeiling)) != 1 {
		t.Fatalf("at its ceiling the sandbox was started %d times with %d at_ceiling records", len(d.started()), len(events.of(MutationAtCeiling)))
	}
	next := stored(t, c, id)
	next.Spec.Resources.Max.Memory = "2Gi"
	if _, err := c.Update(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if got := readNow(t, c, id); got.Status.Phase != PhaseStarting || got.Status.Reason != v1.ReasonMemoryRaised {
		t.Fatalf("with room again the killed sandbox reads %s %q", got.Status.Phase, got.Status.Reason)
	}
	readSizes(t, c)
	obj := readNow(t, c, id)
	if obj.Status.Phase != driver.Running || obj.Status.Resources.Memory != "768Mi" || obj.Status.Moves != 1 {
		t.Fatalf("after the ceiling was raised: %s at %s with %d moves", obj.Status.Phase, obj.Status.Resources.Memory, obj.Status.Moves)
	}
	if got := conditionOf(obj, v1.ConditionMemoryAtCeiling); got.Status != v1.ConditionFalse {
		t.Fatalf("condition after the restart = %+v", got)
	}
}

// TestTheObservedIndexHoldsNoRisingSandboxAsFailed: the index the reaper
// rebuilds from the driver's list holds a sandbox the control plane is
// starting again as Starting, as every read answers it, and one killed at
// its ceiling as the driver reports it.
func TestTheObservedIndexHoldsNoRisingSandboxAsFailed(t *testing.T) {
	clock := newClock()
	d := newSizeDriver(clock, v1.Autosize{CPU: true, Memory: true})
	store := newDurable(true)
	c := openController(t, Options{Driver: d, Clock: clock, Store: store})
	rising, err := realized(t.Context(), c, sizedSandbox(v1.ResourceMax{Memory: "4Gi"}), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	terminal := sizedSandbox(v1.ResourceMax{Memory: "256Mi"})
	terminal.Metadata.Name = "at-its-ceiling"
	atCeiling, err := realized(t.Context(), c, terminal, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	d.kill(rising.Status.ID)
	d.kill(atCeiling.Status.ID)
	d.put(driver.State{ID: "sbx_untracked", Phase: PhaseFailed, Reason: reasonOOMKilled, CreatedAt: epoch})
	if _, err := c.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.index(c.environment)
	got := map[string]string{}
	for _, row := range rows {
		got[row.ID] = row.Phase + " " + row.Reason
	}
	want := map[string]string{
		rising.Status.ID:    PhaseStarting + " " + v1.ReasonMemoryRaised,
		atCeiling.Status.ID: PhaseFailed + " " + reasonOOMKilled,
		"sbx_untracked":     PhaseFailed + " " + reasonOOMKilled,
	}
	for id, phase := range want {
		if got[id] != phase {
			t.Errorf("the index holds %s as %q, want %q", id, got[id], phase)
		}
	}
}

// TestAKillBeforeTheFirstReadIsNoFailedRecord: a main process that needs
// more memory than the sandbox starts with is killed before the create's own
// first read. With room under the ceiling that read is written as it is
// answered, Starting with reason MemoryRaised, and no failed record is
// written for a sandbox that is on its way to running; without room it is
// the failure it always was.
func TestAKillBeforeTheFirstReadIsNoFailedRecord(t *testing.T) {
	create := func(t *testing.T, max v1.ResourceMax) (*Controller, *sizeDriver, *recorded, v1.Sandbox) {
		t.Helper()
		clock := newClock()
		d := newSizeDriver(clock, v1.Autosize{CPU: true, Memory: true})
		d.killedAtCreate = true
		events := &recorded{}
		c := openController(t, Options{Driver: d, Clock: clock, Events: events})
		obj, err := realized(t.Context(), c, sizedSandbox(max), "alice", 0)
		if err != nil {
			t.Fatal(err)
		}
		return c, d, events, obj
	}
	c, d, events, obj := create(t, v1.ResourceMax{Memory: "4Gi"})
	if obj.Status.Phase != PhaseStarting || obj.Status.Reason != v1.ReasonMemoryRaised {
		t.Fatalf("the create's first read is written %s %q, want Starting MemoryRaised", obj.Status.Phase, obj.Status.Reason)
	}
	if failed := events.of(MutationFailed); len(failed) != 0 {
		t.Fatalf("%d failed records were written for a sandbox being started again", len(failed))
	}
	d.set2(func(d *sizeDriver) { d.killedAtCreate = false })
	readSizes(t, c)
	if got := readNow(t, c, obj.Status.ID); got.Status.Phase != driver.Running || got.Status.Resources.Memory != "768Mi" {
		t.Fatalf("after the restart the sandbox is %s at %s", got.Status.Phase, got.Status.Resources.Memory)
	}
	if failed := events.of(MutationFailed); len(failed) != 0 {
		t.Fatalf("%d failed records were written across the restart", len(failed))
	}

	// With no room, the same kill is the failure it was, and is recorded.
	_, _, events, obj = create(t, v1.ResourceMax{Memory: "256Mi"})
	if obj.Status.Phase != PhaseFailed || len(events.of(MutationFailed)) != 1 {
		t.Fatalf("a kill at the ceiling before the first read: %s with %d failed records", obj.Status.Phase, len(events.of(MutationFailed)))
	}
}

// TestAMoveCutShortIsFinished: a control plane that stops in the middle of a
// move, or hands its lease to another replica there, leaves a sandbox whose
// record says it is being started at a larger size. Wherever the move was
// cut, the next process reads the sandbox as starting until it runs, never
// as stopped: with no workload, its first reading finishes the start at the
// size the driver recorded; with the old workload still ending, it reads as
// starting and is finished once the workload is gone; with the new workload
// already running, the reading counts the move and writes what the driver
// reports. A start that fails leaves it stopped and says so.
func TestAMoveCutShortIsFinished(t *testing.T) {
	// cutAt is a sandbox whose move wrote that it is starting and recorded
	// the size, with the workload left as a case names; then the process
	// ended and another opened over the same record.
	cutAt := func(t *testing.T, leave func(d *sizeDriver, id string)) (*Controller, *sizeDriver, *recorded, string) {
		t.Helper()
		clock := newClock()
		d := newSizeDriver(clock, v1.Autosize{CPU: true, Memory: true})
		events := &recorded{}
		dir := t.TempDir()
		c := openController(t, Options{Driver: d, Clock: clock, Events: events, DataDir: dir})
		obj, err := realized(t.Context(), c, sizedSandbox(v1.ResourceMax{Memory: "4Gi"}), "alice", 0)
		if err != nil {
			t.Fatal(err)
		}
		id := obj.Status.ID
		c.mu.Lock()
		marked := clone(c.objects[id])
		marked.Status.Phase, marked.Status.Reason = PhaseStarting, v1.ReasonMemoryRaised
		err = c.persist(t.Context(), marked, MutationStatus)
		c.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		next := driver.Resources{CPU: "500m", Memory: "768Mi"}
		if err := d.Stop(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if err := d.Update(t.Context(), id, driver.Change{Resources: &next}); err != nil {
			t.Fatal(err)
		}
		leave(d, id)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		return openController(t, Options{Driver: d, Clock: clock, Events: events, DataDir: dir}), d, events, id
	}
	// cut is the move cut between its stop and its start.
	cut := func(t *testing.T) (*Controller, *sizeDriver, *recorded, string) {
		t.Helper()
		return cutAt(t, func(*sizeDriver, string) {})
	}
	phaseIs := func(d *sizeDriver, id, phase string) {
		d.set(func(f *fakeDriver) {
			state := f.states[id]
			state.Phase = phase
			f.states[id] = state
		})
	}
	c, d, events, id := cut(t)
	if got := readNow(t, c, id); got.Status.Phase != PhaseStarting || got.Status.Reason != v1.ReasonMemoryRaised {
		t.Fatalf("a sandbox whose move was cut short reads %s %q, want Starting MemoryRaised", got.Status.Phase, got.Status.Reason)
	}
	if _, err := c.Start(t.Context(), id, 0); !errors.Is(err, ErrPhase) {
		t.Fatalf("a client's start of it: %v, want ErrPhase while the control plane finishes the move", err)
	}
	readSizes(t, c)
	got := readNow(t, c, id)
	if got.Status.Phase != driver.Running || got.Status.Resources.Memory != "768Mi" || got.Status.Moves != 1 {
		t.Fatalf("after the move was finished: %s at %s with %d moves", got.Status.Phase, got.Status.Resources.Memory, got.Status.Moves)
	}
	if len(d.started()) != 1 {
		t.Fatalf("the sandbox was started %d times", len(d.started()))
	}
	if records := events.resized(); len(records) != 1 || !records[0].Moved || records[0].To != "768Mi" {
		t.Fatalf("records = %+v", records)
	}

	// The start fails: stopped, said so, and left alone for the backoff.
	c, d, _, id = cut(t)
	d.set(func(f *fakeDriver) { f.startErr = errors.New("no node has room") })
	readSizes(t, c)
	if got := readNow(t, c, id); got.Status.Phase != driver.Stopped {
		t.Fatalf("after a start that failed the sandbox reads %s %q, want Stopped", got.Status.Phase, got.Status.Reason)
	}
	if !c.onHold(id) {
		t.Fatal("a move that could not be finished did not start the backoff")
	}
	// A client may start it now, as it may any stopped sandbox.
	d.set(func(f *fakeDriver) { f.startErr = nil })
	if started, err := c.Start(t.Context(), id, 0); err != nil || started.Status.Phase != driver.Running {
		t.Fatalf("a client's start of the stopped sandbox: %s, %v", started.Status.Phase, err)
	}

	// Cut while the workload the move stopped was still ending: starting,
	// through a reading that has nothing to finish yet, and finished by the
	// first reading after the workload is gone.
	c, d, _, id = cutAt(t, func(d *sizeDriver, id string) { phaseIs(d, id, PhaseStopping) })
	for range 2 {
		if got := readNow(t, c, id); got.Status.Phase != PhaseStarting || got.Status.Reason != v1.ReasonMemoryRaised {
			t.Fatalf("a sandbox whose move was cut while its workload ended reads %s %q, want Starting MemoryRaised", got.Status.Phase, got.Status.Reason)
		}
		readSizes(t, c)
	}
	if n := len(d.started()); n != 0 {
		t.Fatalf("a workload that is still ending was started %d times", n)
	}
	phaseIs(d, id, driver.Stopped)
	readSizes(t, c)
	if got := readNow(t, c, id); got.Status.Phase != driver.Running || got.Status.Resources.Memory != "768Mi" || got.Status.Moves != 1 {
		t.Fatalf("after the workload ended and the move was finished: %s at %s with %d moves", got.Status.Phase, got.Status.Resources.Memory, got.Status.Moves)
	}

	// Cut after the start: the sandbox runs, and the reading that finds it
	// so counts the move and takes the mark off the record, so a stop a
	// client asks for later is read as that stop.
	c, d, events, id = cutAt(t, func(d *sizeDriver, id string) {
		if err := d.Start(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	})
	if got := readNow(t, c, id); got.Status.Phase != driver.Running {
		t.Fatalf("a sandbox whose move was cut after its start reads %s %q, want Running", got.Status.Phase, got.Status.Reason)
	}
	readSizes(t, c)
	kept := stored(t, c, id)
	if kept.Status.Phase != driver.Running || kept.Status.Reason != "" || kept.Status.Moves != 1 || kept.Status.Resources.Memory != "768Mi" {
		t.Fatalf("the record after the reading: %s %q at %s with %d moves, want Running at 768Mi with one move",
			kept.Status.Phase, kept.Status.Reason, kept.Status.Resources.Memory, kept.Status.Moves)
	}
	if records := events.resized(); len(records) != 1 || !records[0].Moved || records[0].To != "768Mi" {
		t.Fatalf("records = %+v, want the one move", records)
	}
	readSizes(t, c)
	if again := stored(t, c, id); again.Status.Moves != 1 || len(events.resized()) != 1 {
		t.Fatalf("a second reading counted the move again: %d moves, %d records", again.Status.Moves, len(events.resized()))
	}
	phaseIs(d, id, PhaseStopping)
	if got := readNow(t, c, id); got.Status.Phase != PhaseStopping {
		t.Fatalf("a sandbox a client is stopping reads %s %q, want Stopping", got.Status.Phase, got.Status.Reason)
	}
}
