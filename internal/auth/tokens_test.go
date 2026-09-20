// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/cella/internal/auth"
	v1 "latere.ai/x/cella/manifest/v1"
)

// list is the revocation list the controller writes and the verifier reads.
// The store's own adapters are held to the same contract by the suite of
// design 010; this one is here so a claim, a rotation and a refusal are one
// test and not three packages.
type list struct {
	rows map[string]time.Time
	err  error
}

func newList() *list { return &list{rows: map[string]time.Time{}} }

func (l *list) Revoke(_ context.Context, jti string, exp time.Time) error {
	if l.err != nil {
		return l.err
	}
	l.rows[jti] = exp
	return nil
}

func (l *list) Revoked(_ context.Context, jti string) (bool, error) {
	if l.err != nil {
		return false, l.err
	}
	_, held := l.rows[jti]
	return held, nil
}

func (l *list) Forget(_ context.Context, before time.Time) (int, error) {
	if l.err != nil {
		return 0, l.err
	}
	n := 0
	for jti, exp := range l.rows {
		if exp.Before(before) {
			delete(l.rows, jti)
			n++
		}
	}
	return n, nil
}

// sandbox is one desired sandbox as the controller hands it to the mint.
func sandbox(id, environment string, expires time.Time) v1.Sandbox {
	var obj v1.Sandbox
	obj.Status.ID = id
	obj.Status.Environment = environment
	obj.Status.ExpiresAt = expires
	return obj
}

