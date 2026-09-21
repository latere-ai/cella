// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// updateCorpus rewrites the golden outputs instead of comparing against them.
// A run that changes one is a schema change, which design 003 says needs its
// own CHANGELOG line.
var updateCorpus = flag.Bool("update", false, "rewrite the golden outputs of manifest/testdata/v1")

// corpusOptions are the fixed options every golden output is resolved under:
// one container environment, the operator's six defaults, a clock that does
// not move, and a name generator that answers the same name twice. Nothing
// here reads a clock or a random source, so a golden file is a fact about the
// schema and not about the run.
func corpusOptions() Options {
	// The environment declares every optional behaviour the corpus asks for,
	// so a row is measured against the schema and not against one driver's
	// capability set.
	env := container("default")
	env.Status.Capabilities = v1.Capabilities{
		Egress: []v1.EgressMode{v1.EgressOpen, v1.EgressAllowlist, v1.EgressNone},
		Mesh:   true, Attach: true, Display: true, Input: true, Resize: true, Files: true,
	}
	o := environmentOptions(env)
	o.Defaults = Defaults{CPU: "1", Memory: "2Gi", Disk: "10Gi", AutoStop: "15m", TTL: "24h", AutoDelete: "72h", Image: fixtureImage}
	o.Now = func() time.Time { return time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC) }
	o.NewName = func() string { return "brave-otter-1a2b" }
	return o
}

// TestGoldenCorpus is design 003's corpus rule: every manifest under
// testdata/v1 decodes from YAML and from its JSON form to one object, and
// resolves under fixed options to the golden output beside it. A change that
// moves a golden file is a schema change.
func TestGoldenCorpus(t *testing.T) {
	rows, err := filepath.Glob(filepath.Join("testdata", "v1", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("the corpus is empty, so this test would pass vacuously")
	}
	for _, row := range rows {
		name := strings.TrimSuffix(filepath.Base(row), ".yaml")
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(row)
			if err != nil {
				t.Fatal(err)
			}
			fromYAML, err := Decode(body, MediaYAML)
			if err != nil {
				t.Fatalf("the YAML row was refused: %v", err)
			}
			// The JSON form of the row is written beside it, so the corpus
			// carries both syntaxes and a reader sees what the rule means.
			asJSON, err := json.MarshalIndent(fromYAML, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			asJSON = append(asJSON, '\n')
			jsonPath := filepath.Join("testdata", "v1", name+".json")
			if *updateCorpus {
				if err := os.WriteFile(jsonPath, asJSON, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			held, err := os.ReadFile(jsonPath)
			if err != nil {
				t.Fatalf("the row has no JSON form: %v", err)
			}
			fromJSON, err := Decode(held, MediaJSON)
			if err != nil {
				t.Fatalf("the JSON row was refused: %v", err)
			}
			left, err := json.Marshal(fromYAML)
			if err != nil {
				t.Fatal(err)
			}
			right, err := json.Marshal(fromJSON)
			if err != nil {
				t.Fatal(err)
			}
			if string(left) != string(right) {
				t.Fatalf("the two syntaxes decode to two objects:\n%s\n%s", left, right)
			}
			resolved, err := Resolve(t.Context(), &fromYAML, corpusOptions())
			if err != nil {
				t.Fatalf("the row does not resolve: %v", err)
			}
			out, err := json.MarshalIndent(resolved.Sandbox, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, '\n')
			goldenPath := filepath.Join("testdata", "v1", name+".golden.json")
			if *updateCorpus {
				if err := os.WriteFile(goldenPath, out, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			golden, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("the row has no golden output: %v", err)
			}
			if string(out) != string(golden) {
				t.Fatalf("the row resolves to\n%s\nand the golden output holds\n%s", out, golden)
			}
			if resolved.Sandbox.Kind != v1.KindSandbox {
				t.Fatalf("the resolved object is a %s", resolved.Sandbox.Kind)
			}
		})
	}
}
