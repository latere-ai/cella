// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"

	"latere.ai/x/cella/internal/auth"
)

// signing builds the mint and the verifier cellad runs, and points the
// fixture's handler at the verifier, so a token this test signs is one the
// API accepts exactly as it accepts the node's own.
func signing(t *testing.T, f *fixture, rows auth.Revocations) *auth.Signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewSigner(auth.SignerOptions{Issuer: "http://cella.test", Audience: "cella", Keys: []*rsa.PrivateKey{key}})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{
		Issuers: []string{f.issuerURL}, Audience: "cella",
		LocalIssuer: "http://cella.test", LocalKeys: signer.PublicKeys(), Revocations: rows,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.h.(*handler).Verifier = verifier
	return signer
}

// rows is the revocation list behind the verifier in these cases. The store's
// own is proven by the suite of design 010.
type rows map[string]bool

func (r rows) Revoked(_ context.Context, jti string) (bool, error) { return r[jti], nil }

// TestWorkloadReachesItsOwnSandbox is spec 006's least-privileged workload as
// the API serves it: a sandbox's own token reads and execs the sandbox it
// names, and reaches nothing else.
func TestWorkloadReachesItsOwnSandbox(t *testing.T) {
	f := setup(t, nil)
	signer := signing(t, f, rows{})

	var mine, other v1.Sandbox
	_ = json.Unmarshal(f.request("POST", "/v1/sandboxes?wait=1", f.alice, createBody, 201), &mine)
	_ = json.Unmarshal(f.request("POST", "/v1/sandboxes?wait=1", f.alice, strings.Replace(createBody, `"work"`, `"other"`, 1), 201), &other)

	token, err := signer.MintWorkload(auth.Workload{Sandbox: mine.Status.ID, Environment: "default"})
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/sandboxes/" + mine.Status.ID

	var read v1.Sandbox
	if err := json.Unmarshal(f.request("GET", base, token.Value, "", 200), &read); err != nil {
		t.Fatal(err)
	}
	if read.Status.ID != mine.Status.ID {
		t.Fatalf("the workload read %s, want its own sandbox", read.Status.ID)
	}
	// The record of the identity is the control plane's own and reaches no
	// caller, the sandbox that holds the token included.
	if read.Status.TokenState != nil {
		t.Errorf("the answer carries the token record: %+v", read.Status.TokenState)
	}
	f.request("POST", base+"/exec?wait=1", token.Value, `{"command":["true"]}`, 200)

	// A sibling is another sandbox, which a workload neither reads nor
	// execs, and its own delete is not its to ask for either. A deny on the
	// request's own action is forbidden rather than not found, which spec
	// 006 keeps for the lookups a resolve makes.
	sibling := "/v1/sandboxes/" + other.Status.ID
	f.request("GET", sibling, token.Value, "", 403)
	f.request("POST", sibling+"/exec?wait=1", token.Value, `{"command":["true"]}`, 403)
	f.request("DELETE", base, token.Value, "", 403)
	f.request("POST", base+"/stop", token.Value, "", 403)
}

// TestRevokedWorkloadTokenIsRefusedByTheAPI: the revocation takes effect at
// the door, on the request that follows it, and not when the token expires.
func TestRevokedWorkloadTokenIsRefusedByTheAPI(t *testing.T) {
	f := setup(t, nil)
	revoked := rows{}
	signer := signing(t, f, revoked)

	var mine v1.Sandbox
	_ = json.Unmarshal(f.request("POST", "/v1/sandboxes?wait=1", f.alice, createBody, 201), &mine)
	token, err := signer.MintWorkload(auth.Workload{
		Sandbox: mine.Status.ID, Environment: "default", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/sandboxes/" + mine.Status.ID
	f.request("GET", base, token.Value, "", 200)
	revoked[token.JTI] = true
	f.request("GET", base, token.Value, "", 401)
}
