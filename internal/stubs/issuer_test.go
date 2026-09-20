// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package stubs_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/stubs"
)

// mint asks the issuer for a token for one subject, and for one audience
// where the caller names it. An empty aud is left out of the body rather
// than sent empty, because a token addressed to "" is addressed to
// something and the point of leaving it out is the issuer's default.
func mint(t *testing.T, issuer, sub, aud string) string {
	t.Helper()
	body := map[string]any{"sub": sub}
	if aud != "" {
		body["aud"] = aud
	}
	code, read := post(t, issuer+"/mint", body, nil)
	if code != http.StatusOK {
		t.Fatalf("the mint route answered %d: %s", code, read)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(read, &out); err != nil || out.Token == "" {
		t.Fatalf("the mint route answered %s", read)
	}
	return out.Token
}

// TestTheIssuerIsOneTheVerifierAccepts is the criterion the whole slice
// rests on: what this stub publishes is what internal/auth reads at start,
// and a token it mints is one the control plane admits. Both algorithms
// spec 006 names are driven, because an installation may run either.
func TestTheIssuerIsOneTheVerifierAccepts(t *testing.T) {
	for _, alg := range []string{stubs.AlgRS256, stubs.AlgES256} {
		t.Run(alg, func(t *testing.T) {
			s := start(t, stubs.Options{Issuer: stubs.IssuerOptions{Algorithm: alg}})
			issuer := s.URL(stubs.RoleIssuer)

			code, body := get(t, issuer+"/.well-known/openid-configuration")
			if code != http.StatusOK {
				t.Fatalf("the discovery document answered %d", code)
			}
			var discovery struct {
				Issuer string `json:"issuer"`
				JWKS   string `json:"jwks_uri"`
			}
			if err := json.Unmarshal(body, &discovery); err != nil {
				t.Fatalf("the discovery document is %s", body)
			}
			if discovery.Issuer != issuer {
				t.Errorf("the document names the issuer %q and is served at %q; a verifier refuses the pair", discovery.Issuer, issuer)
			}
			if code, _ := get(t, discovery.JWKS); code != http.StatusOK {
				t.Fatalf("the key set the document names answered %d", code)
			}

			v, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{
				Issuers: []string{issuer}, Audience: stubs.DefaultAudience,
			})
			if err != nil {
				t.Fatalf("the verifier refused the issuer: %v", err)
			}
			caller, err := v.Verify(mint(t, issuer, "dev", ""))
			if err != nil {
				t.Fatalf("the verifier refused a token this issuer minted: %v", err)
			}
			if want := issuer + "|dev"; caller.Subject != want {
				t.Errorf("the token renders to %q, want %q", caller.Subject, want)
			}
		})
	}
}

// TestTheIssuerMintsWhatItIsAskedFor: a tier proves that cellad refuses a
// reserved subject and a wrong audience, so the issuer mints both rather
// than deciding either.
func TestTheIssuerMintsWhatItIsAskedFor(t *testing.T) {
	s := start(t, stubs.Options{})
	issuer := s.URL(stubs.RoleIssuer)
	v, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{
		Issuers: []string{issuer}, Audience: stubs.DefaultAudience,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	// The stub signs a reserved subject, because refusing one is cellad's
	// answer and not the issuer's: a suite proves the refusal by holding
	// the token it refuses.
	if _, err := v.Verify(mint(t, issuer, "sandbox:sbx_1", "")); err == nil {
		t.Error("cellad accepted an issuer's token whose sub is one of the identities it mints itself")
	}
	if _, err := v.Verify(mint(t, issuer, "dev", "somebody-else")); err == nil {
		t.Error("a token addressed to another audience verified")
	}
}

// TestTheIssuerNamesTheAddressItIsGiven: a stub another container reaches
// signs the URL its callers dial and not the one its listener reports.
func TestTheIssuerNamesTheAddressItIsGiven(t *testing.T) {
	const url = "http://cella-stubs.example:9080"
	s := start(t, stubs.Options{Issuer: stubs.IssuerOptions{URL: url + "/"}})
	code, body := get(t, s.URL(stubs.RoleIssuer)+"/.well-known/openid-configuration")
	if code != http.StatusOK {
		t.Fatalf("the discovery document answered %d", code)
	}
	var discovery struct {
		Issuer string `json:"issuer"`
		JWKS   string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &discovery); err != nil {
		t.Fatal(err)
	}
	if discovery.Issuer != url || discovery.JWKS != url+"/jwks" {
		t.Errorf("the document names %q and %q; the issuer given was %q with its trailing slash", discovery.Issuer, discovery.JWKS, url)
	}
}
