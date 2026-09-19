// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"errors"
	"fmt"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// ParseDuration parses a lifecycle duration: Go syntax, positive, or the word
// never, which reports never true and a zero duration. A driver reads the zero
// duration as no bound, so never needs no sentinel below this package. Zero and
// negative durations are refused: a rule that fires immediately or in the past
// is a mistake, not a policy.
func ParseDuration(d v1.Duration) (time.Duration, bool, error) {
	switch d {
	case "":
		return 0, false, errors.New("duration is empty")
	case v1.DurationNever:
		return 0, true, nil
	}
	value, err := time.ParseDuration(string(d))
	if err != nil {
		return 0, false, fmt.Errorf("duration %q is neither Go syntax nor never", string(d))
	}
	if value <= 0 {
		return 0, false, fmt.Errorf("duration %q must be positive", string(d))
	}
	return value, false, nil
}
