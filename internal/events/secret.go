// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"maps"
	"slices"
	"time"

	"latere.ai/x/cella/controller"
	v1 "latere.ai/x/cella/manifest/v1"
)

// KindSecret is the second object kind this vocabulary carries.
const KindSecret = "Secret"

// The types design 009 names that a Secret produces. A secret's value is not
// in any of them, and neither is its placeholder: what a reader learns is
// that the object changed, which version it is at, and how far it reaches.
const (
	TypeSecretCreated Type = "secret.created"
	TypeSecretUpdated Type = "secret.updated"
	TypeSecretDeleted Type = "secret.deleted"
)

// SecretTypes is every type a Secret produces, in the order design 009's
// table lists them.
var SecretTypes = []Type{TypeSecretCreated, TypeSecretUpdated, TypeSecretDeleted}

// OfSecret is the record object for one secret: its identity and the labels
// the sink files it under.
func OfSecret(obj v1.Secret) Object {
	return Object{
		Kind:   KindSecret,
		ID:     obj.Status.ID,
		Name:   obj.Metadata.Name,
		Owner:  obj.Status.Owner,
		Labels: maps.Clone(obj.Metadata.Labels),
	}
}

// Secret is the data of every secret record: which version the object is at
// and the hosts its value may be sent to. Design 009's table fixes the two
// fields, and the shape holds nothing else, so no scan is needed to prove a
// record carries no credential.
type Secret struct {
	Version int      `json:"version"`
	Hosts   []string `json:"hosts,omitempty"`
}

// SecretOf reduces one Secret to its record's data.
func SecretOf(obj v1.Secret) Secret {
	return Secret{Version: obj.Status.Version, Hosts: slices.Clone(obj.Spec.Scope.Hosts)}
}

// SecretMutation is the record for one act on one Secret. A secret has no
// phase and no terminal transition, so it carries no reason.
func SecretMutation(t Type, obj v1.Secret, a Actor, at time.Time) (Record, error) {
	return build(t, "", OfSecret(obj), nil, SecretOf(obj), a, at)
}

// EmitSecret is the controller's seam of design 009 for the Secret kind, for
// a store that keeps no journal of its own.
func (e *Emitter) EmitSecret(ctx context.Context, a controller.SecretAct) {
	if e == nil || e.journal == nil {
		return
	}
	kind := Type(a.Type)
	if !Deliverable(kind) {
		return
	}
	record, err := SecretMutation(kind, a.Object, ActorFrom(ctx), time.Now().UTC())
	if err != nil {
		e.log.WarnContext(ctx, "the event was not built",
			"type", a.Type, "object", a.Object.Status.ID, "error", err)
		return
	}
	e.Write(ctx, record)
}
