// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit"
	authkitconformance "latere.ai/x/pkg/authkit/conformance"
	"latere.ai/x/pkg/authz/conformance"
	"latere.ai/x/pkg/authz/server"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/internal/auth"
)

// The two variables that point this tier at a running endpoint. Unset,
// every run below is against the stub authorizer of the shared contract,
// which is what a clean clone gets. Set, they are the endpoint a release
// run checks: the Cella section of a deployed control plane.
const (
	envAuthorizerURL   = "CELLA_TEST_AUTHORIZER_URL"
	envAuthorizerToken = "CELLA_TEST_AUTHORIZER_TOKEN"
)

// TestServiceConformance is rule R2 of the family's identity shape as a
// test: cellad verifies that the audience is its own, reads one
// identity, and calls the issuer for nothing but its discovery document
// and its key set. The authenticator under test is the one cellad runs
// in production, built the way the node builds it.
func TestServiceConformance(t *testing.T) {
	authkitconformance.Run(t, authkitconformance.Service{
		Audience: audience,
		New: func(tb testing.TB, issuerURL, _ string) authkit.Authenticator {
			tb.Helper()
			v, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{
				Issuers: []string{issuerURL}, Audience: audience,
			})
			if err != nil {
				tb.Fatalf("the verifier cellad runs would not build: %v", err)
			}
			return v.Authenticator()
		},
	})
}

// TestAuthorizerConformance is the other half: the endpoint cellad asks
// answers the shared contract, for every row of Cella's declared table.
// The suite runs against the stub of latere.ai/x/pkg/authz, and against
// whatever CELLA_TEST_AUTHORIZER_URL names when a release run sets it.
func TestAuthorizerConformance(t *testing.T) {
	url, token, named := endpointFromEnv()
	if !named {
		s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		url, token = s.URL(), s.Token()
	}
	conformance.Run(t, url, token,
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithSubjects(alice, bob))
}

// TestOwnerPolicyConformance: the policy cellad runs with no authorizer
// configured answers the same contract as an operator's endpoint, served
// through the scaffold of latere.ai/x/pkg/authz/server. Cella names no
// page action, so one Decider answers all thirty-two rows, the five
// lists included.
func TestOwnerPolicyConformance(t *testing.T) {
	const token = "conformance-bearer"
	h := server.New(server.Options{
		Bearer:     token,
		Vocabulary: authorizer.Vocabulary(),
		Decider:    policy(),
	})
	endpoint := httptest.NewServer(h)
	defer endpoint.Close()
	conformance.Run(t, endpoint.URL, token,
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithSubjects(alice, bob),
		conformance.WithHTTPClient(&http.Client{}))
}

// TestTheClientSpeaksToTheEndpointItIsPointedAt: the client cellad runs
// reaches the endpoint this tier is pointed at and reads its answers as
// decisions. Against the stub that is a local check; against a deployed
// endpoint it is the release run's.
func TestTheClientSpeaksToTheEndpointItIsPointedAt(t *testing.T) {
	url, token, named := endpointFromEnv()
	if !named {
		s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		url, token = s.URL(), s.Token()
	}
	a := asking(t, url, token, nil)
	if err := a.Check(t.Context()); err != nil {
		t.Fatalf("the endpoint did not deny the probe: %v", err)
	}
}

// endpointFromEnv reads the endpoint a release run points this tier at.
func endpointFromEnv() (url, token string, named bool) {
	url = strings.TrimSpace(os.Getenv(envAuthorizerURL))
	token = strings.TrimSpace(os.Getenv(envAuthorizerToken))
	return url, token, url != ""
}
