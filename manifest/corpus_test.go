// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
)

// update rewrites the golden files instead of comparing against them. It is
// off by default, so a change in resolved output fails the run rather than
// rewriting the record of what the schema was.
var update = flag.Bool("update", false, "rewrite the golden files of the corpus")

// The corpus lives under one directory per outcome: a manifest under valid/
// resolves and its golden is the resolved object, a manifest under invalid/
// is refused and its golden is the refusal.
const (
	corpusValid   = "testdata/v1/valid"
	corpusInvalid = "testdata/v1/invalid"
)

// corpusNow is the clock every corpus resolve reads, so a deadline computed
// from a ttl is the same byte sequence on every run.
var corpusNow = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// corpusEnvironment is the one environment the corpus resolves against: a
// container that declares every capability, so a field is never refused for
// want of a capability and the corpus reads the schema rather than one
// driver's reach.
func corpusEnvironment() v1.Environment {
	env := NativeEnvironment("default")
	env.Spec.Isolation = v1.IsolationContainer
	// The queued mode reads every scheduling field, where a direct
	// environment refuses them all, so the corpus can set each one.
	env.Spec.Scheduling = v1.SchedulingSpec{
		Mode: v1.SchedulingQueued, Queues: []string{"default", "batch"}, DefaultQueue: "default",
	}
	env.Status = v1.EnvironmentStatus{
		Driver:    "k8s",
		Isolation: v1.IsolationContainer,
		Capabilities: v1.Capabilities{
			Egress:    []v1.EgressMode{v1.EgressNone, v1.EgressAllowlist, v1.EgressOpen},
			Mesh:      true,
			Ingress:   true,
			Volumes:   true,
			Snapshots: true,
			Attach:    true,
			Dial:      true,
			Display:   true,
			Input:     true,
			Resize:    true,
			Pool:      true,
			Files:     true,
			Detach:    true,
		},
	}
	return env
}

// corpusSecrets are the two Secrets the corpus may mount, fixed like every
// other input. Two scope one host between them, which is the conflict one
// invalid entry earns.
func corpusSecrets() map[string]v1.Secret {
	secret := func(name, id, host string) v1.Secret {
		return v1.Secret{
			APIVersion: v1.APIVersion,
			Kind:       v1.KindSecret,
			Metadata:   v1.Metadata{Name: name},
			Spec: v1.SecretSpec{
				Scope: v1.SecretScope{Hosts: []string{host}},
				Kind:  v1.SecretStatic,
			},
			Status: v1.SecretStatus{ID: id, Owner: "https://issuer.example.com|alice", Version: 1},
		}
	}
	return map[string]v1.Secret{
		"vendor":  secret("vendor", "sec_01J0000000000000000000VEND", "api.vendor.example"),
		"partner": secret("partner", "sec_01J0000000000000000000PART", "api.vendor.example"),
	}
}

// corpusOptions is the whole of what the corpus resolves under. Nothing here
// varies per entry: an entry that needs a parent, an update or an admission
// step is a unit test, because the corpus is the schema's snapshot and those
// inputs are the caller's state.
func corpusOptions() Options {
	secrets := corpusSecrets()
	return Options{
		Actor: Actor{
			Subject: "https://issuer.example.com|alice",
			Issuer:  "https://issuer.example.com",
			Sub:     "alice",
		},
		Lookup: WithSecrets(FixedEnvironment(corpusEnvironment()),
			func(_ context.Context, nameOrID string) (*v1.Secret, error) {
				for _, secret := range secrets {
					if nameOrID == secret.Metadata.Name || nameOrID == secret.Status.ID {
						found := secret
						return &found, nil
					}
				}
				return nil, ErrNotFound
			}),
		Defaults: Defaults{
			CPU: "1", Memory: "2Gi", Disk: "10Gi",
			AutoStop: "15m", TTL: "24h", AutoDelete: "72h",
			Image: "registry.example/base:1",
		},
		Ceilings: Ceilings{CPU: "8", Memory: "32Gi", Disk: "100Gi", TTL: "168h"},
		Now:      func() time.Time { return corpusNow },
		NewName:  func() string { return "generated-name" },
	}
}

