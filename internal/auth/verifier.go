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

	v1 "latere.ai/x/cella/manifest/v1"
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
	// sandbox is the calling sandbox as the store holds it, set by the
	// endpoint that read it. It is the source of the workload member's
	// tree position and budget, which spec 006 fixes as the store's and
	// never the token's.
	sandbox *v1.SandboxStatus
}

// WithSandbox is the caller with the calling sandbox the control plane
// read for it. The envelope's workload member carries that status, so an
// authorizer decides on the tree position and the budget the store holds
// rather than on a claim the token could have been minted with.
func (c Caller) WithSandbox(status v1.SandboxStatus) Caller {
	c.sandbox = &status
	return c
}

// Sandbox is the sandbox id a workload token names, and false for every
// other caller.
func (c Caller) Sandbox() (string, bool) { return c.reserved(SandboxPrefix) }

// Environment is the environment id an environment key names, and false
// for every other caller.
func (c Caller) Environment() (string, bool) { return c.reserved(EnvironmentPrefix) }

// Spawn is the budget a workload token was minted with: the grant at
// mint and never the balance. It carries no used count, is not re-minted
// on a spawn, and the control plane gates every spawn on the ledger
// rather than on this (spec 022). A caller reads it to learn what it was
// granted without a round trip; nothing here decides on it.
func (c Caller) Spawn() (Spawn, bool) {
	if !c.Minted {
		return Spawn{}, false
	}
	claim, held := c.Claims["spawn"].(map[string]any)
	if !held {
		return Spawn{}, false
	}
	out := Spawn{Mesh: claimString(claim["mesh"])}
	out.Budget, out.Depth = claimInt(claim["budget"]), claimInt(claim["depth"])
	return out, true
}

// claimInt reads a JSON number off a claim. A value of another shape is
// zero, because a claim the control plane never trusts is not worth an
// error path of its own.
func claimInt(value any) int {
	n, ok := value.(float64)
	if !ok {
		return 0
	}
	return int(n)
}

func claimString(value any) string {
	s, _ := value.(string)
	return s
}

func (c Caller) reserved(prefix string) (string, bool) {
	if !c.Minted {
		return "", false
	}
	id, ok := strings.CutPrefix(c.Sub, prefix)
	return id, ok && id != ""
}

// Revocations is the list of every jti cellad revoked, which the
// verifier asks about the tokens cellad itself signed (spec 006). A
// listed issuer's token is its issuer's to revoke and is never asked
// about here.
type Revocations interface {
	Revoked(ctx context.Context, jti string) (bool, error)
}

// VerifierOptions configures a Verifier. Issuers and Audience are
// CELLA_OIDC_ISSUERS and CELLA_OIDC_AUDIENCE; LocalIssuer and LocalKeys
// are CELLA_PUBLIC_URL and the public halves of CELLA_TOKEN_KEY, which
// verify the tokens cellad mints with no fetch at all.
type VerifierOptions struct {
	Issuers  []string
	Audience string
	// Audiences overrides the external audience set; local tokens use Audience.
	Audiences   []string
	LocalIssuer string
	LocalKeys   []*rsa.PublicKey
	HTTP        *http.Client
	CacheTTL    time.Duration
	// FetchTimeout bounds one discovery or key-set read at start.
	FetchTimeout time.Duration
	// Revocations is the list a token cellad minted is checked against.
	// Without one nothing is revoked, which is what a deployment with no
	// store has: every token lives until its exp.
	Revocations Revocations
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
	revocations Revocations
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
	audiences := slices.Clone(o.Audiences)
	if len(audiences) == 0 {
		audiences = []string{o.Audience}
	}
	for _, audience := range audiences {
		if strings.TrimSpace(audience) == "" {
			return nil, errors.New("CELLA_OIDC_AUDIENCE has an empty entry")
		}
	}
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: DefaultFetchTimeout, Transport: otel.Transport(nil)}
	}
	timeout := o.FetchTimeout
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	v := &Verifier{audience: o.Audience, localIssuer: strings.TrimRight(o.LocalIssuer, "/"), revocations: o.Revocations}
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
		Audiences:  audiences,
		CacheTTL:   o.CacheTTL,
		HTTPClient: client,
		// The family's age bound, one rule across the three cores and
		// spec 006's amendment of 2026-09-17: a caller's token is
		// refused once its iat is older than jwt.DefaultMaxTokenAge, a
		// day, whatever exp it carries, so a caller that wants a
		// longer-lived credential re-mints it at its issuer. Origo
		// names the same figure and Lux inherits it. The size bound
		// stays the shared package's. RequireIssuedAt makes the bound
		// one rule rather than a claim a token may drop: a caller's
		// token that stamps no iat is refused, because an age no one
		// can read is not an age within the bound.
		MaxTokenAge:     jwt.DefaultMaxTokenAge,
		RequireIssuedAt: true,
		// A personal access token is narrower than the person who holds
		// it (infrastructure/identity id-13), and the grants its holder
		// chose ride on it as RFC 9396's authorization_details. The claim
		// says what the credential may not do, so a service that reads
		// the token and applies nothing grants more than its holder asked
		// for, silently; the shared validator therefore refuses such a
		// token with grants_unread until the service promises to read it.
		// cellad promises, and the promise is kept at the decision point:
		// the owner policy intersects its own answer with the grants, and
		// an operator's endpoint on latere.ai/x/pkg/authz/server does the
		// same with no code of its own. The conformance run is what turns
		// the promise from trust into evidence.
		ReadsGrants: true,
	})
	// The start-up check above read every issuer's key set and kept none
	// of it: the validator holds its own cache and it is still cold, so
	// without this the first request a node ever serves pays for a
	// discovery document and a key set on the request path. Warm fills
	// the cache once, and a set that does not answer here is the same
	// failure discover refuses to start on: one issuer that does not
	// answer, refused with the variable named rather than left to become
	// a stream of 401s.
	if err := v.validator.Warm(ctx); err != nil {
		return nil, fmt.Errorf("CELLA_OIDC_ISSUERS: %w", err)
	}
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
			// is therefore its bound. No token cellad mints carries a
			// token_use, so none of them is a personal access token and
			// there is no grant on this path to read.
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
	return v.VerifyContext(r.Context(), raw)
}

