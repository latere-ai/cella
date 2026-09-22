// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"maps"
	"time"

	"latere.ai/x/cella/controller"
	v1 "latere.ai/x/cella/manifest/v1"
)

// KindEnvironment is the third object kind this vocabulary carries: the place
// sandboxes run, whether it is the driver this process holds or a data plane
// somewhere else that connects outbound to claim its work.
const KindEnvironment = "Environment"

// The types design 009 names that an Environment produces. None of them
// carries a key: what a reader learns of a key is that one was minted or
// revoked and which jti it was, which is what a revocation is keyed by and is
// not a credential.
const (
	TypeEnvironmentCreated    Type = "environment.created"
	TypeEnvironmentUpdated    Type = "environment.updated"
	TypeEnvironmentRegistered Type = "environment.registered"
	TypeEnvironmentOffline    Type = "environment.offline"
	TypeEnvironmentKeyed      Type = "environment.keyed"
	TypeEnvironmentKeyRevoked Type = "environment.key_revoked"
	TypeEnvironmentDeleted    Type = "environment.deleted"
)

// EnvironmentTypes is every type an Environment produces, in the order design
// 009's table lists them.
var EnvironmentTypes = []Type{
	TypeEnvironmentCreated, TypeEnvironmentUpdated, TypeEnvironmentRegistered,
	TypeEnvironmentOffline, TypeEnvironmentKeyed, TypeEnvironmentKeyRevoked,
	TypeEnvironmentDeleted,
}

// OfEnvironment is the record object for one environment: its identity and
// the labels the sink files it under.
func OfEnvironment(obj v1.Environment) Object {
	id := obj.Status.ID
	if id == "" {
		id = obj.Metadata.Name
	}
	return Object{
		Kind:   KindEnvironment,
		ID:     id,
		Name:   obj.Metadata.Name,
		Owner:  obj.Status.Owner,
		Labels: maps.Clone(obj.Metadata.Labels),
	}
}

// Environment is the data of every environment record: how many workers hold
// a stream, the phase the object reached, and the jti of the key a mint or a
// revocation named. Design 021's table fixes the fields, and the shape holds
// nothing else, so no scan is needed to prove a record carries no credential.
type Environment struct {
	Workers int    `json:"workers,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Reason  string `json:"reason,omitempty"`
	JTI     string `json:"jti,omitempty"`
	Worker  string `json:"worker,omitempty"`
}

// EnvironmentMutation is the record for one act on one Environment. An
// environment's transitions carry no reason of design 009's enum: the reason
// spec 021 gives a phase below Ready is the record's own data.
func EnvironmentMutation(t Type, obj v1.Environment, data Environment, a Actor, at time.Time) (Record, error) {
	return build(t, "", OfEnvironment(obj), nil, data, a, at)
}

// EmitEnvironment is the controller's seam of design 009 for the Environment
// kind, for a store that keeps no journal of its own.
func (e *Emitter) EmitEnvironment(ctx context.Context, a controller.EnvironmentAct) {
	if e == nil || e.journal == nil {
		return
	}
	kind := Type(a.Type)
	if !Deliverable(kind) {
		return
	}
	record, err := EnvironmentMutation(kind, a.Object,
		Environment{Workers: a.Workers, Phase: a.Object.Status.Phase, Reason: a.Reason},
		ActorFrom(ctx), time.Now().UTC())
	if err != nil {
		e.log.WarnContext(ctx, "the event was not built",
			"type", a.Type, "object", a.Object.Metadata.Name, "error", err)
		return
	}
	e.Write(ctx, record)
}
