// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// at is the instant every golden record is stamped with.
var at = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

// sandbox is the object every case below acts on: one resolved manifest with
// the labels a plane stamped on it and an environment value that must not
// reach any record.
func sandbox() v1.Sandbox {
	return v1.Sandbox{
		APIVersion: v1.APIVersion,
		Kind:       "Sandbox",
		Metadata: v1.Metadata{
			Name:   "build",
			Labels: map[string]string{"tenant": "acme", "tier": "gold"},
		},
		Spec: v1.SandboxSpec{
			Environment: "default",
			Image:       "registry.example/base:1",
			Command:     []string{"sleep", "infinity"},
			Workdir:     "/workspace",
			Resources:   v1.Resources{CPU: "1", Memory: "2Gi"},
			Workspace:   v1.Workspace{Path: "/workspace"},
			Lifecycle:   v1.Lifecycle{AutoStop: "15m"},
			Env:         map[string]string{"API_TOKEN": "sk-ant-notasecretbutshapedlikeone", "LANG": "C"},
		},
		Status: v1.SandboxStatus{
			ID: "sbx_01k5pqz5", Owner: "https://issuer.example|alice",
			Environment: "default", Phase: "Running",
		},
	}
}

// person and workload are the two actors a record names.
var (
	person   = Actor{Subject: "https://issuer.example|alice", RequestID: "req_7"}
	workload = Actor{Subject: "sandbox:sbx_parent", Workload: "sbx_parent", RequestID: "req_8"}
)

