// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authkit/jwt"
	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/internal/auth"
)

const audience = "cella"

// newVerifier builds the verifier under test against the given issuers.
func newVerifier(t *testing.T, issuers ...string) *auth.Verifier {
	t.Helper()
	v, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{Issuers: issuers, Audience: audience})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestVerifierRefusals is spec 006's first row. A token from a listed
// issuer with the right audience is accepted, signed RS256 or ES256;
// every other shape is unauthenticated, and a sub carrying a reserved
// prefix is refused however well the token is signed.
func TestVerifierRefusals(t *testing.T) {
	rsa := issuertest.New(t, issuertest.WithDefaultAudience(audience))
	ec := issuertest.New(t, issuertest.WithES256(), issuertest.WithDefaultAudience(audience))
	other := issuertest.New(t, issuertest.WithDefaultAudience(audience))
	v := newVerifier(t, rsa.URL(), ec.URL())

	for _, tc := range []struct {
		name  string
		token string
		want  string
	}{{
		name:  "an RS256 token of a listed issuer",
		token: rsa.Mint(issuertest.Claims{Sub: "alice"}),
	}, {
		name:  "an ES256 token of a listed issuer",
		token: ec.Mint(issuertest.Claims{Sub: "alice"}),
	}, {
		name:  "a token of an issuer that is not listed",
		token: other.Mint(issuertest.Claims{Sub: "alice"}),
		want:  "which CELLA_OIDC_ISSUERS does not list",
	}, {
		name:  "a token for another audience",
		token: rsa.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"luxd"}}),
		want:  "audience",
	}, {
		name:  "a token with no audience at all",
		token: rsa.Mint(issuertest.Claims{Sub: "alice", Omit: []string{"aud"}}),
		want:  "audience",
	}, {
		name:  "a token that has expired",
		token: rsa.Mint(issuertest.Claims{Sub: "alice", Exp: time.Now().Add(-time.Minute).Unix()}),
		want:  "expired",
	}, {
		name:  "a token with no exp at all",
		token: rsa.Mint(issuertest.Claims{Sub: "alice", Omit: []string{"exp"}}),
		want:  "expired",
	}, {
		name:  "a token minted more than a day ago",
		token: rsa.Mint(issuertest.Claims{Sub: "alice", Iat: time.Now().Add(-25 * time.Hour).Unix(), Exp: time.Now().Add(time.Hour).Unix()}),
		want:  ": " + string(jwt.ReasonTooOld) + ":",
	}, {
		name:  "a token that stamps no iat at all",
		token: rsa.Mint(issuertest.Claims{Sub: "alice", Omit: []string{"iat"}}),
		want:  ": " + string(jwt.ReasonTooOld) + ":",
	}, {
		name:  "a token not yet valid",
		token: rsa.Mint(issuertest.Claims{Sub: "alice", Nbf: time.Now().Add(time.Hour).Unix()}),
		want:  "nbf",
	}, {
		name:  "a token naming nobody",
		token: rsa.Mint(issuertest.Claims{Omit: []string{"sub"}}),
		want:  "malformed",
	}, {
		name:  "an unsigned token",
		token: unsigned(t, rsa.URL(), "alice"),
		want:  "signature",
	}, {
		name:  "a token signed with an algorithm outside the two",
		token: hs256(t, rsa.URL(), "alice"),
		want:  "signature",
	}, {
		name:  "a token whose signature does not check out",
		token: tamper(rsa.Mint(issuertest.Claims{Sub: "alice"})),
		want:  "signature",
	}, {
		name:  "an issuer that stamps a sandbox's reserved prefix",
		token: rsa.Mint(issuertest.Claims{Sub: "sandbox:sbx_01J9"}),
		want:  `"sandbox:" is reserved for the identities cellad mints`,
	}, {
		name:  "an issuer that stamps an environment's reserved prefix",
		token: rsa.Mint(issuertest.Claims{Sub: "environment:env_01J9"}),
		want:  `"environment:" is reserved for the identities cellad mints`,
	}, {
		name:  "a bearer that is no token",
		token: "not-a-token",
		want:  "not a JWS any issuer could have signed",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := v.Verify(tc.token)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Verify() refused a sound token: %v", err)
				}
				if c.Sub != "alice" || c.Subject == "" {
					t.Fatalf("Verify() = %+v, want alice", c)
				}
				return
			}
			if err == nil {
				t.Fatalf("Verify() accepted the token as %+v", c)
			}
			if code := auth.CodeOf(err); code != auth.CodeUnauthenticated {
				t.Fatalf("Verify() refused with %q, want %q", code, auth.CodeUnauthenticated)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Verify() = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// TestTokenAgeIsBoundedForIssuersAndNotForEnvironmentKeys is the
// family's age bound, which spec 006's amendment of 2026-09-17 names.
// A listed issuer's token is refused once its iat is older than
// jwt.DefaultMaxTokenAge, a day, whatever exp it carries, which is the
// rule Origo and Lux run too; a caller that wants a longer-lived
// credential re-mints it at its issuer. An environment key is bounded
// by exp alone, because it lives CELLA_ENVIRONMENT_KEY_TTL, a year by
// default, and a worker holds it for that long. A caller's token that
// stamps no iat is refused for the same reason the bound exists: an age
// no one can read is not an age within it.
func TestTokenAgeIsBoundedForIssuersAndNotForEnvironmentKeys(t *testing.T) {
	s := issuertest.New(t, issuertest.WithDefaultAudience(audience))
	v := newVerifier(t, s.URL())

	fresh := s.Mint(issuertest.Claims{
		Sub: "alice",
		Iat: time.Now().Add(-time.Hour).Unix(),
		Exp: time.Now().Add(90 * 24 * time.Hour).Unix(),
	})
	c, err := v.Verify(fresh)
	if err != nil {
		t.Fatalf("a token minted an hour ago and good for three months was refused: %v", err)
	}
	if c.Sub != "alice" {
		t.Errorf("the caller is %q", c.Sub)
	}

	old := s.Mint(issuertest.Claims{
		Sub: "alice",
		Iat: time.Now().Add(-25 * time.Hour).Unix(),
		Exp: time.Now().Add(90 * 24 * time.Hour).Unix(),
	})
	_, err = v.Verify(old)
	if err == nil {
		t.Fatal("a token minted 25 hours ago was accepted, and the bound is a day")
	}
	if want := ": " + string(jwt.ReasonTooOld) + ":"; !strings.Contains(err.Error(), want) {
		t.Errorf("Verify() = %v, want a refusal naming %q", err, want)
	}
	if code := auth.CodeOf(err); code != auth.CodeUnauthenticated {
		t.Errorf("Verify() refused with %q, want %q", code, auth.CodeUnauthenticated)
	}

	none := s.Mint(issuertest.Claims{Sub: "alice", Omit: []string{"iat"}})
	_, err = v.Verify(none)
	if err == nil {
		t.Fatal("a token that stamps no iat was accepted, and an age no one can read is not an age within the bound")
	}
	if want := ": " + string(jwt.ReasonTooOld) + ":"; !strings.Contains(err.Error(), want) {
		t.Errorf("Verify() = %v, want a refusal naming %q", err, want)
	}

	signer := newSignerMintingAt(t, time.Now().Add(-30*24*time.Hour))
	local := verifierFor(t, signer)
	envKey, err := signer.MintEnvironmentKey("env_01J9", 8760*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := local.Verify(envKey.Value)
	if err != nil {
		t.Fatalf("an environment key minted 30 days ago and good for eleven months was refused: %v", err)
	}
	if id, ok := worker.Environment(); !ok || id != "env_01J9" {
		t.Errorf("the caller reads as environment %q %v", id, ok)
	}
}

// TestVerifierForwardsEveryClaimVerbatim: the claims reach the caller as
// the token carried them, the ones the control plane has no type for
// included, because the authorizer is what reads them.
func TestVerifierForwardsEveryClaimVerbatim(t *testing.T) {
	s := issuertest.New(t, issuertest.WithDefaultAudience(audience))
	v := newVerifier(t, s.URL())
	token := s.Mint(issuertest.Claims{Sub: "alice", Email: "alice@example.com", Extra: map[string]any{
		"groups": []any{"research"}, "plan": "team", "org_id": "org_01J9",
	}})
	c, err := v.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"iss", "sub", "aud", "exp", "iat", "email", "groups", "plan", "org_id"} {
		if _, ok := c.Claims[name]; !ok {
			t.Errorf("the claim %q did not reach the caller; every claim is forwarded verbatim", name)
		}
	}
	if got, _ := c.Claims["plan"].(string); got != "team" {
		t.Errorf("the claim the control plane has no type for read as %q", got)
	}
}

// TestSubjectsAreIssuerQualified is spec 006's second row: the same sub
// from two listed issuers is two subjects, and an admin entry matches
// one and not the other.
func TestSubjectsAreIssuerQualified(t *testing.T) {
	one := issuertest.New(t, issuertest.WithDefaultAudience(audience))
	two := issuertest.New(t, issuertest.WithDefaultAudience(audience))
	v := newVerifier(t, one.URL(), two.URL())

	first, err := v.Verify(one.Mint(issuertest.Claims{Sub: "alice"}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := v.Verify(two.Mint(issuertest.Claims{Sub: "alice"}))
	if err != nil {
		t.Fatal(err)
	}
	if first.Sub != second.Sub {
		t.Fatalf("the two tokens name %q and %q; the test needs one sub", first.Sub, second.Sub)
	}
	if first.Subject == second.Subject {
		t.Fatalf("both rendered as %q; two issuers that agree on a sub are two subjects", first.Subject)
	}
	if want := one.URL() + "|alice"; first.Subject != want {
		t.Errorf("the rendered subject is %q, want %q", first.Subject, want)
	}
	admins := []string{first.Subject}
	if !slices.Contains(admins, first.Subject) {
		t.Error("the admin entry does not match the subject it was written from")
	}
	if slices.Contains(admins, second.Subject) {
		t.Error("the admin entry matches the other issuer's subject of the same name")
	}
}

// TestIssuerAtStartAndLater is spec 006's third row. An issuer that does
// not answer at start is a start-up failure, and one that stops
// answering later is served from the key set already cached, so the
// control plane degrades to refusing new keys rather than every request.
func TestIssuerAtStartAndLater(t *testing.T) {
	t.Run("unreachable at start", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := dead.URL
		dead.Close()
		_, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{
			Issuers: []string{url}, Audience: audience, FetchTimeout: time.Second,
		})
		if err == nil {
			t.Fatal("NewVerifier() built a verifier against an issuer that does not answer")
		}
		if !strings.Contains(err.Error(), "CELLA_OIDC_ISSUERS") {
			t.Fatalf("err = %v, want a message naming the variable", err)
		}
	})

	t.Run("a discovery document that names another issuer", func(t *testing.T) {
		var url string
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": "https://elsewhere.example.com", "jwks_uri": url + "/jwks"})
		}))
		defer s.Close()
		url = s.URL
		_, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{Issuers: []string{url}, Audience: audience})
		if err == nil || !strings.Contains(err.Error(), "names the issuer") {
			t.Fatalf("err = %v, want a refusal of a document that names another issuer", err)
		}
	})

	t.Run("a key set with no key the two algorithms can use", func(t *testing.T) {
		var url string
		mux := http.NewServeMux()
		mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": url, "jwks_uri": url + "/jwks"})
		})
		mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "OKP", "crv": "Ed25519"}}})
		})
		s := httptest.NewServer(mux)
		defer s.Close()
		url = s.URL
		_, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{Issuers: []string{url}, Audience: audience})
		if err == nil || !strings.Contains(err.Error(), "none of them is an RS256 or an ES256 key") {
			t.Fatalf("err = %v, want a refusal of a key set the two algorithms cannot use", err)
		}
	})

	t.Run("an issuer that stops answering is served from the cached set", func(t *testing.T) {
		s := issuertest.New(t, issuertest.WithDefaultAudience(audience))
		v, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{
			Issuers: []string{s.URL()}, Audience: audience,
			CacheTTL: time.Millisecond,
			HTTP:     &http.Client{Timeout: 200 * time.Millisecond},
		})
		if err != nil {
			t.Fatal(err)
		}
		token := s.Mint(issuertest.Claims{Sub: "alice"})
		if _, err := v.Verify(token); err != nil {
			t.Fatalf("the first call, with the issuer up: %v", err)
		}
		s.Hang()
		defer s.Resume()
		time.Sleep(5 * time.Millisecond)
		if _, err := v.Verify(token); err != nil {
			t.Fatalf("the issuer went away and the cached key set was not used: %v", err)
		}
	})
}

