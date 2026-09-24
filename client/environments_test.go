// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"
)

// environmentJSON is one Environment as the API answers it.
func environmentJSON(name, phase string) string {
	return `{"apiVersion":"` + v1.APIVersion + `","kind":"Environment","metadata":{"name":"` + name + `"},` +
		`"spec":{"mode":"workers"},"status":{"id":"` + name + `","phase":"` + phase + `"}}`
}

// TestEnvironmentsAndTheirKeys: the Environment kind lists with the same
// grammar as the others, reads and deletes by name, and a key is minted once
// with its jti and ended by that jti.
func TestEnvironmentsAndTheirKeys(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments":
			_, _ = w.Write([]byte(`{"items":[` + environmentJSON("default", "Ready") + `,` + environmentJSON("gpu", "Offline") + `],"next":""}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments/gpu/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"key-once","jti":"jti_1","exp":"2026-10-24T12:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments/gpu/keys" && r.URL.Query().Get("cursor") == "":
			_, _ = w.Write([]byte(`{"items":[{"jti":"jti_0","mintedAt":"2026-09-24T10:00:00Z","exp":"2027-09-24T10:00:00Z","revoked":true,"revokedAt":"2026-09-24T11:00:00Z","mintedBy":"admin"}],"next":"jti_0"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments/gpu/keys":
			_, _ = w.Write([]byte(`{"items":[{"jti":"jti_1","mintedAt":"2026-09-24T12:00:00Z","exp":"2026-10-24T12:00:00Z","revoked":false}],"next":""}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/environments/gpu/keys/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments/missing":
			writeError(w, http.StatusNotFound, "not_found", "Nothing by that name exists.", nil)
		default:
			_, _ = w.Write([]byte(environmentJSON("gpu", "Ready")))
		}
	})
	c := f.client(client.Config{})
	ctx := t.Context()

	items, raws, err := c.ListEnvironments(ctx, client.ListOptions{Labels: []string{"pool=gpu"}, Phase: "Ready"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[1].Metadata.Name != "gpu" || items[1].Status.Phase != "Offline" || !strings.Contains(string(raws[0]), `"default"`) {
		t.Fatalf("the list holds %+v", items)
	}
	if q := f.last().Query; q.Get("label") != "pool=gpu" || q.Get("phase") != "Ready" {
		t.Fatalf("the selectors were sent as %s", q.Encode())
	}
	one, _, err := c.GetEnvironment(ctx, "gpu")
	if err != nil || one.Status.ID != "gpu" {
		t.Fatalf("the read answered %+v, %v", one, err)
	}
	if _, _, err = c.GetEnvironment(ctx, "missing"); client.CodeOf(err) != "not_found" {
		t.Fatalf("a missing environment read as %v", err)
	}

	key, raw, err := c.MintEnvironmentKey(ctx, "gpu")
	if err != nil {
		t.Fatal(err)
	}
	if key.Token != "key-once" || key.JTI != "jti_1" || !key.Expires.Equal(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)) || !strings.Contains(string(raw), "key-once") {
		t.Fatalf("the mint answered %+v", key)
	}
	if got := f.last(); got.Method != http.MethodPost || got.Path != "/v1/environments/gpu/keys" {
		t.Fatalf("the mint called %s %s", got.Method, got.Path)
	}
	keys, err := c.ListEnvironmentKeys(ctx, "gpu")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].JTI != "jti_0" || !keys[0].Revoked || keys[0].RevokedAt == nil || keys[0].MintedBy != "admin" ||
		keys[1].JTI != "jti_1" || keys[1].Revoked || !keys[1].Expires.Equal(key.Expires) {
		t.Fatalf("the key list answered %+v", keys)
	}
	if got := f.last(); got.Path != "/v1/environments/gpu/keys" || got.Query.Get("cursor") != "jti_0" {
		t.Fatalf("the second page was asked as %s ?%s", got.Path, got.Query.Encode())
	}
	if err = c.RevokeEnvironmentKey(ctx, "gpu", key.JTI); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got.Method != http.MethodDelete || got.Path != "/v1/environments/gpu/keys/jti_1" {
		t.Fatalf("the revocation called %s %s", got.Method, got.Path)
	}
}
