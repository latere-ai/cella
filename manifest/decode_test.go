// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// theExample is the manifest of design 003's Overview, reduced to the fields
// this schema carries. It is the corpus's first row, read here as YAML and as
// the JSON form written beside it.
const theExample = `apiVersion: cella.latere.ai/v1beta1
kind: Sandbox
metadata:
  name: dev
  labels:
    team: research
  annotations:
    example.com/ticket: "1234"
spec:
  environment: default
  image: registry.example/base:1
  command: ["/bin/bash", "-l"]
  workdir: /workspace
  user: "1000"
  resources:
    cpu: "1"
    memory: 2Gi
    disk: 10Gi
  workspace:
    path: /workspace
  env:
    LOG_LEVEL: debug
  network:
    egress:
      mode: allowlist
      allowedHosts: ["pypi.org", "*.pythonhosted.org"]
  lifecycle:
    autoStop: 15m
    ttl: 24h
    autoDelete: 72h
`

// TestDecodeTakesYAMLAndJSON: each of the four media types design 003 names
// is read, and a body of any other type is refused.
func TestDecodeTakesYAMLAndJSON(t *testing.T) {
	for _, media := range []string{MediaYAML, MediaXYAML, MediaTextYAML, MediaYAML + "; charset=utf-8"} {
		obj, err := Decode([]byte(theExample), media)
		if err != nil {
			t.Fatalf("%s was refused: %v", media, err)
		}
		if obj.Metadata.Name != "dev" || obj.Spec.Lifecycle.TTL != "24h" {
			t.Fatalf("%s decoded to %+v", media, obj.Metadata)
		}
	}
	// A body that begins with an object brace is JSON under a YAML type,
	// which is the rule that lets one client send either.
	const asJSON = `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"dev"},"spec":{}}`
	if _, err := Decode([]byte(asJSON), MediaTextYAML); err != nil {
		t.Fatalf("a JSON body under a YAML type was refused: %v", err)
	}
	for _, media := range []string{"text/plain", "application/xml", "application/x-tar", "not a media type"} {
		if _, err := Decode([]byte(theExample), media); codeOf(t, err) != "unsupported_media_type" {
			t.Errorf("%s answered %s", media, codeOf(t, err))
		}
	}
}

// TestDecodeYAMLAndJSONAgree is design 003's rule that the two syntaxes carry
// one object: the example decodes from YAML and from its own JSON form to
// objects that are equal field for field.
func TestDecodeYAMLAndJSONAgree(t *testing.T) {
	fromYAML, err := Decode([]byte(theExample), MediaYAML)
	if err != nil {
		t.Fatal(err)
	}
	asJSON, err := jsonOf([]byte(theExample), MediaYAML)
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := Decode(asJSON, MediaJSON)
	if err != nil {
		t.Fatal(err)
	}
	if fromYAML.Metadata.Name != fromJSON.Metadata.Name || fromYAML.Spec.Image != fromJSON.Spec.Image {
		t.Fatalf("the two syntaxes decoded to two objects:\n%+v\n%+v", fromYAML, fromJSON)
	}
	if !slices.Equal(fromYAML.Spec.Command, fromJSON.Spec.Command) {
		t.Fatalf("the commands differ: %v and %v", fromYAML.Spec.Command, fromJSON.Spec.Command)
	}
	if !slices.Equal(fromYAML.Spec.Network.Egress.AllowedHosts, fromJSON.Spec.Network.Egress.AllowedHosts) {
		t.Fatal("the allow lists differ")
	}
	// An anchor and an alias are the document's and not the object's.
	const anchored = `apiVersion: cella.latere.ai/v1beta1
kind: Sandbox
metadata:
  name: anchored
  labels: &shared
    team: research
  annotations:
    <<: *shared
spec: {}
`
	obj, err := Decode([]byte(anchored), MediaYAML)
	if err != nil {
		t.Fatalf("a document with an alias was refused: %v", err)
	}
	if obj.Metadata.Annotations["team"] != "research" {
		t.Fatalf("the alias did not resolve: %v", obj.Metadata.Annotations)
	}
}

