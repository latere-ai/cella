// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/auth"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
)

// setupPool is the fixture over an environment that keeps one prewarmed entry,
// on the native driver, which declares the Pool capability, behind the operator
// policy given, or the owner policy where none is. The entry is made before the
// fixture is returned, so the first create a test sends is one the entry could
// serve.
func setupPool(t *testing.T, policy authz.Authorizer) (*fixture, *native.Driver) {
	t.Helper()
	issuer := issuertest.New(t, issuertest.WithDefaultAudience("cella"))
	verifier, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{Issuers: []string{issuer.URL()}, Audience: "cella"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	c, err := controller.Open(t.Context(), controller.Options{DataDir: t.TempDir(), Driver: d, Environment: "default", Pool: v1.PoolSpec{Size: 1}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	runScheduler(t, c)
	if _, err := c.Refill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if policy == nil {
		policy = &auth.OwnerPolicy{DefaultEnvironment: "default"}
	}
	rec := &apiRecorder{}
	h, err := New(Options{
		Controller: c, Verifier: verifier, Authorizer: auth.NewAuthorizer(policy),
		Metrics: rec, Log: slog.New(slog.DiscardHandler), SpoolDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return &fixture{t: t, url: server.URL, issuerURL: issuer.URL(), alice: issuer.Mint(issuertest.Claims{Sub: "alice"}),
		bob: issuer.Mint(issuertest.Claims{Sub: "bob"}), h: h, c: c, metrics: rec}, d
}

// prewarmed is the environment's pool entries as the driver holds them.
func prewarmed(t *testing.T, d *native.Driver) []runtime.State {
	t.Helper()
	pool := true
	entries, err := d.List(t.Context(), runtime.Filter{Pool: &pool})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// TestPoolIsBehindTheAuthorizer is spec 020's rule that a pool never serves a
// create the authorizer refused. The API asks the authorizer before it calls
// the controller, and the controller holds the owner to the count that same
// decision granted before it takes an entry. A create refused at
// sandbox.create, at environment.use, or at the owner's max_sandboxes leaves
// the entry prewarmed and nobody's, and the same manifest from a caller the
// authorizer allows takes it, so every refusal is of a create the entry would
// have served.
func TestPoolIsBehindTheAuthorizer(t *testing.T) {
	const served = `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"served"},"spec":{}}`
	const other = `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"other"},"spec":{"command":["sh","-c","sleep 30"]}}`
	one := decisionFunc(func(context.Context, authz.Request) (authz.Decision, error) {
		return authz.Decision{Allow: true, Limits: json.RawMessage(`{"max_sandboxes":1}`)}, nil
	})
	for _, tc := range []struct {
		name   string
		policy authz.Authorizer
		// held creates a sandbox first that takes no entry, which is what
		// puts the owner at the count.
		held   bool
		status int
	}{
		{"the authorizer denies the create", refusing(authorizer.ActionSandboxCreate), false, http.StatusForbidden},
		{"the authorizer denies the environment", refusing(authorizer.ActionEnvironmentUse), false, http.StatusNotFound},
		{"the owner is at the count the authorizer granted", one, true, http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, d := setupPool(t, tc.policy)
			if tc.held {
				f.request(http.MethodPost, "/v1/sandboxes?wait=1", f.alice, other, http.StatusCreated)
			}
			before := prewarmed(t, d)
			if len(before) != 1 {
				t.Fatalf("the environment keeps %d entries, want one", len(before))
			}
			f.request(http.MethodPost, "/v1/sandboxes?wait=1", f.alice, served, tc.status)
			after := prewarmed(t, d)
			if len(after) != 1 || after[0].ID != before[0].ID || after[0].Owner != "" {
				t.Fatalf("a refused create touched the entry: %+v, was %+v", after, before)
			}
			for _, obj := range f.c.List() {
				if obj.Status.ID == before[0].ID || obj.Metadata.Name == "served" {
					t.Fatalf("a refused create left the sandbox %s %s", obj.Metadata.Name, obj.Status.ID)
				}
			}
		})
	}

	// The control: the same manifest from a caller the authorizer allows is
	// the create the entry serves.
	f, d := setupPool(t, nil)
	entry := prewarmed(t, d)[0]
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request(http.MethodPost, "/v1/sandboxes?wait=1", f.alice, served, http.StatusCreated), &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Status.ID != entry.ID {
		t.Fatalf("the allowed create is %s, not the entry %s", obj.Status.ID, entry.ID)
	}
	for _, cond := range obj.Status.Conditions {
		if cond.Type == v1.ConditionScheduled && cond.Reason != v1.ReasonFromPool {
			t.Fatalf("the allowed create is Scheduled %s", cond.Reason)
		}
	}
}
