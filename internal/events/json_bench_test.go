// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"testing"
	"time"

	"latere.ai/x/cella/internal/events"
	v1 "latere.ai/x/cella/manifest/v1"
)

// jsonBenchRecords builds the two records a sandbox mostly produces: the
// created record with its manifest summary, and an exec operation record.
func jsonBenchRecords(b *testing.B) (created, exec events.Record) {
	b.Helper()
	at := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	obj := v1.Sandbox{
		APIVersion: v1.APIVersion,
		Kind:       v1.KindSandbox,
		Metadata: v1.Metadata{
			Name:   "ci-build-0042",
			Labels: map[string]string{"app": "ci-runner", "team": "platform", "tier": "build"},
		},
		Spec: v1.SandboxSpec{
			Environment: "fra1-workers",
			Image:       "registry.example.com/acme/builder:1.27",
			Command:     []string{"/bin/sh"},
			Args:        []string{"-c", "go build ./... && go test ./..."},
			Workdir:     "/workspace",
			Resources:   v1.Resources{CPU: "2", Memory: "4Gi", Disk: "20Gi"},
			Env:         map[string]string{"CI": "true", "GOFLAGS": "-mod=readonly", "HOME": "/home/agent"},
		},
		Status: v1.SandboxStatus{ID: "sbx_00000000000000000000000042", Owner: "7f3c1e52-9a44-4d0b-8e61-2b5d3c9a0f18"},
	}
	actor := events.Actor{Subject: "user:7f3c1e52-9a44-4d0b-8e61-2b5d3c9a0f18", RequestID: "req_8c1d5e9f2a7b4c36"}
	var err error
	created, err = events.Mutation(events.TypeCreated, "", events.OfSandbox(obj), events.CreatedOf(obj), actor, at)
	if err != nil {
		b.Fatal(err)
	}
	exec, err = events.Operation(events.TypeExec, events.OfSandbox(obj), events.Exec{ExitCode: 0, DurationMS: 8421}, actor, at.Add(time.Minute))
	if err != nil {
		b.Fatal(err)
	}
	return created, exec
}

// BenchmarkJSONRecordBodyMarshal is the delivery path: the exact bytes the
// feed, the journal and the signed webhook share.
func BenchmarkJSONRecordBodyMarshal(b *testing.B) {
	created, exec := jsonBenchRecords(b)
	for _, tc := range []struct {
		name string
		rec  events.Record
	}{{"created", created}, {"exec", exec}} {
		b.Run(tc.name, func(b *testing.B) {
			body, err := events.Body(tc.rec)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				if _, err := events.Body(tc.rec); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONRecordPayloadMarshal is the journal write: the half of a
// record stored outside the row's columns.
func BenchmarkJSONRecordPayloadMarshal(b *testing.B) {
	created, _ := jsonBenchRecords(b)
	payload, err := events.Payload(created)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		if _, err := events.Payload(created); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONRecordRebuildUnmarshal is the journal read that feeds the
// follow stream and redelivery.
func BenchmarkJSONRecordRebuildUnmarshal(b *testing.B) {
	created, _ := jsonBenchRecords(b)
	payload, err := events.Payload(created)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		if _, err := events.Rebuild(payload, created.ID, 17, string(created.Type), created.Time); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONRecordBuildMarshal is the emit path: a mutation record built
// with its data encoded and redacted.
func BenchmarkJSONRecordBuildMarshal(b *testing.B) {
	at := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	obj := events.Object{Kind: events.KindSandbox, ID: "sbx_00000000000000000000000042", Name: "ci-build-0042", Owner: "7f3c1e52-9a44-4d0b-8e61-2b5d3c9a0f18", Labels: map[string]string{"app": "ci-runner"}}
	actor := events.Actor{Subject: "user:7f3c1e52-9a44-4d0b-8e61-2b5d3c9a0f18", RequestID: "req_8c1d5e9f2a7b4c36"}
	rec, err := events.Operation(events.TypeExec, obj, events.Exec{ExitCode: 0, DurationMS: 8421}, actor, at)
	if err != nil {
		b.Fatal(err)
	}
	body, err := events.Body(rec)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		if _, err := events.Operation(events.TypeExec, obj, events.Exec{ExitCode: 0, DurationMS: 8421}, actor, at); err != nil {
			b.Fatal(err)
		}
	}
}
