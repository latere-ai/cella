// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"time"

	"latere.ai/x/cella/controller"
	v1 "latere.ai/x/cella/manifest/v1"
)

// Emit is the controller's seam of design 009, for a store that keeps no
// journal of its own. The record is built the same way the store bridge
// builds it, so the two paths produce one shape.
func (e *Emitter) Emit(ctx context.Context, a controller.Act) {
	if e == nil || e.journal == nil {
		return
	}
	kind := Type(a.Type)
	if !Deliverable(kind) {
		return
	}
	record, err := Mutation(kind, ReasonOf(a.Object.Status.Reason, kind),
		OfSandbox(a.Object), MutationData(kind, a.Object), ActorFrom(ctx), time.Now().UTC())
	if err != nil {
		e.log.WarnContext(ctx, "the event was not built",
			"type", a.Type, "object", a.Object.Status.ID, "error", err)
		return
	}
	e.Write(ctx, record)
}

// MutationData is the per-type data of design 009's table for an act on a
// sandbox: a create carries the resolved manifest with every environment
// value dropped to its key, and every other act the phase it reached, with
// the reason saying why.
func MutationData(kind Type, obj v1.Sandbox) any {
	if kind == TypeCreated {
		return CreatedOf(obj)
	}
	return Phase{Phase: obj.Status.Phase}
}

// The controller's seam this type satisfies. A change to either side that
// breaks the other is a build failure here rather than a nil emitter at
// start-up.
var _ controller.Events = (*Emitter)(nil)
