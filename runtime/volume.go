// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"fmt"

	v1 "latere.ai/x/cella/manifest/v1"
)

// The phases a Volume moves through on its driver (spec 019). Pending is a
// volume whose source has not been applied yet; Available one every attach
// may use; Failed one whose fill could not complete, with a reason. A volume
// the driver no longer holds is ErrNotFound from InspectVolume, which the
// controller writes as Lost.
const (
	VolumePending   = v1.VolumePending
	VolumeAvailable = v1.VolumeAvailable
	VolumeFailed    = v1.VolumeFailed
)

// The reasons a fill fails, as VolumeState.Reason carries them and spec
// 019's status.reason names them. Unreachable is a source the data plane
// could not fetch, such as an image whose pull failed; TooLarge one whose
// content is larger than the volume; Unsupported one this driver cannot
// apply.
const (
	ReasonSourceUnreachable = "SourceUnreachable"
	ReasonSourceTooLarge    = "SourceTooLarge"
	ReasonSourceUnsupported = "SourceUnsupported"
)

// The source kinds a driver fills a volume from and the access modes it
// attaches one in, which are the contract's own words (spec 019).
const (
	SourceEmpty      = v1.VolumeSourceEmpty
	SourceImage      = v1.VolumeSourceImage
	AccessSingle     = v1.VolumeAccessSingle
	AccessSharedRead = v1.VolumeAccessSharedRead
)

// FillError is a fill that could not complete, from a driver that fills a
// volume before CreateVolume returns: the reason status.reason carries and
// the driver's own sentence. A driver that fills after it returns reports
// the same through InspectVolume instead.
type FillError struct {
	Reason  string
	Message string
}

func (e *FillError) Error() string { return e.Reason + ": " + e.Message }

// VolumeSource is how a volume is filled once, at create. Image is the
// reference the control plane admitted, always pinned by digest, so a fill
// that runs twice copies the same bytes.
type VolumeSource struct {
	Kind  string `json:"kind"`
	Image string `json:"image,omitempty"`
}

// VolumeSpec is one volume as the driver is asked for it. Size is a
// Kubernetes quantity: the capacity to provision and, for an image source,
// the most the copied content may hold.
type VolumeSpec struct {
	ID     string       `json:"id"`
	Name   string       `json:"name,omitempty"`
	Owner  string       `json:"owner,omitempty"`
	Size   string       `json:"size"`
	Access string       `json:"access"`
	Source VolumeSource `json:"source"`
}

// VolumeState is what the driver reports of one volume: its phase, the
// reason and the driver's own sentence where the fill failed, and the
// capacity it granted, in quantity syntax.
type VolumeState struct {
	ID       string `json:"id"`
	Phase    string `json:"phase"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
	Capacity string `json:"capacity,omitempty"`
}

// VolumeDriver is the optional interface of a driver that declares the
// Volumes capability (specs 004 and 019). A volume is storage with a life of
// its own, filled once from its source and attached to a sandbox by a Mount
// in its CreateSpec.
//
// CreateVolume makes the volume and applies its source. A driver may fill
// before it returns or after, and InspectVolume reports Pending until the
// fill is done either way. It is safe to call again for a volume a previous
// call made: a volume whose fill is complete or still running answers
// ErrAlreadyExists, and one a previous process left half filled is filled
// again from the start. A source the driver cannot apply is ErrUnsupported.
//
// InspectVolume answers ErrNotFound for a volume the driver does not hold.
// DeleteVolume removes the volume and whatever its fill left behind, and
// answers nil for one already gone.
type VolumeDriver interface {
	CreateVolume(ctx context.Context, spec VolumeSpec) (Ref, error)
	InspectVolume(ctx context.Context, id string) (VolumeState, error)
	DeleteVolume(ctx context.Context, id string) error
}

// Mount is one volume attached to a sandbox: the manifest's own name for the
// mount, the volume's id, where it appears inside the sandbox, and whether
// the sandbox may write it.
type Mount struct {
	Name     string `json:"name"`
	VolumeID string `json:"volumeId"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// Check refuses a spec no driver can act on: an id, a known access mode and
// source kind, a size, and an image source that names a pinned image and
// nothing else. A driver calls it first, so the refusal is one sentence on
// every driver.
func (s VolumeSpec) Check() error {
	switch {
	case s.ID == "":
		return fmt.Errorf("%w: a volume needs an id", ErrInvalid)
	case s.Size == "":
		return fmt.Errorf("%w: a volume needs a size", ErrInvalid)
	case s.Access != AccessSingle && s.Access != AccessSharedRead:
		return fmt.Errorf("%w: access %q", ErrInvalid, s.Access)
	}
	switch s.Source.Kind {
	case SourceEmpty:
		if s.Source.Image != "" {
			return fmt.Errorf("%w: an empty volume names no image", ErrInvalid)
		}
	case SourceImage:
		if !v1.PinnedImage(s.Source.Image) {
			return fmt.Errorf("%w: the image %q is not pinned by digest", ErrInvalid, s.Source.Image)
		}
	default:
		return fmt.Errorf("%w: a volume filled from %q", ErrUnsupported, s.Source.Kind)
	}
	return nil
}
