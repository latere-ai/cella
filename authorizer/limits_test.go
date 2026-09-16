// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"encoding/json"
	"testing"

	"latere.ai/x/pkg/authz"
)

// TestDecodeLimits: an absent object and an empty one are the zero
// figures, a field the control plane does not know is ignored, and a
// figure below zero is refused, which the caller reads as no decision.
func TestDecodeLimits(t *testing.T) {
	for _, tc := range []struct {
		name, limits string
		want         Limits
		bad          bool
	}{
		{"absent", "", Limits{}, false},
		{"null", "null", Limits{}, false},
		{"empty", "{}", Limits{}, false},
		{"spec 006's example", `{"requests_per_minute": 1200, "max_sandboxes": 10, "max_priority": 5}`,
			Limits{RequestsPerMinute: 1200, MaxSandboxes: 10, MaxPriority: 5}, false},
		{"one ceiling", `{"max_sandboxes": 3}`, Limits{MaxSandboxes: 3}, false},
		{"an explicit zero", `{"max_sandboxes": 0}`, Limits{}, false},
		{"an unknown field", `{"max_seats": 3, "max_priority": 2}`, Limits{MaxPriority: 2}, false},
		{"a rate below zero", `{"requests_per_minute": -1}`, Limits{}, true},
		{"a count below zero", `{"max_sandboxes": -1}`, Limits{}, true},
		{"a priority below zero", `{"max_priority": -1}`, Limits{}, true},
		{"a count that is not a number", `{"max_sandboxes": "ten"}`, Limits{}, true},
		{"not an object", `[1]`, Limits{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := authz.Decision{Allow: true}
			if tc.limits != "" {
				d.Limits = json.RawMessage(tc.limits)
			}
			got, err := DecodeLimits(d)
			if tc.bad {
				if err == nil {
					t.Fatalf("DecodeLimits(%s) = %+v, want an error", tc.limits, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("DecodeLimits(%s) = %+v, %v; want %+v", tc.limits, got, err, tc.want)
			}
		})
	}
}

// TestWireLimitsLeavesOutWhatItDoesNotSet: the type an endpoint answers
// with renders only the ceilings it set, and what it renders decodes to
// the figures it meant.
func TestWireLimitsLeavesOutWhatItDoesNotSet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wire   WireLimits
		json   string
		figure Limits
	}{
		{"none", WireLimits{}, `{}`, Limits{}},
		{"a count alone", WireLimits{MaxSandboxes: ptr(10)}, `{"max_sandboxes":10}`, Limits{MaxSandboxes: 10}},
		{"a count of zero", WireLimits{MaxSandboxes: ptr(0)}, `{"max_sandboxes":0}`, Limits{}},
		{"spec 006's example", WireLimits{RequestsPerMinute: ptr(1200), MaxSandboxes: ptr(10), MaxPriority: ptr(5)},
			`{"requests_per_minute":1200,"max_sandboxes":10,"max_priority":5}`,
			Limits{RequestsPerMinute: 1200, MaxSandboxes: 10, MaxPriority: 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.wire)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.json {
				t.Errorf("rendered %s, want %s", raw, tc.json)
			}
			got, err := DecodeLimits(authz.Decision{Allow: true, Limits: raw})
			if err != nil || got != tc.figure {
				t.Fatalf("the answer decodes to %+v, %v; want %+v", got, err, tc.figure)
			}
		})
	}
}

func ptr(n int) *int { return &n }
