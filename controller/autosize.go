// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"latere.ai/x/cella/autosize"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// The two acts of spec 078, as design 009 names them. Each is a status write
// with a record whose payload the act supplies.
const (
	MutationResized   = "sandbox.resized"
	MutationAtCeiling = "sandbox.at_ceiling"
)

// AutosizeLease is the lease the reading loop runs under, so one replica
// reads and raises, and MetricLeaseAutosize its kind for LeaseHeld.
const (
	AutosizeLease       = "autosize"
	MetricLeaseAutosize = "autosize"
)

// reasonOOMKilled is the driver's word for a workload the kernel killed for
// memory: the reason of design 005's edge from Running to Failed.
const reasonOOMKilled = "OOMKilled"

// killedExit is the exit code of a command a signal nobody can catch ended,
// which is how a process the kernel killed for memory reports itself.
const killedExit = 137

// resizeSettle is how long a raise waits for the substrate to say what
// became of it, and resizePoll how often it asks. A raise the substrate has
// not answered by then is read again at the next interval.
const (
	resizeSettle = 10 * time.Second
	resizePoll   = 250 * time.Millisecond
)

// sizeWakes bounds the readings asked for between two ticks of the loop. One
// that does not fit is dropped: the next interval reads the sandbox anyway.
const sizeWakes = 256

// What asked for one reading of one sandbox. The interval reads everything;
// a command that ended badly and a sandbox that just started are read for
// the kill count alone, which is what they can have changed.
const (
	readInterval = iota
	readCommand
)

// ResizedData is design 009's payload for sandbox.resized: which resource
// changed, from what to what in quantity syntax, why, and whether the change
// took a restart on a node with room.
type ResizedData struct {
	Resource string `json:"resource"`
	From     string `json:"from"`
	To       string `json:"to"`
	Reason   string `json:"reason"`
	Moved    bool   `json:"moved"`
}

// AtCeilingData is design 009's payload for sandbox.at_ceiling: which
// resource is at its ceiling, how much of it is in use in bytes, and the
// ceiling as the manifest wrote it.
type AtCeilingData struct {
	Resource string `json:"resource"`
	Used     int64  `json:"used"`
	Max      string `json:"max"`
}

// DataWriter is a Durable that writes an object together with a record whose
// payload the act supplies, in one transaction. A store without it takes the
// status write and the record apart.
type DataWriter interface {
	WriteData(ctx context.Context, obj v1.Sandbox, mutation string, data any) error
}

// sizer is what the controller remembers of sizing between readings. None of
// it is desired state: a process that restarts reads every sandbox again and
// rebuilds it, at the cost of one reading that raises nothing.
//
// Its lock is a leaf: nothing is called while it is held, so it is taken
// with or without the controller's.
type sizer struct {
	// sessions is how many exec, attach, dial and archive sessions are open
	// in each sandbox, which is what a move waits out.
	sessions map[string]int
	// watched is every sandbox with a memory ceiling, so the end of a
	// command asks about one without the controller's lock.
	watched map[string]struct{}
	// readings is the policy's memory of each sandbox, usage the last
	// working set read, and reasons why the size last asked for was asked.
	readings map[string]*autosize.Readings
	usage    map[string]v1.Usage
	reasons  map[string]string
	// hold is until when a sandbox's memory is left alone, after a move
	// that found no room.
	hold map[string]time.Time
	// busy is every sandbox with a reading or a move in flight. A second
	// one for the same sandbox is dropped rather than run beside it.
	busy map[string]struct{}
	// closed says the controller is closing, after which no reading leaves
	// work running behind it.
	closed bool
}

func newSizer() sizer {
	return sizer{
		sessions: map[string]int{}, watched: map[string]struct{}{},
		readings: map[string]*autosize.Readings{}, usage: map[string]v1.Usage{},
		reasons: map[string]string{}, hold: map[string]time.Time{}, busy: map[string]struct{}{},
	}
}

// sized reports whether a manifest names any ceiling, which is what gives a
// sandbox a status.resources at all.
func sized(obj v1.Sandbox) bool { return obj.Spec.Resources.Max != (v1.ResourceMax{}) }

