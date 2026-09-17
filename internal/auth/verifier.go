// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authkit/jwt"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/bearer"
	"latere.ai/x/pkg/otel"
)

// The reserved prefixes of spec 006: the sub of a token cellad minted
// names what the token identifies, and no listed issuer may mint one.
const (
	SandboxPrefix     = "sandbox:"
	EnvironmentPrefix = "environment:"
)

// reservedPrefixes is the pair above, in the order a message lists them.
var reservedPrefixes = []string{SandboxPrefix, EnvironmentPrefix}

// Caller is a verified bearer. Subject is the rendered subject every
// owner field, every event and every authorizer request carries: the
// issuer and the sub joined for a listed issuer's token, and the bare
// sub for one cellad minted. Claims is every claim of the token,
// verbatim and uninterpreted; it is handed to the authorizer as it came
// and read by nothing here.
type Caller struct {
	Subject string
	Issuer  string
	Sub     string
	Claims  map[string]any
	// Minted reports that cellad signed this token: a sandbox's identity
	// or an environment's key, rather than a person from a listed issuer.
	Minted bool
}

// Sandbox is the sandbox id a workload token names, and false for every
// other caller.
func (c Caller) Sandbox() (string, bool) { return c.reserved(SandboxPrefix) }

// Environment is the environment id an environment key names, and false
// for every other caller.
func (c Caller) Environment() (string, bool) { return c.reserved(EnvironmentPrefix) }

func (c Caller) reserved(prefix string) (string, bool) {
	if !c.Minted {
		return "", false
	}
	id, ok := strings.CutPrefix(c.Sub, prefix)
	return id, ok && id != ""
}

// VerifierOptions configures a Verifier. Issuers and Audience are
// CELLA_OIDC_ISSUERS and CELLA_OIDC_AUDIENCE; LocalIssuer and LocalKeys
// are CELLA_PUBLIC_URL and the public halves of CELLA_TOKEN_KEY, which
// verify the tokens cellad mints with no fetch at all.
type VerifierOptions struct {
	Issuers     []string
	Audience    string
	LocalIssuer string
	LocalKeys   []*rsa.PublicKey
	HTTP        *http.Client
	CacheTTL    time.Duration
	// FetchTimeout bounds one discovery or key-set read at start.
	FetchTimeout time.Duration
}

// DefaultFetchTimeout bounds one start-up read of an issuer.
const DefaultFetchTimeout = 10 * time.Second

// Verifier verifies a bearer against the listed issuers and against
// cellad's own key set. The listed issuers are one validator holding the
// list, each issuer answering for its own tokens alone, and cellad's own
// issuer is a second holding every key of CELLA_TOKEN_KEY under its kid,
// which is how a two-key rotation verifies both while the operator holds
// the overlap.
type Verifier struct {
	audience    string
	issuers     []string
	validator   *jwt.Validator
	localIssuer string
	local       *jwt.Validator
}