// corpusResolved is an accepted entry's golden: the resolved object, the
// secrets it mounts as the lookup answered them, and the warnings the
// environment returned, under names a reader of the file recognizes.
type corpusResolved struct {
	Sandbox  v1.Sandbox  `json:"sandbox"`
	Secrets  []v1.Secret `json:"secrets,omitempty"`
	Warnings []string    `json:"warnings,omitempty"`
}

// corpusRefusal is an invalid entry's golden: the code and the paths of the
// refusal and not its developer detail, which is wording and not contract.
type corpusRefusal struct {
	Code  string   `json:"code"`
	Path  string   `json:"path,omitempty"`
	Paths []string `json:"paths,omitempty"`
}

// corpusEntries is every input of one corpus directory, by base name.
func corpusEntries(t *testing.T, dir string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inputs []string
	for _, name := range names {
		if !strings.HasSuffix(name, ".golden.json") {
			inputs = append(inputs, name)
		}
	}
	if len(inputs) == 0 {
		t.Fatalf("%s holds no manifest, so the corpus proves nothing", dir)
	}
	slices.Sort(inputs)
	return inputs
}

// compareGolden holds one encoded outcome to its golden file, or writes the
// file when -update is set.
func compareGolden(t *testing.T, input string, got []byte) {
	t.Helper()
	path := strings.TrimSuffix(input, ".json") + ".golden.json"
	got = append(got, '\n')
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the golden of %s: %v; run go test ./manifest -run TestGoldenCorpus -update to write it", input, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s resolves to\n%s\nand its golden holds\n%s", input, got, want)
	}
}

