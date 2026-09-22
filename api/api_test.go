// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"latere.ai/x/cella/api"
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