// NewVerifier reads every issuer's discovery document and key set and
// refuses to build when one is unreachable, names another issuer, names
// no jwks_uri, or publishes no key that RS256 or ES256 could use. That
// is spec 006's start-up rule: an issuer that is wrong is a deployment
// to fix, not a stream of 401s to read in a log.
//
// Afterwards nothing here fetches on a request path. The shared verifier
// caches each set for its TTL, refreshes on an unknown kid under its own
// rate limit, and serves the stale set while a refresh fails, so an
// issuer that goes away later degrades to refusing new keys.
func NewVerifier(ctx context.Context, o VerifierOptions) (*Verifier, error) {
	if len(o.Issuers) == 0 {
		return nil, errors.New("CELLA_OIDC_ISSUERS names no issuer, and there is no anonymous access")
	}
	if o.Audience == "" {
		return nil, errors.New("CELLA_OIDC_AUDIENCE is empty, and a token addressed to nobody is a token addressed to everybody")
	}
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: DefaultFetchTimeout, Transport: otel.Transport(nil)}
	}
	timeout := o.FetchTimeout
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	v := &Verifier{audience: o.Audience, localIssuer: strings.TrimRight(o.LocalIssuer, "/")}
	for _, raw := range o.Issuers {
		iss := strings.TrimRight(raw, "/")
		if slices.Contains(v.issuers, iss) {
			return nil, fmt.Errorf("CELLA_OIDC_ISSUERS lists %s twice", iss)
		}
		if err := discover(ctx, client, iss, timeout); err != nil {
			return nil, err
		}
		v.issuers = append(v.issuers, iss)
	}
	v.validator = jwt.New(jwt.Config{
		Issuers:    v.issuers,
		Audiences:  []string{o.Audience},
		CacheTTL:   o.CacheTTL,
		HTTPClient: client,
		// The family's age bound, one rule across the three cores and
		// spec 006's amendment of 2026-09-17: a caller's token is
		// refused once its iat is older than jwt.DefaultMaxTokenAge, a
		// day, whatever exp it carries, so a caller that wants a
		// longer-lived credential re-mints it at its issuer. Origo
		// names the same figure and Lux inherits it. The size bound
		// stays the shared package's.
		MaxTokenAge: jwt.DefaultMaxTokenAge,
	})
	if len(o.LocalKeys) > 0 {
		if v.localIssuer == "" {
			return nil, errors.New("CELLA_TOKEN_KEY holds a key while CELLA_PUBLIC_URL is unset, so a token cellad minted would name no issuer")
		}
		set := make([]jwt.LocalKey, len(o.LocalKeys))
		for i, key := range o.LocalKeys {
			set[i] = jwt.LocalKey{KeyID: Thumbprint(key), Key: key}
		}
		v.local = jwt.New(jwt.Config{
			LocalIssuer: v.localIssuer,
			LocalKeys:   set,
			Audiences:   []string{o.Audience},
			// The other half of the same decision: cellad's own tokens
			// carry no age bound, because an environment key lives
			// CELLA_ENVIRONMENT_KEY_TTL, a year by default, and its exp
			// is therefore its bound.
			MaxTokenAge: -1,
		})
	}
	return v, nil
}

// Issuers lists the issuers this verifier accepts, in configured order.
func (v *Verifier) Issuers() []string { return append([]string(nil), v.issuers...) }

// Audience is the aud a token must carry to be accepted.
func (v *Verifier) Audience() string { return v.audience }

// Authenticator is the listed issuers' validator behind the family's
// authkit.Authenticator, which is the shape latere.ai/x/pkg/authkit's
// conformance suite drives: one verified token, one identity, and no
// call to an issuer beyond its discovery document and its key set. It
// is the validator cellad runs, not a second one built for a test.
func (v *Verifier) Authenticator() *jwt.Authenticator {
	return jwt.NewAuthenticator(v.validator)
}

// Authenticate reads the bearer of a request and verifies it. A request
// with no bearer is unauthenticated: there is no anonymous access and no
// API key.
func (v *Verifier) Authenticate(r *http.Request) (Caller, error) {
	raw, ok := bearer.FromRequest(r)
	if !ok || raw == "" {
		return Caller{}, refuse(CodeUnauthenticated, "the request carries no bearer token")
	}
	return v.Verify(raw)
}

// Verify verifies one bearer. The token's own iss selects the key set it
// is checked against, so a token of an issuer that is not listed is
// refused before a signature is tried, and a token cellad minted takes
// the local path and reaches no network.
//
// A listed issuer's token whose sub carries a reserved prefix is
// refused: no issuer mints a sandbox's or an environment's identity.
func (v *Verifier) Verify(raw string) (Caller, error) {
	var claims map[string]any
	if err := jwt.DecodePayload(raw, &claims); err != nil {
		return Caller{}, refuse(CodeUnauthenticated, "the bearer is not a JWS any issuer could have signed: %v", err)
	}
	iss, _ := claims["iss"].(string)
	iss = strings.TrimRight(iss, "/")
	if v.localIssuer != "" && iss == v.localIssuer {
		return v.verifyMinted(raw, claims)
	}
	if !slices.Contains(v.issuers, iss) {
		return Caller{}, refuse(CodeUnauthenticated, "the token names the issuer %s, which CELLA_OIDC_ISSUERS does not list", strconv.Quote(iss))
	}
	c, err := v.validator.Validate(raw)
	if err != nil {
		return Caller{}, refuse(CodeUnauthenticated, "issuer %s: %s: %v", iss, jwt.ReasonOf(err), err)
	}
	for _, prefix := range reservedPrefixes {
		if strings.HasPrefix(c.Sub, prefix) {
			return Caller{}, refuse(CodeUnauthenticated,
				"issuer %s stamped the sub %s, and %q is reserved for the identities cellad mints", iss, strconv.Quote(c.Sub), prefix)
		}
	}
	return Caller{Subject: authz.Subject(iss, c.Sub), Issuer: iss, Sub: c.Sub, Claims: claims}, nil
}

