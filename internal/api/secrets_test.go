// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/egress"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/store"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime/native"
)

// sealedKey is the key the sealed fixtures open their store with: 32 bytes,
// fixed, so a failure is reproducible.
var sealedKey = []byte("0123456789abcdef0123456789abcdef")

// setupSealed is setup over a store that holds secret values. The plain
// fixture's store was given no key, which is the deployment that serves every
// other kind and refuses a value.
func setupSealed(t *testing.T, policy authz.Authorizer) *fixture {
	t.Helper()
	envelope, err := store.NewEnvelope(sealedKey)
	if err != nil {
		t.Fatal(err)
	}
	return setupWithStore(t, policy, func(dir string) (controller.Store, error) {
		return controller.OpenSealedFileStore(dir, envelope)
	})
}

// acceptingGateway is a gateway of the environment that takes every map and
// acknowledges it, which is what a sandbox with a mounted secret needs before
// it exists. It keeps what it was sent, so a test reads the boundary the
// control plane compiled rather than guessing at it.
type acceptingGateway struct {
	mu   sync.Mutex
	sent []egress.Map
}

func (g *acceptingGateway) Send(_ context.Context, m egress.Map) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sent = append(g.sent, m)
	return nil
}
func (g *acceptingGateway) Purge(context.Context, string) {}
func (g *acceptingGateway) CA() string                    { return "" }

// last is the newest map this gateway was handed for one principal.
func (g *acceptingGateway) last(principal string) (egress.Map, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, v := range slices.Backward(g.sent) {
		if v.Principal == principal {
			return v, true
		}
	}
	return egress.Map{}, false
}

// setupWithStore is the fixture over a store the caller opens.
func setupWithStore(t *testing.T, policy authz.Authorizer, open func(string) (controller.Store, error)) *fixture {
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
	desired, err := open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gateway := &acceptingGateway{}
	c, err := controller.Open(controller.Options{Store: desired, Driver: d, Environment: "default",
		TouchInterval: time.Millisecond, Egress: gateway})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if policy == nil {
		policy = &auth.OwnerPolicy{DefaultEnvironment: "default"}
	}
	h, err := New(Options{Controller: c, Verifier: verifier, Authorizer: auth.NewAuthorizer(policy)})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	f := &fixture{t: t, url: server.URL, issuerURL: issuer.URL(),
		alice: issuer.Mint(issuertest.Claims{Sub: "alice"}), bob: issuer.Mint(issuertest.Claims{Sub: "bob"}),
		h: h, c: c, gateway: gateway}
	if deferred, ok := policy.(*failingPolicy); ok {
		f.failing = &deferred.armed
	}
	return f
}

// secretBody is one Secret manifest as a caller writes it.
func secretBody(name, host, value string) string {
	return `{"apiVersion":"` + v1.APIVersion + `","kind":"Secret","metadata":{"name":"` + name + `"},` +
		`"spec":{"scope":{"hosts":["` + host + `"]},"value":"` + value + `"}}`
}

func decodeSecret(t *testing.T, body []byte) v1.Secret {
	t.Helper()
	var got v1.Secret
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the answer is %s: %v", body, err)
	}
	return got
}

