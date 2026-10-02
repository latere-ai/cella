// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"fmt"

	"latere.ai/x/pkg/authz"
)

// Limits are what an allow granted the subject, decoded from the
// answer's limits object one wire name to one figure. Zero is no
// figure: an absent field grants nothing and takes nothing away. No
// variable of cellad's sets any of the three; the allow is their only
// source.
type Limits struct {
	// RequestsPerMinute is a request rate for this subject (spec 008).
	// cellad holds no rate limit, so it refuses a request whose allow
	// carries a figure above zero with capability_unsupported rather
	// than serve it unlimited; 0 limits nothing.
	RequestsPerMinute int
	// MaxSandboxes is the subject's sandbox ceiling, the count spec 007
	// defines: every desired sandbox of the subject whose phase is not
	// Deleting, a queued and a stopped one included, because each holds
	// a name and a workspace. cellad reads it from the allow of
	// sandbox.create, for a create and a spawn alike, and refuses a
	// create past it with quota_exceeded. 0 is no ceiling.
	MaxSandboxes int
	// MaxPriority caps scheduling.priority (spec 003, spec 020). cellad
	// decodes it and does not apply it; a plane built on the packages
	// applies it by passing it to manifest.Resolve as
	// Options.Limits.MaxPriority. 0 is no ceiling.
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
