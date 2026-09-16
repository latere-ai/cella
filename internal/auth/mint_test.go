// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/cella/internal/auth"
)

const publicURL = "https://cella.example.com"

// keys holds the generated signing keys, because a 2048-bit RSA key
// costs more than every assertion in this package together.
var keys struct {
	sync.Mutex
	list []*rsa.PrivateKey
}

// key is the nth generated signing key, counting from one.
func key(t *testing.T, n int) *rsa.PrivateKey {
	t.Helper()
	keys.Lock()
	defer keys.Unlock()
	for len(keys.list) < n {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		keys.list = append(keys.list, k)
	}
	return keys.list[n-1]
}

// newSigner builds a signer over the given keys, in the order given.
func newSigner(t *testing.T, ks ...*rsa.PrivateKey) *auth.Signer {
	t.Helper()
	s, err := auth.NewSigner(auth.SignerOptions{Issuer: publicURL, Audience: audience, Keys: ks})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestWorkloadTokenIsVerifiable is spec 006's row: a workload token
// verifies with a generic JWT library against /.well-known/jwks.json,
// and its kid is the RFC 7638 thumbprint. The check here uses the
// standard library alone, from the served key set, so nothing cellad
// mints is trusted because cellad minted it.
func TestWorkloadTokenIsVerifiable(t *testing.T) {
	s := newSigner(t, key(t, 1))
	tok, err := s.MintWorkload(auth.Workload{
		Sandbox: "sbx_01J9", Environment: "env_01J9",
		Spawn: &auth.Spawn{Budget: 4, Depth: 1, Mesh: "msh_01J9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	served := servedKeys(t, s)
	header, payload := verify(t, tok.Value, served)

	if header["kid"] != auth.Thumbprint(&key(t, 1).PublicKey) {
		t.Errorf("the kid is %q, want the RFC 7638 thumbprint %q", header["kid"], auth.Thumbprint(&key(t, 1).PublicKey))
	}
	if header["alg"] != "RS256" {
		t.Errorf("alg = %q, want RS256", header["alg"])
	}
	if payload["iss"] != publicURL {
		t.Errorf("iss = %v, want CELLA_PUBLIC_URL", payload["iss"])
	}
	if payload["sub"] != "sandbox:sbx_01J9" {
		t.Errorf("sub = %v, want the sandbox's reserved prefix", payload["sub"])
	}
	if payload["environment"] != "env_01J9" {
		t.Errorf("environment = %v", payload["environment"])
	}
	if aud, _ := payload["aud"].([]any); len(aud) != 1 || aud[0] != audience {
		t.Errorf("aud = %v, want [%s]", payload["aud"], audience)
	}
	if payload["jti"] != tok.JTI {
		t.Errorf("the jti in the token is %v and the one reported is %q; a revocation keys on one of them", payload["jti"], tok.JTI)
	}
	if spawn, _ := payload["spawn"].(map[string]any); spawn == nil || spawn["budget"] != float64(4) {
		t.Errorf("spawn = %v, want the budget the sandbox was minted with", payload["spawn"])
	}
}

// TestWorkloadTokenExpiresWithItsSandbox: the expiry is the sandbox's
// own or twenty-four hours from the mint, whichever is sooner.
func TestWorkloadTokenExpiresWithItsSandbox(t *testing.T) {
	s := newSigner(t, key(t, 1))
	for _, tc := range []struct {
		name  string
		given time.Duration
		want  time.Duration
	}{
		{"a sandbox that outlives the cap", 72 * time.Hour, auth.WorkloadTokenLifetime},
		{"a sandbox that expires first", time.Hour, time.Hour},
		{"a sandbox that never expires", 0, auth.WorkloadTokenLifetime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := auth.Workload{Sandbox: "sbx_01J9"}
			if tc.given != 0 {
				w.ExpiresAt = time.Now().Add(tc.given)
			}
			tok, err := s.MintWorkload(w)
			if err != nil {
				t.Fatal(err)
			}
			if d := time.Until(tok.ExpiresAt).Round(time.Minute); d != tc.want {
				t.Errorf("the token expires in %v, want %v", d, tc.want)
			}
		})
	}
}

// TestKeyRotationByConfiguration is spec 006's row: with two PEM blocks,
// tokens signed by the second still verify; after the block is removed
// they do not; and the first block signs.
func TestKeyRotationByConfiguration(t *testing.T) {
	old, fresh := key(t, 1), key(t, 2)

	before := newSigner(t, old)
	signedByOld, err := before.MintWorkload(auth.Workload{Sandbox: "sbx_OLD"})
	if err != nil {
		t.Fatal(err)
	}

	// The operator prepends the new key. The overlap: two blocks, the
	// first signing and the second still verifying.
	during := newSigner(t, fresh, old)
	signedByNew, err := during.MintWorkload(auth.Workload{Sandbox: "sbx_NEW"})
	if err != nil {
		t.Fatal(err)
	}
	if during.KeyID() != auth.Thumbprint(&fresh.PublicKey) {
		t.Error("the signing key is not the first block; the first block signs")
	}
	if got := len(servedKeys(t, during)); got != 2 {
		t.Errorf("the key set serves %d key(s); both blocks are published during an overlap", got)
	}

	v := verifierFor(t, during)
	for _, tok := range []auth.Token{signedByOld, signedByNew} {
		if _, err := v.Verify(tok.Value); err != nil {
			t.Errorf("during the overlap a token was refused: %v", err)
		}
	}

	// The operator removes the old block once every token it signed has
	// expired. The tokens it signed stop verifying at that moment.
	after := verifierFor(t, newSigner(t, fresh))
	if _, err := after.Verify(signedByNew.Value); err != nil {
		t.Errorf("after the rotation the new key's token was refused: %v", err)
	}
	if _, err := after.Verify(signedByOld.Value); err == nil {
		t.Error("after the old block was removed its token still verified")
	} else if auth.CodeOf(err) != auth.CodeUnauthenticated {
		t.Errorf("the refusal is %q, want %q", auth.CodeOf(err), auth.CodeUnauthenticated)
	}
}

// TestMintedTokensCarryTheirOwnSubject: an environment key names its
// environment and a workload token its sandbox, and the verifier reads
// each back as what it is.
func TestMintedTokensCarryTheirOwnSubject(t *testing.T) {
	s := newSigner(t, key(t, 1))
	v := verifierFor(t, s)

	work, err := s.MintWorkload(auth.Workload{Sandbox: "sbx_01J9"})
	if err != nil {
		t.Fatal(err)
	}
	caller, err := v.Verify(work.Value)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := caller.Sandbox(); !ok || id != "sbx_01J9" {
		t.Errorf("the caller reads as sandbox %q %v", id, ok)
	}
	if _, ok := caller.Environment(); ok {
		t.Error("a workload token read as an environment key")
	}
	if caller.Subject != "sandbox:sbx_01J9" {
		t.Errorf("the rendered subject is %q; a minted token's subject is its bare sub", caller.Subject)
	}

	envKey, err := s.MintEnvironmentKey("env_01J9", 8760*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	caller, err = v.Verify(envKey.Value)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := caller.Environment(); !ok || id != "env_01J9" {
		t.Errorf("the caller reads as environment %q %v", id, ok)
	}
	if _, ok := caller.Sandbox(); ok {
		t.Error("an environment key read as a workload token")
	}
}

// TestTwoKeysOnOneEnvironmentAreTwoJTIs: an environment may hold several
// keys so that each worker on it carries its own, which means each mint
// is a jti of its own.
func TestTwoKeysOnOneEnvironmentAreTwoJTIs(t *testing.T) {
	s := newSigner(t, key(t, 1))
	seen := map[string]bool{}
	for range 64 {
		tok, err := s.MintEnvironmentKey("env_01J9", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if len(tok.JTI) != 26 {
			t.Fatalf("the jti %q is %d characters; a ULID is 26", tok.JTI, len(tok.JTI))
		}
		if seen[tok.JTI] {
			t.Fatalf("the jti %q was minted twice", tok.JTI)
		}
		seen[tok.JTI] = true
	}
}

// TestSignerRefusalsAreNamedAtStart: a signer with no key or no issuer
// is a wiring mistake, and a mint with no subject is a bug in a caller.
func TestSignerRefusalsAreNamedAtStart(t *testing.T) {
	if _, err := auth.NewSigner(auth.SignerOptions{Issuer: publicURL, Audience: audience}); err == nil {
		t.Error("NewSigner() built a signer with no key")
	}
	if _, err := auth.NewSigner(auth.SignerOptions{Audience: audience, Keys: []*rsa.PrivateKey{key(t, 1)}}); err == nil {
		t.Error("NewSigner() built a signer with no issuer")
	}
	s := newSigner(t, key(t, 1))
	if _, err := s.MintWorkload(auth.Workload{}); err == nil {
		t.Error("MintWorkload() minted a token for no sandbox")
	}
	if _, err := s.MintEnvironmentKey("", time.Hour); err == nil {
		t.Error("MintEnvironmentKey() minted a key for no environment")
	}
	if _, err := s.MintEnvironmentKey("env_01J9", 0); err == nil {
		t.Error("MintEnvironmentKey() minted a key that expires at the moment it is minted")
	}
}

// verifierFor builds a verifier that checks the signer's tokens the way
// cellad does: the local path, against the configured keys, with no
// fetch at all. One listed issuer is configured because a control plane
// always has one.
func verifierFor(t *testing.T, s *auth.Signer) *auth.Verifier {
	t.Helper()
	issuer := issuertest.New(t, issuertest.WithDefaultAudience(audience))
	v, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{
		Issuers: []string{issuer.URL()}, Audience: audience,
		LocalIssuer: s.Issuer(), LocalKeys: s.PublicKeys(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// servedKeys reads the key set the signer publishes, as a third service
// would: over HTTP, from the path spec 002's table names.
func servedKeys(t *testing.T, s *auth.Signer) []map[string]string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", auth.JWKSPath, nil)
	s.JWKS().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%s answered %d", auth.JWKSPath, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/jwk-set+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var doc struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Keys
}

// verify checks a compact JWS against a served key set with the standard
// library alone, which is what a third service holds.
func verify(t *testing.T, token string, served []map[string]string) (header, payload map[string]any) {
	t.Helper()
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		t.Fatalf("the token is %d segments, not three", len(parts))
	}
	header = decodeSegment(t, parts[0])
	payload = decodeSegment(t, parts[1])
	kid, _ := header["kid"].(string)

	var pub *rsa.PublicKey
	for _, k := range served {
		if k["kid"] != kid {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k["n"])
		if err != nil {
			t.Fatal(err)
		}
		e, err := base64.RawURLEncoding.DecodeString(k["e"])
		if err != nil {
			t.Fatal(err)
		}
		pub = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if pub == nil {
		t.Fatalf("the key set names no key %q, so nothing can verify the token", kid)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("the token does not verify against the published key set: %v", err)
	}
	return header, payload
}

func decodeSegment(t *testing.T, seg string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