// yamlTwinAgrees holds the YAML form of an accepted entry, where one is
// written beside it, to the object its JSON form decodes to: the two syntaxes
// carry one object, and an anchor or an alias is the document's and not the
// object's.
func yamlTwinAgrees(t *testing.T, input string, fromJSON v1.Sandbox) {
	t.Helper()
	body, err := os.ReadFile(strings.TrimSuffix(input, ".json") + ".yaml")
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	fromYAML, err := Decode(body, MediaYAML)
	if err != nil {
		t.Fatalf("the YAML form was refused: %v", err)
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
}

// TestGoldenCorpus is the schema's snapshot of spec 003: every manifest under
// testdata/v1/valid resolves to its golden object under one fixed set of
// options, and decodes from its YAML form to the same object where one is
// written beside it; every manifest under testdata/v1/invalid is refused with
// the code and the paths its golden names. A change that alters a golden file
// is a schema change.
func TestGoldenCorpus(t *testing.T) {
	// A YAML form without its JSON form would never be read, so the corpus
	// refuses one rather than carry a row that proves nothing.
	twins, err := filepath.Glob(filepath.Join(corpusValid, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(twins) == 0 {
		t.Fatalf("%s holds no YAML form, so the corpus reads one syntax", corpusValid)
	}
	for _, twin := range twins {
		if _, err := os.Stat(strings.TrimSuffix(twin, ".yaml") + ".json"); err != nil {
			t.Errorf("%s has no JSON form: %v", twin, err)
		}
	}
	for _, input := range corpusEntries(t, corpusValid) {
		t.Run(filepath.Base(input), func(t *testing.T) {
			body, err := os.ReadFile(input)
			if err != nil {
				t.Fatal(err)
			}
			obj, err := Decode(body, MediaJSON)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			yamlTwinAgrees(t, input, obj)
			resolved, err := Resolve(t.Context(), &obj, corpusOptions())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			encoded, err := json.MarshalIndent(corpusResolved{
				Sandbox: resolved.Sandbox, Secrets: resolved.Secrets, Warnings: resolved.Warnings,
			}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			compareGolden(t, input, encoded)
		})
	}
	for _, input := range corpusEntries(t, corpusInvalid) {
		t.Run(filepath.Base(input), func(t *testing.T) {
			body, err := os.ReadFile(input)
			if err != nil {
				t.Fatal(err)
			}
			obj, decodeErr := Decode(body, MediaJSON)
			err = decodeErr
			if err == nil {
				_, err = Resolve(t.Context(), &obj, corpusOptions())
			}
			var known *Error
			if err == nil {
				t.Fatalf("%s resolved; the corpus holds it as a refusal", input)
			} else if !errors.As(err, &known) {
				t.Fatalf("%s failed with %v, which is not a contract error", input, err)
			}
			encoded, err := json.MarshalIndent(corpusRefusal{Code: known.Code, Path: known.Path, Paths: known.Paths}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			compareGolden(t, input, encoded)
		})
	}
}

// TestCorpusCoversTheSchema holds the corpus to its purpose. Every field of
// the Sandbox schema is set by some accepted manifest, so a field added
// without a corpus entry fails here, and every code an invalid entry earns is
// a code this package emits, so a golden cannot record a refusal the contract
// does not have.
func TestCorpusCoversTheSchema(t *testing.T) {
	set := map[string]bool{}
	for _, input := range corpusEntries(t, corpusValid) {
		body, err := os.ReadFile(input)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(body, &document); err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		collectPaths(document, "", set)
	}
	for _, path := range schemaPaths(reflect.TypeFor[v1.Sandbox](), "") {
		if !set[path] {
			t.Errorf("no accepted manifest of the corpus sets %s", path)
		}
	}
	emitted := emittedCodes(t)
	for _, input := range corpusEntries(t, corpusInvalid) {
		body, err := os.ReadFile(strings.TrimSuffix(input, ".json") + ".golden.json")
		if err != nil {
			t.Fatal(err)
		}
		var refusal corpusRefusal
		if err := json.Unmarshal(body, &refusal); err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if !emitted[refusal.Code] {
			t.Errorf("%s earns %q, which no refusal of this package emits", input, refusal.Code)
		}
	}
}

// schemaPaths is every leaf field of a kind, as a caller writes it: the JSON
// names joined by dots, with a slice's element flattened into its parent's
// path. status is the server's and is not a caller's field.
func schemaPaths(t reflect.Type, prefix string) []string {
	var paths []string
	for field := range t.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" || (prefix == "" && name == "status") {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		inner := field.Type
		for inner.Kind() == reflect.Pointer || inner.Kind() == reflect.Slice {
			inner = inner.Elem()
		}
		if inner.Kind() == reflect.Struct && inner.PkgPath() == t.PkgPath() {
			paths = append(paths, schemaPaths(inner, path)...)
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

// collectPaths records every path a document sets, in the same shape
// schemaPaths returns.
func collectPaths(value any, prefix string, out map[string]bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, inner := range typed {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			out[path] = true
			collectPaths(inner, path, out)
		}
	case []any:
		for _, inner := range typed {
			collectPaths(inner, prefix, out)
		}
	}
}

// emittedCodes is every code this package raises, read from the calls that
// raise one. Reading the syntax rather than a list keeps the two from
// drifting: a code the package stops emitting stops being one a golden may
// record.
func emittedCodes(t *testing.T) map[string]bool {
	t.Helper()
	codes := map[string]bool{CodeAdmissionRefused: true, CodeAdmissionUnavailable: true}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			name, ok := call.Fun.(*ast.Ident)
			if !ok || (name.Name != "fail" && name.Name != "failAt" && name.Name != "failPaths") {
				return true
			}
			if literal, ok := call.Args[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
				codes[strings.Trim(literal.Value, `"`)] = true
			}
			return true
		})
	}
	if len(codes) < 10 {
		t.Fatalf("the package emits %d codes, which is too few to be the contract's table", len(codes))
	}
	return codes
}