// TestRecordShapes holds one record per type to the shape design 009's table
// names, field for field, so a change to the wire is a change to this file.
func TestRecordShapes(t *testing.T) {
	obj := sandbox()
	stopped := obj
	stopped.Status.Phase, stopped.Status.Reason = "Stopped", "AutoStop"
	for _, tc := range []struct {
		name string
		make func() (Record, error)
		want string
	}{
		{
			name: "created",
			make: func() (Record, error) {
				return Mutation(TypeCreated, "", OfSandbox(obj), CreatedOf(obj), person, at)
			},
			want: `{
				"id":"evt_fixed","seq":0,"type":"sandbox.created","time":"2026-09-19T12:00:00Z",
				"object":{"kind":"Sandbox","id":"sbx_01k5pqz5","name":"build",
					"owner":"https://issuer.example|alice","labels":{"tenant":"acme","tier":"gold"}},
				"subject":"https://issuer.example|alice","requestId":"req_7",
				"data":{"environment":"default","image":"registry.example/base:1",
					"command":["sleep","infinity"],"workdir":"/workspace",
					"resources":{"cpu":"1","memory":"2Gi"},"workspace":{"path":"/workspace"},
					"lifecycle":{"autoStop":"15m"},"env":["API_TOKEN","LANG"],
					"labels":{"tenant":"acme","tier":"gold"}}}`,
		},
		{
			name: "started takes Request from a person",
			make: func() (Record, error) {
				return Mutation(TypeStarted, "", OfSandbox(obj), Phase{Phase: "Running"}, person, at)
			},
			want: `{
				"id":"evt_fixed","seq":0,"type":"sandbox.started","time":"2026-09-19T12:00:00Z",
				"object":{"kind":"Sandbox","id":"sbx_01k5pqz5","name":"build",
					"owner":"https://issuer.example|alice","labels":{"tenant":"acme","tier":"gold"}},
				"subject":"https://issuer.example|alice","requestId":"req_7",
				"reason":"Request","data":{"phase":"Running"}}`,
		},
		{
			name: "stopped by the reaper",
			make: func() (Record, error) {
				return Mutation(TypeStopped, ReasonAutoStop, OfSandbox(stopped),
					Phase{Phase: "Stopped"}, Actor{}, at)
			},
			want: `{
				"id":"evt_fixed","seq":0,"type":"sandbox.stopped","time":"2026-09-19T12:00:00Z",
				"object":{"kind":"Sandbox","id":"sbx_01k5pqz5","name":"build",
					"owner":"https://issuer.example|alice","labels":{"tenant":"acme","tier":"gold"}},
				"subject":"controller","requestId":"",
				"reason":"AutoStop","data":{"phase":"Stopped"}}`,
		},
		{
			name: "lost",
			make: func() (Record, error) {
				return Mutation(TypeLost, ReasonLost, OfSandbox(obj), Phase{Phase: "Lost"}, Actor{}, at)
			},
			want: `{
				"id":"evt_fixed","seq":0,"type":"sandbox.lost","time":"2026-09-19T12:00:00Z",
				"object":{"kind":"Sandbox","id":"sbx_01k5pqz5","name":"build",
					"owner":"https://issuer.example|alice","labels":{"tenant":"acme","tier":"gold"}},
				"subject":"controller","requestId":"","reason":"Lost","data":{"phase":"Lost"}}`,
		},
		{
			name: "exec names no command",
			make: func() (Record, error) {
				return Operation(TypeExec, OfSandbox(obj), Exec{ExitCode: 7, DurationMS: 42}, workload, at)
			},
			want: `{
				"id":"evt_fixed","seq":0,"type":"sandbox.exec","time":"2026-09-19T12:00:00Z",
				"object":{"kind":"Sandbox","id":"sbx_01k5pqz5","name":"build",
					"owner":"https://issuer.example|alice","labels":{"tenant":"acme","tier":"gold"}},
				"sandbox":{"kind":"Sandbox","id":"sbx_01k5pqz5","name":"build",
					"owner":"https://issuer.example|alice","labels":{"tenant":"acme","tier":"gold"}},
				"subject":"sandbox:sbx_parent","workload":{"id":"sbx_parent"},"requestId":"req_8",
				"data":{"exitCode":7,"durationMs":42}}`,
		},
		{
			name: "files names no content",
			make: func() (Record, error) {
				return Operation(TypeFiles, OfSandbox(obj), Files{
					Direction: DirectionExport, Paths: []string{"/workspace/out"}, Bytes: 2048,
				}, person, at)
			},
			want: `{
				"id":"evt_fixed","seq":0,"type":"sandbox.files","time":"2026-09-19T12:00:00Z",
				"object":{"kind":"Sandbox","id":"sbx_01k5pqz5","name":"build",
					"owner":"https://issuer.example|alice","labels":{"tenant":"acme","tier":"gold"}},
				"sandbox":{"kind":"Sandbox","id":"sbx_01k5pqz5","name":"build",
					"owner":"https://issuer.example|alice","labels":{"tenant":"acme","tier":"gold"}},
				"subject":"https://issuer.example|alice","requestId":"req_7",
				"data":{"direction":"export","paths":["/workspace/out"],"bytes":2048}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record, err := tc.make()
			if err != nil {
				t.Fatalf("building the record: %v", err)
			}
			if !strings.HasPrefix(record.ID, IDPrefix) || len(record.ID) != len(IDPrefix)+26 {
				t.Errorf("the id is %q, want the prefix and a 26 character ULID", record.ID)
			}
			record.ID = "evt_fixed"
			body, err := Body(record)
			if err != nil {
				t.Fatalf("encoding the record: %v", err)
			}
			sameJSON(t, body, []byte(tc.want))
			if strings.Contains(string(body), "sk-ant-") {
				t.Errorf("an environment value reached the record: %s", body)
			}
		})
	}
}

// TestRecordCarriesLabelsAndSeqEverywhere holds every type to the two the
// platform's sink reads on every record.
func TestRecordCarriesLabelsAndSeqEverywhere(t *testing.T) {
	obj := sandbox()
	for _, kind := range Types {
		record, err := Mutation(kind, "", OfSandbox(obj), Phase{Phase: "Running"}, person, at)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		record.Seq = 4
		body, err := Body(record)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		var decoded struct {
			Seq    int64 `json:"seq"`
			Object struct {
				Labels map[string]string `json:"labels"`
			} `json:"object"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if decoded.Seq != 4 || decoded.Object.Labels["tenant"] != "acme" {
			t.Errorf("%s: the record reads seq %d, labels %v", kind, decoded.Seq, decoded.Object.Labels)
		}
	}
}

// TestPayloadRebuildsTheSameBody holds the journal's split to its one rule:
// the columns plus the payload are the body the wire carried.
func TestPayloadRebuildsTheSameBody(t *testing.T) {
	record, err := Mutation(TypeStopped, ReasonExpired, OfSandbox(sandbox()),
		Phase{Phase: "Stopped"}, person, at)
	if err != nil {
		t.Fatal(err)
	}
	record.Seq = 11
	want, err := Body(record)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := Payload(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), `"seq":11`) || strings.Contains(string(payload), record.ID) {
		t.Errorf("the payload repeats a column: %s", payload)
	}
	back, err := Rebuild(payload, record.ID, record.Seq, string(record.Type), record.Time)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Body(back)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("the rebuilt body is\n%s\nwant\n%s", got, want)
	}
}

