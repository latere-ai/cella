// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// jsonBenchSandbox is one listed sandbox as the control plane answers it.
func jsonBenchSandbox(i int) v1.Sandbox {
	at := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
	exit := 0
	return v1.Sandbox{
		APIVersion: v1.APIVersion,
		Kind:       v1.KindSandbox,
		Metadata: v1.Metadata{
			Name:   fmt.Sprintf("ci-build-%04d", i),
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
		Status: v1.SandboxStatus{
			ID:             fmt.Sprintf("sbx_%026d", i),
			Owner:          "7f3c1e52-9a44-4d0b-8e61-2b5d3c9a0f18",
			Environment:    "fra1-workers",
			Driver:         "podman",
			Isolation:      "container",
			Phase:          "Running",
			Conditions:     []v1.Condition{{Type: "Ready", Status: "True", Reason: "Running", Since: at}},
			CreatedAt:      at,
			StartedAt:      at.Add(2 * time.Second),
			LastActivityAt: at.Add(40 * time.Second),
			ExitCode:       &exit,
		},
	}
}

// BenchmarkJSONSandboxListEncode is GET /v1/sandboxes: one page of objects
// written through the handler's negotiating responder.
func BenchmarkJSONSandboxListEncode(b *testing.B) {
	for _, size := range []struct {
		name string
		n    int
	}{{"small", 1}, {"medium", 15}, {"large", 300}} {
		b.Run(size.name, func(b *testing.B) {
			items := make([]v1.Sandbox, size.n)
			for i := range items {
				items[i] = jsonBenchSandbox(i)
			}
			page := map[string]any{"items": items, "next": "c2J4XzAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDMw"}
			probe := httptest.NewRecorder()
			respond(probe, http.StatusOK, page)
			b.ReportAllocs()
			b.SetBytes(int64(probe.Body.Len()))
			for b.Loop() {
				w := httptest.NewRecorder()
				respond(w, http.StatusOK, page)
				if w.Code != http.StatusOK {
					b.Fatal(w.Code)
				}
			}
		})
	}
}

// BenchmarkJSONSandboxItemEncode is GET /v1/sandboxes/{id}.
func BenchmarkJSONSandboxItemEncode(b *testing.B) {
	obj := jsonBenchSandbox(7)
	probe := httptest.NewRecorder()
	respond(probe, http.StatusOK, obj)
	b.ReportAllocs()
	b.SetBytes(int64(probe.Body.Len()))
	for b.Loop() {
		w := httptest.NewRecorder()
		respond(w, http.StatusOK, obj)
		if w.Code != http.StatusOK {
			b.Fatal(w.Code)
		}
	}
}

// BenchmarkJSONExecResultEncode is the answer to POST .../exec?wait=1 with
// the output a build prints.
func BenchmarkJSONExecResultEncode(b *testing.B) {
	line := "ok  \tlatere.ai/x/cella/internal/api\t1.284s\tcoverage: 81.2% of statements\n"
	for _, size := range []struct {
		name  string
		lines int
	}{{"small", 6}, {"medium", 130}, {"large", 2600}} {
		b.Run(size.name, func(b *testing.B) {
			res := execResult{
				ExitCode:   0,
				Stdout:     strings.Repeat(line, size.lines),
				Stderr:     "warning: \"GOFLAGS\" overrides the module cache\n",
				DurationMS: 8421,
			}
			probe := httptest.NewRecorder()
			respond(probe, http.StatusOK, res)
			b.ReportAllocs()
			b.SetBytes(int64(probe.Body.Len()))
			for b.Loop() {
				w := httptest.NewRecorder()
				respond(w, http.StatusOK, res)
				if w.Code != http.StatusOK {
					b.Fatal(w.Code)
				}
			}
		})
	}
}

// BenchmarkJSONExecRequestDecode is the body of POST .../exec: one strict
// object, read through the handler's own decoder and validation.
func BenchmarkJSONExecRequestDecode(b *testing.B) {
	h := &handler{MaxBodyBytes: 65536}
	obj := jsonBenchSandbox(7)
	body := `{"command":["/bin/sh","-c","cd /workspace && go test ./... -run TestIntegration -count=1"],` +
		`"env":{"CI":"true","GOFLAGS":"-mod=readonly","TERM":"xterm-256color"},"workdir":"/workspace","timeout":"10m"}`
	run := func() error {
		r := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+obj.Status.ID+"/exec", strings.NewReader(body))
		w := httptest.NewRecorder()
		if _, _, ok := h.execRequest(w, r, obj); !ok {
			return fmt.Errorf("exec request refused: %d %s", w.Code, w.Body.String())
		}
		return nil
	}
	if err := run(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		if err := run(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONErrorEnvelopeEncode is every refusal the API writes.
func BenchmarkJSONErrorEnvelopeEncode(b *testing.B) {
	err := fmt.Errorf("sandbox not found")
	probe := httptest.NewRecorder()
	respondError(probe, err)
	b.ReportAllocs()
	b.SetBytes(int64(probe.Body.Len()))
	for b.Loop() {
		w := httptest.NewRecorder()
		respondError(w, err)
		if w.Code == http.StatusOK {
			b.Fatal(w.Code)
		}
	}
}
