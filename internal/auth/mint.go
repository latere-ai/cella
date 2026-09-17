// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// WorkloadTokenLifetime caps a workload token: the sandbox's own expiry
// or this, whichever is sooner, so a sandbox that never expires still
// carries an identity that does.
const WorkloadTokenLifetime = 24 * time.Hour

// JWKSPath is where the public half of every configured key is served,
// so a platform or a third service verifies a sandbox's or a worker's
// identity without asking cellad.
const JWKSPath = "/.well-known/jwks.json"

// Spawn is the budget a workload token carries from desired state at
// mint: a copy the control plane never trusts over the store, and which
// the ledger of spec 022 is the only enforcement of.
type Spawn struct {
	Budget int    `json:"budget"`
	Depth  int    `json:"depth"`
	Mesh   string `json:"mesh,omitempty"`
}

// Workload is what a workload token identifies: the sandbox, the
// environment it runs in, when the sandbox itself expires, and the
// budget it was minted with.
type Workload struct {
	Sandbox     string
	Environment string
	ExpiresAt   time.Time
	Spawn       *Spawn
}

// Token is one minted credential: the compact JWS, the jti that revokes
// it, and when it stops verifying anywhere.
type Token struct {
	Value     string
	JTI       string
	ExpiresAt time.Time
}

// SignerOptions configures a Signer. Issuer is CELLA_PUBLIC_URL and
// Audience is CELLA_OIDC_AUDIENCE, so a token cellad minted verifies by
// the same two rules as a caller's. Keys is CELLA_TOKEN_KEY in its
// configured order: the first signs, and every one is published.
type SignerOptions struct {
	Issuer   string
	Audience string
	Keys     []*rsa.PrivateKey
	Now      func() time.Time
}

// Signer mints the two tokens cellad issues and publishes the key set
// they verify against. Rotation is the operator's: prepending a key
// makes it the signing key, every key stays in the published set, and
// removing a block stops the tokens it signed. The overlap is therefore
// in the configuration and survives a restart.
type Signer struct {
	issuer   string
	audience string
	keys     []*rsa.PrivateKey
	kids     []string
	now      func() time.Time
	jwks     []byte
}

// NewSigner builds the signer over the configured keys.
func NewSigner(o SignerOptions) (*Signer, error) {
	if len(o.Keys) == 0 {
		return nil, errors.New("CELLA_TOKEN_KEY holds no key, and cellad signs every sandbox's identity with it")
	}
	if o.Issuer == "" {
		return nil, errors.New("CELLA_PUBLIC_URL is unset, and it is the iss of every token cellad mints")
	}
	s := &Signer{
		issuer:   strings.TrimRight(o.Issuer, "/"),
		audience: o.Audience,
		keys:     o.Keys,
		now:      o.Now,
	}
	if s.now == nil {
		s.now = time.Now
	}
	set := keyDocument{Keys: make([]publicKey, 0, len(o.Keys))}
	for _, k := range o.Keys {
		kid := Thumbprint(&k.PublicKey)
		s.kids = append(s.kids, kid)
		set.Keys = append(set.Keys, publicKey{
			Kty: "RSA", Use: "sig", Alg: "RS256", Kid: kid,
			N: b64(k.N.Bytes()), E: b64(big.NewInt(int64(k.E)).Bytes()),
		})
	}
	raw, err := json.Marshal(set)
	if err != nil {
		return nil, err
	}
	s.jwks = raw
	return s, nil
}

// Issuer is the iss every minted token carries.
func (s *Signer) Issuer() string { return s.issuer }

// KeyID is the kid of the key that signs, the first block's.
func (s *Signer) KeyID() string { return s.kids[0] }

// KeyIDs lists the kid of every published key, in configured order.
func (s *Signer) KeyIDs() []string { return append([]string(nil), s.kids...) }

// PublicKeys lists the public half of every configured key, which is
// what the verifier checks a minted token against.
func (s *Signer) PublicKeys() []*rsa.PublicKey {
	out := make([]*rsa.PublicKey, len(s.keys))
	for i, k := range s.keys {
		out[i] = &k.PublicKey
	}
	return out
}

// MintWorkload signs a sandbox's identity. Its expiry is the sandbox's
// own or WorkloadTokenLifetime from now, whichever is sooner, so a
// process inside a sandbox holds an identity that dies with it.
func (s *Signer) MintWorkload(w Workload) (Token, error) {
	if w.Sandbox == "" {
		return Token{}, errors.New("a workload token names a sandbox")
	}
	now := s.now()
	exp := now.Add(WorkloadTokenLifetime)
	if !w.ExpiresAt.IsZero() && w.ExpiresAt.Before(exp) {
		exp = w.ExpiresAt
	}
	return s.mint(minted{
		Sub:         SandboxPrefix + w.Sandbox,
		Environment: w.Environment,
		Spawn:       w.Spawn,
	}, now, exp)
}