// TestSecretRoutes is the grammar of design 008 over the Secret kind, and the
// one rule every route of it shares: no answer carries a value.
func TestSecretRoutes(t *testing.T) {
	f := setupSealed(t, nil)

	created := decodeSecret(t, f.request(http.MethodPost, "/v1/secrets", f.alice,
		secretBody("github", "api.github.com", "ghp_canary"), http.StatusCreated))
	if created.Spec.Value != "" {
		t.Fatalf("a create answered with the value: %+v", created.Spec)
	}
	if !strings.HasPrefix(created.Status.ID, v1.SecretIDPrefix) || created.Status.Owner == "" || created.Status.Version != 1 {
		t.Fatalf("status = %+v", created.Status)
	}
	if created.Spec.Inject.Header != "Authorization" || created.Spec.Inject.Scheme != v1.SchemeBearer {
		t.Fatalf("the defaults did not apply: %+v", created.Spec.Inject)
	}

	t.Run("aSecondCreateOfOneNameIsRefused", func(t *testing.T) {
		f.request(http.MethodPost, "/v1/secrets", f.alice,
			secretBody("github", "api.github.com", "ghp_other"), http.StatusConflict)
	})

	t.Run("readByNameAndById", func(t *testing.T) {
		for _, key := range []string{"github", created.Status.ID} {
			got := decodeSecret(t, f.request(http.MethodGet, "/v1/secrets/"+key, f.alice, "", http.StatusOK))
			if got.Status.ID != created.Status.ID || got.Spec.Value != "" {
				t.Fatalf("%s answered %+v", key, got)
			}
		}
	})

	t.Run("anotherSubjectSeesNothing", func(t *testing.T) {
		f.request(http.MethodGet, "/v1/secrets/github", f.bob, "", http.StatusNotFound)
		body := f.request(http.MethodGet, "/v1/secrets", f.bob, "", http.StatusOK)
		if !strings.Contains(string(body), `"items":[]`) {
			t.Fatalf("bob's list is %s", body)
		}
	})

	t.Run("aValueUpdateBumpsTheVersion", func(t *testing.T) {
		got := decodeSecret(t, f.request(http.MethodPut, "/v1/secrets/github", f.alice,
			secretBody("github", "api.github.com", "ghp_rotated"), http.StatusOK))
		if got.Status.Version != 2 || got.Spec.Value != "" {
			t.Fatalf("the update answered %+v", got)
		}
		// An update that carries no value leaves the version where it was.
		again := decodeSecret(t, f.request(http.MethodPut, "/v1/secrets/github", f.alice,
			`{"apiVersion":"`+v1.APIVersion+`","kind":"Secret","spec":{"scope":{"hosts":["api.github.com","*.githubusercontent.com"]}}}`,
			http.StatusOK))
		if again.Status.Version != 2 || len(again.Spec.Scope.Hosts) != 2 {
			t.Fatalf("the second update answered %+v", again)
		}
	})

	t.Run("aPutCreatesAName", func(t *testing.T) {
		got := decodeSecret(t, f.request(http.MethodPut, "/v1/secrets/openai", f.alice,
			`{"apiVersion":"`+v1.APIVersion+`","kind":"Secret","spec":{"scope":{"hosts":["api.openai.com"]},"value":"sk-x"}}`,
			http.StatusCreated))
		if got.Metadata.Name != "openai" {
			t.Fatalf("the created name is %q", got.Metadata.Name)
		}
		// A body that names another object is refused rather than renamed.
		f.request(http.MethodPut, "/v1/secrets/openai2", f.alice,
			secretBody("openai", "api.openai.com", "sk-y"), http.StatusBadRequest)
	})

	t.Run("theListNeverCarriesAValue", func(t *testing.T) {
		body := f.request(http.MethodGet, "/v1/secrets", f.alice, "", http.StatusOK)
		if strings.Contains(string(body), "ghp_") || strings.Contains(string(body), "sk-") {
			t.Fatalf("the list carries a value: %s", body)
		}
		var page struct {
			Items []v1.Secret `json:"items"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 2 {
			t.Fatalf("the list has %d items", len(page.Items))
		}
	})

	t.Run("theListPages", func(t *testing.T) {
		body := f.request(http.MethodGet, "/v1/secrets?limit=1", f.alice, "", http.StatusOK)
		var page struct {
			Items []v1.Secret `json:"items"`
			Next  string      `json:"next"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Next == "" {
			t.Fatalf("the first page is %s", body)
		}
		rest := f.request(http.MethodGet, "/v1/secrets?cursor="+page.Next, f.alice, "", http.StatusOK)
		if strings.Contains(string(rest), page.Items[0].Status.ID) {
			t.Fatalf("the second page repeats the first: %s", rest)
		}
		f.request(http.MethodGet, "/v1/secrets?limit=0", f.alice, "", http.StatusBadRequest)
	})

	t.Run("delete", func(t *testing.T) {
		f.request(http.MethodDelete, "/v1/secrets/openai", f.alice, "", http.StatusOK)
		f.request(http.MethodGet, "/v1/secrets/openai", f.alice, "", http.StatusNotFound)
		f.request(http.MethodDelete, "/v1/secrets/openai", f.alice, "", http.StatusNotFound)
	})

	t.Run("refusals", func(t *testing.T) {
		f.request(http.MethodPost, "/v1/secrets", f.alice,
			`{"apiVersion":"`+v1.APIVersion+`","kind":"Secret","metadata":{"name":"no-scope"},"spec":{"value":"x"}}`,
			http.StatusBadRequest)
		f.request(http.MethodPost, "/v1/secrets", f.alice,
			`{"apiVersion":"`+v1.APIVersion+`","kind":"Secret","metadata":{"name":"no-value"},"spec":{"scope":{"hosts":["api.example.com"]}}}`,
			http.StatusBadRequest)
		f.request(http.MethodPost, "/v1/secrets", f.alice, `{`, http.StatusBadRequest)
	})
}

// TestASecretNeedsAKey is the deployment that stores no value: every other
// kind is served and a value is refused with the code that says the server
// cannot provide it.
func TestASecretNeedsAKey(t *testing.T) {
	f := setup(t, nil)
	f.request(http.MethodPost, "/v1/secrets", f.alice,
		secretBody("github", "api.github.com", "ghp_canary"), http.StatusUnprocessableEntity)
	body := f.request(http.MethodGet, "/v1/secrets", f.alice, "", http.StatusOK)
	if !strings.Contains(string(body), `"items":[]`) {
		t.Fatalf("the list is %s", body)
	}
}

// TestMountingASecret is resolve stage 4 over the API's own lookup: a sandbox
// mounts a secret its caller holds, the status names it, and the boundary the
// map carries joins the secret's scope.
func TestMountingASecret(t *testing.T) {
	f := setupSealed(t, nil)
	f.request(http.MethodPost, "/v1/secrets", f.alice,
		secretBody("github", "api.github.com", "ghp_canary"), http.StatusCreated)

	body := f.request(http.MethodPost, "/v1/sandboxes", f.alice,
		`{"apiVersion":"`+v1.APIVersion+`","kind":"Sandbox","metadata":{"name":"work"},`+
			`"spec":{"command":["/bin/sh","-c","sleep 30"],"secrets":[{"name":"github","env":"GITHUB_TOKEN"}]}}`,
		http.StatusCreated)
	var sandbox v1.Sandbox
	if err := json.Unmarshal(body, &sandbox); err != nil {
		t.Fatal(err)
	}
	if len(sandbox.Status.Secrets.Mounted) != 1 || sandbox.Status.Secrets.Mounted[0] != "github" {
		t.Fatalf("status.secrets = %+v", sandbox.Status.Secrets)
	}
	if len(sandbox.Status.Secrets.NotInjectable) != 0 {
		t.Fatalf("notInjectable = %v", sandbox.Status.Secrets.NotInjectable)
	}
	// The control plane's own record of the boundary never leaves the
	// process, so no answer carries the placeholder or the credential.
	if strings.Contains(string(body), "egressState") || strings.Contains(string(body), "cph_") {
		t.Fatalf("the answer carries the control plane's own record: %s", body)
	}
	// The secret now reports the sandbox that mounts it.
	got := decodeSecret(t, f.request(http.MethodGet, "/v1/secrets/github", f.alice, "", http.StatusOK))
	if got.Status.MountedBy != 1 {
		t.Fatalf("mountedBy = %d, want 1", got.Status.MountedBy)
	}
	f.request(http.MethodDelete, "/v1/sandboxes/"+sandbox.Status.ID, f.alice, "", http.StatusAccepted)
}

// TestAMountTheAuthorizerRefusesIsNotFound keeps existence from leaking: a
// secret this caller may not mount answers the way one that does not exist
// answers.
func TestAMountTheAuthorizerRefusesIsNotFound(t *testing.T) {
	f := setupSealed(t, refusing(authorizer.ActionSecretMount))
	f.request(http.MethodPost, "/v1/secrets", f.alice,
		secretBody("github", "api.github.com", "ghp_canary"), http.StatusCreated)
	body := f.request(http.MethodPost, "/v1/sandboxes", f.alice,
		`{"apiVersion":"`+v1.APIVersion+`","kind":"Sandbox","metadata":{"name":"work"},`+
			`"spec":{"command":["/bin/sh","-c","sleep 30"],"secrets":[{"name":"github","env":"GITHUB_TOKEN"}]}}`,
		http.StatusNotFound)
	if !strings.Contains(string(body), "not_found") {
		t.Fatalf("the answer is %s", body)
	}
}

// refusing is the owner policy with one action denied, so a test drives the
// branch a deny takes without writing a policy of its own.
func refusing(action string) authz.Authorizer {
	return refusingPolicy{inner: &auth.OwnerPolicy{DefaultEnvironment: "default"}, action: action}
}

type refusingPolicy struct {
	inner  authz.Authorizer
	action string
}

func (p refusingPolicy) Authorize(ctx context.Context, req authz.Request) (authz.Decision, error) {
	if req.Action == p.action {
		return authz.Decision{Allow: false, Reason: "this caller may not mount that secret"}, nil
	}
	return p.inner.Authorize(ctx, req)
}

// TestLiveUpdateAndRevoke is the rotation rule of spec 018: a value written
// after the sandbox exists reaches the gateway at a higher version without
// the sandbox restarting, and a delete takes the entry away and says so in
// the sandbox's own status.
func TestLiveUpdateAndRevoke(t *testing.T) {
	f := setupSealed(t, nil)
	f.request(http.MethodPost, "/v1/secrets", f.alice,
		secretBody("github", "api.github.com", "ghp_first"), http.StatusCreated)
	body := f.request(http.MethodPost, "/v1/sandboxes", f.alice,
		`{"apiVersion":"`+v1.APIVersion+`","kind":"Sandbox","metadata":{"name":"work"},`+
			`"spec":{"command":["/bin/sh","-c","sleep 30"],"secrets":[{"name":"github","env":"GITHUB_TOKEN"}]}}`,
		http.StatusCreated)
	var sandbox v1.Sandbox
	if err := json.Unmarshal(body, &sandbox); err != nil {
		t.Fatal(err)
	}
	principal := egress.Principal(sandbox.Status.ID)
	first, held := f.gateway.last(principal)
	if !held || len(first.Entries) != 1 || first.Entries[0].Value != "ghp_first" {
		t.Fatalf("the first map is %+v", first)
	}
	placeholder := first.Entries[0].Placeholder

	f.request(http.MethodPut, "/v1/secrets/github", f.alice,
		secretBody("github", "api.github.com", "ghp_second"), http.StatusOK)
	second, _ := f.gateway.last(principal)
	if len(second.Entries) != 1 || second.Entries[0].Value != "ghp_second" {
		t.Fatalf("the rotated map is %+v", second)
	}
	if second.Version <= first.Version {
		t.Fatalf("the rotated map is version %d, want above %d", second.Version, first.Version)
	}
	// The placeholder does not change under the running workload, which is
	// what makes a rotation invisible to it.
	if second.Entries[0].Placeholder != placeholder {
		t.Fatal("the rotation changed the placeholder the sandbox holds")
	}

	f.request(http.MethodDelete, "/v1/secrets/github", f.alice, "", http.StatusOK)
	third, _ := f.gateway.last(principal)
	if len(third.Entries) != 0 || third.Version <= second.Version {
		t.Fatalf("the map after the delete is %+v", third)
	}
	after := decodeSandbox(t, f.request(http.MethodGet, "/v1/sandboxes/"+sandbox.Status.ID, f.alice, "", http.StatusOK))
	if len(after.Status.Secrets.NotInjectable) != 1 || after.Status.Secrets.NotInjectable[0] != "github" {
		t.Fatalf("status.secrets = %+v, want github notInjectable", after.Status.Secrets)
	}
	f.request(http.MethodDelete, "/v1/sandboxes/"+sandbox.Status.ID, f.alice, "", http.StatusAccepted)
}

func decodeSandbox(t *testing.T, body []byte) v1.Sandbox {
	t.Helper()
	var got v1.Sandbox
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the answer is %s: %v", body, err)
	}
	return got
}