// bytesOf is a quantity as a number of bytes, and zero for one that is empty
// or cannot be read.
func bytesOf(q string) int64 {
	if q == "" {
		return 0
	}
	milli, err := manifest.ParseQuantity(v1.Quantity(q))
	if err != nil {
		return 0
	}
	return milli / 1000
}

// sizeFor is the size a sandbox is to be run at: the CPU it was granted, the
// CPU ceiling as its limit, and the memory it holds now, which is the amount
// its manifest names until it has been raised. It is the one derivation, so
// a create, a recovery and an update ask the driver for the same size.
func sizeFor(obj v1.Sandbox) driver.Resources {
	r := driver.Resources{
		CPU: string(obj.Spec.Resources.CPU), Memory: string(obj.Spec.Resources.Memory),
		Disk: string(obj.Spec.Resources.Disk), CPULimit: string(obj.Spec.Resources.Max.CPU),
	}
	if held := obj.Status.Resources.Memory; held != "" && bytesOf(string(held)) > bytesOf(r.Memory) {
		r.Memory = string(held)
	}
	return r
}

// heldResources is what a sandbox holds of its environment's capacity: the
// CPU it is guaranteed, the memory it has been raised to, and its disk.
func heldResources(obj v1.Sandbox) v1.Resources {
	r := obj.Spec.Resources
	if held := obj.Status.Resources.Memory; held != "" && bytesOf(string(held)) > bytesOf(string(r.Memory)) {
		r.Memory = held
	}
	return r
}

// sizeAsked is status.resources as a create writes it, before any driver has
// reported: the CPU limit, the memory and the workspace the manifest asks
// for.
func sizeAsked(obj v1.Sandbox) v1.Size {
	cpu := obj.Spec.Resources.Max.CPU
	if cpu == "" {
		cpu = obj.Spec.Resources.CPU
	}
	return v1.Size{CPU: cpu, Memory: obj.Spec.Resources.Memory, Disk: obj.Spec.Resources.Disk}
}

// sizeHeld is status.resources as a driver's report gives it. A resource the
// driver does not report keeps what the status already says.
func sizeHeld(current v1.Size, r driver.Resources) v1.Size {
	out := current
	if cpu := r.CPULimit; cpu != "" {
		out.CPU = v1.Quantity(cpu)
	} else if r.CPU != "" {
		out.CPU = v1.Quantity(r.CPU)
	}
	if r.Memory != "" {
		out.Memory = v1.Quantity(r.Memory)
	}
	if r.Disk != "" {
		out.Disk = v1.Quantity(r.Disk)
	}
	return out
}

// conditionStatus is the status of one condition, and empty where the
// sandbox does not carry it.
func conditionStatus(obj v1.Sandbox, kind string) string {
	for _, c := range obj.Status.Conditions {
		if c.Type == kind {
			return c.Status
		}
	}
	return ""
}

// watch keeps the set of sandboxes with a memory ceiling in step with one
// write of desired state. It runs under the controller's lock.
func (c *Controller) watch(obj v1.Sandbox) {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	if obj.Spec.Resources.Max.Memory != "" {
		c.size.watched[obj.Status.ID] = struct{}{}
		return
	}
	delete(c.size.watched, obj.Status.ID)
}

// unwatch forgets everything remembered of a sandbox that is gone.
func (c *Controller) unwatch(id string) {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	delete(c.size.watched, id)
	delete(c.size.readings, id)
	delete(c.size.usage, id)
	delete(c.size.reasons, id)
	delete(c.size.hold, id)
}

func (c *Controller) watched(id string) bool {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	_, ok := c.size.watched[id]
	return ok
}

// sessionOpened and sessionClosed count the sessions a client holds open in
// one sandbox. A move waits for the count to reach zero, so no client's
// command is cut by a restart the control plane chose.
func (c *Controller) sessionOpened(id string) {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	c.size.sessions[id]++
}

func (c *Controller) sessionClosed(id string) {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	if c.size.sessions[id] <= 1 {
		delete(c.size.sessions, id)
		return
	}
	c.size.sessions[id]--
}

func (c *Controller) sessionsOpen(id string) int {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	return c.size.sessions[id]
}

