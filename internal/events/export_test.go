// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import "time"

// SetFollowPoll shortens the interval a follower of one object reads a shared
// journal at, so a test of that read waits milliseconds and not a second.
func SetFollowPoll(e *Emitter, d time.Duration) { e.poll = d }
