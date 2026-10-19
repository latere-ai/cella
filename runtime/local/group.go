// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// attachedStage is a command with stdin as the sandbox's record keeps it while
// it runs: the process group it leads and the process holding its pipes. The
// pipes end with that process, but a command that neither reads nor writes
// them outlives it, so a driver that did not start the command still finds it
// here and ends it.
type attachedStage struct {
	Name string
	// Group is the command's process group, which the sandbox runtime it runs
	// under leads, and Holder the process whose driver started it.
	Group  process
	Holder process
}

// process names one process by its id and its start time, so a process that
// later received the same id is never taken for it.
type process struct {
	PID   int
	Start int64
}

// running reports whether the process is alive and is the one named: its id
// answers a signal and it started when the record says.
func (p process) running(ctx context.Context) bool {
	if p.PID <= 0 || syscall.Kill(p.PID, 0) != nil {
		return false
	}
	started, err := startTime(ctx, p.PID)
	return err == nil && started == p.Start
}

// identify names a running process by its id and its start time.
func identify(ctx context.Context, pid int) (process, error) {
	started, err := startTime(ctx, pid)
	if err != nil {
		return process{}, err
	}
	return process{PID: pid, Start: started}, nil
}

// end kills the command's whole process group while its leader is still the
// command's. A group whose leader has exited cannot be proven the command's,
// since its id may have been handed to another process, and is left alone;
// that leader is the sandbox runtime, which waits on the command.
func (s attachedStage) end(ctx context.Context) error {
	if !s.Group.running(ctx) {
		return nil
	}
	if err := syscall.Kill(-s.Group.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("local: ending the command %s: %w", s.Name, err)
	}
	return nil
}

// psStartLayout is how ps prints a start time under the C locale, on macOS
// and on procps alike: "Sat Sep 12 21:17:31 2026".
const psStartLayout = "Mon Jan _2 15:04:05 2006"

// startTime is a process's start time in Unix seconds, read through ps with
// the locale pinned, which is how the sandbox runtime reads its own stages,
// so one code path serves macOS and Linux. A test replaces it.
var startTime = func(ctx context.Context, pid int) (int64, error) {
	command := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=") //nolint:gosec // a fixed command over an integer pid
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.Output()
	if err != nil {
		return 0, fmt.Errorf("local: reading the start time of process %d: %w", pid, err)
	}
	text := strings.Join(strings.Fields(string(output)), " ")
	started, err := time.ParseInLocation(psStartLayout, text, time.Local)
	if err != nil {
		return 0, fmt.Errorf("local: reading the start time of process %d: ps printed %q: %w", pid, text, err)
	}
	return started.Unix(), nil
}

// holder is this process as an attached command's record names it, read the
// first time and kept once read. It runs under the driver's lock.
func (d *Driver) holder(ctx context.Context) (process, error) {
	if d.self.PID != 0 {
		return d.self, nil
	}
	self, err := identify(ctx, os.Getpid())
	if err != nil {
		return process{}, err
	}
	d.self = self
	return self, nil
}

// endOrphans ends the attached commands a process that was killed outright
// left running in this data directory: every one recorded whose holder no
// longer runs. A command whose holder still runs is that process's, another
// worker's over the same directory or this one's. Only a data directory that
// cannot be read is an error: a sandbox whose record does not read or does
// not write, or whose command could not be ended, keeps its entry, so its
// next stop or delete ends the command or answers why it could not, and one
// sandbox does not keep the driver from starting.
func (d *Driver) endOrphans(ctx context.Context) error {
	entries, err := os.ReadDir(filepath.Join(d.data, "sandboxes"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validID.MatchString(entry.Name()) {
			continue
		}
		r, err := d.load(entry.Name())
		if err != nil || len(r.Attached) == 0 {
			continue
		}
		var kept []attachedStage
		for _, s := range r.Attached {
			if s.Holder.running(ctx) || s.end(ctx) != nil {
				kept = append(kept, s)
			}
		}
		if len(kept) < len(r.Attached) {
			r.Attached = kept
			_ = d.save(r) // a record left as it was is ended again by the sandbox's stop or delete
		}
	}
	return nil
}

// forgetAttached drops one attached command from its sandbox's record once it
// has ended. A sandbox deleted meanwhile has no record to drop it from.
func (d *Driver) forgetAttached(id, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.load(id)
	if errors.Is(err, driver.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	r.Attached = slices.DeleteFunc(r.Attached, func(s attachedStage) bool { return s.Name == name })
	return d.save(r)
}
