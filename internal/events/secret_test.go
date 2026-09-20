// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/internal/events"
	v1 "latere.ai/x/cella/manifest/v1"
)

// aSecret is one Secret as the store hands it to the record builder.
func aSecret() v1.Secret {
	return v1.Secret{
		APIVersion: v1.APIVersion, Kind: v1.KindSecret,
		Metadata: v1.Metadata{Name: "vendor", Labels: map[string]string{"tenant": "acme"}},
		Spec: v1.SecretSpec{
			Kind:  v1.SecretStatic,
			Scope: v1.SecretScope{Hosts: []string{"api.vendor.example"}},
			Value: "sk-a-value-that-must-not-travel",
		},
		Status: v1.SecretStatus{ID: "sec_a", Owner: "alice", Version: 3},
	}
}

// TestSecretRecords is design 009's Secret row: the three types, the object
// with its labels, the version and the hosts, and nothing else.
func TestSecretRecords(t *testing.T) {
	for _, kind := range events.SecretTypes {
		if !events.Deliverable(kind) {
			t.Fatalf("%s is not delivered", kind)
		}
		if events.Terminal(kind) {
			t.Fatalf("%s is terminal; a secret has no phase to end", kind)
		}
		record, err := events.SecretMutation(kind, aSecret(), events.Actor{Subject: "alice"}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if record.Object.Kind != events.KindSecret || record.Object.ID != "sec_a" ||
			record.Object.Name != "vendor" || record.Object.Owner != "alice" {
			t.Fatalf("the object is %+v", record.Object)
		}
		if record.Object.Labels["tenant"] != "acme" {
			t.Fatalf("the labels are %v", record.Object.Labels)
		}
		if record.Reason != "" || record.Sandbox != nil {
			t.Fatalf("the record carries %q and %+v", record.Reason, record.Sandbox)
		}
		var data events.Secret
		if err = json.Unmarshal(record.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.Version != 3 || !slices.Equal(data.Hosts, []string{"api.vendor.example"}) {
			t.Fatalf("the data is %+v", data)
		}
		body, err := events.Body(record)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "sk-a-value") {
			t.Fatalf("the record carries the value: %s", body)
		}
	}
	// A type outside the vocabulary is not delivered, and a record with no
	// object is not a record at all.
	if events.Deliverable("secret.rotated") {
		t.Fatal("a type outside the table is delivered")
	}
	if _, err := events.SecretMutation(events.TypeSecretCreated, v1.Secret{}, events.Actor{}, time.Now()); err == nil {
		t.Fatal("a secret with no id produced a record")
	}
}

// TestCreatedCarriesTheMountedNames is design 009's rule for a sandbox that
// mounts one: the record says which secrets, and never the key they arrive
// under or the placeholder that stands in for a value.
func TestCreatedCarriesTheMountedNames(t *testing.T) {
	obj := v1.Sandbox{
		APIVersion: v1.APIVersion, Kind: "Sandbox", Metadata: v1.Metadata{Name: "work"},
		Spec: v1.SandboxSpec{
			Secrets: []v1.SecretMount{{Name: "openai", Env: "OPENAI_API_KEY"}, {Name: "github", Env: "GITHUB_TOKEN"}},
		},
		Status: v1.SandboxStatus{ID: "sbx_a", Owner: "alice"},
	}
	got := events.CreatedOf(obj)
	if !slices.Equal(got.Mounts, []string{"github", "openai"}) {
		t.Fatalf("the created record names %v", got.Mounts)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	// The redactor leaves the names alone and would have blanked them under
	// a key ending in "secrets", which is why the field is named for what it
	// holds: the mounts, not the secrets.
	if strings.Contains(string(encoded), "***") {
		t.Fatalf("the created record was redacted: %s", encoded)
	}
	for _, unwanted := range []string{"OPENAI_API_KEY", "GITHUB_TOKEN", "cph_"} {
		if strings.Contains(string(encoded), unwanted) {
			t.Fatalf("the created record carries %q: %s", unwanted, encoded)
		}
	}
	if len(events.CreatedOf(v1.Sandbox{}).Mounts) != 0 {
		t.Fatal("a sandbox that mounts nothing names a secret")
	}
}