// TestSecretRouteRefusals drives the branch each route takes when something
// before the store says no: a body too large to read, a manifest the contract
// refuses, and a decision the authorizer declined.
func TestSecretRouteRefusals(t *testing.T) {
	f := setupSealed(t, nil)
	f.request(http.MethodPost, "/v1/secrets", f.alice,
		secretBody("github", "api.github.com", "ghp_canary"), http.StatusCreated)
	oversized := secretBody("big", "api.example.com", strings.Repeat("x", 70<<10))

	t.Run("aBodyLargerThanThisServerReads", func(t *testing.T) {
		f.request(http.MethodPost, "/v1/secrets", f.alice, oversized, http.StatusRequestEntityTooLarge)
		f.request(http.MethodPut, "/v1/secrets/github", f.alice, oversized, http.StatusRequestEntityTooLarge)
		f.request(http.MethodPut, "/v1/secrets/fresh", f.alice, oversized, http.StatusRequestEntityTooLarge)
	})

	t.Run("aBodyTheContractRefuses", func(t *testing.T) {
		f.request(http.MethodPut, "/v1/secrets/github", f.alice, `{`, http.StatusBadRequest)
		f.request(http.MethodPut, "/v1/secrets/fresh", f.alice, `{`, http.StatusBadRequest)
		// A create through PUT still needs a value.
		f.request(http.MethodPut, "/v1/secrets/fresh", f.alice,
			`{"apiVersion":"`+v1.APIVersion+`","kind":"Secret","spec":{"scope":{"hosts":["api.example.com"]}}}`,
			http.StatusBadRequest)
		// An update still needs a scope.
		f.request(http.MethodPut, "/v1/secrets/github", f.alice,
			`{"apiVersion":"`+v1.APIVersion+`","kind":"Secret","spec":{}}`, http.StatusBadRequest)
	})

	t.Run("aCreateThroughPutWithNoKey", func(t *testing.T) {
		plain := setup(t, nil)
		plain.request(http.MethodPut, "/v1/secrets/fresh", plain.alice,
			secretBody("fresh", "api.example.com", "x"), http.StatusUnprocessableEntity)
	})

	for _, tc := range []struct {
		name, action, method, path string
		body                       string
	}{
		{"read", authorizer.ActionSecretRead, http.MethodGet, "/v1/secrets/github", ""},
		{"delete", authorizer.ActionSecretDelete, http.MethodDelete, "/v1/secrets/github", ""},
		{"update", authorizer.ActionSecretUpdate, http.MethodPut, "/v1/secrets/github", secretBody("github", "api.github.com", "x")},
		{"create", authorizer.ActionSecretCreate, http.MethodPost, "/v1/secrets", secretBody("other", "api.other.com", "x")},
	} {
		t.Run("aDecisionTheAuthorizerDeclined/"+tc.name, func(t *testing.T) {
			denied := setupSealed(t, refusing(tc.action))
			denied.request(http.MethodPost, "/v1/secrets", denied.alice,
				secretBody("github", "api.github.com", "ghp_canary"), statusFor(tc.action))
			if tc.action == authorizer.ActionSecretCreate {
				return
			}
			denied.request(tc.method, tc.path, denied.alice, tc.body, http.StatusForbidden)
		})
	}

	t.Run("aListTheAuthorizerDeclined", func(t *testing.T) {
		denied := setupSealed(t, refusing(authorizer.ActionSecretList))
		denied.request(http.MethodGet, "/v1/secrets", denied.alice, "", http.StatusForbidden)
	})

	t.Run("aLookupTheAuthorizerCouldNotDecide", func(t *testing.T) {
		broken := setupSealed(t, failing(authorizer.ActionSecretMount))
		broken.request(http.MethodPost, "/v1/secrets", broken.alice,
			secretBody("github", "api.github.com", "ghp_canary"), http.StatusCreated)
		broken.request(http.MethodPost, "/v1/sandboxes", broken.alice,
			`{"apiVersion":"`+v1.APIVersion+`","kind":"Sandbox","metadata":{"name":"work"},`+
				`"spec":{"command":["/bin/sh","-c","sleep 30"],"secrets":[{"name":"github","env":"GITHUB_TOKEN"}]}}`,
			http.StatusServiceUnavailable)
	})

	t.Run("aReadTheAuthorizerCouldNotDecideEndsTheList", func(t *testing.T) {
		broken := setupSealed(t, failingAfter(authorizer.ActionSecretRead))
		broken.request(http.MethodPost, "/v1/secrets", broken.alice,
			secretBody("github", "api.github.com", "ghp_canary"), http.StatusCreated)
		broken.failing.Store(true)
		broken.request(http.MethodGet, "/v1/secrets", broken.alice, "", http.StatusServiceUnavailable)
	})
}

