// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"latere.ai/x/pkg/audit"

	v1 "latere.ai/x/cella/manifest/v1"
)

// IDPrefix is the kind prefix design 001 gives an event.
const IDPrefix = "evt_"

// alphabet is Crockford's base32, the ULID encoding.
const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// NewID is a record's id: the prefix and a ULID, so ids sort by the instant
// they were made. Order within an object is still the sequence; the id
// orders records of different objects for a reader with no other key.
func NewID() string {
	var b [16]byte
	ms := uint64(time.Now().UTC().UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	// crypto/rand.Read fills the slice entirely and never reports an error,
	// which is what the standard library documents.
	_, _ = rand.Read(b[6:])
	// 26 characters of five bits hold 130, so the 128 are read behind two
	// zero bits.
	var out [26]byte
	acc, bits, pos := uint32(0), 2, 0
	for _, x := range b {
		acc = acc<<8 | uint32(x)
		for bits += 8; bits >= 5; pos++ {
			bits -= 5
			out[pos] = alphabet[(acc>>uint(bits))&31]
		}
	}
	return IDPrefix + string(out[:])
}

// OfSandbox is the record object for one sandbox: its identity and the
// labels the sink files it under.
func OfSandbox(obj v1.Sandbox) Object {
	return Object{
		Kind:   KindSandbox,
		ID:     obj.Status.ID,
		Name:   obj.Metadata.Name,
		Owner:  obj.Status.Owner,
		Labels: maps.Clone(obj.Metadata.Labels),
	}
}

// Mutation is the record for one act the controller took on one object. A
// terminal transition with no reason on the object and an actor that is not
// the control plane took Request: a person asked for it, which is the value
// design 009's enum has for that act.
func Mutation(t Type, reason Reason, obj Object, data any, a Actor, at time.Time) (Record, error) {
	if Terminal(t) && reason == "" && a.Subject != SubjectController {
		reason = ReasonRequest
	}
	return build(t, reason, obj, nil, data, a, at)
}

// Operation is the record for one operation on one sandbox: an act that
// changes no desired state and still belongs in the feed. The sandbox is
// named twice, as the object the record is about and as the sandbox in
// context, because a sink reads the second on every record that has one.
func Operation(t Type, obj Object, data any, a Actor, at time.Time) (Record, error) {
	context := obj
	return build(t, "", obj, &context, data, a, at)
}

// build assembles a record and redacts its data. Redaction is the second
// line: the structural rule is that each Data type below holds no content at
// all, and audit.RedactJSON catches a credential that reached a field the
// shape does admit, such as a workspace path or an image reference.
func build(t Type, reason Reason, obj Object, sandbox *Object, data any, a Actor, at time.Time) (Record, error) {
	if reason != "" && !reason.Known() {
		return Record{}, fmt.Errorf("events: %q is not a reason of design 009", reason)
	}
	r := Record{
		ID:        NewID(),
		Type:      t,
		Time:      at.UTC(),
		Object:    obj,
		Sandbox:   sandbox,
		Subject:   a.Subject,
		Workload:  a.workload(),
		RequestID: a.RequestID,
		Reason:    reason,
	}
	if r.Subject == "" {
		r.Subject = SubjectController
	}
	if data != nil {
		encoded, err := json.Marshal(data)
		if err != nil {
			return Record{}, fmt.Errorf("events: encoding the data of %s: %w", t, err)
		}
		r.Data = audit.RedactJSON(encoded)
	}
	return r, r.Valid()
}

// Phase is the data of every lifecycle transition: the phase the object
// reached. The reason says why it reached it.
type Phase struct {
	Phase string `json:"phase"`
}

// Exec is the data of sandbox.exec: how the command ended and how long it
// took. The command itself is never here. A command line is where a secret
// reaches a process, and a record that cannot hold the string needs no scan
// to prove it does not.
type Exec struct {
	ExitCode   int   `json:"exitCode"`
	DurationMS int64 `json:"durationMs"`
}

// Files is the data of sandbox.files: which operation ran, which way a
// transfer went, which workspace paths it named, and how many bytes moved. No
// file content and no file body.
type Files struct {
	Operation string   `json:"operation,omitempty"`
	Direction string   `json:"direction,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	Bytes     int64    `json:"bytes"`
}

// The two directions a transfer runs in.
const (
	DirectionImport = "import"
	DirectionExport = "export"
)

// The operations a sandbox.files record names. The two transfers carry a
// direction as well; the per-file operations are the whole of design 033's
// routes.
const (
	OperationImport = DirectionImport
	OperationExport = DirectionExport
	OperationRead   = "read"
	OperationWrite  = "write"
	OperationStat   = "stat"
	OperationList   = "list"
	OperationMkdir  = "mkdir"
	OperationRemove = "remove"
	OperationMove   = "move"
)

// Created is the data of sandbox.created: the manifest as the resolver left
// it, with every environment value dropped to its key and every mounted
// secret dropped to its name. A value belongs to the sandbox and never to a
// record, and neither does the placeholder that stands in for one.
type Created struct {
	Environment string       `json:"environment,omitempty"`
	Image       string       `json:"image,omitempty"`
	Command     []string     `json:"command,omitempty"`
	Args        []string     `json:"args,omitempty"`
	Workdir     string       `json:"workdir,omitempty"`
	User        string       `json:"user,omitempty"`
	Resources   v1.Resources `json:"resources,omitzero"`
	Workspace   v1.Workspace `json:"workspace,omitzero"`
	Lifecycle   v1.Lifecycle `json:"lifecycle,omitzero"`
	Env         []string     `json:"env,omitempty"`
	// Mounts are the secrets the manifest mounts, by name. The key is not
	// "secrets" because audit.RedactJSON blanks every field whose key ends
	// in secret or secrets, and a name is not a credential.
	Mounts []string          `json:"mounts,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// CreatedOf reduces one resolved manifest to the created record's data.
func CreatedOf(obj v1.Sandbox) Created {
	return Created{
		Environment: obj.Spec.Environment,
		Image:       obj.Spec.Image,
		Command:     slices.Clone(obj.Spec.Command),
		Args:        slices.Clone(obj.Spec.Args),
		Workdir:     obj.Spec.Workdir,
		User:        obj.Spec.User,
		Resources:   obj.Spec.Resources,
		Workspace:   obj.Spec.Workspace,
		Lifecycle:   obj.Spec.Lifecycle,
		Env:         slices.Sorted(maps.Keys(obj.Spec.Env)),
		Mounts:      mountedNames(obj),
		Labels:      maps.Clone(obj.Metadata.Labels),
	}
}

// mountedNames is the secrets a manifest mounts, by name. The environment key
// each arrives under is the sandbox's own business and the placeholder is the
// gateway's, so neither is here.
func mountedNames(obj v1.Sandbox) []string {
	if len(obj.Spec.Secrets) == 0 {
		return nil
	}
	out := make([]string, 0, len(obj.Spec.Secrets))
	for _, mount := range obj.Spec.Secrets {
		out = append(out, mount.Name)
	}
	slices.Sort(out)
	return out
}
