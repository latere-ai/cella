// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"
)

// Defaults for the identity variables of spec 006. The three figures the
// authorizer contract fixes are the shared package's, not restated here:
// an allow is capped at authz.MaxTTL and a deny is held authz.DenyTTL.
const (
	// DefaultOIDCAudience is the aud a caller's token must contain and
	// the aud the tokens cellad mints carry.
	DefaultOIDCAudience = "cella"
	// DefaultAuthorizerTimeout bounds one decision, its retry included.
	DefaultAuthorizerTimeout = authz.Timeout
	// DefaultAuthorizerCache is how long an allow whose answer names no
	// ttl is cached.
	DefaultAuthorizerCache = authz.DefaultTTL
	// DefaultEnvironmentKeyTTL is an environment key's lifetime from mint.
	DefaultEnvironmentKeyTTL = 8760 * time.Hour
	// DefaultEnvironment is the environment every subject may use under
	// the owner policy. The variable is spec 021's; spec 006 reads it
	// because the policy's use row names it.
	DefaultEnvironment = "default"
)

// TokenKeyBits is the smallest RSA key CELLA_TOKEN_KEY may carry, and
// TokenKeyBlocks the most blocks it may hold: one signing key and one
// predecessor, which is what makes a rotation two deploys and no outage.
const (
	TokenKeyBits   = 2048
	TokenKeyBlocks = 2
)

// Identity is the configuration of spec 006: who cellad accepts a token
// from, what it signs its own tokens with, and who decides what a caller
// may do. It is a part of Config; the fields are named for the variables
// without the CELLA_ prefix.
type Identity struct {
	// OIDCIssuers is CELLA_OIDC_ISSUERS, the issuer URLs whose tokens are
	// accepted, each with its trailing slash removed so that one issuer
	// written two ways is one issuer.
	OIDCIssuers []string
	// OIDCInsecureIssuers is CELLA_OIDC_INSECURE_ISSUERS, the entries of
	// the list above that may be http:// on a host other than loopback.
	OIDCInsecureIssuers []string
	// OIDCAudience is the first configured audience, used for local tokens.
	OIDCAudience string
	// OIDCAudiences is the set accepted from external OIDC issuers.
	OIDCAudiences []string
	// PublicURL is CELLA_PUBLIC_URL, the absolute URL callers reach the
	// public listener at and the iss of every token cellad mints.
	PublicURL string
	// TokenKeys is CELLA_TOKEN_KEY parsed: the first signs, and every
	// key's public half is served at /.well-known/jwks.json.
	TokenKeys []*rsa.PrivateKey
	// AuthorizerURL and AuthorizerToken are the endpoint cellad asks and
	// the bearer it sends. An unset URL selects the owner policy; a URL
	// without a token is a start-up failure.
	AuthorizerURL   string
	AuthorizerToken string
	// AuthorizerTimeout bounds one call and AuthorizerCache is how long
	// an allow whose answer names no ttl is held.
	AuthorizerTimeout time.Duration
	AuthorizerCache   time.Duration
	// AdminSubjects is CELLA_ADMIN_SUBJECTS, the rendered subjects the
	// owner policy lets act on every object. Read and unused with an
	// authorizer set.
	AdminSubjects []string
	// EnvironmentKeyTTL is an environment key's lifetime from mint.
	EnvironmentKeyTTL time.Duration
	// DefaultEnvironment is the environment the owner policy lets every
	// subject use.
	DefaultEnvironment string
}

// loadIdentity reads spec 006's variables and returns one problem per
// variable that cannot be read, so a deployment is fixed in one round
// rather than one variable per restart.
func (i *Identity) loadIdentity(getenv Getenv) []string {
	var problems []string
	i.OIDCInsecureIssuers = splitList(getenv("CELLA_OIDC_INSECURE_ISSUERS"))
	i.OIDCIssuers = issuerList(getenv("CELLA_OIDC_ISSUERS"), i.OIDCInsecureIssuers, &problems)
	if len(i.OIDCIssuers) == 0 {
		problems = append(problems, "CELLA_OIDC_ISSUERS names no issuer; a control plane that verifies no token admits nobody, and there is no anonymous access")
	}
	for entry := range strings.SplitSeq(withDefault(getenv("CELLA_OIDC_AUDIENCE"), DefaultOIDCAudience), ",") {
		audience := strings.TrimSpace(entry)
		if audience == "" || slices.Contains(i.OIDCAudiences, audience) {
			problems = append(problems, "CELLA_OIDC_AUDIENCE requires distinct nonempty entries")
			continue
		}
		i.OIDCAudiences = append(i.OIDCAudiences, audience)
	}
	if len(i.OIDCAudiences) > 0 {
		i.OIDCAudience = i.OIDCAudiences[0]
	}

	i.PublicURL = strings.TrimRight(strings.TrimSpace(getenv("CELLA_PUBLIC_URL")), "/")
	if _, ok := endpoint(i.PublicURL); !ok {
		problems = append(problems, "CELLA_PUBLIC_URL is "+strconv.Quote(i.PublicURL)+", not an absolute http:// or https:// URL with a host; it is the iss of every token cellad mints")
	}

	keys, problem := tokenKeys(getenv("CELLA_TOKEN_KEY"))
	if problem != "" {
		problems = append(problems, problem)
	}
	i.TokenKeys = keys

	i.AuthorizerURL = strings.TrimSpace(getenv("CELLA_AUTHORIZER_URL"))
	i.AuthorizerToken = strings.TrimSpace(getenv("CELLA_AUTHORIZER_TOKEN"))
	if i.AuthorizerURL != "" {
		if u, ok := endpoint(i.AuthorizerURL); !ok {
			problems = append(problems, "CELLA_AUTHORIZER_URL is "+strconv.Quote(i.AuthorizerURL)+", not an absolute http:// or https:// URL with a host")
		} else if !isHTTPS(u) && !isLoopback(u) {
			problems = append(problems, "CELLA_AUTHORIZER_URL is http:// on a host other than loopback; a decision and the bearer that authorizes it do not travel in the clear")
		}
		if i.AuthorizerToken == "" {
			problems = append(problems, "CELLA_AUTHORIZER_TOKEN is unset while CELLA_AUTHORIZER_URL is set; the endpoint requires a bearer")
		}
	}
	i.AuthorizerTimeout = duration(getenv, "CELLA_AUTHORIZER_TIMEOUT", DefaultAuthorizerTimeout, &problems)
	i.AuthorizerCache = duration(getenv, "CELLA_AUTHORIZER_CACHE", DefaultAuthorizerCache, &problems)
	i.EnvironmentKeyTTL = duration(getenv, "CELLA_ENVIRONMENT_KEY_TTL", DefaultEnvironmentKeyTTL, &problems)

	i.AdminSubjects = authz.ParseSubjects(getenv("CELLA_ADMIN_SUBJECTS"))
	for _, s := range i.AdminSubjects {
		if _, _, ok := authz.SplitSubject(s); !ok {
			problems = append(problems, "CELLA_ADMIN_SUBJECTS entry "+strconv.Quote(s)+" is not a rendered subject of the form <issuer>|<sub>")
		}
	}
	i.DefaultEnvironment = withDefault(getenv("CELLA_DEFAULT_ENVIRONMENT"), DefaultEnvironment)
	return problems
}

