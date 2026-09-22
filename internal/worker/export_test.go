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