// readingsOf runs one step of the policy's memory of a sandbox under the
// sizer's lock.
func (c *Controller) readingsOf(id string, step func(*autosize.Readings) bool) bool {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	r := c.size.readings[id]
	if r == nil {
		r = &autosize.Readings{}
		c.size.readings[id] = r
	}
	return step(r)
}

// onHold reports whether a sandbox's memory is being left alone after a move
// that found no room.
func (c *Controller) onHold(id string) bool {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	until, held := c.size.hold[id]
	if !held {
		return false
	}
	if !c.clock.Now().Before(until) {
		delete(c.size.hold, id)
		return false
	}
	return true
}

// withUsage is the object as a read returns it: with the last working set
// the loop read, which is held here and written with the sandbox's next
// status write rather than once a reading.
func (c *Controller) withUsage(obj v1.Sandbox) v1.Sandbox {
	if !sized(obj) {
		return obj
	}
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	if reading, held := c.size.usage[obj.Status.ID]; held {
		obj.Status.Usage = &reading
	}
	return obj
}

// persistWith is persist for an act whose record carries a payload of its
// own. A store that writes the two together commits them in one transaction;
// one that cannot takes the status and then the record.
func (c *Controller) persistWith(ctx context.Context, obj v1.Sandbox, mutation string, data any) error {
	if c.durable == nil {
		if err := c.persist(ctx, obj, MutationStatus); err != nil {
			return err
		}
		c.emitData(ctx, mutation, clone(obj), data)
		return nil
	}
	writer, together := c.durable.(DataWriter)
	if !together {
		if err := c.persist(ctx, obj, MutationStatus); err != nil {
			return err
		}
		if c.journaller != nil {
			return c.journaller.WriteRecord(ctx, clone(obj), mutation, data)
		}
		return nil
	}
	id := obj.Status.ID
	previous, held := c.objects[id]
	c.objects[id] = clone(obj)
	if err := writer.WriteData(ctx, clone(obj), mutation, data); err != nil {
		if held {
			c.objects[id] = previous
		} else {
			delete(c.objects, id)
		}
		return err
	}
	c.watch(obj)
	c.notify()
	return nil
}

// RunAutosize reads every sandbox with a ceiling once per interval until ctx
// ends, and in between reads the ones a failed command or a start asked
// about. Each tick runs only while the autosize lease is held.
func (c *Controller) RunAutosize(ctx context.Context) {
	ticks, stop := c.clock.Ticker(c.autosizeInterval)
	defer stop()
	// The first pass runs at once, as the reaper's does: a process that
	// starts with sandboxes running takes the baseline of each kill count
	// before the first interval ends.
	c.autosizeTick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			c.autosizeTick(ctx)
		case id := <-c.sizeWake:
			if c.autosizeLeaseHeld(ctx) {
				if err := c.sizeOne(ctx, id, nil, readCommand); err != nil {
					c.log.WarnContext(ctx, "a sandbox could not be read for its size", "sandbox", id, "err", err)
				}
			}
		}
	}
}

func (c *Controller) autosizeLeaseHeld(ctx context.Context) bool {
	held, err := c.lease.Acquire(ctx, AutosizeLease, LeaseTTL)
	c.metrics.LeaseHeld(MetricLeaseAutosize, err == nil && held)
	if err != nil {
		c.log.WarnContext(ctx, "autosize lease unavailable", "lease", AutosizeLease, "err", err)
		return false
	}
	return held
}

func (c *Controller) autosizeTick(ctx context.Context) {
	if !c.autosizeLeaseHeld(ctx) {
		return
	}
	if acted, err := c.Autosize(ctx); err != nil {
		c.log.WarnContext(ctx, "autosize pass incomplete", "read", acted, "err", err)
	}
}

// Autosize reads every sandbox with a ceiling on every placeable environment
// once, and reports how many it read. An environment below Ready is left
// alone, as the reaper leaves it: a data plane reporting nothing is not one
// whose workloads use nothing.
func (c *Controller) Autosize(ctx context.Context) (int, error) {
	read := 0
	var failed error
	for _, environment := range c.placeable() {
		ids := c.sizedOn(environment)
		if len(ids) == 0 {
			continue
		}
		d, err := c.driverFor(environment)
		if err != nil {
			failed = errors.Join(failed, err)
			continue
		}
		// One list is one view of the environment for every sandbox of
		// this pass, and a list that fails is no list rather than an
		// empty one.
		states, err := d.List(ctx, driver.Filter{IDs: ids})
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("autosize: reading the environment %s: %w", environment, err))
			continue
		}
		for i := range states {
			if err := c.sizeOne(ctx, states[i].ID, &states[i], readInterval); err != nil {
				failed = errors.Join(failed, fmt.Errorf("autosize: %s: %w", states[i].ID, err))
				continue
			}
			read++
		}
	}
	return read, failed
}