// MintEnvironmentKey signs one worker's credential for an environment.
// An environment may hold several, so that each worker on it carries its
// own and one is revoked without the others.
func (s *Signer) MintEnvironmentKey(environment string, ttl time.Duration) (Token, error) {
	if environment == "" {
		return Token{}, errors.New("an environment key names an environment")
	}
	if ttl <= 0 {
		return Token{}, fmt.Errorf("an environment key's lifetime is %v; CELLA_ENVIRONMENT_KEY_TTL is positive", ttl)
	}
	now := s.now()
	return s.mint(minted{Sub: EnvironmentPrefix + environment}, now, now.Add(ttl))
}

// minted is the payload of a token cellad signs: the envelope every
// verifier reads, the jti a revocation is keyed by, and the two members
// a workload token adds. Nothing here is a membership claim: cellad's
// own tokens name what they identify and confer nothing by themselves.
type minted struct {
	Iss         string   `json:"iss"`
	Sub         string   `json:"sub"`
	Aud         []string `json:"aud"`
	Exp         int64    `json:"exp"`
	Iat         int64    `json:"iat"`
	JTI         string   `json:"jti"`
	Environment string   `json:"environment,omitempty"`
	Spawn       *Spawn   `json:"spawn,omitempty"`
}

// mint signs one payload with the first configured key.
func (s *Signer) mint(payload minted, now, exp time.Time) (Token, error) {
	jti, err := newJTI(now)
	if err != nil {
		return Token{}, err
	}
	payload.Iss = s.issuer
	payload.Aud = []string{s.audience}
	payload.Iat = now.Unix()
	payload.Exp = exp.Unix()
	payload.JTI = jti

	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": s.KeyID()})
	if err != nil {
		return Token{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Token{}, err
	}
	signing := b64(header) + "." + b64(body)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.keys[0], crypto.SHA256, digest[:])
	if err != nil {
		return Token{}, err
	}
	return Token{Value: signing + "." + b64(sig), JTI: jti, ExpiresAt: exp}, nil
}

// JWKS serves the public half of every configured key. It is what lets a
// platform or a third service trust a sandbox's or a worker's identity
// offline; the set changes only when the configuration does, so it is
// rendered once at start.
func (s *Signer) JWKS() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/jwk-set+json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(s.jwks)
	})
}

// keyDocument and publicKey are the JWK set as it is served.
type keyDocument struct {
	Keys []publicKey `json:"keys"`
}

type publicKey struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// Thumbprint is the kid of an RSA key: the RFC 7638 JWK thumbprint,
// SHA-256, base64url. It is computed from the key alone, so an operator
// who moves a key between deployments moves its kid with it and a token
// signed before the move still names the key that signed it.
func Thumbprint(pub *rsa.PublicKey) string {
	// RFC 7638 section 3: the required members of the JWK, no others, in
	// lexicographic order, with no whitespace.
	canonical := fmt.Sprintf(`{"e":%q,"kty":"RSA","n":%q}`, b64(big.NewInt(int64(pub.E)).Bytes()), b64(pub.N.Bytes()))
	sum := sha256.Sum256([]byte(canonical))
	return b64(sum[:])
}

// crockford is the alphabet of a ULID, RFC-free and case-insensitive.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newJTI mints the jti of one token: a ULID, the millisecond of the mint
// and eighty random bits, so two tokens minted in one millisecond are
// two jtis and a revocation list sorts by time.
//
// The prefixed ids of spec 001 are the store's ([[010-state]]); this
// mints the one id spec 006 owns.
func newJTI(now time.Time) (string, error) {
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[:8], uint64(now.UnixMilli())<<16)
	if _, err := rand.Read(raw[6:]); err != nil {
		return "", err
	}
	// The first character carries the top three bits of the first byte;
	// the remaining 125 bits are twenty-five characters of five, which
	// is the whole of the 128 over twenty-six characters.
	var out [26]byte
	out[0] = crockford[raw[0]>>5]
	value, bits, i := uint16(raw[0]&0x1f), 5, 1
	for _, b := range raw[1:] {
		value, bits = value<<8|uint16(b), bits+8
		for bits >= 5 {
			bits -= 5
			out[i] = crockford[(value>>bits)&0x1f]
			i++
		}
	}
	return string(out[:]), nil
}

// b64 renders bytes the way every JOSE segment is rendered.
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