// TestRebuildReportsUnreadableBytes: a row whose payload is not a record is
// an error and not a half-built delivery.
func TestRebuildReportsUnreadableBytes(t *testing.T) {
	if _, err := Rebuild([]byte("{"), "evt_a", 1, "sandbox.created", at); err == nil {
		t.Fatal("a broken payload rebuilt into a record")
	}
	back, err := Rebuild(nil, "evt_a", 1, "sandbox.created", at)
	if err != nil || back.Type != TypeCreated || back.Seq != 1 {
		t.Fatalf("an empty payload rebuilt to %+v: %v", back, err)
	}
}

// TestReasonOfHoldsTheEnum: a driver names failures design 009's enum does
// not, and DriverFailed is the value the enum has for one.
func TestReasonOfHoldsTheEnum(t *testing.T) {
	for _, tc := range []struct {
		status string
		kind   Type
		want   Reason
	}{
		{"", TypeStopped, ""},
		{"AutoStop", TypeStopped, ReasonAutoStop},
		{"Exited", TypeStopped, ReasonExited},
		{"ProcessUnrecoverable", TypeFailed, ReasonDriverFailed},
		{"ProcessUnrecoverable", TypeRecovering, ""},
	} {
		if got := ReasonOf(tc.status, tc.kind); got != tc.want {
			t.Errorf("ReasonOf(%q, %s) = %q, want %q", tc.status, tc.kind, got, tc.want)
		}
	}
}

// TestBuildRefusesAReasonOutsideTheEnum: the enum is closed, so a caller that
// invents a value fails here rather than at the sink.
func TestBuildRefusesAReasonOutsideTheEnum(t *testing.T) {
	if _, err := Mutation(TypeStopped, "Whenever", OfSandbox(sandbox()), nil, person, at); err == nil {
		t.Fatal("a reason outside the enum built a record")
	}
	if _, err := Mutation(TypeStopped, ReasonAutoStop, Object{}, nil, person, at); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("a record with no object is %v, want ErrIncomplete", err)
	}
}

// TestRedactionCatchesACredentialInADmittedField: the structural rule is that
// no data shape holds content, and redaction is the line behind it for a
// field that does take free text.
func TestRedactionCatchesACredentialInAnAdmittedField(t *testing.T) {
	obj := sandbox()
	obj.Spec.Command = []string{"sh", "-c", "curl -H 'Authorization: bearer ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'"}
	record, err := Mutation(TypeCreated, "", OfSandbox(obj), CreatedOf(obj), person, at)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(record.Data), "ghp_") {
		t.Errorf("a credential survived redaction: %s", record.Data)
	}
}

// TestDeliverableIsTheClosedSet: the two acts design 010 journals and design
// 009 does not name are not delivered.
func TestDeliverableIsTheClosedSet(t *testing.T) {
	for _, kind := range Types {
		if !Deliverable(kind) {
			t.Errorf("%s is in the table and not deliverable", kind)
		}
	}
	for _, kind := range []Type{"sandbox.deleting", "sandbox.status", "sandbox.saved"} {
		if Deliverable(kind) {
			t.Errorf("%s is journaled and must not be delivered", kind)
		}
	}
}

// TestActorFromNamesTheControlPlane: a loop that runs under no request is the
// control plane acting on its own.
func TestActorFromNamesTheControlPlane(t *testing.T) {
	if got := ActorFrom(t.Context()); got.Subject != SubjectController {
		t.Errorf("an actorless context names %q", got.Subject)
	}
	ctx := WithActor(t.Context(), person)
	if got := ActorFrom(ctx); got != person {
		t.Errorf("the actor reads back %+v", got)
	}
}

// TestNewIDSortsByTime: two ids made a millisecond apart order the way they
// were made.
func TestNewIDSortsByTime(t *testing.T) {
	first := NewID()
	time.Sleep(2 * time.Millisecond)
	second := NewID()
	if first >= second {
		t.Errorf("%s was made before %s and does not sort before it", first, second)
	}
}

// sameJSON compares two documents as values, so a field order is not a
// failure and a changed field is.
func sameJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("the record is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("the golden is not JSON: %v", err)
	}
	gs, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(gs) != string(ws) {
		t.Errorf("the record is\n%s\nwant\n%s", gs, ws)
	}
}