// sizedOn is every sandbox of one environment whose manifest names a
// ceiling, in id order.
func (c *Controller) sizedOn(environment string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ids []string
	for id, obj := range c.objects {
		if obj.Status.Environment == environment && sized(obj) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// wakeSizing asks the loop for one reading of one sandbox before its next
// tick. It never blocks.
func (c *Controller) wakeSizing(id string) {
	select {
	case c.sizeWake <- id:
	default:
	}
}

// sizing is one sandbox as a reading acts on it: the object as desired state
// held it when the reading began, the driver of its environment, whether
// that driver sizes memory, and the memory ceiling in bytes, zero for none.
type sizing struct {
	obj    v1.Sandbox
	driver driver.Driver
	memory bool
	max    int64
}

// claim takes one sandbox for a reading, or reports that it is not one to
// read: gone, without a ceiling, still being created, already being read or
// moved, or in a phase no driver holds it in.
func (c *Controller) claim(id string) (sizing, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, held := c.objects[id]
	if !held || !sized(obj) || c.creating(obj) {
		return sizing{}, false
	}
	switch obj.Status.Phase {
	case PhaseDeleting, PhaseQueued, PhaseLost, PhaseRecovering:
		return sizing{}, false
	}
	d, err := c.driverFor(obj.Status.Environment)
	if err != nil {
		return sizing{}, false
	}
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	if _, busy := c.size.busy[id]; busy {
		return sizing{}, false
	}
	c.size.busy[id] = struct{}{}
	max := bytesOf(string(obj.Spec.Resources.Max.Memory))
	return sizing{obj: clone(obj), driver: d, memory: max > 0 && d.Capabilities().Autosize.Memory, max: max}, true
}

func (c *Controller) release(id string) {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	delete(c.size.busy, id)
}

// follow runs what a reading started and does not wait for: the verdict on a
// raise, or a move. It takes over the claim on the sandbox and gives it up
// when the work ends, and it runs on the controller's own context, so it
// outlives the request or the tick that began it and ends with the
// controller.
func (c *Controller) follow(id string, work func(ctx context.Context) error) {
	// The count is raised under the lock Close sets closed under, so no
	// work begins after Close started waiting for the work there is.
	c.sizeMu.Lock()
	if c.size.closed {
		delete(c.size.busy, id)
		c.sizeMu.Unlock()
		return
	}
	c.sizeWork.Add(1)
	c.sizeMu.Unlock()
	go func() {
		defer c.sizeWork.Done()
		defer c.release(id)
		if err := work(c.sizeCtx); err != nil {
			c.log.WarnContext(c.sizeCtx, "a change of size did not finish", "sandbox", id, "err", err)
		}
	}()
}

// stopSizing ends the work readings left running and waits for it.
func (c *Controller) stopSizing() {
	c.sizeMu.Lock()
	c.size.closed = true
	c.sizeMu.Unlock()
	c.sizeStop()
	c.sizeWork.Wait()
}

// sizeOne is one reading of one sandbox and what the policy makes of it.
// observed is the driver's state of it where the caller already holds one.
func (c *Controller) sizeOne(ctx context.Context, id string, observed *driver.State, why int) error {
	s, ok := c.claim(id)
	if !ok {
		return nil
	}
	handed := false
	defer func() {
		if !handed {
			c.release(id)
		}
	}()
	state := observed
	if state == nil {
		read, err := s.driver.Inspect(ctx, id)
		if errors.Is(err, driver.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		state = &read
	}
	if state.Resources.IsZero() {
		// A driver that reports no size is one that changes none.
		return nil
	}
	held := bytesOf(state.Resources.Memory)
	switch state.Phase {
	case PhaseFailed:
		if state.Reason != reasonOOMKilled || !s.memory {
			return nil
		}
		if held >= s.max {
			// Killed at its ceiling: there is nothing more to give, and
			// the sandbox stays Failed as one with no ceiling does.
			return c.ceiling(ctx, s, true, true, -1)
		}
		if c.onHold(id) {
			return nil
		}
		handed = true
		c.follow(id, func(ctx context.Context) error {
			return c.move(ctx, s, *state, autosize.NextMemory(held, s.max), autosize.ReasonOOM)
		})
		return nil
	case driver.Stopped:
		return c.recordSize(ctx, id, *state, false)
	case driver.Running:
	default:
		return nil
	}
	if state.Resize != nil {
		move, err := c.outstanding(ctx, s, *state)
		if err != nil || !move {
			return err
		}
		handed = true
		c.follow(id, func(ctx context.Context) error {
			return c.move(ctx, s, *state, bytesOf(state.Resize.Resources.Memory), "")
		})
		return nil
	}
	if err := c.recordSize(ctx, id, *state, false); err != nil {
		return err
	}
	reader, reads := s.driver.(driver.MemoryReader)
	if !s.memory || !reads {
		return nil
	}
	rose := false
	kills, err := reader.MemoryKills(ctx, id)
	switch {
	case err == nil:
		rose = c.readingsOf(id, func(r *autosize.Readings) bool { return r.Kills(kills.Count, kills.Instance) })
	case !errors.Is(err, driver.ErrNotRunning) && !errors.Is(err, driver.ErrUnsupported):
		c.log.WarnContext(ctx, "the count of kills for memory could not be read", "sandbox", id, "err", err)
	}
	pressed, used := false, int64(-1)
	if why == readInterval {
		usage, err := reader.MemoryUsage(ctx, id)
		switch {
		case err == nil:
			used = usage.WorkingSet
			pressed = c.readingsOf(id, func(r *autosize.Readings) bool { return r.Usage(usage.WorkingSet, held, usage.ReadAt) })
			c.sizeMu.Lock()
			c.size.usage[id] = v1.Usage{Memory: usage.WorkingSet, ReadAt: usage.ReadAt}
			c.sizeMu.Unlock()
		case !errors.Is(err, driver.ErrNotRunning) && !errors.Is(err, driver.ErrUnsupported):
			c.log.WarnContext(ctx, "the working set could not be read", "sandbox", id, "err", err)
		}
	}
	if held >= s.max {
		if !rose && used < 0 {
			return nil
		}
		return c.ceiling(ctx, s, rose || autosize.AtCeiling(used, s.max), rose, used)
	}
	reason := ""
	switch {
	case rose:
		reason = autosize.ReasonOOM
	case pressed:
		reason = autosize.ReasonUsage
	}
	if reason == "" || c.onHold(id) {
		// A sandbox below its ceiling is not at it, whatever it uses.
		return c.ceiling(ctx, s, false, false, used)
	}
	target := autosize.NextMemory(held, s.max)
	next := state.Resources
	next.Memory, next.Disk = autosize.Quantity(target), ""
	c.sizeMu.Lock()
	c.size.reasons[id] = reason
	c.sizeMu.Unlock()
	c.readingsOf(id, func(r *autosize.Readings) bool { r.Raised(); return false })
	if err := s.driver.Update(ctx, id, driver.Change{Resources: &next}); err != nil {
		// The driver may have recorded the size and failed to give it in
		// place. What it reports next says which, and the follow-up acts
		// on that.
		c.log.WarnContext(ctx, "a raise of memory was not taken in place", "sandbox", id, "to", next.Memory, "err", err)
	}
	handed = true
	c.follow(id, func(ctx context.Context) error { return c.settle(ctx, s) })
	return nil
}

// outstanding acts on a size a sandbox was asked to hold and does not hold
// yet, and reports whether the sandbox is to be moved for it. A raise the
// substrate is still applying is left alone; one it was never given in place
// is given again; and one the node cannot hold moves the sandbox once no
// session is open in it. A change that raises no memory moves nothing: CPU
// is compressible, and its limit follows at the next start.
func (c *Controller) outstanding(ctx context.Context, s sizing, state driver.State) (bool, error) {
	id := state.ID
	if reader, reads := s.driver.(driver.MemoryReader); reads && s.memory {
		// The baseline is kept current while the raise is outstanding, so
		// a kill the raise already answers does not ask for a second one.
		if kills, err := reader.MemoryKills(ctx, id); err == nil {
			c.readingsOf(id, func(r *autosize.Readings) bool { return r.Kills(kills.Count, kills.Instance) })
		}
	}
	asked := bytesOf(state.Resize.Resources.Memory)
	if !s.memory || asked <= bytesOf(state.Resources.Memory) {
		return false, nil
	}
	switch state.Resize.Reason {
	case driver.ResizeInProgress:
		return false, nil
	case driver.ResizeAtRestart:
		next := state.Resize.Resources
		next.Disk = ""
		if err := s.driver.Update(ctx, id, driver.Change{Resources: &next}); err == nil {
			return false, nil
		}
	}
	if c.sessionsOpen(id) > 0 || c.onHold(id) {
		return false, nil
	}
	return true, nil
}

// settle waits for the substrate's verdict on a raise it was just asked for:
// applied, and the new size is recorded at once; refused or deferred, and the
// sandbox is moved if no session is open in it. A raise still being applied
// when the wait ends is read again at the next interval.
func (c *Controller) settle(ctx context.Context, s sizing) error {
	id := s.obj.Status.ID
	wait, cancel := context.WithTimeout(ctx, c.settleWait)
	defer cancel()
	for {
		state, err := s.driver.Inspect(wait, id)
		if errors.Is(err, driver.ErrNotFound) || wait.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case state.Phase != driver.Running:
			return nil
		case state.Resize == nil:
			return c.recordSize(ctx, id, state, false)
		case state.Resize.Reason != driver.ResizeInProgress:
			if c.sessionsOpen(id) > 0 {
				return nil
			}
			return c.move(ctx, s, state, bytesOf(state.Resize.Resources.Memory), "")
		}
		select {
		case <-wait.Done():
			return nil
		case <-time.After(c.settlePoll):
		}
	}
}

// move restarts a sandbox at a larger memory size: the size is recorded, the
// sandbox is stopped where it still runs, and it is started again, so its
// driver places it where there is room. A start that fails is undone: the
// previous size is recorded again, the sandbox is started at it, and its
// memory is left alone for MoveBackoff, so a cluster with no room is not
// asked once a reading.
//
// It runs with the controller's lock released, as a create does, and marks
// the sandbox so that no start, stop or deadline rule crosses it. A delete
// is taken at any time, and what the move made is removed when it ends.
func (c *Controller) move(ctx context.Context, s sizing, state driver.State, target int64, reason string) error {
	id := s.obj.Status.ID
	if target <= bytesOf(state.Resources.Memory) {
		return nil
	}
	c.mu.Lock()
	if cur, held := c.objects[id]; !held || cur.Status.Phase == PhaseDeleting || c.creating(cur) {
		c.mu.Unlock()
		return nil
	}
	c.moving[id] = struct{}{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.moving, id)
		c.mu.Unlock()
	}()
	if reason != "" {
		c.sizeMu.Lock()
		c.size.reasons[id] = reason
		c.sizeMu.Unlock()
	}
	previous := state.Resources
	previous.Disk = ""
	next := previous
	next.Memory = autosize.Quantity(target)
	d := s.driver
	// The record takes the size first, so the start below renders it. The
	// driver's own attempt to give it in place is not what a move is for,
	// and an error from that attempt is the reason the sandbox is being
	// moved; an error that left the record without the size ends the move
	// before anything is restarted for nothing.
	if err := d.Update(ctx, id, driver.Change{Resources: &next}); err != nil {
		recorded, readErr := d.Inspect(ctx, id)
		if errors.Is(readErr, driver.ErrNotFound) {
			return nil
		}
		if readErr != nil || !asksFor(recorded, target) {
			return errors.Join(fmt.Errorf("recording %s of memory for %s: %w", next.Memory, id, err), readErr)
		}
	}
	moved := func() error {
		if state.Phase == driver.Running {
			if err := d.Stop(ctx, id); err != nil {
				return err
			}
		}
		return d.Start(ctx, id)
	}()
	if moved != nil {
		c.sizeMu.Lock()
		c.size.hold[id] = c.clock.Now().Add(autosize.MoveBackoff)
		delete(c.size.reasons, id)
		c.sizeMu.Unlock()
		undone := d.Update(ctx, id, driver.Change{Resources: &previous})
		if undone == nil {
			undone = d.Start(ctx, id)
		}
		return errors.Join(fmt.Errorf("moving %s to %s of memory: %w", id, next.Memory, moved),
			undone, c.afterMove(ctx, id, d, false))
	}
	return c.afterMove(ctx, id, d, true)
}

// asksFor reports whether a sandbox holds, or was asked to hold, this much
// memory.
func asksFor(state driver.State, memory int64) bool {
	if bytesOf(state.Resources.Memory) == memory {
		return true
	}
	return state.Resize != nil && bytesOf(state.Resize.Resources.Memory) == memory
}

// afterMove writes what a move left: the phase and the instants the driver
// reports, and where the move held, the new size with its record. A sandbox
// deleted while it moved has what the move made removed instead.
func (c *Controller) afterMove(ctx context.Context, id string, d driver.Driver, moved bool) error {
	state, err := d.Inspect(ctx, id)
	if errors.Is(err, driver.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	c.mu.Lock()
	cur, held := c.objects[id]
	gone := !held || cur.Status.Phase == PhaseDeleting
	c.mu.Unlock()
	if gone {
		// The delete ran beside the move and may have found a Pod the
		// move then replaced. The retried delete, or this one, removes it.
		if err := d.Delete(ctx, id); err != nil && !errors.Is(err, driver.ErrNotFound) {
			return err
		}
		return nil
	}
	return c.recordSize(ctx, id, state, moved)
}

// recordSize brings status.resources to what the driver reports the sandbox
// holds, and writes sandbox.resized where its memory changed. It is the one
// writer of that status, so a size is never reported without its record. A
// reading that finds the size unchanged writes nothing.
func (c *Controller) recordSize(ctx context.Context, id string, state driver.State, moved bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, held := c.objects[id]
	if !held || !sized(obj) || obj.Status.Phase == PhaseDeleting {
		return nil
	}
	current := obj.Status.Resources
	if current == (v1.Size{}) {
		current = sizeAsked(obj)
	}
	want := sizeHeld(current, state.Resources)
	if want == obj.Status.Resources && !moved {
		return nil
	}
	obj = clone(obj)
	from := obj.Status.Resources.Memory
	obj.Status.Resources = want
	c.sizeMu.Lock()
	if reading, read := c.size.usage[id]; read {
		obj.Status.Usage = &reading
	}
	reason := c.size.reasons[id]
	c.sizeMu.Unlock()
	if moved {
		obj.Status.Moves++
		obj = observe(obj, state)
	}
	if from == "" || bytesOf(string(from)) == bytesOf(string(want.Memory)) {
		return c.persist(ctx, obj, MutationStatus)
	}
	if reason == "" {
		// A size nobody here asked for is one the manifest set, or one a
		// raise reached after the process that asked for it was replaced.
		reason = autosize.ReasonUsage
		if bytesOf(string(want.Memory)) == bytesOf(string(obj.Spec.Resources.Memory)) {
			reason = autosize.ReasonUpdate
		}
	}
	c.sizeMu.Lock()
	delete(c.size.reasons, id)
	c.sizeMu.Unlock()
	// A sandbox with room again is no longer at its ceiling.
	if bytesOf(string(want.Memory)) < bytesOf(string(obj.Spec.Resources.Max.Memory)) && conditionStatus(obj, v1.ConditionMemoryAtCeiling) == v1.ConditionTrue {
		obj.Status.Conditions = setCondition(obj.Status.Conditions, belowCeiling(c.clock.Now()))
	}
	return c.persistWith(ctx, obj, MutationResized, ResizedData{
		Resource: autosize.ResourceMemory, From: string(from), To: string(want.Memory), Reason: reason, Moved: moved,
	})
}

// belowCeiling is the MemoryAtCeiling condition of a sandbox with room.
func belowCeiling(now time.Time) v1.Condition {
	return v1.Condition{
		Type: v1.ConditionMemoryAtCeiling, Status: v1.ConditionFalse, Reason: v1.ReasonBelowCeiling, Since: now,
		Message: "The sandbox's memory is below its ceiling.",
	}
}

// ceiling writes the MemoryAtCeiling condition where a reading crossed the
// warning line, either way, and emits sandbox.at_ceiling on the way up. The
// condition is stored, so the event is written once for one crossing however
// often the sandbox is read and whichever process reads it. killed says the
// crossing is a process the kernel killed rather than a working set near the
// line, and used is the working set of this reading, negative where it took
// none.
func (c *Controller) ceiling(ctx context.Context, s sizing, at, killed bool, used int64) error {
	id := s.obj.Status.ID
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, held := c.objects[id]
	if !held || obj.Status.Phase == PhaseDeleting {
		return nil
	}
	was := conditionStatus(obj, v1.ConditionMemoryAtCeiling)
	now := c.clock.Now()
	switch {
	case at && was != v1.ConditionTrue:
		obj = clone(obj)
		reason, message := v1.ReasonUsage, "The sandbox's workload is using nearly all the memory its ceiling allows."
		if killed {
			reason, message = v1.ReasonOOM, "A process of the sandbox was killed for memory with its memory at the ceiling."
		}
		obj.Status.Conditions = setCondition(obj.Status.Conditions, v1.Condition{
			Type: v1.ConditionMemoryAtCeiling, Status: v1.ConditionTrue, Reason: reason, Since: now, Message: message,
		})
		c.sizeMu.Lock()
		reading, read := c.size.usage[id]
		c.sizeMu.Unlock()
		if read {
			obj.Status.Usage = &reading
		}
		// A kill found without a reading of the working set is reported
		// with the last one taken, and with the ceiling itself where none
		// was: the workload reached it.
		switch {
		case used >= 0:
		case read:
			used = reading.Memory
		default:
			used = s.max
		}
		return c.persistWith(ctx, obj, MutationAtCeiling, AtCeilingData{
			Resource: autosize.ResourceMemory, Used: used, Max: string(obj.Spec.Resources.Max.Memory),
		})
	case !at && was == v1.ConditionTrue:
		obj = clone(obj)
		obj.Status.Conditions = setCondition(obj.Status.Conditions, belowCeiling(now))
		return c.persist(ctx, obj, MutationStatus)
	}
	return nil
}

// trackedExec is a command a client runs, counted as a session open in its
// sandbox until it ends, and read for a kill for memory when it ends badly.
type trackedExec struct {
	driver.Exec
	c      *Controller
	id     string
	closed func()
}

// Wait reports the command's end. A command that ended with a non-zero exit
// code, or whose stream failed, may have lost a process to the memory limit,
// so the sandbox is read before the exit is reported: at once for a command
// a signal ended or a stream that broke, which is what a kill looks like,
// and by the loop for any other failure, where the raise follows and the
// answer does not wait for it.
func (t *trackedExec) Wait(ctx context.Context) (int, error) {
	code, err := t.Exec.Wait(ctx)
	t.closed()
	if ctx.Err() != nil || (err == nil && code == 0) || !t.c.watched(t.id) {
		return code, err
	}
	if err != nil || code == killedExit {
		if readErr := t.c.sizeOne(ctx, t.id, nil, readCommand); readErr != nil {
			t.c.log.WarnContext(ctx, "a sandbox could not be read after a command was killed", "sandbox", t.id, "err", readErr)
		}
		return code, err
	}
	t.c.wakeSizing(t.id)
	return code, err
}

func (t *trackedExec) Close() error {
	t.closed()
	return t.Exec.Close()
}

// trackedSession is a terminal, counted the same way until it is closed or
// its command ends.
type trackedSession struct {
	driver.Session
	closed func()
}

func (t *trackedSession) Wait(ctx context.Context) (int, error) {
	code, err := t.Session.Wait(ctx)
	if ctx.Err() == nil {
		t.closed()
	}
	return code, err
}

func (t *trackedSession) Close() error {
	t.closed()
	return t.Session.Close()
}

// session counts one session open in a sandbox and returns its close, which
// uncounts it the first time it is called, whichever of the session's ends
// that is.
func (c *Controller) session(id string) func() {
	c.sessionOpened(id)
	var once sync.Once
	return func() { once.Do(func() { c.sessionClosed(id) }) }
}
