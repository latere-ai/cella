// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

func TestParseQuantity(t *testing.T) {
	for _, tc := range []struct {
		in   v1.Quantity
		want int64
	}{
		{"1", 1000},
		{"2", 2000},
		{"0.5", 500},
		{"1.5", 1500},
		{"500m", 500},
		{"1500m", 1500},
		{"1000u", 1},
		{"1e3", 1000000},
		{"1E3", 1000000},
		{"1e-3", 1},
		{"+2", 2000},
		{"-1", -1000},
		{"1k", 1000000},
		{"1M", 1000000000},
		{"1P", 1000000000000000000},
		{"1Ki", 1024000},
		{"1500Ki", 1536000000},
		{"2Gi", 2147483648000},
		{"1.5Gi", 1610612736000},
		{"1Pi", 1125899906842624000},
		{"0.0005Ki", 512},
		{"0", 0},
	} {
		got, err := ParseQuantity(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseQuantity(%q) = %d, %v, want %d", tc.in, got, err, tc.want)
		}
	}
	// Refused: no digits, a suffix the subset does not have, two decimal
	// points, an exponent that is not a number, precision finer than a
	// milli-unit, and a value whose milli-unit form leaves int64.
	for _, in := range []v1.Quantity{"", " ", "abc", "1.5.2", "1.", ".", "m", "Gi", "1 2", "5Zi", "2i", "1e", "1e1.5", "1n", "1u", "0.0001", "1.5e-3", "1e9999", "2Ei", "1E", "99999999999999999999"} {
		if got, err := ParseQuantity(in); err == nil {
			t.Errorf("ParseQuantity(%q) = %d, want an error", in, got)
		}
	}
}