// Verify verifies one bearer. The token's own iss selects the key set it
// is checked against, so a token of an issuer that is not listed is
// refused before a signature is tried, and a token cellad minted takes
// the local path and reaches no network.
//
// A listed issuer's token whose sub carries a reserved prefix is
// refused: no issuer mints a sandbox's or an environment's identity.
func (v *Verifier) Verify(raw string) (Caller, error) {
	return v.VerifyContext(context.Background(), raw)
}

// VerifyContext is Verify under the caller's own context, which is what
// bounds the one read the revocation list costs for a token cellad
// minted. Every request path takes this one; Verify is for a caller with
// no request of its own.
func (v *Verifier) VerifyContext(ctx context.Context, raw string) (Caller, error) {
	var claims map[string]any
	if err := jwt.DecodePayload(raw, &claims); err != nil {
		return Caller{}, refuse(CodeUnauthenticated, "the bearer is not a JWS any issuer could have signed: %v", err)
	}
	iss, _ := claims["iss"].(string)
	iss = strings.TrimRight(iss, "/")
	if v.localIssuer != "" && iss == v.localIssuer {
		return v.verifyMinted(ctx, raw, claims)
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
func (v *Verifier) verifyMinted(ctx context.Context, raw string, claims map[string]any) (Caller, error) {
	if v.local == nil {
		return Caller{}, refuse(CodeUnauthenticated, "the token names cellad as its issuer, and CELLA_TOKEN_KEY holds no key to check it against")
	}
	c, err := v.local.Validate(raw)
	if err != nil {
		return Caller{}, refuse(CodeUnauthenticated, "cellad's own token: %s: %v", jwt.ReasonOf(err), err)
	}
	// Every token cellad signs carries a jti, and the jti is how it is
	// ended before its exp: the controller revokes the one it replaces at
	// a rotation, at a recovery and at a delete (spec 006). A token
	// without one could not be ended at all, so it is not one of ours.
	jti, _ := claims["jti"].(string)
	if jti == "" {
		return Caller{}, refuse(CodeUnauthenticated, "cellad's own token carries no jti, and a credential that cannot be revoked is not one cellad issued")
	}
	if v.revocations != nil {
		revoked, err := v.revocations.Revoked(ctx, jti)
		if err != nil {
			// A list that cannot answer leaves the token unproven, and an
			// unproven token is not an accepted one.
			return Caller{}, refuse(CodeAuthorizerUnavailable, "the revocation list did not answer for %s: %v", jti, err)
		}
		if revoked {
			return Caller{}, refuse(CodeUnauthenticated, "the token %s was revoked", jti)
		}
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