// TestDecodeRefusals is design 003's refusal table over both syntaxes, with
// the version read before the kind and both before any field.
func TestDecodeRefusals(t *testing.T) {
	for _, tc := range []struct{ name, body, media, code string }{
		{"a second YAML document", "apiVersion: cella.latere.ai/v1beta1\nkind: Sandbox\nspec: {}\n---\napiVersion: cella.latere.ai/v1beta1\nkind: Sandbox\n", MediaYAML, "multi_document"},
		{"a second JSON document", `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox"} {}`, MediaJSON, "multi_document"},
		{"another version", "apiVersion: cella.latere.ai/v2\nkind: Sandbox\n", MediaYAML, "unsupported_version"},
		{"another kind", "apiVersion: cella.latere.ai/v1beta1\nkind: Widget\n", MediaYAML, "unsupported_kind"},
		{"the version before the kind", "apiVersion: cella.latere.ai/v2\nkind: Widget\n", MediaYAML, "unsupported_version"},
		{"the version before an unknown field", "apiVersion: cella.latere.ai/v2\nkind: Sandbox\nnonesuch: 1\n", MediaYAML, "unsupported_version"},
		{"the kind before an unknown field", "apiVersion: cella.latere.ai/v1beta1\nkind: Widget\nnonesuch: 1\n", MediaYAML, "unsupported_kind"},
		{"a document that is not a mapping", "- one\n- two\n", MediaYAML, "bad_request"},
		{"an empty body", "", MediaYAML, "bad_request"},
		{"YAML that does not parse", "apiVersion: [\n", MediaYAML, "bad_request"},
		{"a field name that is not a string", "apiVersion: cella.latere.ai/v1beta1\nkind: Sandbox\nspec:\n  env:\n    1: one\n", MediaYAML, "invalid_field"},
		{"a value of the wrong shape", "apiVersion: cella.latere.ai/v1beta1\nkind: Sandbox\nmetadata: a string\n", MediaYAML, "bad_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode([]byte(tc.body), tc.media)
			if got := codeOf(t, err); got != tc.code {
				t.Fatalf("the refusal is %s; the rule says %s: %v", got, tc.code, err)
			}
		})
	}
}

// TestUnknownFieldNamesThePath is design 003's rule that an unknown field is
// refused wherever it sits and that the refusal says where. A caller fixing a
// manifest needs the path, and encoding/json names only the field.
func TestUnknownFieldNamesThePath(t *testing.T) {
	const head = "apiVersion: cella.latere.ai/v1beta1\nkind: Sandbox\n"
	for _, tc := range []struct{ name, body, path string }{
		{"at the root", head + "nonesuch: 1\n", "nonesuch"},
		{"in metadata", head + "metadata:\n  nonesuch: 1\n", "metadata.nonesuch"},
		{"in spec", head + "spec:\n  nonesuch: 1\n", "spec.nonesuch"},
		{"two below spec", head + "spec:\n  resources:\n    nonesuch: 1\n", "spec.resources.nonesuch"},
		{"three below spec", head + "spec:\n  network:\n    egress:\n      nonesuch: 1\n", "spec.network.egress.nonesuch"},
		{"in workspace", head + "spec:\n  workspace:\n    nonesuch: 1\n", "spec.workspace.nonesuch"},
		{"in lifecycle", head + "spec:\n  lifecycle:\n    nonesuch: 1\n", "spec.lifecycle.nonesuch"},
		{"in display", head + "spec:\n  display:\n    nonesuch: 1\n", "spec.display.nonesuch"},
		{"in mesh", head + "spec:\n  mesh:\n    nonesuch: 1\n", "spec.mesh.nonesuch"},
		{"in spawn", head + "spec:\n  mesh:\n    spawn:\n      nonesuch: 1\n", "spec.mesh.spawn.nonesuch"},
		{"in a list entry", head + "spec:\n  secrets:\n    - name: one\n      nonesuch: 1\n", "spec.secrets[0].nonesuch"},
		{"in the second list entry", head + "spec:\n  secrets:\n    - name: one\n    - name: two\n      nonesuch: 1\n", "spec.secrets[1].nonesuch"},
		{"in status", head + "status:\n  nonesuch: 1\n", "status.nonesuch"},
		{"in a status list entry", head + "status:\n  conditions:\n    - type: Ready\n      nonesuch: 1\n", "status.conditions[0].nonesuch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode([]byte(tc.body), MediaYAML)
			var known *Error
			if !errors.As(err, &known) || known.Code != "unknown_field" {
				t.Fatalf("the refusal is %v, want unknown_field", err)
			}
			if !slices.Contains(known.Paths, tc.path) {
				t.Fatalf("the refusal names %v, want %s", known.Paths, tc.path)
			}
		})
	}
	// A member of a map the schema declares open is a value and never a
	// field, so a label or an environment entry is never unknown.
	for _, body := range []string{
		head + "metadata:\n  labels:\n    anything: yes\n",
		head + "metadata:\n  annotations:\n    example.com/anything: \"1\"\n",
		head + "spec:\n  env:\n    ANYTHING: one\n",
	} {
		if _, err := Decode([]byte(body), MediaYAML); err != nil {
			t.Errorf("an open map's member was refused: %v", err)
		}
	}
	// Every unknown field of one document is named, not only the first.
	_, err := Decode([]byte(head+"nonesuch: 1\nmetadata:\n  other: 2\n"), MediaYAML)
	var known *Error
	if !errors.As(err, &known) || len(known.Paths) != 2 {
		t.Fatalf("the refusal names %v, want both fields", err)
	}
	// The other kinds share the rule and the decoder.
	if _, err := DecodeSecret([]byte("apiVersion: cella.latere.ai/v1beta1\nkind: Secret\nspec:\n  nonesuch: 1\n"), MediaYAML); codeOf(t, err) != "unknown_field" {
		t.Errorf("a Secret's unknown field answered %s", codeOf(t, err))
	}
	if _, err := DecodeEnvironment([]byte("apiVersion: cella.latere.ai/v1beta1\nkind: Environment\nspec:\n  nonesuch: 1\n"), MediaYAML); codeOf(t, err) != "unknown_field" {
		t.Errorf("an Environment's unknown field answered %s", codeOf(t, err))
	}
}

