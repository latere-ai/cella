// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"fmt"

	"latere.ai/x/pkg/authz"
)

// Limits are what an allow granted the subject, decoded from the
// answer's limits object one wire name to one figure. Zero is the
// configured value or no ceiling: an absent field grants nothing and
// takes nothing away.
type Limits struct {
	// RequestsPerMinute overrides CELLA_REQUESTS_PER_MINUTE for this
	// subject (spec 008); 0 keeps the configured rate.
	RequestsPerMinute int
	// MaxSandboxes overrides CELLA_MAX_SANDBOXES_PER_SUBJECT, the live
	// sandboxes one subject holds (spec 007); 0 is no ceiling.
	MaxSandboxes int
	// MaxPriority caps scheduling.priority and reaches Resolve as the
	// manifest's Limits.MaxPriority (spec 003, spec 020); 0 is no
	// ceiling.
	MaxPriority int
}

// WireLimits is the limits object as an answer carries it, exported so
// an authorizer renders its answer through the type cellad decodes
// rather than through three string literals. Every member is optional; a
// member the object does not name stays nil, and omitempty leaves it
// out, so an authorizer that sets one ceiling sends one and the other
// two stay absent, which grants nothing and takes nothing away. All
// three are pointers, so a member deliberately set to zero is still sent
// and decodes to zero, the same grant as absence.
type WireLimits struct {
	RequestsPerMinute *int `json:"requests_per_minute,omitempty"`
	MaxSandboxes      *int `json:"max_sandboxes,omitempty"`
	MaxPriority       *int `json:"max_priority,omitempty"`
}

// DecodeLimits reads a decision's limits object. A decision with no
// limits is the zero Limits, and a field the control plane does not know
// is ignored. An object that does not parse or a figure below zero is an
// error, and the caller treats the answer as no decision: a ceiling the
// control plane cannot read is not a ceiling it can hold.
func DecodeLimits(d authz.Decision) (Limits, error) {
	var w WireLimits
	if err := d.DecodeLimits(&w); err != nil {
		return Limits{}, err
	}
	var l Limits
	for _, f := range []struct {
		name string
		src  *int
		dst  *int
	}{
		{"requests_per_minute", w.RequestsPerMinute, &l.RequestsPerMinute},
		{"max_sandboxes", w.MaxSandboxes, &l.MaxSandboxes},
		{"max_priority", w.MaxPriority, &l.MaxPriority},
	} {
		if f.src == nil {
			continue
		}
		if *f.src < 0 {
			return Limits{}, fmt.Errorf("limits.%s is %d, below zero", f.name, *f.src)
		}
		*f.dst = *f.src
	}
	return l, nil
}
