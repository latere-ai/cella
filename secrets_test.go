// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	document "latere.ai/x/cella/api"
	v1 "latere.ai/x/cella/manifest/v1"
)

// TestTheLastUseResolutionIsDocumented holds the two places a reader learns
// how coarse status.lastUsedAt is to the constant that makes it so: the API
// document's description of the field and the manifest reference's
// paragraph on it. A change to the constant that leaves either sentence
// behind fails here.
func TestTheLastUseResolutionIsDocumented(t *testing.T) {
	stated := strconv.Itoa(int(v1.SecretLastUsedResolution/time.Minute)) + " minutes"
	if v1.SecretLastUsedResolution%time.Minute != 0 {
		t.Fatalf("the resolution is %v, which the documents cannot state in whole minutes", v1.SecretLastUsedResolution)
	}

	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Description string `json:"description"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := yaml.Unmarshal(document.Document, &doc); err != nil {
		t.Fatalf("api/openapi.yaml does not parse: %v", err)
	}
	field, ok := doc.Components.Schemas["SecretStatus"].Properties["lastUsedAt"]
	if !ok {
		t.Fatal("api/openapi.yaml does not describe status.lastUsedAt")
	}
	if !strings.Contains(field.Description, stated) {
		t.Errorf("api/openapi.yaml describes lastUsedAt as %q, which does not state %q", field.Description, stated)
	}

	reference, err := os.ReadFile(filepath.Join("docs", "manifest.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, paragraph, found := strings.Cut(string(reference), "`status.lastUsedAt`")
	paragraph, _, _ = strings.Cut(paragraph, "\n\n")
	if !found {
		t.Fatal("docs/manifest.md does not describe status.lastUsedAt")
	}
	if !strings.Contains(strings.Join(strings.Fields(paragraph), " "), stated) {
		t.Errorf("docs/manifest.md describes lastUsedAt as %q, which does not state %q", paragraph, stated)
	}
}
