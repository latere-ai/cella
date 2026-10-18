// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"regexp"
	"strings"
	"time"
)

// KindVolume is the kind a Volume manifest declares.
const KindVolume = "Volume"

// VolumeIDPrefix is the kind prefix a Volume's id carries.
const VolumeIDPrefix = "vol_"

// Volume is storage with a life of its own (spec 019): created and deleted
// by its owner, filled once from its source, attached to a sandbox by a
// mount in the sandbox's manifest, and kept when the sandbox ends.
type Volume struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   Metadata     `json:"metadata"`
	Spec       VolumeSpec   `json:"spec"`
	Status     VolumeStatus `json:"status,omitzero"`
}

// VolumeSpec is what the owner declared. Environment is where the volume
// lives, and a volume never leaves it; Size is the capacity in quantity
// syntax; Access is who may attach it and how; Source is how it is filled
// once, at create; Retain keeps it after its last sandbox, and is true when
// absent.
type VolumeSpec struct {
	Environment string       `json:"environment,omitempty"`
	Size        Quantity     `json:"size,omitempty"`
	Class       string       `json:"class,omitempty"`
	Access      string       `json:"access,omitempty"`
	Source      VolumeSource `json:"source,omitzero"`
	Retain      *bool        `json:"retain,omitempty"`
}

// Retains reports whether the volume outlives its last sandbox, which is
// what an absent retain says.
func (s VolumeSpec) Retains() bool { return s.Retain == nil || *s.Retain }

// The two access modes. A single volume takes one read-write attach or any
// number of read-only ones, never both at once; a shared-read volume takes
// read-only attaches only, any number of them.
const (
	VolumeAccessSingle     = "single"
	VolumeAccessSharedRead = "shared-read"
)

// VolumeAccesses is every access mode, in the order the contract lists them.
var VolumeAccesses = []string{VolumeAccessSingle, VolumeAccessSharedRead}

// VolumeSource is how a volume is filled: exactly the field its kind names
// is set. Image is an OCI reference, stored as the reference the control
// plane admitted, which is pinned by digest. Snapshot, Archive and SHA256
// are the contract's other sources.
type VolumeSource struct {
	Kind     string `json:"kind,omitempty"`
	Snapshot string `json:"snapshot,omitempty"`
	Image    string `json:"image,omitempty"`
	Archive  string `json:"archive,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}

// The four source kinds of the contract.
const (
	VolumeSourceEmpty    = "empty"
	VolumeSourceSnapshot = "snapshot"
	VolumeSourceImage    = "image"
	VolumeSourceArchive  = "archive"
)

// VolumeSources is every source kind, in the order the contract lists them.
var VolumeSources = []string{VolumeSourceEmpty, VolumeSourceSnapshot, VolumeSourceImage, VolumeSourceArchive}

// VolumeStatus is the server's. Phase moves from Pending to Available once
// the source is applied, to Failed with a reason where it cannot be, to
// Deleting on a delete, and to Lost where the driver no longer holds the
// volume. Message is the driver's own sentence on a failed fill. AttachedTo
// is derived at read from the sandboxes whose desired state mounts the
// volume, Stopped ones included.
type VolumeStatus struct {
	ID         string    `json:"id"`
	Owner      string    `json:"owner"`
	Phase      string    `json:"phase"`
	Reason     string    `json:"reason,omitempty"`
	Message    string    `json:"message,omitempty"`
	AttachedTo []string  `json:"attachedTo"`
	Capacity   Quantity  `json:"capacity,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt,omitzero"`
}

// The phases of a Volume.
const (
	VolumePending   = "Pending"
	VolumeAvailable = "Available"
	VolumeFailed    = "Failed"
	VolumeDeleting  = "Deleting"
	VolumeLost      = "Lost"
)

// VolumeMount is one entry of a Sandbox's spec.volumes: the mount's own name,
// where the volume appears inside the sandbox, the volume by name among the
// caller's own or by vol_ id, and whether the sandbox may write it.
type VolumeMount struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Volume   string `json:"volume"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// VolumeAttachment is one mount as the sandbox's status reports it: the
// mount's name, the id of the volume it was bound to at create, and whether
// the volume is attached. Binding is by id, so a volume deleted and recreated
// under one name is a different volume to a sandbox that exists.
type VolumeAttachment struct {
	Name     string `json:"name"`
	Volume   string `json:"volume"`
	Attached bool   `json:"attached"`
}

// pinnedImage is an OCI reference pinned by digest: a repository, optionally
// a tag, and a sha256 digest of 64 lower-case hex digits.
var pinnedImage = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// PinnedImage reports whether an image reference is pinned by digest, which
// is the one form a volume's image source is stored in.
func PinnedImage(ref string) bool { return pinnedImage.MatchString(ref) }

// ImageDigest is the digest of a pinned reference, sha256:<hex>, and the
// empty string for any other. A reader that verifies a volume's content
// compares this and never the repository name.
func ImageDigest(ref string) string {
	if !PinnedImage(ref) {
		return ""
	}
	_, digest, _ := strings.Cut(ref, "@")
	return digest
}