// unsigned builds a token whose header names the none algorithm, the
// oldest way to present a token nobody signed.
func unsigned(t *testing.T, iss, sub string) string {
	t.Helper()
	return segment(t, map[string]any{"alg": "none", "typ": "JWT"}) + "." + claimSegment(t, iss, sub) + "."
}

// hs256 builds a token signed with a shared secret, an algorithm outside
// the two the family verifies.
func hs256(t *testing.T, iss, sub string) string {
	t.Helper()
	signing := segment(t, map[string]any{"alg": "HS256", "typ": "JWT"}) + "." + claimSegment(t, iss, sub)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write([]byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// tamper flips the first character of a token's signature, which every
// bit of reaches the verification.
func tamper(token string) string {
	i := strings.LastIndex(token, ".") + 1
	if token[i] == 'A' {
		return token[:i] + "B" + token[i+1:]
	}
	return token[:i] + "A" + token[i+1:]
}

func claimSegment(t *testing.T, iss, sub string) string {
	t.Helper()
	return segment(t, map[string]any{
		"iss": iss, "sub": sub, "aud": []string{audience},
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	})
}

func segment(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// pat is a personal access token of a listed issuer, narrowed to one
// action on one sandbox: the credential class in token_use and the
// grants its holder chose in RFC 9396's authorization_details.
func pat(iss *issuertest.Server, grants ...map[string]any) string {
	claims := map[string]any{"token_use": "pat"}
	if grants != nil {
		claims["authorization_details"] = grants
	}
	return iss.Mint(issuertest.Claims{Sub: "alice", Extra: claims})
}

// grant is one authorization_details entry, as auth mints it.
func grant(action, kind, id string) map[string]any {
	return map[string]any{
		"type": "latere-authz", "actions": []any{action},
		"datatypes": []any{kind}, "locations": []any{"https://api.latere.ai"},
		"identifier": id,
	}
}

// TestVerifierReadsTheGrantsAPersonalAccessTokenCarries is id-13 at
// cellad's door. A personal access token is narrower than the person who
// holds it, and the claim says what the credential may *not* do, so a
// validator that does not read it grants more than its holder asked for.
// latere.ai/x/pkg/authkit/jwt refuses such a token with grants_unread
// until the service promises to read it, and cellad promises: the
// intersection is applied at the decision point, which is what
// TestOwnerPolicyNarrowsByTheGrants holds it to.
//
// The claims reach the authorizer verbatim, so the grants a decision
// point reads are the ones the issuer signed.
func TestVerifierReadsTheGrantsAPersonalAccessTokenCarries(t *testing.T) {
	iss := issuertest.New(t, issuertest.WithDefaultAudience(audience))
	v := newVerifier(t, iss.URL())

	c, err := v.Verify(pat(iss, grant("cella:sandbox.read", "Sandbox", "sbx_01J9")))
	if err != nil {
		t.Fatalf("a personal access token carrying its grants was refused: %v", err)
	}
	if use := c.Claims["token_use"]; use != "pat" {
		t.Errorf("the caller's claims carry token_use %v; the credential class reaches the decision point verbatim", use)
	}
	grants, err := authz.ParseGrants(c.Claims)
	if err != nil {
		t.Fatalf("the claims the verifier handed on are no grants: %v", err)
	}
	if len(grants) != 1 || len(grants[0].Actions) != 1 ||
		grants[0].Actions[0] != "cella:sandbox.read" || grants[0].Identifier != "sbx_01J9" {
		t.Fatalf("the decision point reads %+v; it reads the grant the issuer signed", grants)
	}

	// A personal access token with no grants at all is a token nobody
	// wrote a grant for. The door does not refuse it, because there is no
	// claim to leave unread; the decision point denies it, which is
	// TestOwnerPolicyDeniesAGrantlessPAT.
	if _, err := v.Verify(pat(iss)); err != nil {
		t.Errorf("a personal access token carrying no grants was refused at the door: %v", err)
	}
}
