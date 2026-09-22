// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	document "latere.ai/x/cella/api"
)

// placeholder matches one path parameter, whatever it is named. The mux calls
// a sandbox's key `id` and a secret's `key`; the document names each after
// what a reader of the contract would call it. The two are compared by shape,
// so a rename on either side is not a drift.
var placeholder = regexp.MustCompile(`\{[^}]+\}`)

// anyMethod is every operation an OpenAPI path item can hold, which is what
// a pattern that names no method is documented as: the document has no word
// for any method, and a pattern that answers them all answers each of these.
var anyMethod = []string{"GET", "PUT", "POST", "DELETE", "OPTIONS", "HEAD", "PATCH", "TRACE"}

// shapes is the set of `METHOD path` a list of routes reaches, with every
// placeholder reduced to its position, the verb route expanded into the verbs
// the handler answers, and a pattern with no method into every method.
func shapes(routes []string) map[string]bool {
	out := map[string]bool{}
	for _, route := range routes {
		method, path, ok := strings.Cut(route, " ")
		if !ok {
			for _, each := range anyMethod {
				out[each+" "+placeholder.ReplaceAllString(route, "{}")] = true
			}
			continue
		}
		path = placeholder.ReplaceAllString(path, "{}")
		if strings.HasSuffix(path, "/{}") && strings.HasPrefix(path, "/v1/sandboxes/{}/") && method == "POST" &&
			strings.Count(path, "/") == 4 {
			// The item route carries the sandbox verbs behind one pattern;
			// the document names each verb, because a client generator cannot
			// build a call from a wildcard.
			for _, verb := range []string{"start", "stop"} {
				out[method+" /v1/sandboxes/{}/"+verb] = true
			}
			continue
		}
		out[method+" "+path] = true
	}
	return out
}

// documented reads every operation of the API document as `METHOD path`.
func documented(t *testing.T) map[string]bool {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(document.Document, &doc); err != nil {
		t.Fatalf("api/openapi.yaml does not parse: %v", err)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("api/openapi.yaml describes no path")
	}
	out := map[string]bool{}
	for path, operations := range doc.Paths {
		for method := range operations {
			// `parameters` is a sibling of the operations and not one of them.
			if method == "parameters" || method == "summary" || method == "description" {
				continue
			}
			out[strings.ToUpper(method)+" "+placeholder.ReplaceAllString(path, "{}")] = true
		}
	}
	return out
}

// TestTheDocumentAndTheMuxAgree is design 008's rule read in both directions:
// a route this server registers with no operation in the document, and an
// operation in the document this server does not serve, each fail here. The
// generator of design 008 is not built, so this test is what keeps the
// document and the server from drifting apart.
func TestTheDocumentAndTheMuxAgree(t *testing.T) {
	f := setup(t, nil)
	handler, ok := f.h.(*handler)
	if !ok {
		t.Fatalf("New returned %T", f.h)
	}
	if len(handler.patterns) == 0 {
		t.Fatal("the handler recorded no route, so this test would pass vacuously")
	}
	served, described := shapes(handler.patterns), documented(t)
	for route := range served {
		if !described[route] {
			t.Errorf("the server serves %s and api/openapi.yaml describes no operation for it", route)
		}
	}
	for route := range described {
		if !served[route] {
			t.Errorf("api/openapi.yaml describes %s and the server serves no such route", route)
		}
	}
}

// TestEveryRouteIsRegisteredOnce holds the record the document is compared
// against to one entry per method and path, so a pattern registered twice
// cannot hide a route behind another.
func TestEveryRouteIsRegisteredOnce(t *testing.T) {
	f := setup(t, nil)
	handler := f.h.(*handler)
	seen := slices.Clone(handler.patterns)
	slices.Sort(seen)
	if compacted := slices.Compact(slices.Clone(seen)); len(compacted) != len(seen) {
		t.Fatalf("a pattern is registered more than once: %v", seen)
	}
}
