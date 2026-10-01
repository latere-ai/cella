// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// Protocol is the version of the frame vocabulary below. It is the
// subprotocol name a WebSocket transport would negotiate and the value the
// gateway sends in its hello, so a control plane and a gateway of different
// releases refuse one another rather than half understand.
const Protocol = "cella.egress.v1"

// The frame types. A gateway connects outbound and the control plane never
// dials it, so every frame travels on a stream the gateway opened: the maps
// come down the one it holds open, the acknowledgments and records go up
// the one it posts.
const (
	// FrameHello is the gateway's first frame: who it is, what it already
	// holds, and the certificate authority it terminates TLS with.
	FrameHello = "hello"
	// FrameSnapshot is every map of the environment. It is authoritative:
	// the gateway replaces its whole registry and drops every principal
	// absent from it, so a purge missed while disconnected still lands.
	FrameSnapshot = "snapshot"
	// FramePut is one new or changed map, applied when its version is above
	// the held one.
	FramePut = "put"
	// FramePurge says a principal is gone.
	FramePurge = "purge"
	// FrameAck is the gateway confirming a version, whether it applied it or
	// already held it.
	FrameAck = "ack"
	// FrameHeartbeat keeps an idle stream alive and proves the far end is
	// still there.
	FrameHeartbeat = "heartbeat"
	// FrameRecord is one connection the gateway handled.
	FrameRecord = "record"
	// FrameUse is the gateway reporting that it substituted one secret's
	// value into a request of one sandbox, which the control plane writes as
	// the secret's status.lastUsedAt. A control plane of a release before it
	// logs the frame as unknown and reads on, so a gateway sends it without
	// asking what the far end speaks.
	FrameUse = "use"
)

// HeartbeatInterval is how often each side sends a heartbeat, and
// HeartbeatTimeout how long without one before the stream is closed.
const (
	HeartbeatInterval = 15 * time.Second
	HeartbeatTimeout  = 45 * time.Second
)

// Frame is one message of the sync protocol, one JSON object per text
// message. The type names which field carries the payload; every other field
// is absent.
type Frame struct {
	Type     string    `json:"type"`
	Hello    *Hello    `json:"hello,omitempty"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
	Put      *Map      `json:"put,omitempty"`
	Purge    *Purge    `json:"purge,omitempty"`
	Ack      *Ack      `json:"ack,omitempty"`
	Record   *Record   `json:"record,omitempty"`
	Use      *Use      `json:"use,omitempty"`
}

// Hello is what a connecting gateway says about itself.
type Hello struct {
	Protocol  string `json:"protocol"`
	GatewayID string `json:"gatewayId"`
	// Principal, when set, asks for one sandbox's map only, which is what a
	// per-pod gateway beside one sandbox wants.
	Principal string `json:"principal,omitempty"`
	// Versions is what the gateway already holds, so a control plane can see
	// at a glance what a reconnect changed. The snapshot is sent whatever it
	// says, because the snapshot is what makes the gateway whole.
	Versions map[string]int64 `json:"versions,omitempty"`
	// CAPEM is the certificate the gateway signs its leaves with. The
	// control plane projects it into every sandbox of the environment, so a
	// workload trusts the door it is pointed at and nothing else.
	CAPEM string `json:"caPem,omitempty"`
}

// Snapshot is the authoritative set of maps for the connection's scope.
type Snapshot struct {
	Maps []Map `json:"maps"`
}

// Purge names a principal whose map is gone.
type Purge struct {
	Principal string `json:"principal"`
}

// Ack is a gateway confirming it holds a principal at a version.
type Ack struct {
	Principal string `json:"principal"`
	Version   int64  `json:"version"`
}

// Record is one connection a gateway decided on: what was asked for, what was
// decided, and how much moved. It carries no header, no body, no value, no
// placeholder and no credential, which is what makes it safe to keep, serve
// and deliver to a sink.
type Record struct {
	Principal string    `json:"principal"`
	At        time.Time `json:"at"`
	Door      string    `json:"door"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Decision  string    `json:"decision"`
	// Reason says which rule decided, for a decision that was not allowed.
	Reason string `json:"reason,omitempty"`
	// Substituted names the secrets whose values replaced a placeholder on
	// this connection. Names, never values.
	Substituted []string `json:"substituted,omitempty"`
	// Method, Path and Status are present only where the gateway saw them:
	// on the reverse door and on a terminated connection. Path carries no
	// query string, because a query string carries values.
	Method     string `json:"method,omitempty"`
	Path       string `json:"path,omitempty"`
	Status     int    `json:"status,omitempty"`
	BytesOut   int64  `json:"bytesOut,omitempty"`
	BytesIn    int64  `json:"bytesIn,omitempty"`
	DurationMS int64  `json:"durationMs,omitempty"`
}

// maxRecordPathBytes bounds the path a record carries, so a caller cannot
// grow the control plane's memory one request at a time.
const maxRecordPathBytes = 512

// ErrRecordPrincipal is a record that names no sandbox.
var ErrRecordPrincipal = errors.New("egress: a record names no sandbox")

// Normalize holds a record to what a record may carry, at the boundary where
// it enters the control plane. The gateway is trusted to send no secret and
// this is the second lock on the same door: the query string goes, a path is
// cut to its bound, and a field of the placeholder shape is dropped rather
// than stored.
func (r *Record) Normalize() error {
	if SandboxOf(r.Principal) == "" {
		return ErrRecordPrincipal
	}
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	r.At = r.At.UTC()
	if path, _, ok := strings.Cut(r.Path, "?"); ok {
		r.Path = path
	}
	if len(r.Path) > maxRecordPathBytes {
		r.Path = r.Path[:maxRecordPathBytes]
	}
	if IsPlaceholder(r.Path) || IsPlaceholder(r.Host) {
		return errors.New("egress: a record carries a placeholder")
	}
	if slices.ContainsFunc(r.Substituted, IsPlaceholder) {
		return errors.New("egress: a record names a placeholder instead of a secret")
	}
	return nil
}

// Use is one secret substituted into one sandbox's request: the sandbox's
// principal, the Secret's id, and the instant of the substitution. Like a
// record it carries no value, no placeholder and no credential, and it carries
// no host and no path either: the control plane needs to know that a secret is
// used, not where it went.
type Use struct {
	Principal string    `json:"principal"`
	Secret    string    `json:"secret"`
	At        time.Time `json:"at"`
}

// ErrUse is a use the control plane will not write: one that names no
// sandbox, or no secret by its id.
var ErrUse = errors.New("egress: a use names no sandbox or no secret id")

// Normalize holds a use to what one may carry, where it enters the control
// plane: a sandbox's principal, a Secret's id, and a time in UTC.
func (u *Use) Normalize() error {
	if SandboxOf(u.Principal) == "" || !strings.HasPrefix(u.Secret, v1.SecretIDPrefix) {
		return ErrUse
	}
	u.At = u.At.UTC()
	return nil
}

// Encode writes one frame as one JSON text message. The protocol is one
// frame per message, so a reader never has to find a boundary inside one.
func Encode(f Frame) ([]byte, error) { return json.Marshal(f) }

// Decode reads one frame from one message and refuses a type the protocol
// does not have, so an unknown frame is a refusal rather than a silent skip.
func Decode(message []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(message, &f); err != nil {
		return Frame{}, err
	}
	switch f.Type {
	case FrameHello, FrameSnapshot, FramePut, FramePurge, FrameAck, FrameHeartbeat, FrameRecord, FrameUse:
		return f, nil
	default:
		return Frame{}, errors.New("egress: unknown frame type " + f.Type)
	}
}
