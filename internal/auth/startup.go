// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto/rsa"
	"net/http"
	"time"

	"latere.ai/x/pkg/otel"
)

// Mode is which authorizer this deployment runs, for the line the node
// logs at start. An operator reads it to know whether the endpoint they
// configured was picked up.
type Mode string

const (
	// ModeAuthorizer is CELLA_AUTHORIZER_URL set: every decision is one
	// call to the operator's endpoint.
	ModeAuthorizer Mode = "authorizer"
	// ModeOwnerPolicy is CELLA_AUTHORIZER_URL unset: the built-in policy
	// of spec 006 decides, from CELLA_ADMIN_SUBJECTS and the owner of
	// each object.
	ModeOwnerPolicy Mode = "owner policy"
)

// Options is everything spec 006's variables carry, as the node hands
// them over. The field names are the variables without the CELLA_
// prefix, so the mapping in cmd/cellad is a line per variable.
type Options struct {
	Issuers            []string
	Audience           string
	PublicURL          string
	TokenKeys          []*rsa.PrivateKey
	AuthorizerURL      string
	AuthorizerToken    string
	AuthorizerTimeout  time.Duration
	AdminSubjects      []string
	DefaultEnvironment string
	// HTTP sends the discovery reads and the authorizer's calls. An
	// instrumented client is built when none is given.
	HTTP *http.Client
	// Now is the clock the decision cache runs on.
	Now func() time.Time
	// Observe receives every authorizer call's result and duration, for
	// the metric of spec 017. Optional.
	Observe func(result string, seconds float64)
}

// Identity is what the node holds once spec 006 is wired: who a caller
// is, what cellad signs its own tokens with, and who decides what a
// caller may do.
type Identity struct {
	Verifier   *Verifier
	Signer     *Signer
	Authorizer *Authorizer
	Mode       Mode
}

// Start builds the three and refuses to start on anything spec 006 says
// is a start-up failure: an issuer that does not answer or publishes no
// usable key, a signing key that cannot sign, and an authorizer URL with
// no bearer. Every refusal names the variable, so a deployment is fixed
// rather than guessed at.
//
// It is the only place the three are built, so a node, a test tier and
// the check command all get the same wiring.
func Start(ctx context.Context, o Options) (*Identity, error) {
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: DefaultFetchTimeout, Transport: otel.Transport(nil)}
	}
	signer, err := NewSigner(SignerOptions{
		Issuer: o.PublicURL, Audience: o.Audience, Keys: o.TokenKeys, Now: o.Now,
	})
	if err != nil {
		return nil, err
	}
	verifier, err := NewVerifier(ctx, VerifierOptions{
		Issuers: o.Issuers, Audience: o.Audience,
		LocalIssuer: o.PublicURL, LocalKeys: signer.PublicKeys(),
		HTTP: client,
	})
	if err != nil {
		return nil, err
	}
	id := &Identity{Verifier: verifier, Signer: signer, Mode: ModeOwnerPolicy}
	if o.AuthorizerURL == "" {
		// CELLA_ADMIN_SUBJECTS is read here and read nowhere else; with
		// an authorizer set it is read and unused.
		id.Authorizer = NewAuthorizer(&OwnerPolicy{Admins: o.AdminSubjects, DefaultEnvironment: o.DefaultEnvironment})
		return id, nil
	}
	asking, err := NewClient(ClientOptions{
		URL: o.AuthorizerURL, Token: o.AuthorizerToken, HTTP: client,
		Timeout: o.AuthorizerTimeout, Now: o.Now, Observe: o.Observe,
	})
	if err != nil {
		return nil, err
	}
	id.Authorizer = NewAuthorizer(asking)
	id.Mode = ModeAuthorizer
	return id, nil
}