// TestWorkloadTokensMintClaims pins the whole claim set the controller mints,
// which is spec 006's table: these claims and no others. A claim nobody
// intended is a claim a reader may act on.
func TestWorkloadTokensMintClaims(t *testing.T) {
	s := newSigner(t, key(t, 1))
	tokens, err := auth.NewWorkloadTokens(s, newList())
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	value, jti, exp, err := tokens.Mint(t.Context(), sandbox("sbx_01J9", "env_01J9", time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	_, payload := verify(t, value, servedKeys(t, s))

	want := []string{"aud", "environment", "exp", "iat", "iss", "jti", "sub"}
	got := slices.Sorted(maps.Keys(payload))
	if !slices.Equal(got, want) {
		t.Errorf("the token carries %v, want exactly %v", got, want)
	}
	if payload["sub"] != "sandbox:sbx_01J9" || payload["environment"] != "env_01J9" || payload["iss"] != publicURL {
		t.Errorf("the identity claims are %v", payload)
	}
	if aud, _ := payload["aud"].([]any); len(aud) != 1 || aud[0] != audience {
		t.Errorf("aud = %v, want [%s]", payload["aud"], audience)
	}
	if payload["jti"] != jti {
		t.Errorf("the token's jti is %v and the one reported is %q; a revocation keys on one of them", payload["jti"], jti)
	}
	if d := exp.Sub(before).Round(time.Minute); d != auth.WorkloadTokenLifetime {
		t.Errorf("a sandbox that never expires took a token of %v, want %v", d, auth.WorkloadTokenLifetime)
	}
	if iat, _ := payload["iat"].(float64); int64(iat) < before.Unix()-1 {
		t.Errorf("iat = %v, want the mint", payload["iat"])
	}
}

// TestWorkloadTokensMintIsCappedByTheSandbox: the identity dies with the
// sandbox, so a sandbox that ends in an hour carries a token that ends with
// it rather than one good for a day after it is gone.
func TestWorkloadTokensMintIsCappedByTheSandbox(t *testing.T) {
	s := newSigner(t, key(t, 1))
	tokens, err := auth.NewWorkloadTokens(s, newList())
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	_, _, exp, err := tokens.Mint(t.Context(), sandbox("sbx_01J9", "env_01J9", expires))
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(expires) {
		t.Errorf("the token expires at %v, want the sandbox's own expiry %v", exp, expires)
	}
}

// TestWorkloadTokensRefuseASandboxWithNoID keeps a token that names nothing
// from being signed at all.
func TestWorkloadTokensRefuseASandboxWithNoID(t *testing.T) {
	tokens, err := auth.NewWorkloadTokens(newSigner(t, key(t, 1)), newList())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := tokens.Mint(t.Context(), v1.Sandbox{}); err == nil {
		t.Fatal("a token was minted for a sandbox with no id")
	}
	if _, err := auth.NewWorkloadTokens(nil, newList()); err == nil {
		t.Fatal("a mint was built with no signer")
	}
}

// TestRevokedTokenIsRefused is spec 006's row: the jti is how a token cellad
// minted is ended before its exp, and the verifier is where the end takes
// effect.
func TestRevokedTokenIsRefused(t *testing.T) {
	s := newSigner(t, key(t, 1))
	rows := newList()
	tokens, err := auth.NewWorkloadTokens(s, rows)
	if err != nil {
		t.Fatal(err)
	}
	v := verifierWithList(t, s, rows)
	value, jti, exp, err := tokens.Mint(t.Context(), sandbox("sbx_01J9", "env_01J9", time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	caller, err := v.VerifyContext(t.Context(), value)
	if err != nil {
		t.Fatalf("a token nobody revoked was refused: %v", err)
	}
	if id, ok := caller.Sandbox(); !ok || id != "sbx_01J9" {
		t.Fatalf("the caller reads as sandbox %q %v", id, ok)
	}
	if err := tokens.Revoke(t.Context(), jti, exp); err != nil {
		t.Fatal(err)
	}
	_, err = v.VerifyContext(t.Context(), value)
	if auth.CodeOf(err) != auth.CodeUnauthenticated {
		t.Fatalf("a revoked token was refused with %v (%v), want %v", auth.CodeOf(err), err, auth.CodeUnauthenticated)
	}
	// The sweep drops the row once the token could no longer be presented,
	// and the list is then as short as the live tokens make it.
	n, err := tokens.Forget(t.Context(), exp.Add(time.Second))
	if err != nil || n != 1 {
		t.Fatalf("Forget = %d %v, want the one expired row", n, err)
	}
}

// TestRevocationListThatCannotAnswerRefusesTheToken keeps an unproven token
// from being an accepted one: a store that is down is not a token that is
// good.
func TestRevocationListThatCannotAnswerRefusesTheToken(t *testing.T) {
	s := newSigner(t, key(t, 1))
	rows := newList()
	tokens, err := auth.NewWorkloadTokens(s, rows)
	if err != nil {
		t.Fatal(err)
	}
	v := verifierWithList(t, s, rows)
	value, _, _, err := tokens.Mint(t.Context(), sandbox("sbx_01J9", "env_01J9", time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	rows.err = errors.New("the store is unreachable")
	_, err = v.VerifyContext(t.Context(), value)
	if auth.CodeOf(err) != auth.CodeAuthorizerUnavailable {
		t.Fatalf("a token the list could not answer for was refused with %v (%v), want %v", auth.CodeOf(err), err, auth.CodeAuthorizerUnavailable)
	}
}

// TestMintedTokenWithoutAJTIIsRefused: a credential nobody can revoke is not
// one this control plane issued, whatever key signed it.
func TestMintedTokenWithoutAJTIIsRefused(t *testing.T) {
	s := newSigner(t, key(t, 1))
	v := verifierWithList(t, s, newList())
	forged := signedLocally(t, key(t, 1), map[string]any{
		"iss": publicURL, "sub": "sandbox:sbx_01J9", "aud": []string{audience},
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	})
	if _, err := v.VerifyContext(t.Context(), forged); auth.CodeOf(err) != auth.CodeUnauthenticated {
		t.Fatalf("a minted token with no jti was refused with %v (%v), want %v", auth.CodeOf(err), err, auth.CodeUnauthenticated)
	}
}

// TestWorkloadTokensWithoutAListEndNothing: a deployment with no store mints
// tokens that live to their exp, and says so by doing nothing rather than by
// failing an act it was asked to take.
func TestWorkloadTokensWithoutAList(t *testing.T) {
	tokens, err := auth.NewWorkloadTokens(newSigner(t, key(t, 1)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tokens.Revoke(t.Context(), "01JLIVE", time.Now().Add(time.Hour)); err != nil {
		t.Errorf("Revoke with no list = %v", err)
	}
	if n, err := tokens.Forget(t.Context(), time.Now()); err != nil || n != 0 {
		t.Errorf("Forget with no list = %d %v", n, err)
	}
}

// verifierWithList builds the verifier cellad runs with a revocation list
// behind it.
func verifierWithList(t *testing.T, s *auth.Signer, rows auth.Revocations) *auth.Verifier {
	t.Helper()
	issuer := issuertest.New(t, issuertest.WithDefaultAudience(audience))
	v, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{
		Issuers: []string{issuer.URL()}, Audience: audience,
		LocalIssuer: s.Issuer(), LocalKeys: s.PublicKeys(), Revocations: rows,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// signedLocally signs claims with one of cellad's own keys, which is how a
// token that cellad's mint would never produce is put in front of the
// verifier.
func signedLocally(t *testing.T, k *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	signing := segment(t, map[string]any{"alg": "RS256", "typ": "JWT", "kid": auth.Thumbprint(&k.PublicKey)}) +
		"." + segment(t, claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// TestTheMintCarriesTheGrantFromDesiredState: the spawn claim is the budget
// and the mesh the store holds at the moment of the mint, and a sandbox that
// may create nothing and is in no mesh carries no claim at all.
func TestTheMintCarriesTheGrantFromDesiredState(t *testing.T) {
	signer := newSigner(t, key(t, 1))
	tokens, err := auth.NewWorkloadTokens(signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	obj := v1.Sandbox{Status: v1.SandboxStatus{
		ID: "sbx_01J9", Environment: "env_01J9", Mesh: "msh_01J9",
		Spawn: v1.SpawnStatus{Budget: 4, Used: 3, Depth: 2},
	}}
	value, _, _, err := tokens.Mint(t.Context(), obj)
	if err != nil {
		t.Fatal(err)
	}
	_, claims := verify(t, value, servedKeys(t, signer))
	spawn, ok := claims["spawn"].(map[string]any)
	if !ok {
		t.Fatalf("the token carries spawn %v", claims["spawn"])
	}
	// The grant, not the balance: the claim carries no used count, because
	// a copy of a moving number would be wrong the instant after it was
	// signed and the ledger is the enforcement.
	if _, held := spawn["used"]; held {
		t.Errorf("the claim carries a used count: %v", spawn)
	}
	for name, want := range map[string]any{"budget": float64(4), "depth": float64(2), "mesh": "msh_01J9"} {
		if got := spawn[name]; got != want {
			t.Errorf("spawn.%s = %v, want %v", name, got, want)
		}
	}

	bare := v1.Sandbox{Status: v1.SandboxStatus{ID: "sbx_01J9", Environment: "env_01J9"}}
	value, _, _, err = tokens.Mint(t.Context(), bare)
	if err != nil {
		t.Fatal(err)
	}
	_, claims = verify(t, value, servedKeys(t, signer))
	if got, held := claims["spawn"]; held {
		t.Errorf("a sandbox granted nothing carries spawn %v", got)
	}
}
