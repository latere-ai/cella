// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// jsonBenchSandbox is a manifest as a client applies it: a build sandbox with
// labels, a workspace and n environment variables of realistic width.
func jsonBenchSandbox(n int) v1.Sandbox {
	env := make(map[string]string, n)
	for i := range n {
		env[fmt.Sprintf("APP_SETTING_%05d", i)] = fmt.Sprintf("value-%05d-%s", i, strings.Repeat("x", 24))
	}
	return v1.Sandbox{
		APIVersion: v1.APIVersion,
		Kind:       v1.KindSandbox,
		Metadata: v1.Metadata{
			Name:   "ci-build-0042",
			Labels: map[string]string{"app": "ci-runner", "team": "platform", "tier": "build"},
		},
		Spec: v1.SandboxSpec{
			Image:     "registry.example.com/acme/builder:1.27",
			Command:   []string{"/bin/sh"},
			Args:      []string{"-c", "go build ./... && go test ./..."},
			Workdir:   "/workspace",
			Resources: v1.Resources{CPU: "2", Memory: "4Gi", Disk: "20Gi"},
			Env:       env,
		},
	}
}

// jsonBenchManifestSizes are the apply body sizes: a typical manifest, one
// with a large environment, and an upper bound with thousands of variables.
var jsonBenchManifestSizes = []struct {
	name string
	n    int
}{{"small", 4}, {"medium", 250}, {"large", 4500}}

func BenchmarkJSONSandboxManifestDecode(b *testing.B) {
	for _, size := range jsonBenchManifestSizes {
		b.Run(size.name, func(b *testing.B) {
			body, err := json.Marshal(jsonBenchSandbox(size.n))
			if err != nil {
				b.Fatal(err)
			}
			if _, err := Decode(body, MediaJSON); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				if _, err := Decode(body, MediaJSON); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONSandboxManifestYAMLDecode is the same apply sent as YAML,
// which the decoder converts to JSON before reading it.
func BenchmarkJSONSandboxManifestYAMLDecode(b *testing.B) {
	for _, size := range jsonBenchManifestSizes[:2] {
		b.Run(size.name, func(b *testing.B) {
			obj := jsonBenchSandbox(size.n)
			var y strings.Builder
			y.WriteString("apiVersion: " + obj.APIVersion + "\nkind: Sandbox\nmetadata:\n  name: ci-build-0042\n  labels:\n    app: ci-runner\n    team: platform\n    tier: build\nspec:\n")
			y.WriteString("  image: registry.example.com/acme/builder:1.27\n  command: [/bin/sh]\n  args: [\"-c\", \"go build ./... && go test ./...\"]\n  workdir: /workspace\n  resources:\n    cpu: \"2\"\n    memory: 4Gi\n    disk: 20Gi\n  env:\n")
			for i := range size.n {
				fmt.Fprintf(&y, "    APP_SETTING_%05d: value-%05d-%s\n", i, i, strings.Repeat("x", 24))
			}
			body := []byte(y.String())
			if _, err := Decode(body, MediaYAML); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				if _, err := Decode(body, MediaYAML); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONEnvironmentManifestDecode is an operator applying an
// environment, which shares the sandbox decoder's version, kind and unknown
// field checks.
func BenchmarkJSONEnvironmentManifestDecode(b *testing.B) {
	body, err := json.Marshal(v1.Environment{
		APIVersion: v1.APIVersion,
		Kind:       v1.KindEnvironment,
		Metadata:   v1.Metadata{Name: "fra1-workers", Labels: map[string]string{"region": "fra1"}},
		Spec:       v1.EnvironmentSpec{Mode: "workers"},
	})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := DecodeEnvironment(body, MediaJSON); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		if _, err := DecodeEnvironment(body, MediaJSON); err != nil {
			b.Fatal(err)
		}
	}
}
