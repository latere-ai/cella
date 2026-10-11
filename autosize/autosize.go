// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package autosize is the policy of sizing a sandbox inside a ceiling: when
// its memory is raised, by how much, and when a workload at its ceiling is
// said to be there. It computes over numbers and holds no connection and no
// lock, so the controller that applies the policy, a test that proves it and
// a platform that explains a size to a person all read one rule.
//
// Every figure the policy is made of is a constant here and is declared
// nowhere else.
package autosize

import (
	"math"
	"strconv"
	"time"
)

// The figures of the policy.
const (
	// DefaultInterval is how often a running sandbox's usage is read where
	// the operator names no interval.
	DefaultInterval = time.Minute
	// DiskGrowAt is the fraction of the workspace's claim in use that
	// expands it, and DiskStepMin the smallest expansion in bytes.
	DiskGrowAt        = 0.80
	DiskStepMin int64 = 5 << 30
	// MemoryGrowAt is the fraction of the memory limit that the working
	// set reaches before memory is raised, and MemoryGrowReadings how many
	// readings in a row must find it there.
	MemoryGrowAt       = 0.85
	MemoryGrowReadings = 2
	// MemoryStepFactor is the growth factor of one memory step and
	// MemoryStepMin its smallest size in bytes: a step is the greater of
	// the two.
	MemoryStepFactor       = 1.5
	MemoryStepMin    int64 = 512 << 20
	// MemoryQuantum is what a memory size is a multiple of, in bytes,
	// unless the ceiling cuts it short.
	MemoryQuantum int64 = 256 << 20
	// CeilingWarnAt is the fraction of the ceiling in use, with the size
	// already at the ceiling, at which a sandbox is said to be at it.
	CeilingWarnAt = 0.95
	// MoveBackoff is how long a sandbox's memory is left alone after a
	// move that found no room for it.
	MoveBackoff = 10 * time.Minute
)

// NextMemory is the memory limit a sandbox holding limit bytes is raised to
// under a ceiling: the greater of the limit times MemoryStepFactor and the
// limit plus MemoryStepMin, rounded up to MemoryQuantum and then held to the
// ceiling. A sandbox already at or above its ceiling stays where it is, so a
// ceiling equal to the size is a size that does not change.
func NextMemory(limit, ceiling int64) int64 {
	if limit <= 0 || ceiling <= limit {
		return limit
	}
	scaled := math.Ceil(float64(limit) * MemoryStepFactor)
	if scaled >= float64(ceiling) {
		return ceiling
	}
	next := max(int64(scaled), limit+MemoryStepMin)
	if rem := next % MemoryQuantum; rem != 0 {
		next += MemoryQuantum - rem
	}
	// A sum past the largest integer wraps below the limit it started
	// from, and the ceiling is the answer there as well.
	if next <= limit || next > ceiling {
		return ceiling
	}
	return next
}

// MemoryPressed reports whether a working set is at or above MemoryGrowAt of
// the limit it runs under.
func MemoryPressed(workingSet, limit int64) bool {
	return limit > 0 && float64(workingSet) >= MemoryGrowAt*float64(limit)
}

// AtCeiling reports whether usage is at or above CeilingWarnAt of a ceiling.
func AtCeiling(used, ceiling int64) bool {
	return ceiling > 0 && float64(used) >= CeilingWarnAt*float64(ceiling)
}

// Quantity writes a number of bytes in quantity syntax with the largest
// binary suffix that divides it, so a size the policy computed reads as a
// person would have written it: 805306368 is 768Mi.
func Quantity(bytes int64) string {
	for _, unit := range []struct {
		size   int64
		suffix string
	}{{1 << 30, "Gi"}, {1 << 20, "Mi"}, {1 << 10, "Ki"}} {
		if bytes >= unit.size && bytes%unit.size == 0 {
			return strconv.FormatInt(bytes/unit.size, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(bytes, 10)
}

// Readings is what the policy remembers of one sandbox between readings: how
// many in a row found its working set at the grow line, and the kill count
// last read of which workload instance. The zero value is a sandbox never
// read. It is not safe for concurrent use; its owner serializes it.
type Readings struct {
	pressed  int
	sampled  time.Time
	counted  bool
	instance string
	kills    int64
}

// Usage records one sample of the working set under a limit and reports
// whether it completes MemoryGrowReadings in a row at the grow line. A sample
// taken at an instant already recorded is the same sample read twice and
// counts once, and a sample below the line starts the count again.
func (r *Readings) Usage(workingSet, limit int64, at time.Time) bool {
	if !at.IsZero() && at.Equal(r.sampled) {
		return false
	}
	r.sampled = at
	if !MemoryPressed(workingSet, limit) {
		r.pressed = 0
		return false
	}
	r.pressed++
	return r.pressed >= MemoryGrowReadings
}

// Kills records the count of processes the kernel killed for memory in one
// workload instance and reports whether it rose since the last reading of
// that instance. The first count of an instance is its baseline and raises
// nothing, whatever it is, and a rise is reported once however many kills it
// counted.
func (r *Readings) Kills(count int64, instance string) bool {
	if !r.counted || instance != r.instance {
		r.counted, r.instance, r.kills = true, instance, count
		return false
	}
	rose := count > r.kills
	r.kills = count
	return rose
}

// Raised forgets the readings a raise answered, so the next one is decided
// by readings taken at the new limit.
func (r *Readings) Raised() { r.pressed = 0 }