// issuerList reads the issuer list: each entry an absolute URL, each one
// distinct after the trailing slash goes, and each https:// unless it is
// on a loopback address or the operator has listed it insecure.
func issuerList(raw string, insecure []string, problems *[]string) []string {
	var out []string
	for _, s := range splitList(raw) {
		iss := strings.TrimRight(s, "/")
		u, ok := endpoint(iss)
		if !ok {
			*problems = append(*problems, "CELLA_OIDC_ISSUERS entry "+strconv.Quote(s)+" is not an absolute http:// or https:// URL with a host")
			continue
		}
		if slices.Contains(out, iss) {
			*problems = append(*problems, "CELLA_OIDC_ISSUERS lists "+iss+" twice")
			continue
		}
		if !isHTTPS(u) && !isLoopback(u) && !slices.Contains(insecure, iss) && !slices.Contains(insecure, s) {
			*problems = append(*problems, "CELLA_OIDC_ISSUERS "+iss+" is http:// on a host other than loopback; use https:// or list it in CELLA_OIDC_INSECURE_ISSUERS")
			continue
		}
		out = append(out, iss)
	}
	return out
}

// tokenKeys parses CELLA_TOKEN_KEY: one or two PEM blocks, each an RSA
// private key of at least TokenKeyBits. The first block signs; every
// block is served in the key set, which is what makes a rotation the
// operator's own and durable across a restart.
func tokenKeys(raw string) ([]*rsa.PrivateKey, string) {
	rest := []byte(strings.TrimSpace(raw))
	if len(rest) == 0 {
		return nil, "CELLA_TOKEN_KEY is unset; cellad signs the identity of every sandbox and every environment worker with it"
	}
	var keys []*rsa.PrivateKey
	for block, tail := pem.Decode(rest); block != nil; block, tail = pem.Decode(rest) {
		rest = tail
		key, err := parseRSAPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Sprintf("CELLA_TOKEN_KEY block %d is a %s block that is no RSA private key: %v", len(keys)+1, block.Type, err)
		}
		if bits := key.N.BitLen(); bits < TokenKeyBits {
			return nil, fmt.Sprintf("CELLA_TOKEN_KEY block %d is an RSA key of %d bits; %d is the smallest accepted", len(keys)+1, bits, TokenKeyBits)
		}
		keys = append(keys, key)
	}
	switch {
	case len(keys) == 0:
		return nil, "CELLA_TOKEN_KEY holds no PEM block that is an RSA private key"
	case len(keys) > TokenKeyBlocks:
		return nil, fmt.Sprintf("CELLA_TOKEN_KEY holds %d keys; a rotation is one signing key and at most one predecessor, so %d is the most", len(keys), TokenKeyBlocks)
	}
	return keys, ""
}

// parseRSAPrivateKey accepts the two PEM bodies openssl writes, PKCS#1
// and PKCS#8, so an operator's `openssl genrsa` and `openssl genpkey`
// are one variable.
func parseRSAPrivateKey(der []byte) (*rsa.PrivateKey, error) {
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	any, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	key, ok := any.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the key is a %T, and a token is signed RS256", any)
	}
	return key, nil
}

// duration reads an optional duration variable, falling back to def.
func duration(getenv Getenv, name string, def time.Duration, problems *[]string) time.Duration {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		*problems = append(*problems, name+" is "+strconv.Quote(raw)+", not a duration such as 5s or 8760h")
		return def
	}
	if d <= 0 {
		*problems = append(*problems, name+" is "+raw+"; a deadline and a lifetime are both positive")
		return def
	}
	return d
}

// splitList reads a comma-separated variable, trimming each entry and
// dropping the empties.
func splitList(raw string) []string {
	var out []string
	for s := range strings.SplitSeq(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// endpoint parses an absolute http or https URL with a host.
func endpoint(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, false
	}
	return u, true
}

func isHTTPS(u *url.URL) bool { return u != nil && u.Scheme == "https" }

// isLoopback reports whether a URL names this machine, the one place an
// http:// endpoint carries a bearer safely.
func isLoopback(u *url.URL) bool {
	if u == nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