// TestYAMLLimits is design 003's two bounds. Each is refused with
// invalid_field, and each in well under a tenth of a second: a document that
// costs more to expand than it cost to send is refused before it is expanded.
func TestYAMLLimits(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"an alias chain", billionLaughs()},
		{"nesting", deepNesting(MaxDepth + 8)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Now()
			_, err := Decode([]byte(tc.body), MediaYAML)
			if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
				t.Errorf("the refusal took %s", elapsed)
			}
			if got := codeOf(t, err); got != "invalid_field" {
				t.Fatalf("the refusal is %s: %v", got, err)
			}
		})
	}
	// A document within both bounds is accepted, so the bounds refuse what
	// they name and nothing else.
	if _, err := Decode([]byte(deepNesting(8)), MediaYAML); codeOf(t, err) != "unknown_field" {
		t.Errorf("a document within the bounds was refused for its shape: %v", err)
	}
}

// billionLaughs is the classic alias bomb: each level names the one below it
// nine times, so the document is small and its expansion is not.
func billionLaughs() string {
	var b strings.Builder
	b.WriteString("apiVersion: cella.latere.ai/v1beta1\nkind: Sandbox\nmetadata:\n  name: bomb\n")
	b.WriteString("a: &a [\"payload\",\"payload\",\"payload\",\"payload\",\"payload\",\"payload\",\"payload\",\"payload\",\"payload\"]\n")
	for level := 'b'; level <= 'j'; level++ {
		previous := string(level - 1)
		b.WriteString(string(level) + ": &" + string(level) + " [")
		for i := range 9 {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("*" + previous)
		}
		b.WriteString("]\n")
	}
	return b.String()
}

// deepNesting is one mapping inside another, as deep as asked.
func deepNesting(depth int) string {
	var b strings.Builder
	b.WriteString("apiVersion: cella.latere.ai/v1beta1\nkind: Sandbox\ndeep:\n")
	for i := range depth {
		b.WriteString(strings.Repeat("  ", i+1) + "down:\n")
	}
	b.WriteString(strings.Repeat("  ", depth+1) + "bottom: 1\n")
	return b.String()
}

// TestTheExampleResolves: the manifest of design 003's Overview resolves
// without error under options that grant every capability it uses, which is
// what makes the document in the spec a manifest and not an illustration.
func TestTheExampleResolves(t *testing.T) {
	obj, err := Decode([]byte(theExample), MediaYAML)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(t.Context(), &obj, corpusOptions())
	if err != nil {
		t.Fatalf("the example does not resolve: %v", err)
	}
	if resolved.Sandbox.Spec.Environment != "default" || resolved.Sandbox.Spec.Workspace.Path != "/workspace" {
		t.Fatalf("the example resolved to %+v", resolved.Sandbox.Spec)
	}
	if resolved.Sandbox.Status.ID != "" || resolved.Sandbox.Status.Phase != "" {
		t.Fatalf("a resolve wrote a status: %+v", resolved.Sandbox.Status)
	}
}
