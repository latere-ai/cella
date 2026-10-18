// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker

import "time"

// ShortenWriteDeadline sets the deadline every socket's frame write runs
// under, and returns what puts the default back.
func ShortenWriteDeadline(d time.Duration) (restore func()) {
	writeDeadlineOverride.Store(int64(d))
	return func() { writeDeadlineOverride.Store(0) }
}

// SetAfter replaces the reconnect's timer, so a test reads the waits the loop
// chose without sleeping through them.
func (w *Worker) SetAfter(after func(time.Duration) <-chan time.Time) { w.after = after }

// MinBackoff and MaxBackoff are the shortest and the longest wait between
// two dials.
const (
	MinBackoff = minBackoff
	MaxBackoff = maxBackoff
)
