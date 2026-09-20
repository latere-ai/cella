// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package events is design 009: one record per mutation and per operation,
// journaled with the act that produced it and delivered signed to the sink
// the operator names.
//
// The record is the wire. A sink decodes the JSON below and nothing else, so
// the field names here are the contract and not a rendering choice. What a
// record never carries is structural: the data of each type is a fixed shape
// with no command text, no file content, no environment or secret value and
// no token, and audit.RedactJSON runs over it behind that rule.
package events

import (
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// Record is one event.
//
// Seq, ID, Type and Time are the journal row's columns. They are cleared
// before the rest is stored and written back when the row is read, so one
// row rebuilds one body and a retry signs the bytes the first attempt did.
type Record struct {
	ID   string    `json:"id"`
	Seq  int64     `json:"seq"`
	Type Type      `json:"type"`
	Time time.Time `json:"time"`
	// Object is what the record is about, with the labels the sink files it
	// under. Sandbox names the sandbox in context where the object is not
	// itself the whole story, which is every operation.
	Object  Object  `json:"object"`
	Sandbox *Object `json:"sandbox,omitempty"`
	// Subject is the rendered subject of design 006, or SubjectController
	// for an act the control plane took on its own. Workload is set where
	// the bearer was a sandbox's own token.
	Subject  string    `json:"subject"`
	Workload *Workload `json:"workload,omitempty"`
	// RequestID is the X-Request-ID of the request that caused the act, and
	// empty for the reaper and the recovery loop.
	RequestID string `json:"requestId"`
	// Reason is set on every terminal transition and empty elsewhere.
	Reason Reason `json:"reason,omitempty"`
	// Data is the per-type shape of design 009's table, redacted.
	Data json.RawMessage `json:"data,omitempty"`
}

// Object is what a record is about: the identity of one object of one kind,
// and the labels a plane stamped on it. The sink files a record under the
// tenant its labels name and under nothing else, so the labels travel with
// every record rather than being looked up.
type Object struct {
	Kind   string            `json:"kind"`
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Owner  string            `json:"owner"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Workload is the sandbox a workload token names. Design 006's full workload
// reference arrives with the tokens of slice 045; the id is the part that
// exists today and the object leaves room for the rest.
type Workload struct {
	ID string `json:"id"`
}

// KindSandbox is the only object kind this slice emits about.
const KindSandbox = "Sandbox"

// Type is design 009's closed event vocabulary. A type outside it is not
// delivered: Deliverable reports the difference, and the journal still holds
// the row.
type Type string

// The types design 009 names that a Sandbox produces.
const (
	TypeCreated    Type = "sandbox.created"
	TypeUpdated    Type = "sandbox.updated"
	TypeStarted    Type = "sandbox.started"
	TypeStopped    Type = "sandbox.stopped"
	TypeDeleted    Type = "sandbox.deleted"
	TypeFailed     Type = "sandbox.failed"
	TypeLost       Type = "sandbox.lost"
	TypeRecovering Type = "sandbox.recovering"
	TypeRecovered  Type = "sandbox.recovered"
	TypeExec       Type = "sandbox.exec"
	TypeFiles      Type = "sandbox.files"
)

// Types is every type this slice emits, in the order design 009's table
// lists them. A test walks it.
var Types = []Type{
	TypeCreated, TypeUpdated, TypeStarted, TypeStopped, TypeDeleted,
	TypeFailed, TypeLost, TypeRecovering, TypeRecovered, TypeExec, TypeFiles,
	TypeSecretCreated, TypeSecretUpdated, TypeSecretDeleted,
}

// Deliverable reports whether a journal row's type is one the sink receives.
// The controller writes rows for two acts that are not events, the deleting
// intent and the status write that follows a driver read; both are journaled
// because design 010 records every mutation, and neither is delivered.
func Deliverable(t Type) bool {
	return slices.Contains(Types, t)
}

// Terminal reports whether a type ends a phase, which is where design 009
// requires a reason.
func Terminal(t Type) bool {
	switch t {
	case TypeStarted, TypeStopped, TypeDeleted, TypeFailed, TypeLost:
		return true
	}
	return false
}

// Reason is design 009's one transition enum, written in one case.
type Reason string

// The reasons this slice writes. The rest of design 009's enum belongs to
// the slices that end a sandbox for those causes.
const (
	ReasonRequest           Reason = "Request"
	ReasonAutoStop          Reason = "AutoStop"
	ReasonAutoDelete        Reason = "AutoDelete"
	ReasonExpired           Reason = "Expired"
	ReasonExited            Reason = "Exited"
	ReasonLost              Reason = "Lost"
	ReasonCreateFailed      Reason = "CreateFailed"
	ReasonDriverFailed      Reason = "DriverFailed"
	ReasonRecoveryExhausted Reason = "RecoveryExhausted"
)

// Reasons is every reason this slice writes; a test holds each record's
// reason to it.
var Reasons = []Reason{
	ReasonRequest, ReasonAutoStop, ReasonAutoDelete, ReasonExpired,
	ReasonExited, ReasonLost, ReasonCreateFailed, ReasonDriverFailed,
	ReasonRecoveryExhausted,
}

// ReasonOf maps a status reason to the closed enum. A driver names failures
// of its own that design 009's enum does not, and DriverFailed is the value
// the enum has for a failure this contract does not name; the sandbox's own
// status keeps the driver's word for it. A reason outside the enum on a
// transition that is not terminal is dropped, because only a terminal
// transition carries one.
func ReasonOf(status string, t Type) Reason {
	reason := Reason(status)
	switch {
	case status == "":
		return ""
	case reason.Known():
		return reason
	case Terminal(t):
		return ReasonDriverFailed
	}
	return ""
}

// Known reports whether a reason is in the enum.
func (r Reason) Known() bool {
	return slices.Contains(Reasons, r)
}

// ErrIncomplete is a record missing what the sink cannot store it without.
var ErrIncomplete = errors.New("events: the record has no id, type or object")

// Valid reports what the sink's own check reports, so a record this process
// would deliver and the sink would refuse never leaves here.
func (r Record) Valid() error {
	if r.ID == "" || r.Type == "" || r.Object.ID == "" {
		return ErrIncomplete
	}
	return nil
}

// Body is the exact bytes delivered and signed. It is one function so the
// journal, the feed and the wire agree.
func Body(r Record) ([]byte, error) {
	return json.Marshal(r)
}

// Payload is the half of a record the journal stores outside its columns.
// The columns hold the id, the sequence, the type and the time; clearing
// them here keeps one fact in one place, and Rebuild puts them back.
func Payload(r Record) ([]byte, error) {
	r.ID, r.Seq, r.Type, r.Time = "", 0, "", time.Time{}
	return json.Marshal(r)
}

// Rebuild is Payload's inverse: one journal row becomes the record it was
// written from, with the columns authoritative over anything in the bytes.
func Rebuild(payload []byte, id string, seq int64, typ string, at time.Time) (Record, error) {
	var r Record
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &r); err != nil {
			return Record{}, err
		}
	}
	r.ID, r.Seq, r.Type, r.Time = id, seq, Type(typ), at.UTC()
	return r, nil
}
