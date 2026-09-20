// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package stubs

import (
	"fmt"
	"net/http"
	"strings"

	"latere.ai/x/pkg/authkit/issuertest"
)

// DefaultAudience is the aud a minted token carries when the request
// names none, and the aud spec 006 gives this core.
const DefaultAudience = "cella"

// The algorithms the issuer signs with. A verifier of spec 006 accepts
// both, and which one a key set publishes is what this flag chooses.
const (
	AlgRS256 = "rs256"
	AlgES256 = "es256"
)

// IssuerOptions configures the issuer role: a real OpenID Connect issuer
// over a generated key set, with the control route that mints a token for
// any subject and audience asked, so a caller in a test or at a prompt has
// one to send.
type IssuerOptions struct {
	// Addr is the listen address, empty to turn the role off.
	Addr string
	// URL is the iss every token carries and the base of the discovery
	// document. Empty takes the listener's own address, which is right for
	// a stub reached over loopback and wrong for one reached by another
	// container, whose callers name the address they dial.
	URL string
	// Audience is the aud a minted token carries when the mint request
	// names none.
	Audience string
	// Algorithm is AlgRS256 or AlgES256.
	Algorithm string
}

// newIssuer builds the issuer's handler on a resolved listen address.
// The stub is the family's own, so a token this mints is the token every
// other core's tier verifies, and the discovery document is the one
// internal/auth reads at start.
func newIssuer(addr string, o IssuerOptions) (http.Handler, func(), error) {
	url := strings.TrimRight(strings.TrimSpace(o.URL), "/")
	if url == "" {
		url = "http://" + addr
	}
	audience := o.Audience
	if audience == "" {
		audience = DefaultAudience
	}
	opts := []issuertest.Option{issuertest.WithIssuer(url), issuertest.WithDefaultAudience(audience)}
	switch strings.ToLower(strings.TrimSpace(o.Algorithm)) {
	case "", AlgRS256:
		opts = append(opts, issuertest.WithRS256())
	case AlgES256:
		opts = append(opts, issuertest.WithES256())
	default:
		return nil, nil, fmt.Errorf("the algorithm is %q; %s or %s", o.Algorithm, AlgRS256, AlgES256)
	}
	s := issuertest.NewHandler(opts...)
	return s.Handler(), s.Close, nil
}
