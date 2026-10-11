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

// What asked for one reading of one sandbox. The interval reads everything,
// the working set included. A wake, from a command that failed, a sandbox
// that started or an update that changed a size, reads the sandbox's state
// and its kill count, which is what those can have changed. A command a
// signal ended is read for the kill count first and for anything else only
// where the count rose or could not be read, so a client whose command was
// killed for another reason waits for one command and no more.
const (
	readInterval = iota
	readWake
	readKilled
)

// leaseFresh is how long the loop takes the autosize lease as held after it
// last acquired it, so a wake between two ticks does not ask the store. It
// is a third of the lease's term, which is when a holder renews.
const leaseFresh = LeaseTTL / 3

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
	// moving is every sandbox the control plane is starting again, at a
	// larger size or at the one it holds, with the reason a read of it
	// carries, from before its workload is stopped until what the restart
	// left is written. A start, a stop and the deadline rules leave such a
	// sandbox alone, and a read of it answers that it is starting. waking
	// is every sandbox the
	// loop has been asked to read and has not read yet, so many readers
	// that find one kill ask for one reading.
	moving map[string]string
	waking map[string]struct{}
	// restarts counts the starts in a row of a sandbox at the size it
	// holds, made because it was killed for memory where no larger size
	// could be placed, and restarted is when the last of them was made.
	// Both begin again with every move that finds no room, with a raise
	// that is given, and with a client's own start.
	restarts  map[string]int
	restarted map[string]time.Time
	// unread counts the readings of the kill count in a row that failed,
	// so one that fails while a container is ending is not reported and
	// one that keeps failing is.
	unread map[string]int
	// closed says the controller is closing, after which no reading leaves
	// work running behind it.
	closed bool
}

func newSizer() sizer {
	return sizer{
		sessions: map[string]int{}, watched: map[string]struct{}{},
		readings: map[string]*autosize.Readings{}, usage: map[string]v1.Usage{},
		reasons: map[string]string{}, hold: map[string]time.Time{}, busy: map[string]struct{}{},
		unread: map[string]int{}, moving: map[string]string{}, waking: map[string]struct{}{},
		restarts: map[string]int{}, restarted: map[string]time.Time{},
	}
}

// sized reports whether a manifest names any ceiling, which is what gives a
// sandbox a status.resources at all.
func sized(obj v1.Sandbox) bool { return obj.Spec.Resources.Max != (v1.ResourceMax{}) }

