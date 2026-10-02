// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	document "latere.ai/x/cella/api"
	"latere.ai/x/cella/egress"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime/remote"
)

// jsonMedia is the media type an object route answers in and reads.
const jsonMedia = "application/json"

// payload is one media type of a request or a response body as the document
// describes it: the wire example, or the schema that says the bytes are
// opaque.
type payload struct {
	Example any `yaml:"example"`
	Schema  struct {
		Format string `yaml:"format"`
	} `yaml:"schema"`
}

// operation is the part of one operation the example rules read.
type operation struct {
	ID          string `yaml:"operationId"`
	RequestBody struct {
		Content map[string]payload `yaml:"content"`
	} `yaml:"requestBody"`
	Responses map[string]struct {
		Content map[string]payload `yaml:"content"`
	} `yaml:"responses"`
}

// answer is the first success the operation documents, the lowest 2xx, and
// the bodies under it. The code is empty where it documents none, as a route
// that only redirects or only upgrades does.
func (o operation) answer() (string, map[string]payload) {
	first := ""
	for code := range o.Responses {
		if strings.HasPrefix(code, "2") && (first == "" || code < first) {
			first = code
		}
	}
	return first, o.Responses[first].Content
}

// operations reads every operation of the API document, by operationId.
func operations(t *testing.T) map[string]operation {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(document.Document, &doc); err != nil {
		t.Fatalf("api/openapi.yaml does not parse: %v", err)
	}
	out := map[string]operation{}
	for path, item := range doc.Paths {
		for method, node := range item {
			// `parameters` is a sibling of the operations and not one of them.
			if method == "parameters" || method == "summary" || method == "description" {
				continue
			}
			var op operation
			if err := node.Decode(&op); err != nil {
				t.Fatalf("%s %s does not decode: %v", method, path, err)
			}
			if _, twice := out[op.ID]; twice || op.ID == "" {
				t.Fatalf("%s %s has the operationId %q, which is empty or used twice", method, path, op.ID)
			}
			out[op.ID] = op
		}
	}
	if len(out) == 0 {
		t.Fatal("api/openapi.yaml describes no operation")
	}
	return out
}

// bodiless is every operation whose documented success has no body to show,
// and why. The document names 200 for each of them. The server upgrades the
// five sockets, so their answer is 101 and frames, and it answers the two
// deletions 204.
var bodiless = map[string]string{
	"execSocket":           "upgrades to a WebSocket",
	"attachSandbox":        "upgrades to a WebSocket",
	"sandboxScreen":        "upgrades to a WebSocket",
	"workerOperations":     "upgrades to a WebSocket",
	"gatewaySync":          "upgrades to a WebSocket",
	"deleteEnvironment":    "answers 204 with no body",
	"revokeEnvironmentKey": "answers 204 with no body",
}

// TestEveryAnswerShowsItsBody holds each operation's first success to a body
// a reader can see: a wire example, or a schema of format binary where the
// bytes are a file, an image or a stream of frames. A 204 has none, and an
// operation with no 2xx has no body this rule reads. A reference built from
// the document shows the example beside the route, so an operation added
// without one fails here.
func TestEveryAnswerShowsItsBody(t *testing.T) {
	described := operations(t)
	for id, op := range described {
		code, content := op.answer()
		reason, exempt := bodiless[id]
		switch {
		case code == "" || code == "204":
			if exempt {
				t.Errorf("%s is listed as bodiless and documents no 2xx body to exempt", id)
			}
		case exempt:
			if len(content) != 0 {
				t.Errorf("%s is listed as bodiless because it %s, and its %s response describes a body", id, reason, code)
			}
		case len(content) == 0:
			t.Errorf("%s: the %s response describes no body; give it an example, or a binary schema", id, code)
		default:
			for media, body := range content {
				if body.Example == nil && body.Schema.Format != "binary" {
					t.Errorf("%s: the %s response has neither an example nor a binary schema under %s", id, code, media)
				}
			}
		}
	}
	for id := range bodiless {
		if _, held := described[id]; !held {
			t.Errorf("%s is listed as bodiless and the document describes no such operation", id)
		}
	}
}

