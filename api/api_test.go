// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"latere.ai/x/cella/api"
	"latere.ai/x/cella/client"
)

// TestTheDocumentIsServed: the handler answers the carried document as YAML,
// under no credential, which is what a client generator reaches it with.
func TestTheDocumentIsServed(t *testing.T) {
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	res, err := http.Get(server.URL + api.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the document answered %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != api.MediaType {
		t.Errorf("the document answered Content-Type %q", got)
	}
	if res.Header.Get("Cache-Control") == "" {
		t.Error("the document answered no cache policy")
	}
	var served map[string]any
	if err := yaml.NewDecoder(res.Body).Decode(&served); err != nil {
		t.Fatalf("the served document does not parse: %v", err)
	}
	if served["openapi"] == nil || served["paths"] == nil {
		t.Fatalf("the served document has no version or no paths: %v", served)
	}
}

// TestTheDocumentNamesNoInstallation: the document travels to every reader of
// this open control plane, so the one server it names is the one it was
// served from and no host of anyone's.
func TestTheDocumentNamesNoInstallation(t *testing.T) {
	body := string(api.Document)
	for _, coordinate := range []string{"https://", "http://", ".latere.ai"} {
		if strings.Contains(body, coordinate) {
			t.Errorf("the document names %q", coordinate)
		}
	}
}

// TestTheDocumentUnderAPublicPath: a control plane whose public URL has a path
// serves the document with every path under it by the route rule, and the
// rest of the document as carried; with no path it serves the carried bytes.
func TestTheDocumentUnderAPublicPath(t *testing.T) {
	same, err := api.Under("")
	if err != nil || string(same) != string(api.Document) {
		t.Fatalf("with no public path the document changed: %v", err)
	}
	const public = "/v1/environments"
	handler, err := api.HandlerUnder(public)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	res, err := http.Get(server.URL + api.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != api.MediaType {
		t.Fatalf("the document answered %d as %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	var served, carried map[string]any
	if err := yaml.NewDecoder(res.Body).Decode(&served); err != nil {
		t.Fatalf("the served document does not parse: %v", err)
	}
	if err := yaml.Unmarshal(api.Document, &carried); err != nil {
		t.Fatal(err)
	}
	servedPaths, _ := served["paths"].(map[string]any)
	carriedPaths, _ := carried["paths"].(map[string]any)
	if len(carriedPaths) < 30 || len(servedPaths) != len(carriedPaths) {
		t.Fatalf("the served document has %d paths, the carried one %d", len(servedPaths), len(carriedPaths))
	}
	for path, operations := range carriedPaths {
		moved := client.Route(public, path)
		if !strings.HasPrefix(moved, public+"/") || strings.HasPrefix(moved, public+"/v1/") {
			t.Errorf("%s moved to %s", path, moved)
		}
		if !reflect.DeepEqual(servedPaths[moved], operations) {
			t.Errorf("%s is served as %s with other operations", path, moved)
		}
	}
	delete(served, "paths")
	delete(carried, "paths")
	if !reflect.DeepEqual(served, carried) {
		t.Error("the served document differs from the carried one outside its paths")
	}
}
