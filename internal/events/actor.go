// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import "context"

// SubjectController is the subject of a record for an act no request asked
// for: the reaper's deadline rules and the recovery loop.
const SubjectController = "controller"

// Actor is who caused an act. It travels on the context because the acts
// that produce records are several calls below the handler that knows it,
// and because a controller loop has none: the zero Actor is the control
// plane acting on its own.
type Actor struct {
	// Subject is design 006's rendered subject.
	Subject string
	// Workload is the sandbox id where the bearer was a workload token, and
	// empty for a person.
	Workload string
	// RequestID is design 008's request id, the X-Request-ID of the
	// response the caller reads.
	RequestID string
}

type actorKey struct{}

// WithActor puts the actor on the context. The API calls it once per
// request, before anything that can mutate an object runs.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom reads the actor a request put on the context, or the control
// plane's own where none did.
func ActorFrom(ctx context.Context) Actor {
	a, ok := ctx.Value(actorKey{}).(Actor)
	if !ok || a.Subject == "" {
		return Actor{Subject: SubjectController, Workload: a.Workload, RequestID: a.RequestID}
	}
	return a
}

// workload is the record's workload reference, or nil for a person.
func (a Actor) workload() *Workload {
	if a.Workload == "" {
		return nil
	}
	return &Workload{ID: a.Workload}
}
