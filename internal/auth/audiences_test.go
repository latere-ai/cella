// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"crypto/rsa"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/cella/internal/auth"
)

func TestAudienceSetIdentityEndToEnd(t *testing.T) {
	issuer := issuertest.New(t)
	identity, err := auth.Start(t.Context(), auth.Options{
		Issuers: []string{issuer.URL()}, Audience: "cella",
		Audiences: []string{"cella", "platform.example"},
		PublicURL: publicURL, TokenKeys: []*rsa.PrivateKey{key(t, 1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, audience := range []string{"cella", "platform.example", "unrelated"} {
		token := issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{audience}})
		_, err := identity.Verifier.Verify(token)
		if (err == nil) != (audience != "unrelated") {
			t.Fatalf("audience %q: %v", audience, err)
		}
	}
	token, err := identity.Signer.MintEnvironmentKey("env-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	caller, err := identity.Verifier.Verify(token.Value)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := caller.Environment(); !ok || id != "env-test" {
		t.Fatalf("caller = %+v", caller)
	}
	// The local verifier must not widen when an external platform audience is added.
	other, err := auth.NewSigner(auth.SignerOptions{Issuer: publicURL, Audience: "platform.example", Keys: []*rsa.PrivateKey{key(t, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := other.MintEnvironmentKey("env-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.Verifier.Verify(otherToken.Value); err == nil {
		t.Fatal("local token for external audience accepted")
	}
}

func TestVerifierRefusesEmptyAudienceEntry(t *testing.T) {
	if _, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{Issuers: []string{"https://issuer.example"}, Audience: "cella", Audiences: []string{" "}}); err == nil {
		t.Fatal("empty audience accepted")
	}
}
