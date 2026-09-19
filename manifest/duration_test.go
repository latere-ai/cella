// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

func TestParseDuration(t *testing.T) {
	for _, tc := range []struct {
		in    v1.Duration
		want  time.Duration
		never bool
	}{
		{"15m", 15 * time.Minute, false},
		{"24h", 24 * time.Hour, false},
		{"1h30m", 90 * time.Minute, false},
		{"never", 0, true},
	} {
		got, never, err := ParseDuration(tc.in)
		if err != nil || got != tc.want || never != tc.never {
			t.Errorf("ParseDuration(%q) = %v, %v, %v, want %v, %v", tc.in, got, never, err, tc.want, tc.never)
		}
	}
	// Refused: absent, not Go syntax, zero, and negative. The hosted manifest
	// refused never on every field; this contract takes it on all three.
	for _, in := range []v1.Duration{"", "soon", "later", "0", "0s", "-5m", "Never", "15"} {
		if _, _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) was accepted", in)
		}
	}
}