// statusFor is the status a create takes when the named action is the one
// that was declined: a declined create is forbidden, and any other decline
// lets the create through.
func statusFor(action string) int {
	if action == authorizer.ActionSecretCreate {
		return http.StatusForbidden
	}
	return http.StatusCreated
}

// failing is the owner policy with one action the endpoint cannot answer at
// all, which is the branch a caller reads as authorizer_unavailable.
func failing(action string) authz.Authorizer { return newFailingPolicy(action, false) }

// failingAfter is failing from the moment a test says so, for a route that
// must first have an object to read.
func failingAfter(action string) authz.Authorizer { return newFailingPolicy(action, true) }

func newFailingPolicy(action string, deferred bool) *failingPolicy {
	p := &failingPolicy{inner: &auth.OwnerPolicy{DefaultEnvironment: "default"}, action: action}
	p.armed.Store(!deferred)
	return p
}

type failingPolicy struct {
	inner  authz.Authorizer
	action string
	armed  atomic.Bool
}

func (p *failingPolicy) Authorize(ctx context.Context, req authz.Request) (authz.Decision, error) {
	if req.Action == p.action && p.armed.Load() {
		return authz.Decision{}, errors.New("the endpoint did not answer")
	}
	return p.inner.Authorize(ctx, req)
}
