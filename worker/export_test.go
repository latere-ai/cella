// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"time"
)

// ShortenWriteDeadline sets the deadline every socket's frame write runs
// under, and returns what puts the default back.
func ShortenWriteDeadline(d time.Duration) (restore func()) {
	writeDeadlineOverride.Store(int64(d))
	return func() { writeDeadlineOverride.Store(0) }
}

// ShortenHeldAfter sets how long a create waits on the policy before it is
// reported held, and returns what puts the default back.
func ShortenHeldAfter(d time.Duration) (restore func()) {
	previous := heldAfter
	heldAfter = d
	return func() { heldAfter = previous }
}

// Worker is one worker built the way Run builds it, for the tests that read
// what it learned or replace its timer.
type Worker = worker

// New builds the worker Run would run.
func New(o Options) (*Worker, error) { return newWorker(o) }

// Run is what Run does with the worker New built.
func (w *worker) Run(ctx context.Context) error { return w.run(ctx) }

// Environment is the environment this worker serves, empty until it has
// registered once, and ID the worker id the control plane minted.
func (w *worker) Environment() string { return w.environment }
func (w *worker) ID() string          { return w.id }

// SetAfter replaces the reconnect's timer, so a test reads the waits the loop
// chose without sleeping through them.
func (w *worker) SetAfter(after func(time.Duration) <-chan time.Time) { w.after = after }

// MinBackoff and MaxBackoff are the shortest and the longest wait between
// two dials.
const (
	MinBackoff = minBackoff
	MaxBackoff = maxBackoff
)
