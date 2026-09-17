// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/internal/auth"
)

// options is a sound identity: one stub issuer, one signing key, and no
// authorizer, which is the owner policy.
func options(t *testing.T, ks ...*rsa.PrivateKey) auth.Options {
	t.Helper()
	if len(ks) == 0 {
		ks = []*rsa.PrivateKey{key(t, 1)}
	}
	return auth.Options{
		Issuers:            []string{issuertest.New(t, issuertest.WithDefaultAudience(audience)).URL()},
		Audience:           audience,
		PublicURL:          publicURL,
		TokenKeys:          ks,
		DefaultEnvironment: defaultEnvironment,
	}
}

// TestStartSelectsTheOwnerPolicy: with no endpoint configured the
// built-in policy decides, and the node says so.
func TestStartSelectsTheOwnerPolicy(t *testing.T) {
	o := options(t)
	o.AdminSubjects = []string{alice}
	id, err := auth.Start(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if id.Mode != auth.ModeOwnerPolicy {
		t.Fatalf("Mode = %q, want %q", id.Mode, auth.ModeOwnerPolicy)
	}
	if err := id.Authorizer.Check(t.Context()); err != nil {
		t.Fatalf("the owner policy did not deny the probe: %v", err)
	}
	if got := id.Verifier.Issuers(); len(got) != 1 || got[0] != o.Issuers[0] {
		t.Errorf("the verifier verifies for %q, want %q", got, o.Issuers)
	}
	if id.Verifier.Audience() != audience {
		t.Errorf("the verifier accepts the audience %q", id.Verifier.Audience())
	}
	if got := id.Signer.KeyIDs(); len(got) != 1 || got[0] != id.Signer.KeyID() {
		t.Errorf("the published kids are %q and the signing kid is %q", got, id.Signer.KeyID())
	}

	// An admin acts on another subject's object; a stranger does not.
	d, err := id.Authorizer.Decide(t.Context(), caller(alice, "alice", nil), info,
		authorizer.ActionSandboxDelete, auth.Sandbox{ID: "sbx_01J9", Owner: bob}.Resource())
	if err != nil {
		t.Fatalf("CELLA_ADMIN_SUBJECTS did not reach the policy: %v", err)
	}
	if d.Filter != nil {
		t.Errorf("a delete answered a filter: %+v", d.Filter)
	}
}

// TestStartSelectsTheAuthorizer: with an endpoint configured every
// decision is one call to it, and the owner policy is not consulted.
func TestStartSelectsTheAuthorizer(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	o := options(t)
	o.AuthorizerURL, o.AuthorizerToken = s.URL(), s.Token()
	o.AdminSubjects = []string{alice}
	id, err := auth.Start(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if id.Mode != auth.ModeAuthorizer {
		t.Fatalf("Mode = %q, want %q", id.Mode, auth.ModeAuthorizer)
	}
	if err := id.Authorizer.Check(t.Context()); err != nil {
		t.Fatalf("the stub did not deny the probe: %v", err)
	}
	// CELLA_ADMIN_SUBJECTS is read and unused: the endpoint decides,
	// and it denies the subject the owner policy would have allowed.
	s.SetRules(stub.Rule{Allow: false, Reason: "the endpoint said so"})
	_, err = id.Authorizer.Decide(t.Context(), caller(alice, "alice", nil), info,
		authorizer.ActionSandboxRead, auth.Sandbox{ID: "sbx_01J9", Owner: alice}.Resource())
	if auth.CodeOf(err) != auth.CodeForbidden {
		t.Fatalf("err = %v; with an endpoint set it decides and the admin list does not", err)
	}
}

// TestStartRefusals: every start-up failure spec 006 names is reported
// by the one call that wires identity.
func TestStartRefusals(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	for _, tc := range []struct {
		name string
		edit func(*auth.Options)
		want string
	}{{
		name: "no signing key",
		edit: func(o *auth.Options) { o.TokenKeys = nil },
		want: "CELLA_TOKEN_KEY",
	}, {
		name: "no public URL",
		edit: func(o *auth.Options) { o.PublicURL = "" },
		want: "CELLA_PUBLIC_URL",
	}, {
		name: "no issuer",
		edit: func(o *auth.Options) { o.Issuers = nil },
		want: "CELLA_OIDC_ISSUERS",
	}, {
		name: "an issuer that does not answer",
		edit: func(o *auth.Options) { o.Issuers = []string{deadURL} },
		want: "CELLA_OIDC_ISSUERS",
	}, {
		name: "no audience",
		edit: func(o *auth.Options) { o.Audience = "" },
		want: "CELLA_OIDC_AUDIENCE",
	}, {
		name: "an authorizer with no bearer",
		edit: func(o *auth.Options) { o.AuthorizerURL = "https://authz.example.com/decide" },
		want: "CELLA_AUTHORIZER_TOKEN is unset",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			o := options(t)
			tc.edit(&o)
			_, err := auth.Start(t.Context(), o)
			if err == nil {
				t.Fatal("Start() built an identity out of a configuration spec 006 refuses")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a message naming %q", err, tc.want)
			}
		})
	}
}

// TestAuthenticateReadsTheBearer: a request with no bearer is
// unauthenticated, because there is no anonymous access and no API key.
func TestAuthenticateReadsTheBearer(t *testing.T) {
	s := issuertest.New(t, issuertest.WithDefaultAudience(audience))
	id, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{s.URL()}, Audience: audience,
		PublicURL: publicURL, TokenKeys: []*rsa.PrivateKey{key(t, 1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	token := s.Mint(issuertest.Claims{Sub: "alice"})

	r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	c, err := id.Verifier.Authenticate(r)
	if err != nil {
		t.Fatalf("a request carrying a sound bearer was refused: %v", err)
	}
	if c.Subject != authz.Subject(s.URL(), "alice") {
		t.Errorf("the caller is %q", c.Subject)
	}

	for _, header := range []string{"", "Basic abc", "Bearer ", "Bearer not-a-token"} {
		r := httptest.NewRequest(http.MethodGet, "/v1/sandboxes", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if _, err := id.Verifier.Authenticate(r); auth.CodeOf(err) != auth.CodeUnauthenticated {
			t.Errorf("the header %q answered %v, want unauthenticated", header, err)
		}
	}
}

// TestADenyWithNoReasonStillSaysSomething: an endpoint that denies and
// names nothing still gives a developer a line to read.
func TestADenyWithNoReasonStillSaysSomething(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	s.SetRules(stub.Rule{Allow: false})
	a := asking(t, s.URL(), s.Token(), nil)
	_, err := a.Decide(t.Context(), caller(alice, "alice", nil), info,
		authorizer.ActionSandboxRead, auth.Sandbox{ID: "sbx_01J9", Owner: bob}.Resource())
	if auth.CodeOf(err) != auth.CodeForbidden {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "named no reason") {
		t.Errorf("err = %v, want a detail that says the endpoint named none", err)
	}
}

// TestTheDefaultEnvironmentIsMatchedByNameOrByID: an operator writes the
// name, and the control plane knows the id.
func TestTheDefaultEnvironmentIsMatchedByNameOrByID(t *testing.T) {
	p := policy()
	for _, res := range []authz.Resource{
		auth.Environment{ID: "env_01J9", Name: defaultEnvironment}.Resource(),
		auth.Environment{ID: defaultEnvironment}.Resource(),
	} {
		if d := decide(t, p, bob, authorizer.ActionEnvironmentUse, res); !d.Allow {
			t.Errorf("the default environment named %+v was refused: %s", res, d.Reason)
		}
	}
	unnamed := &auth.OwnerPolicy{Admins: []string{alice}}
	res := auth.Environment{ID: "env_01J9", Name: defaultEnvironment}.Resource()
	if d := decide(t, unnamed, bob, authorizer.ActionEnvironmentUse, res); d.Allow {
		t.Error("with CELLA_DEFAULT_ENVIRONMENT unset an environment was still every subject's")
	}
}
