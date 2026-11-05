// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"fmt"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/remote"
)

// jsonBenchStates builds n sandbox states the way a worker's driver reports
// them: labels, ports, conditions and the timestamps.
func jsonBenchStates(n int) []runtime.State {
	base := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	exit := 0
	out := make([]runtime.State, n)
	for i := range out {
		at := base.Add(time.Duration(i) * time.Minute)
		out[i] = runtime.State{
			ID:        fmt.Sprintf("sbx_%026d", i),
			Name:      fmt.Sprintf("build-%04d", i),
			Owner:     "user:7f3c1e52-9a44-4d0b-8e61-2b5d3c9a0f18",
			Phase:     "Running",
			Isolation: "container",
			ExitCode:  &exit,
			Labels: map[string]string{
				"cella.latere.ai/owner": "7f3c1e52-9a44-4d0b-8e61-2b5d3c9a0f18",
				"app":                   "ci-runner",
				"team":                  "platform",
			},
			Ports: []runtime.PortState{{Name: "web", Port: 8080, State: "Open"}},
			Conditions: []v1.Condition{
				{Type: "Ready", Status: "True", Reason: "Running", Since: at},
				{Type: "WorkspaceReady", Status: "True", Reason: "Mounted", Since: at},
			},
			CreatedAt:      at,
			StartedAt:      at.Add(2 * time.Second),
			LastActivityAt: at.Add(40 * time.Second),
			Resources:      runtime.Resources{CPU: "2", Memory: "4Gi", Disk: "20Gi"},
		}
	}
	return out
}

// jsonBenchSizes are the observed-state report sizes: a worker with one
// sandbox, a busy one, and a relist of a whole environment.
var jsonBenchSizes = []struct {
	name string
	n    int
}{{"small", 1}, {"medium", 20}, {"large", 400}}

func BenchmarkJSONStateMessageEncode(b *testing.B) {
	for _, size := range jsonBenchSizes {
		b.Run(size.name, func(b *testing.B) {
			m := remote.Message{Type: remote.MessageState, States: jsonBenchStates(size.n)}
			frame, err := remote.EncodeMessage(remote.NoOperation, m)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(frame)))
			for b.Loop() {
				if _, err := remote.EncodeMessage(remote.NoOperation, m); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkJSONStateMessageDecode(b *testing.B) {
	for _, size := range jsonBenchSizes {
		b.Run(size.name, func(b *testing.B) {
			frame, err := remote.EncodeMessage(remote.NoOperation, remote.Message{Type: remote.MessageState, States: jsonBenchStates(size.n)})
			if err != nil {
				b.Fatal(err)
			}
			_, _, payload, err := remote.DecodeFrame(frame)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for b.Loop() {
				if _, err := remote.DecodeMessage(payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// jsonBenchOperation is one Exec call going down to a worker.
func jsonBenchOperation() remote.Message {
	return remote.Message{
		Type:      remote.MessageOperation,
		Operation: remote.OpExec,
		Request: &remote.Request{
			ID: "sbx_00000000000000000000000042",
			Exec: &remote.ExecRequest{
				Command: []string{"/bin/sh", "-c", "cd /workspace && go test ./... -run TestIntegration -count=1"},
				Env: map[string]string{
					"CI":      "true",
					"GOFLAGS": "-mod=readonly",
					"HOME":    "/home/agent",
					"PATH":    "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin",
					"TERM":    "xterm-256color",
				},
				Workdir: "/workspace",
				TTY:     true,
				Stdin:   true,
				Timeout: 10 * time.Minute,
			},
		},
	}
}

func BenchmarkJSONOperationMessageEncode(b *testing.B) {
	m := jsonBenchOperation()
	op := remote.NewOperationID()
	frame, err := remote.EncodeMessage(op, m)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	for b.Loop() {
		if _, err := remote.EncodeMessage(op, m); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJSONOperationMessageDecode(b *testing.B) {
	frame, err := remote.EncodeMessage(remote.NewOperationID(), jsonBenchOperation())
	if err != nil {
		b.Fatal(err)
	}
	_, _, payload, err := remote.DecodeFrame(frame)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		if _, err := remote.DecodeMessage(payload); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONResultMessageRoundTrip is a worker's answer to a List call
// coming back up and being read by the control plane.
func BenchmarkJSONResultMessageRoundTrip(b *testing.B) {
	m := remote.Message{
		Type:      remote.MessageResult,
		Operation: remote.OpList,
		OK:        true,
		Response:  &remote.Response{States: jsonBenchStates(5)},
	}
	jsonBenchRoundTrip(b, m)
}

// BenchmarkJSONHelloMessageRoundTrip is the worker's hello frame.
func BenchmarkJSONHelloMessageRoundTrip(b *testing.B) {
	jsonBenchRoundTrip(b, remote.Message{
		Type:   remote.MessageHello,
		Worker: "worker-3f9a1c7e5b2d4a60",
		Window: 1 << 20,
		Watch:  true,
	})
}

func jsonBenchRoundTrip(b *testing.B, m remote.Message) {
	b.Helper()
	op := remote.NewOperationID()
	frame, err := remote.EncodeMessage(op, m)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	for b.Loop() {
		raw, err := remote.EncodeMessage(op, m)
		if err != nil {
			b.Fatal(err)
		}
		_, _, payload, err := remote.DecodeFrame(raw)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := remote.DecodeMessage(payload); err != nil {
			b.Fatal(err)
		}
	}
}
