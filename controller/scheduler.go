// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import "time"

// DefaultScheduleInterval is how often the scheduler loop passes over the
// queued environments when nothing wakes it sooner (spec 020).
const DefaultScheduleInterval = 5 * time.Second
