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

// KindVolume is the object kind of storage with a life of its own (spec 019).
const KindVolume = "Volume"

// The types design 009 names that a Volume produces. An attach and a detach
// name the sandbox in context and the mode it holds the volume in; a failure
// carries the reason the fill could not complete.
const (
	TypeVolumeCreated  Type = "volume.created"
	TypeVolumeUpdated  Type = "volume.updated"
	TypeVolumeAttached Type = "volume.attached"
	TypeVolumeDetached Type = "volume.detached"
	TypeVolumeFailed   Type = "volume.failed"
	TypeVolumeDeleted  Type = "volume.deleted"
)

// VolumeTypes is every type a Volume produces, in the order design 009's
// table lists them.
var VolumeTypes = []Type{
	TypeVolumeCreated, TypeVolumeUpdated, TypeVolumeAttached, TypeVolumeDetached,
	TypeVolumeFailed, TypeVolumeDeleted,
}

// The reasons a fill fails, which design 009's enum carries for
// volume.failed.
const (
	ReasonSourceUnreachable Reason = "SourceUnreachable"
	ReasonSourceTooLarge    Reason = "SourceTooLarge"
	ReasonSourceUnsupported Reason = "SourceUnsupported"
)

// OfVolume is the record object for one volume: its identity and the labels
// the sink files it under.
func OfVolume(obj v1.Volume) Object {
	return Object{
		Kind:   KindVolume,
		ID:     obj.Status.ID,
		Name:   obj.Metadata.Name,
		Owner:  obj.Status.Owner,
		Labels: maps.Clone(obj.Metadata.Labels),
	}
}

// Volume is the data of every volume record: the phase the volume is in, how
// it is filled, and for an attach or a detach the mode the sandbox holds it
// in. The image is a pinned reference, never a credential.
type Volume struct {
	Phase  string `json:"phase,omitempty"`
	Access string `json:"access,omitempty"`
	Source string `json:"source,omitempty"`
	Image  string `json:"image,omitempty"`
	Mode   string `json:"mode,omitempty"`
}

// VolumeMutation is the record for one act on one Volume. An attach and a
// detach name the sandbox in context; a failure carries its reason when the
// reason is one of design 009's enum and none otherwise.
func VolumeMutation(t Type, obj v1.Volume, sandbox *v1.Sandbox, mode string, a Actor, at time.Time) (Record, error) {
	var context *Object
	if sandbox != nil {
		sb := OfSandbox(*sandbox)
		context = &sb
	}
	var reason Reason
	if t == TypeVolumeFailed && Reason(obj.Status.Reason).Known() {
		reason = Reason(obj.Status.Reason)
	}
	data := Volume{
		Phase: obj.Status.Phase, Access: obj.Spec.Access,
		Source: obj.Spec.Source.Kind, Image: obj.Spec.Source.Image, Mode: mode,
	}
	return build(t, reason, OfVolume(obj), context, data, a, at)
}

// EmitVolume is the controller's seam of design 009 for the Volume kind, for
// a store that keeps no journal of its own.
func (e *Emitter) EmitVolume(ctx context.Context, a controller.VolumeAct) {
	if e == nil || e.journal == nil {
		return
	}
	kind := Type(a.Type)
	if !Deliverable(kind) {
		return
	}
	record, err := VolumeMutation(kind, a.Object, a.Sandbox, a.Mode, ActorFrom(ctx), time.Now().UTC())
	if err != nil {
		e.log.WarnContext(ctx, "the event was not built",
			"type", a.Type, "object", a.Object.Status.ID, "error", err)
		return
	}
	e.Write(ctx, record)
}
