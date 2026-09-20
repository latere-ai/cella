// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/runtime/remote"
)

// keyed is a control plane that signs its own tokens: the signer of spec 006,
// the revocation list of spec 010, and the two key routes over them.
type keyed struct {
	*fixture
	keys   *auth.EnvironmentKeys
	hub    *remote.Hub
	signer *auth.Signer
}

// setupKeyed stands a control plane up with a signer, so the key routes have
// something to mint with and the verifier has something to check against.
func setupKeyed(t *testing.T, policy authz.Authorizer) *keyed {
	t.Helper()
	issuer := issuertest.New(t, issuertest.WithDefaultAudience("cella"))
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewSigner(auth.SignerOptions{
		Issuer: "https://control.example.test", Audience: "cella", Keys: []*rsa.PrivateKey{key},
	})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	revocations := store.NewRevocations(journal)
	verifier, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{
		Issuers: []string{issuer.URL()}, Audience: "cella",
		LocalIssuer: signer.Issuer(), LocalKeys: signer.PublicKeys(), Revocations: revocations,
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	c, err := controller.Open(controller.Options{DataDir: t.TempDir(), Driver: d, Environment: "default"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	keys, err := auth.NewEnvironmentKeys(signer, revocations, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if policy == nil {
		policy = &auth.OwnerPolicy{DefaultEnvironment: "default", Admins: []string{issuer.URL() + "|admin"}}
	}
	hub := remote.NewHub(remote.HubOptions{Offline: time.Minute})
	h, err := New(Options{
		Controller: c, Verifier: verifier, Authorizer: auth.NewAuthorizer(policy),
		Keys: keys, Workers: hub,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	f := &fixture{
		t: t, url: server.URL, issuerURL: issuer.URL(),
		alice: issuer.Mint(issuertest.Claims{Sub: "admin"}),
		bob:   issuer.Mint(issuertest.Claims{Sub: "bob"}),
		h:     h, c: c,
	}
	return &keyed{fixture: f, keys: keys, hub: hub, signer: signer}
}

type mintedKey struct {
	Token string `json:"token"`
	JTI   string `json:"jti"`
	Exp   string `json:"exp"`
}

// TestEnvironmentKeyRoutes is the mint and the revoke of spec 021 over HTTP:
// the route a data plane is installed from, and the one that ends a key.
func TestEnvironmentKeyRoutes(t *testing.T) {
	p := setupKeyed(t, nil)

	var first mintedKey
	if err := json.Unmarshal(p.request(http.MethodPost, "/v1/environments/default/keys", p.alice, "", http.StatusCreated), &first); err != nil {
		t.Fatalf("the mint's answer did not decode: %v", err)
	}
	switch {
	case first.Token == "":
		t.Fatalf("the mint returned no key")
	case first.JTI == "":
		t.Errorf("the mint returned no jti, and the jti is what revokes the key")
	case first.Exp == "":
		t.Errorf("the mint returned no expiry")
	}
	if _, err := time.Parse(time.RFC3339, first.Exp); err != nil {
		t.Errorf("the expiry %q is not an instant: %v", first.Exp, err)
	}

	// The key names its environment and nothing else. It reaches the worker
	// routes of that environment and no route that decides on a subject.
	caller, err := p.verify(t, first.Token)
	if err != nil {
		t.Fatalf("the minted key does not verify: %v", err)
	}
	environment, isEnvironment := caller.Environment()
	if !isEnvironment || environment != "default" {
		t.Errorf("the key names %q as an environment (%t), want default", environment, isEnvironment)
	}
	p.request(http.MethodGet, "/v1/sandboxes", first.Token, "", http.StatusForbidden)

	// An environment holds several keys, so a worker and a gateway each
	// carry their own and one is revoked without ending the others.
	var second mintedKey
	if err = json.Unmarshal(p.request(http.MethodPost, "/v1/environments/default/keys", p.alice, "", http.StatusCreated), &second); err != nil {
		t.Fatalf("the second mint's answer did not decode: %v", err)
	}
	if second.JTI == first.JTI || second.Token == first.Token {
		t.Errorf("two mints returned one key")
	}

	// The revocation ends the first key on its next request and leaves the
	// second one working.
	p.request(http.MethodDelete, "/v1/environments/default/keys/"+first.JTI, p.alice, "", http.StatusNoContent)
	if _, err = p.verify(t, first.Token); err == nil {
		t.Errorf("the revoked key still verifies")
	}
	if _, err = p.verify(t, second.Token); err != nil {
		t.Errorf("revoking one key ended another: %v", err)
	}
	// A revocation is idempotent: a recovery that retried revokes a jti it
	// already revoked.
	p.request(http.MethodDelete, "/v1/environments/default/keys/"+first.JTI, p.alice, "", http.StatusNoContent)
}

// TestEnvironmentKeyRefusals holds who may mint and what may be minted for.
func TestEnvironmentKeyRefusals(t *testing.T) {
	p := setupKeyed(t, nil)
	for _, tc := range []struct {
		name   string
		method string
		path   string
		token  func() string
		status int
	}{
		{"a subject the owner policy does not make an admin", http.MethodPost,
			"/v1/environments/default/keys", func() string { return p.bob }, http.StatusForbidden},
		{"an environment this control plane does not hold", http.MethodPost,
			"/v1/environments/eu-gpu/keys", func() string { return p.alice }, http.StatusNotFound},
		{"a revocation on an environment this control plane does not hold", http.MethodDelete,
			"/v1/environments/eu-gpu/keys/01JABC", func() string { return p.alice }, http.StatusNotFound},
		{"no bearer at all", http.MethodPost,
			"/v1/environments/default/keys", func() string { return "" }, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.request(tc.method, tc.path, tc.token(), "", tc.status)
		})
	}
}

// TestEnvironmentKeyRoutesWithoutASigner holds what a control plane that
// signs nothing answers: the route is refused as a capability rather than
// failing somewhere below.
func TestEnvironmentKeyRoutesWithoutASigner(t *testing.T) {
	f := setup(t, nil)
	f.request(http.MethodPost, "/v1/environments/default/keys", f.alice, "", http.StatusUnprocessableEntity)
	f.request(http.MethodDelete, "/v1/environments/default/keys/01JABC", f.alice, "", http.StatusUnprocessableEntity)
}

// TestEnvironmentRoutes holds the read and the list of the kind.
func TestEnvironmentRoutes(t *testing.T) {
	p := setupKeyed(t, nil)
	var obj v1.Environment
	if err := json.Unmarshal(p.request(http.MethodGet, "/v1/environments/default", p.alice, "", http.StatusOK), &obj); err != nil {
		t.Fatalf("the environment did not decode: %v", err)
	}
	switch {
	case obj.Kind != v1.KindEnvironment:
		t.Errorf("the object is a %q", obj.Kind)
	case obj.Metadata.Name != "default":
		t.Errorf("the environment is named %q", obj.Metadata.Name)
	case obj.Spec.Mode != v1.EnvironmentInprocess:
		t.Errorf("the control plane's own environment is %q, want %q", obj.Spec.Mode, v1.EnvironmentInprocess)
	case obj.Status.Driver != "native":
		t.Errorf("the environment reports the driver %q, want native", obj.Status.Driver)
	case obj.Status.Phase != v1.EnvironmentReady:
		t.Errorf("the environment reports the phase %q", obj.Status.Phase)
	}

	var list struct {
		Items []v1.Environment `json:"items"`
	}
	if err := json.Unmarshal(p.request(http.MethodGet, "/v1/environments", p.alice, "", http.StatusOK), &list); err != nil {
		t.Fatalf("the list did not decode: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Metadata.Name != "default" {
		t.Errorf("the list is %v, want the one environment this control plane drives", list.Items)
	}
	p.request(http.MethodGet, "/v1/environments/eu-gpu", p.alice, "", http.StatusNotFound)
}

// TestWorkerRegistrationRoute is spec 021's registration: a worker with a
// valid key registers and receives a wrk_ id, and one whose driver or
// isolation class differs from the environment's is refused.
func TestWorkerRegistrationRoute(t *testing.T) {
	p := setupKeyed(t, nil)
	key := p.mintKey(t)

	body := `{"driver":"podman","isolation":"container","capabilities":{"egress":null,"files":true}}`
	var registered remote.Registered
	if err := json.Unmarshal(p.request(http.MethodPost, "/v1/environments/self/workers", key, body, http.StatusCreated), &registered); err != nil {
		t.Fatalf("the registration answer did not decode: %v", err)
	}
	if !strings.HasPrefix(registered.Worker, "wrk_") {
		t.Errorf("the worker id is %q, want a wrk_ id", registered.Worker)
	}
	if registered.Environment != "default" {
		t.Errorf("the registration names the environment %q", registered.Environment)
	}
	if registered.HeartbeatInterval == "" || registered.Lease == "" {
		t.Errorf("the registration names no heartbeat interval or lease: %+v", registered)
	}

	// One environment is one data plane: a second worker reporting another
	// driver is refused rather than admitted beside the first.
	mismatch := `{"driver":"k8s","isolation":"container"}`
	p.request(http.MethodPost, "/v1/environments/self/workers", key, mismatch, http.StatusUnprocessableEntity)
	isolation := `{"driver":"podman","isolation":"vm"}`
	p.request(http.MethodPost, "/v1/environments/self/workers", key, isolation, http.StatusUnprocessableEntity)
	// A registration naming no driver is a defect on the worker's side.
	p.request(http.MethodPost, "/v1/environments/self/workers", key, `{}`, http.StatusBadRequest)
	// A body the registration does not have is refused rather than ignored.
	p.request(http.MethodPost, "/v1/environments/self/workers", key, `{"driver":"podman","isolation":"container","pool":2}`, http.StatusBadRequest)
}

// TestEnvironmentKeyReachesItsOwnEnvironmentOnly holds the rule the gateway
// stream already applies: a key names one environment, and a path naming
// another is refused.
func TestEnvironmentKeyReachesItsOwnEnvironmentOnly(t *testing.T) {
	p := setupKeyed(t, nil)
	key := p.mintKey(t)
	body := `{"driver":"podman","isolation":"container"}`
	p.request(http.MethodPost, "/v1/environments/eu-gpu/workers", key, body, http.StatusForbidden)
	// A key reaches no route that decides on a subject, whichever it is.
	p.request(http.MethodGet, "/v1/environments", key, "", http.StatusForbidden)
	p.request(http.MethodPost, "/v1/environments/default/keys", key, "", http.StatusForbidden)
}

// TestRevokedKeyIsRefusedOnTheWorkerRoutes holds spec 021's rule that a key
// revoked through the route stops working at once, on the data plane routes
// as everywhere else.
func TestRevokedKeyIsRefusedOnTheWorkerRoutes(t *testing.T) {
	p := setupKeyed(t, nil)
	var key mintedKey
	if err := json.Unmarshal(p.request(http.MethodPost, "/v1/environments/default/keys", p.alice, "", http.StatusCreated), &key); err != nil {
		t.Fatalf("the mint's answer did not decode: %v", err)
	}
	body := `{"driver":"podman","isolation":"container"}`
	p.request(http.MethodPost, "/v1/environments/self/workers", key.Token, body, http.StatusCreated)
	p.request(http.MethodDelete, "/v1/environments/default/keys/"+key.JTI, p.alice, "", http.StatusNoContent)
	p.request(http.MethodPost, "/v1/environments/self/workers", key.Token, body, http.StatusUnauthorized)
	p.request(http.MethodGet, "/v1/environments/self/operations", key.Token, "", http.StatusUnauthorized)
}

// TestWorkerRoutesWithoutAHub holds what a control plane that serves no
// worker environment answers on the two routes an environment key reaches.
func TestWorkerRoutesWithoutAHub(t *testing.T) {
	p := setupKeyed(t, nil)
	key := p.mintKey(t)
	p.h.(*handler).Workers = nil
	p.request(http.MethodPost, "/v1/environments/self/workers", key, `{"driver":"native","isolation":"none"}`, http.StatusUnprocessableEntity)
}

// mintKey takes one environment key through the route, which is how a data
// plane is keyed in a deployment.
func (p *keyed) mintKey(t *testing.T) string {
	t.Helper()
	var key mintedKey
	if err := json.Unmarshal(p.request(http.MethodPost, "/v1/environments/default/keys", p.alice, "", http.StatusCreated), &key); err != nil {
		t.Fatalf("the mint's answer did not decode: %v", err)
	}
	return key.Token
}

// verify runs one bearer through the handler's own verifier, which is what
// every route reads and what the revocation list answers for.
func (p *keyed) verify(t *testing.T, token string) (auth.Caller, error) {
	t.Helper()
	return p.h.(*handler).Verifier.VerifyContext(t.Context(), token)
}