// verifyMinted checks a token cellad signed. Every key of
// CELLA_TOKEN_KEY is in the local set under its own kid, so a token
// signed by the predecessor verifies until the operator removes its
// block, and a kid the set does not hold is refused rather than tried
// against every key the node holds.
func (v *Verifier) verifyMinted(raw string, claims map[string]any) (Caller, error) {
	if v.local == nil {
		return Caller{}, refuse(CodeUnauthenticated, "the token names cellad as its issuer, and CELLA_TOKEN_KEY holds no key to check it against")
	}
	c, err := v.local.Validate(raw)
	if err != nil {
		return Caller{}, refuse(CodeUnauthenticated, "cellad's own token: %s: %v", jwt.ReasonOf(err), err)
	}
	return Caller{Subject: c.Sub, Issuer: v.localIssuer, Sub: c.Sub, Claims: claims, Minted: true}, nil
}

// discovery is the part of an OpenID Connect discovery document cellad
// reads: the issuer it claims to be and where its keys are.
type discovery struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// discover is spec 006's start-up rule, and only that: cellad refuses to
// start when an issuer is unreachable, names another issuer, names no
// jwks_uri, or publishes no key the two algorithms could use. The
// validator discovers and refreshes the same document on its own
// afterwards, so nothing here is kept.
func discover(ctx context.Context, client *http.Client, iss string, timeout time.Duration) error {
	var doc discovery
	if err := fetchJSON(ctx, client, iss+"/.well-known/openid-configuration", timeout, &doc); err != nil {
		return fmt.Errorf("CELLA_OIDC_ISSUERS: issuer %s: %w", iss, err)
	}
	if got := strings.TrimRight(doc.Issuer, "/"); got != iss {
		return fmt.Errorf("CELLA_OIDC_ISSUERS: issuer %s: the discovery document names the issuer %s, so the tokens it signs would carry another iss", iss, strconv.Quote(doc.Issuer))
	}
	if doc.JWKSURI == "" {
		return fmt.Errorf("CELLA_OIDC_ISSUERS: issuer %s: the discovery document names no jwks_uri", iss)
	}
	var set keySet
	if err := fetchJSON(ctx, client, doc.JWKSURI, timeout, &set); err != nil {
		return fmt.Errorf("CELLA_OIDC_ISSUERS: issuer %s: key set: %w", iss, err)
	}
	usable := 0
	for _, k := range set.Keys {
		if k.usable() {
			usable++
		}
	}
	if usable == 0 {
		return fmt.Errorf("CELLA_OIDC_ISSUERS: issuer %s: the key set at %s holds %d key(s) and none of them is an RS256 or an ES256 key", iss, doc.JWKSURI, len(set.Keys))
	}
	return nil
}

// keySet and key are the shape of a JWKS as far as the start-up check
// reads it: enough to tell a key the two algorithms can use from one
// they cannot. Every signature is the shared verifier's own work.
type keySet struct {
	Keys []key `json:"keys"`
}

type key struct {
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// usable reports whether RS256 or ES256 could verify a signature with
// this key: an RSA key with a modulus and an exponent, or a P-256 key
// with both coordinates. A key that names another algorithm is not one
// of the two, whatever else it carries.
func (k key) usable() bool {
	switch k.Kty {
	case "RSA":
		return k.N != "" && k.E != "" && (k.Alg == "" || k.Alg == "RS256")
	case "EC":
		return k.Crv == "P-256" && k.X != "" && k.Y != "" && (k.Alg == "" || k.Alg == "ES256")
	}
	return false
}

// maxDocumentBytes bounds a discovery document and a key set, which are
// both small and neither of which is read from a trusted peer.
const maxDocumentBytes = 1 << 20

// fetchJSON reads one JSON document under its own deadline.
func fetchJSON(ctx context.Context, client *http.Client, url string, timeout time.Duration, v any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s answered %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes))
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("GET %s: the answer is not the JSON document expected: %w", url, err)
	}
	return nil
}