// page is the answer of a list route.
type page[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"next"`
}

// answers is the Go value each operation's handler encodes as its JSON
// answer, by operationId. An example is decoded into it with unknown fields
// refused, so an example names only fields the handler writes.
var answers = map[string]func() any{
	"createSandbox":  func() any { return new(v1.Sandbox) },
	"listSandboxes":  func() any { return new(page[v1.Sandbox]) },
	"applySandbox":   func() any { return new(v1.Sandbox) },
	"readSandbox":    func() any { return new(v1.Sandbox) },
	"deleteSandbox":  func() any { return new(v1.Sandbox) },
	"startSandbox":   func() any { return new(v1.Sandbox) },
	"stopSandbox":    func() any { return new(v1.Sandbox) },
	"execSandbox":    func() any { return new(execResult) },
	"statFile":       func() any { return new(entry) },
	"listFiles":      func() any { return new(page[entry]) },
	"sandboxDisplay": func() any { return new(displayStatus) },
	"sandboxInput":   func() any { return new(inputResult) },
	"sandboxPorts":   func() any { return new(portList) },
	// The egress list carries no cursor.
	"sandboxEgress": func() any {
		return new(struct {
			Items []egress.Record `json:"items"`
		})
	},
	"objectEvents":        func() any { return new(page[events.Record]) },
	"createSecret":        func() any { return new(v1.Secret) },
	"listSecrets":         func() any { return new(page[v1.Secret]) },
	"applySecret":         func() any { return new(v1.Secret) },
	"readSecret":          func() any { return new(v1.Secret) },
	"deleteSecret":        func() any { return new(v1.Secret) },
	"listEnvironments":    func() any { return new(page[v1.Environment]) },
	"createEnvironment":   func() any { return new(v1.Environment) },
	"readEnvironment":     func() any { return new(v1.Environment) },
	"applyEnvironment":    func() any { return new(v1.Environment) },
	"mintEnvironmentKey":  func() any { return new(mintedKey) },
	"listEnvironmentKeys": func() any { return new(page[keyItem]) },
	"registerWorker":      func() any { return new(remote.Registered) },
}

// requests is how each operation's handler reads its JSON body, by
// operationId: the manifest decoders for the three kinds, and a strict decode
// into the handler's own type for the rest.
var requests = map[string]func([]byte) error{
	"createSandbox":     func(b []byte) error { _, err := manifest.Decode(b, jsonMedia); return err },
	"applySandbox":      func(b []byte) error { _, err := manifest.Decode(b, jsonMedia); return err },
	"createSecret":      func(b []byte) error { _, err := manifest.DecodeSecret(b, jsonMedia); return err },
	"applySecret":       func(b []byte) error { _, err := manifest.DecodeSecret(b, jsonMedia); return err },
	"createEnvironment": func(b []byte) error { _, err := manifest.DecodeEnvironment(b, jsonMedia); return err },
	"applyEnvironment":  func(b []byte) error { _, err := manifest.DecodeEnvironment(b, jsonMedia); return err },
	"execSandbox":       func(b []byte) error { return strict(b, new(execRequest)) },
	"makeDirectory":     func(b []byte) error { return strict(b, new(pathRequest)) },
	"moveFile":          func(b []byte) error { return strict(b, new(moveRequest)) },
	"sandboxInput":      func(b []byte) error { return strict(b, new(inputBatch)) },
	"registerWorker":    func(b []byte) error { return strict(b, new(remote.Registration)) },
}

// strict decodes one JSON document and refuses a field the type does not
// have.
func strict(raw []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// TestTheExamplesAreTheHandlersOwnShapes holds every JSON example to the type
// its handler encodes or decodes. A field renamed in the code and left in an
// example, or an example written with a field the server never sends, fails
// here, and so does a JSON example on an operation this test knows no type
// for.
func TestTheExamplesAreTheHandlersOwnShapes(t *testing.T) {
	for id, op := range operations(t) {
		if _, content := op.answer(); content[jsonMedia].Example != nil {
			raw, err := json.Marshal(content[jsonMedia].Example)
			if err != nil {
				t.Fatalf("%s: the answer's example is not JSON: %v", id, err)
			}
			if into, known := answers[id]; !known {
				t.Errorf("%s has a JSON example for its answer and no type here to hold it to", id)
			} else if err := strict(raw, into()); err != nil {
				t.Errorf("%s: the answer's example is not what the handler encodes: %v", id, err)
			}
		}
		if example := op.RequestBody.Content[jsonMedia].Example; example != nil {
			raw, err := json.Marshal(example)
			if err != nil {
				t.Fatalf("%s: the request's example is not JSON: %v", id, err)
			}
			if read, known := requests[id]; !known {
				t.Errorf("%s has a JSON example for its request and no decoder here to hold it to", id)
			} else if err := read(raw); err != nil {
				t.Errorf("%s: the request's example is not a body the handler reads: %v", id, err)
			}
		}
	}
}