// bytesOf is a quantity as a number of bytes, and zero for one that is empty
// or cannot be read.
func bytesOf(q string) int64 { return milliOf(q) / 1000 }

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
	delete(c.size.unread, id)
	delete(c.size.restarts, id)
	delete(c.size.restarted, id)
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
// ends, and in between reads the ones a failed command, a start or a read
// asked about. Each tick runs only while the autosize lease is held. A
// control plane over a driver that sizes memory has to run it: a read
// answers that a sandbox killed for memory under its ceiling is starting on
// the strength of this loop starting it.
func (c *Controller) RunAutosize(ctx context.Context) {
	ticks, stop := c.clock.Ticker(c.autosizeInterval)
	defer stop()
	// The first pass runs at once, as the reaper's does: a process that
	// starts with sandboxes running takes the baseline of each kill count
	// before the first interval ends.
	// acquired is when this loop last took the lease, which a wake inside
	// leaseFresh of it does not take again.
	var acquired time.Time
	held := func() bool {
		if !acquired.IsZero() && time.Since(acquired) < leaseFresh {
			return true
		}
		if !c.autosizeLeaseHeld(ctx) {
			acquired = time.Time{}
			return false
		}
		acquired = time.Now()
		return true
	}
	tick := func() {
		acquired = time.Time{}
		if !held() {
			return
		}
		if read, err := c.Autosize(ctx); err != nil {
			c.log.WarnContext(ctx, "autosize pass incomplete", "read", read, "err", err)
		}
	}
	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			tick()
		case id := <-c.sizeWake:
			c.woken(id)
			if !held() {
				continue
			}
			if err := c.sizeOne(ctx, id, nil, readWake); err != nil {
				c.log.WarnContext(ctx, "a sandbox could not be read for its size", "sandbox", id, "err", err)
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
// tick. It never blocks, and a sandbox already asked about is not asked
// about twice: a kill many readers find is one reading.
func (c *Controller) wakeSizing(id string) {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	if _, asked := c.size.waking[id]; asked {
		return
	}
	select {
	case c.sizeWake <- id:
		c.size.waking[id] = struct{}{}
	default:
	}
}

// woken takes one sandbox off the list of those asked about, which the loop
// does as it begins the reading.
func (c *Controller) woken(id string) {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	delete(c.size.waking, id)
}

// isMoving reports whether the control plane is starting a sandbox again.
func (c *Controller) isMoving(id string) bool {
	_, moving := c.movingAs(id)
	return moving
}

// movingAs is the reason a read of a sandbox being started again carries.
func (c *Controller) movingAs(id string) (reason string, moving bool) {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	reason, moving = c.size.moving[id]
	return reason, moving
}

// setMoving marks a sandbox as being started again with the reason its reads
// carry, and returns what takes the mark off.
func (c *Controller) setMoving(id, reason string) (done func()) {
	c.sizeMu.Lock()
	c.size.moving[id] = reason
	c.sizeMu.Unlock()
	return func() {
		c.sizeMu.Lock()
		delete(c.size.moving, id)
		c.sizeMu.Unlock()
	}
}

// raises reports whether a sandbox's workload was killed for memory with room
// left to grow: the driver reports the kill, the manifest names a memory
// ceiling above what the sandbox holds, and its driver sizes memory. Such a
// sandbox is the control plane's to start again, and fate says how.
func (c *Controller) raises(obj v1.Sandbox, state driver.State, d driver.Driver) bool {
	if state.Phase != PhaseFailed || state.Reason != reasonOOMKilled || state.Resources.IsZero() {
		return false
	}
	ceiling := bytesOf(string(obj.Spec.Resources.Max.Memory))
	return ceiling != 0 && bytesOf(state.Resources.Memory) < ceiling && d.Capabilities().Autosize.Memory
}

// What the control plane does with a sandbox its driver reports.
const (
	// fateNone is nothing: the sandbox is what its driver says it is.
	fateNone = iota
	// fateRaise is a start at the next size, by a move.
	fateRaise
	// fateRestart is a start at the size it holds, because a move just
	// found no room for a larger one.
	fateRestart
	// fateGiveUp is leaving it Failed: it was started at the size it holds
	// as often as SameSizeRestarts allows and killed again.
	fateGiveUp
)

// fate is what the control plane is going to do with a sandbox whose driver
// reports state. It is the one rule, read by the loop that acts and by every
// read that presents the sandbox, so a sandbox reads as starting exactly when
// the loop is going to start it, and as Failed exactly when nothing will.
// A sandbox killed for memory with room under its ceiling is started again at
// the next size. Inside MoveBackoff, after a move that found no room, it is
// started again at the size it holds instead, so a kill there does not leave
// it down until the backoff ends; that is done SameSizeRestarts times in a
// row and no more, so a workload killed at every start does not run the
// control plane in a circle. A sandbox given up on stays so until a client
// acts, which the stored condition holds across the backoff's end and a
// restart of the control plane.
func (c *Controller) fate(obj v1.Sandbox, state driver.State, d driver.Driver) int {
	if !c.raises(obj, state, d) || exhausted(obj) {
		return fateNone
	}
	id := obj.Status.ID
	if !c.onHold(id) {
		return fateRaise
	}
	c.sizeMu.Lock()
	restarts := c.size.restarts[id]
	c.sizeMu.Unlock()
	if restarts >= autosize.SameSizeRestarts {
		return fateGiveUp
	}
	return fateRestart
}

// exhausted reports whether the record says the control plane gave up
// starting the sandbox at the size it holds.
func exhausted(obj v1.Sandbox) bool {
	for _, cond := range obj.Status.Conditions {
		if cond.Type == v1.ConditionMemoryUnplaced {
			return cond.Status == v1.ConditionTrue && cond.Reason == v1.ReasonRestartsExhausted
		}
	}
	return false
}

// reopen takes back, on a record about to be written, that the control plane
// gave up starting the sandbox at the size it holds: a client has acted, and
// the sandbox is the control plane's to start again. The condition goes on
// saying that no larger size could be placed, which is still so.
func (c *Controller) reopen(obj *v1.Sandbox) {
	if !exhausted(*obj) {
		return
	}
	held, max := bytesOf(string(obj.Status.Resources.Memory)), bytesOf(string(obj.Spec.Resources.Max.Memory))
	if held == 0 {
		held = bytesOf(string(obj.Spec.Resources.Memory))
	}
	obj.Status.Conditions = setCondition(slices.Clone(obj.Status.Conditions), unplaced(held, max, v1.ReasonNoRoom, c.clock.Now()))
}

// spaced reports whether SameSizeRestartBackoff has passed since a sandbox
// was last started again at the size it holds.
func (c *Controller) spaced(id string) bool {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	last, started := c.size.restarted[id]
	return !started || !c.clock.Now().Before(last.Add(autosize.SameSizeRestartBackoff))
}

// recount ends the count of starts at the size a sandbox holds: its memory
// was raised, or a client started it, and what follows is counted anew.
func (c *Controller) recount(id string) {
	c.sizeMu.Lock()
	defer c.sizeMu.Unlock()
	delete(c.size.restarts, id)
	delete(c.size.restarted, id)
}

// rising reports the reason a read of a sandbox answers that it is starting
// where its driver says otherwise, empty where it answers what the driver
// says, and whether the loop could act now on what the read found. A sandbox
// being started again reads so until it runs. One whose workload was killed
// for memory reads so from the kill, because the loop is going to start it
// again: at a larger size, or at the size it holds where no larger one could
// be placed. One whose start was cut short reads so while the workload that
// was stopped is ending and once it is gone, because the loop is about to
// finish the start. A sandbox killed at its ceiling, with no ceiling, or
// given up on is none of these: it is Failed until a client acts.
func (c *Controller) rising(obj v1.Sandbox, state driver.State, d driver.Driver) (reason string, wake bool) {
	id := obj.Status.ID
	if reason, moving := c.movingAs(id); moving {
		if state.Phase == driver.Running {
			return "", false
		}
		return reason, false
	}
	switch c.fate(obj, state, d) {
	case fateRaise:
		return v1.ReasonMemoryRaised, true
	case fateRestart:
		// The loop makes this start once SameSizeRestartBackoff has
		// passed since the last one, and is asked for it then.
		return v1.ReasonMemoryNotRaised, c.spaced(id)
	case fateGiveUp:
		// Failed, and the loop writes why.
		return "", true
	}
	reason = marked(obj)
	if reason == "" || c.onHold(id) {
		return "", false
	}
	switch state.Phase {
	case driver.Stopped:
		return reason, true
	case PhaseStopping:
		return reason, false
	}
	return "", false
}

// marked is the reason of a record that says the control plane is starting
// the sandbox again, which only such a start writes and every end of one
// replaces, and empty for any other record.
func marked(obj v1.Sandbox) string {
	if obj.Status.Phase != PhaseStarting {
		return ""
	}
	switch obj.Status.Reason {
	case v1.ReasonMemoryRaised, v1.ReasonMemoryNotRaised:
		return obj.Status.Reason
	}
	return ""
}

// interrupted reports a start by the control plane that was cut short: the
// record says the sandbox is being started again and the driver holds no
// workload for it. The control plane that began it stopped, or handed its
// lease to another replica, between the stop and the start. The loop
// finishes the start, so until then the sandbox reads as it did before.
func interrupted(obj v1.Sandbox, state driver.State) bool {
	return marked(obj) != "" && state.Phase == driver.Stopped
}

// present is one driver read as a reader of the sandbox is given it: observe,
// with a sandbox the control plane is starting again answered as Starting,
// with the reason that says at which size, whatever its driver reports in
// between: a workload that ended, no workload at all, or one coming up. The
// answer is computed from the read itself and not from what the loop has
// noticed, so there is no instant at which such a sandbox reads as Failed or
// Stopped: not before the loop's next reading, not while the old workload is
// removed, not before the new one is ready. A read that finds the kill
// before the loop has asks the loop for its reading now.
func (c *Controller) present(obj v1.Sandbox, state driver.State, d driver.Driver) v1.Sandbox {
	out := observe(obj, state)
	reason, wake := c.rising(obj, state, d)
	if wake {
		c.wakeSizing(obj.Status.ID)
	}
	if reason == "" {
		return out
	}
	out.Status.Phase, out.Status.Reason, out.Status.ExitCode = PhaseStarting, reason, nil
	out.Status.StoppedAt = time.Time{}
	return out
}

// presentState is present for a driver's state on its way to a store: the
// phase and the reason a read of the sandbox would answer. A state with no
// desired sandbox behind it is the driver's own and is left as it is.
func (c *Controller) presentState(state driver.State, d driver.Driver) driver.State {
	c.mu.Lock()
	obj, held := c.objects[state.ID]
	c.mu.Unlock()
	if !held {
		return state
	}
	if reason, _ := c.rising(obj, state, d); reason != "" {
		return risen(state, reason)
	}
	return state
}

// risen is a driver's state with the phase and the reason a sandbox on its
// way back reads as.
func risen(state driver.State, reason string) driver.State {
	state.Phase, state.Reason, state.ExitCode, state.StoppedAt = PhaseStarting, reason, nil, time.Time{}
	return state
}

// accounted is a driver's state as the sizing code records it when a start
// it made has ended: the state itself, or the sandbox as starting where the
// workload was killed for memory again before this read and the loop is
// going to start it once more. The stored status is what a read answers
// with when the driver cannot be reached, so it never says that a sandbox
// on its way back failed.
func (c *Controller) accounted(obj v1.Sandbox, state driver.State, d driver.Driver) driver.State {
	switch c.fate(obj, state, d) {
	case fateRaise:
		return risen(state, v1.ReasonMemoryRaised)
	case fateRestart:
		return risen(state, v1.ReasonMemoryNotRaised)
	}
	return state
}

// unplaced is the MemoryUnplaced condition of a sandbox holding held bytes
// under a ceiling of max: true, with the size that found no room, for the
// reason given.
func unplaced(held, max int64, reason string, now time.Time) v1.Condition {
	asked := autosize.Quantity(autosize.NextMemory(held, max))
	message := "No node had room for " + asked + " of memory, so the sandbox keeps the " + autosize.Quantity(held) + " it holds. More is asked for again later."
	if reason == v1.ReasonRestartsExhausted {
		message = "No node had room for " + asked + " of memory, and the sandbox was killed for memory again each time it was started at the " +
			autosize.Quantity(held) + " it holds. Start it to run it again at that size."
	}
	return v1.Condition{Type: v1.ConditionMemoryUnplaced, Status: v1.ConditionTrue, Reason: reason, Since: now, Message: message}
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
	reader, reads := s.driver.(driver.MemoryReader)
	reads = reads && s.memory
	// asked says this reading already asked for the kill count, and rose
	// what the count said. A count that could not be read is not asked for
	// again in the same reading: the answer would be the same, and one
	// reading is one failure toward the warning.
	rose, asked := false, false
	if why == readKilled && reads {
		asked = true
		var counted bool
		if rose, counted = c.countKills(ctx, id, reader); counted && !rose {
			// The command was not killed for memory. Nothing else of the
			// sandbox is read on its account.
			return nil
		}
	}
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
		// The rule every read presents this sandbox by: it reads as
		// starting from the kill on, and here the start is made.
		switch c.fate(s.obj, *state, s.driver) {
		case fateRaise:
			handed = true
			c.follow(id, func(ctx context.Context) error {
				return c.move(ctx, s, *state, autosize.NextMemory(held, s.max), autosize.ReasonOOM)
			})
		case fateRestart:
			// A start made too soon after the last one waits for the
			// first reading past SameSizeRestartBackoff.
			if !c.spaced(id) {
				return nil
			}
			handed = true
			c.follow(id, func(ctx context.Context) error { return c.restart(ctx, s) })
		case fateGiveUp:
			return c.giveUp(ctx, s, held)
		default:
			if s.memory && state.Reason == reasonOOMKilled && held >= s.max {
				// Killed at its ceiling: there is nothing more to give,
				// and the sandbox stays Failed as one with no ceiling
				// does.
				return c.ceiling(ctx, s, true, true, -1)
			}
		}
		return nil
	case driver.Stopped:
		if interrupted(s.obj, *state) && !c.onHold(id) {
			handed = true
			c.follow(id, func(ctx context.Context) error { return c.finishMove(ctx, s) })
			return nil
		}
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
	// A sandbox that runs under a record saying it is being started again
	// is a start whose last write was lost, with the process that made it.
	// The record is brought to what the driver reports here, and a start
	// at a larger size is counted as the move it was.
	switch marked(s.obj) {
	case v1.ReasonMemoryRaised:
		if err := c.recordSize(ctx, id, *state, true); err != nil {
			return err
		}
	case v1.ReasonMemoryNotRaised:
		if err := c.afterRestart(ctx, id, s.driver); err != nil {
			return err
		}
	default:
		if err := c.recordSize(ctx, id, *state, false); err != nil {
			return err
		}
	}
	if !reads {
		return nil
	}
	if !asked {
		rose, _ = c.countKills(ctx, id, reader)
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
		case ctx.Err() == nil && !errors.Is(err, driver.ErrNotRunning) && !errors.Is(err, driver.ErrUnsupported):
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

// killReadsBeforeWarning is how many readings of the kill count in a row
// fail before the failure is logged. A container the kernel just killed
// fails the one reading that was in flight, which is the kill being found
// and not a reading the operator is missing.
const killReadsBeforeWarning = 2

// countKills reads the kernel's count of kills for memory in one sandbox and
// reports whether it rose since the last reading of the same workload
// instance, and whether it could be read at all. A sandbox that is not
// running and a substrate that serves no count are no signal and no
// failure; any other error that repeats is logged once, because it is a
// reading the operator expects and is not getting, as from an image with
// no cat in it.
func (c *Controller) countKills(ctx context.Context, id string, reader driver.MemoryReader) (rose, counted bool) {
	kills, err := reader.MemoryKills(ctx, id)
	if err == nil {
		c.sizeMu.Lock()
		delete(c.size.unread, id)
		c.sizeMu.Unlock()
		return c.readingsOf(id, func(r *autosize.Readings) bool { return r.Kills(kills.Count, kills.Instance) }), true
	}
	if ctx.Err() != nil || errors.Is(err, driver.ErrNotRunning) || errors.Is(err, driver.ErrUnsupported) {
		return false, false
	}
	c.sizeMu.Lock()
	c.size.unread[id]++
	failed := c.size.unread[id]
	c.sizeMu.Unlock()
	if failed == killReadsBeforeWarning {
		c.log.WarnContext(ctx, "the count of kills for memory cannot be read, so a process killed for memory in this sandbox raises nothing", "sandbox", id, "err", err)
	}
	return false, false
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
		c.countKills(ctx, id, reader)
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
	cur, held := c.objects[id]
	if !held || cur.Status.Phase == PhaseDeleting || c.creating(cur) {
		c.mu.Unlock()
		return nil
	}
	// From here until what the move left is written, every read of the
	// sandbox answers that it is starting. The mark comes before the write
	// and both before the workload is touched, so no reader meets the
	// stopped or absent workload of a move as a sandbox that stopped.
	defer c.setMoving(id, v1.ReasonMemoryRaised)()
	cur = clone(cur)
	cur.Status.Phase, cur.Status.Reason, cur.Status.ExitCode = PhaseStarting, v1.ReasonMemoryRaised, nil
	if err := c.persist(ctx, cur, MutationStatus); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("recording that %s is being started at a larger size: %w", id, err)
	}
	c.mu.Unlock()
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
		if readErr != nil || !asksFor(recorded, next) {
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
		// No room: the sandbox's memory is left alone for the backoff,
		// and a kill inside it is answered by a start at the size the
		// sandbox holds, counted from here.
		c.sizeMu.Lock()
		c.size.hold[id] = c.clock.Now().Add(autosize.MoveBackoff)
		delete(c.size.reasons, id)
		delete(c.size.restarts, id)
		delete(c.size.restarted, id)
		c.sizeMu.Unlock()
		undone := d.Update(ctx, id, driver.Change{Resources: &previous})
		if undone == nil {
			undone = d.Start(ctx, id)
		}
		return errors.Join(fmt.Errorf("moving %s to %s of memory: %w", id, next.Memory, moved),
			undone, c.afterMove(ctx, id, d, false, bytesOf(previous.Memory)))
	}
	return c.afterMove(ctx, id, d, true, 0)
}

// restart starts a sandbox again at the size it holds: its workload was
// killed for memory inside the backoff of a move that found no room, so no
// larger size is asked for now and the sandbox is not left down until one
// is. It is the start a stop and a start make, counted so that it is made
// SameSizeRestarts times in a row and no more. Nothing of the size changes,
// so it is no move and writes no record of one. A start the driver cannot
// make is counted like one whose workload is killed, and leaves the sandbox
// as its driver then reports it: killed still, which the next reading
// answers, or stopped where the driver had removed the ended workload,
// which a client's start brings back.
func (c *Controller) restart(ctx context.Context, s sizing) error {
	id := s.obj.Status.ID
	c.mu.Lock()
	cur, held := c.objects[id]
	if !held || cur.Status.Phase == PhaseDeleting || c.creating(cur) {
		c.mu.Unlock()
		return nil
	}
	// As in a move, the mark and the record come before the workload is
	// touched, so no read meets the ended or absent workload as a sandbox
	// that failed or stopped.
	defer c.setMoving(id, v1.ReasonMemoryNotRaised)()
	cur = clone(cur)
	cur.Status.Phase, cur.Status.Reason, cur.Status.ExitCode = PhaseStarting, v1.ReasonMemoryNotRaised, nil
	if err := c.persist(ctx, cur, MutationStatus); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("recording that %s is being started at the size it holds: %w", id, err)
	}
	c.mu.Unlock()
	c.sizeMu.Lock()
	c.size.restarts[id]++
	c.size.restarted[id] = c.clock.Now()
	c.sizeMu.Unlock()
	if err := s.driver.Start(ctx, id); err != nil {
		return errors.Join(fmt.Errorf("starting %s again at the size it holds: %w", id, err), c.afterRestart(ctx, id, s.driver))
	}
	return c.afterRestart(ctx, id, s.driver)
}

// afterRestart writes what a start at the size a sandbox holds left: what
// its driver reports of it, and no size, since none changed. A sandbox
// deleted meanwhile has what the start made removed instead.
func (c *Controller) afterRestart(ctx context.Context, id string, d driver.Driver) error {
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
		if err := d.Delete(ctx, id); err != nil && !errors.Is(err, driver.ErrNotFound) {
			return err
		}
		return nil
	}
	state = c.accounted(cur, state, d)
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, held = c.objects[id]
	if !held || cur.Status.Phase == PhaseDeleting {
		return nil
	}
	return c.persist(ctx, observe(clone(cur), state), MutationStatus)
}

// giveUp leaves a sandbox Failed that was started at the size it holds as
// often as SameSizeRestarts allows and was killed for memory again. The
// condition says so and is what keeps it Failed past the end of the backoff
// and a restart of the control plane, until a client starts it, changes its
// size or deletes it.
func (c *Controller) giveUp(ctx context.Context, s sizing, held int64) error {
	id := s.obj.Status.ID
	c.mu.Lock()
	defer c.mu.Unlock()
	obj, tracked := c.objects[id]
	if !tracked || obj.Status.Phase == PhaseDeleting || exhausted(obj) {
		return nil
	}
	obj = clone(obj)
	obj.Status.Conditions = setCondition(obj.Status.Conditions, unplaced(held, s.max, v1.ReasonRestartsExhausted, c.clock.Now()))
	c.log.WarnContext(ctx, "a sandbox killed for memory at a size no larger one could replace is left failed",
		"sandbox", id, "memory", autosize.Quantity(held), "starts", autosize.SameSizeRestarts)
	return c.persist(ctx, obj, MutationStatus)
}

// finishMove finishes a start by the control plane that was cut short between
// its stop and its start: the sandbox is started at the size its driver
// recorded, and what that left is written as the start it finishes would
// have written it, a move's with its count and record, or a start at the
// size the sandbox held. A start that fails leaves it stopped, with the
// record saying so, and its memory alone for MoveBackoff.
func (c *Controller) finishMove(ctx context.Context, s sizing) error {
	id := s.obj.Status.ID
	c.mu.Lock()
	cur, held := c.objects[id]
	reason := marked(cur)
	if !held || reason == "" || c.creating(cur) {
		c.mu.Unlock()
		return nil
	}
	done := c.setMoving(id, reason)
	c.mu.Unlock()
	defer done()
	moved := reason == v1.ReasonMemoryRaised
	if err := s.driver.Start(ctx, id); err != nil {
		c.sizeMu.Lock()
		c.size.hold[id] = c.clock.Now().Add(autosize.MoveBackoff)
		c.sizeMu.Unlock()
		return errors.Join(fmt.Errorf("finishing the start of %s: %w", id, err), c.afterMove(ctx, id, s.driver, false, 0))
	}
	if !moved {
		return c.afterRestart(ctx, id, s.driver)
	}
	return c.afterMove(ctx, id, s.driver, true, 0)
}

// asksFor reports whether a sandbox holds, or was asked to hold, a size: the
// memory and the CPU limit of it, each compared by value.
func asksFor(state driver.State, want driver.Resources) bool {
	same := func(r driver.Resources) bool {
		return bytesOf(r.Memory) == bytesOf(want.Memory) && milliOf(cpuLimit(r)) == milliOf(cpuLimit(want))
	}
	return same(state.Resources) || (state.Resize != nil && same(state.Resize.Resources))
}

// cpuLimit is the most CPU a size allows: its limit, or the CPU it grants
// where it names none.
func cpuLimit(r driver.Resources) string {
	if r.CPULimit != "" {
		return r.CPULimit
	}
	return r.CPU
}

// milliOf is a quantity in thousandths, and zero for one that is empty or
// cannot be read.
func milliOf(q string) int64 {
	if q == "" {
		return 0
	}
	milli, err := manifest.ParseQuantity(v1.Quantity(q))
	if err != nil {
		return 0
	}
	return milli
}

// afterMove writes what a move left: the phase and the instants the driver
// reports, and where the move held, the new size with its record. A move
// that was undone for want of room leaves the MemoryUnplaced condition, with
// the size that could not be placed over the held bytes the sandbox keeps;
// held is zero where no size was asked for. A sandbox deleted while it moved
// has what the move made removed instead.
func (c *Controller) afterMove(ctx context.Context, id string, d driver.Driver, moved bool, held int64) error {
	state, err := d.Inspect(ctx, id)
	if errors.Is(err, driver.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	c.mu.Lock()
	cur, tracked := c.objects[id]
	gone := !tracked || cur.Status.Phase == PhaseDeleting
	c.mu.Unlock()
	if gone {
		// The delete ran beside the move and may have found a Pod the
		// move then replaced. The retried delete, or this one, removes it.
		if err := d.Delete(ctx, id); err != nil && !errors.Is(err, driver.ErrNotFound) {
			return err
		}
		return nil
	}
	state = c.accounted(cur, state, d)
	if moved {
		return c.recordSize(ctx, id, state, true)
	}
	// The move was undone. The record said the sandbox was starting at a
	// larger size, and now says what its driver reports of it: running or
	// stopped at the size it had, which is the driver's own account and
	// the end of the control plane's part in it.
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, tracked = c.objects[id]
	if !tracked || cur.Status.Phase == PhaseDeleting {
		return nil
	}
	cur = observe(clone(cur), state)
	if max := bytesOf(string(cur.Spec.Resources.Max.Memory)); held > 0 && held < max {
		cur.Status.Conditions = setCondition(cur.Status.Conditions, unplaced(held, max, v1.ReasonNoRoom, c.clock.Now()))
	}
	return c.persist(ctx, cur, MutationStatus)
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
	// A larger size that was given is placed: what was counted against the
	// old one is over, and the condition says so.
	if bytesOf(string(want.Memory)) > bytesOf(string(from)) {
		c.recount(id)
		if conditionStatus(obj, v1.ConditionMemoryUnplaced) == v1.ConditionTrue {
			obj.Status.Conditions = setCondition(obj.Status.Conditions, v1.Condition{
				Type: v1.ConditionMemoryUnplaced, Status: v1.ConditionFalse, Reason: v1.ReasonMemoryRaised, Since: c.clock.Now(),
				Message: "The sandbox was given the memory it needed.",
			})
		}
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
		if readErr := t.c.sizeOne(ctx, t.id, nil, readKilled); readErr != nil {
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
