// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// AudienceCella is the audience of the Cella token a client hands an engine
// under the key "cella", for the environment's keys. The platform token's,
// for the device's record, is the embedder's (CredentialsOptions).
const AudienceCella = "cella"

// TokenClaims are what the engine needs to know of a token a client hands:
// whom it is addressed to, whom it names, when it expires, and whether it
// is in that person's own context. The engine verifies no token: the
// service a token is presented to does.
type TokenClaims struct {
	Audiences []string
	Subject   string
	Expiry    time.Time
	// Personal is whether the token is in its person's own context rather
	// than an organization's; a device is personal. Context names the
	// organization, for the refusal's detail.
	Personal bool
	Context  string
}

// TokenReader reads the claims of a token a client hands. The embedder
// supplies it, since how a plane's tokens carry a person and a context is
// the plane's.
type TokenReader func(token string) (TokenClaims, error)

// CredentialsOptions build a store.
type CredentialsOptions struct {
	// Now is the clock; nil is time.Now. Wait is how long a call waits for
	// a client to hand a token; zero is CredentialsWait.
	Now  func() time.Time
	Wait time.Duration
	// PlatformAudience is the audience of the platform token a client hands
	// under the key "platform"; empty takes none.
	PlatformAudience string
	// Read reads a handed token's claims; nil refuses every token.
	Read TokenReader
}

// What a client reads for each audience in the status.
const (
	CredentialValid  = "valid"
	CredentialNeeded = "needed"
)

// credentialsMargin is how long before its expiry a held token counts as
// needed, so a client has time to hand a new one before a call fails.
const credentialsMargin = time.Minute

// CredentialsWait is how long a call that needs a token waits for a client
// to hand one before it fails.
const CredentialsWait = 30 * time.Second

// ErrCredentialsNeeded is a call that waited for a token no client handed.
var ErrCredentialsNeeded = errors.New("no client handed a valid token for this audience in time")

// ErrNoClientCredentials is a token handed to an engine that serves with the
// saved login, which takes none from a client.
var ErrNoClientCredentials = errors.New("this engine serves with the saved login and takes no token from a client")

// The codes a refused token is answered with, beside the general
// invalid_credentials: a token in an organization's context, and a token of
// another person than the one whose device this computer serves.
const (
	CodeCredentialsOrg         = "credentials_org_context"
	CodeCredentialsOtherPerson = "credentials_other_person"
)

// CredentialsError is a token a client handed that the engine refuses: not
// addressed to its audience, expired or with no expiry, in an
// organization's context, or another person's. Code is one of the codes
// above, or empty for any other refusal.
type CredentialsError struct {
	Code   string
	Reason string
}

func (e *CredentialsError) Error() string { return e.Reason }

// HeldToken is one token a client handed: the bearer and when it expires.
type HeldToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CredentialsState is, for each audience, whether the engine holds a token it
// can use or needs a new one.
type CredentialsState struct {
	Cella    string `json:"cella"`
	Platform string `json:"platform"`
}

// Credentials holds the tokens a client handed the engine, in memory only:
// never written to a file or the activity log.
type Credentials struct {
	now      func() time.Time
	wait     time.Duration
	platform string
	read     TokenReader

	mu   sync.Mutex
	held map[string]HeldToken
	// heldSubject is the person the held tokens name.
	heldSubject string
	// owner is the person whose device this computer serves, once the
	// platform confirmed it: every token handed must then name owner. Empty,
	// any person's tokens are taken.
	owner string
	// changed is closed and replaced at each Put, waking every waiting call.
	changed chan struct{}
	timers  map[string]*time.Timer
	// onNeed tells the engine's clients that a token turned needed.
	onNeed func()
}

// NewCredentials builds an empty store.
func NewCredentials(o CredentialsOptions) *Credentials {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Wait <= 0 {
		o.Wait = CredentialsWait
	}
	return &Credentials{
		now: o.Now, wait: o.Wait, platform: o.PlatformAudience, read: o.Read,
		held: map[string]HeldToken{}, changed: make(chan struct{}), timers: map[string]*time.Timer{},
	}
}

// PlatformAudience is the audience of the platform token, "" when the store
// takes none.
func (c *Credentials) PlatformAudience() string { return c.platform }

// notifyNeed sets what is called each time a held token turns needed.
func (c *Credentials) notifyNeed(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onNeed = f
}

// SetOwner ties the tokens to owner, the person the platform confirmed as
// the owner of the device this computer serves: a token of another person
// is refused from now on, and tokens held for another are dropped. An empty
// owner, for a computer not connected or a device whose owner is not
// confirmed yet, takes any person's tokens.
func (c *Credentials) SetOwner(owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.owner = owner
	if owner != "" && c.heldSubject != "" && c.heldSubject != owner {
		c.dropLocked()
	}
}

// Owner is the confirmed owner the tokens are tied to, "" when none is.
func (c *Credentials) Owner() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owner
}

// Drop forgets every held token, for tokens the platform answered as
// another person's than the device's owner, so the client hands others.
func (c *Credentials) Drop() {
	c.mu.Lock()
	c.dropLocked()
	f := c.onNeed
	c.mu.Unlock()
	if f != nil {
		f()
	}
}

// Subject is the person the held tokens name, "" when none is held.
func (c *Credentials) Subject() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.heldSubject
}

// dropLocked forgets every held token. The caller holds mu.
func (c *Credentials) dropLocked() {
	for audience, timer := range c.timers {
		timer.Stop()
		delete(c.timers, audience)
	}
	c.held, c.heldSubject = map[string]HeldToken{}, ""
}

// Put keeps the tokens given, by audience, after checking every one: a token
// for another audience, one that expires within a minute, one in an
// organization's context, tokens that name different people, and a token of
// another person than the device's confirmed owner are refused, and nothing
// is kept.
func (c *Credentials) Put(tokens map[string]HeldToken) error {
	now := c.now()
	checked := map[string]HeldToken{}
	subject := ""
	for audience, t := range tokens {
		expiry, sub, err := checkToken(audience, t, now, c.read)
		if err != nil {
			return err
		}
		if subject != "" && sub != subject {
			return &CredentialsError{Reason: "the tokens handed together name different people"}
		}
		subject = sub
		checked[audience] = HeldToken{Token: t.Token, ExpiresAt: expiry}
	}
	if len(checked) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.owner != "" && subject != c.owner {
		return &CredentialsError{Code: CodeCredentialsOtherPerson, Reason: "the token is another person's than the one whose device this computer serves"}
	}
	if c.heldSubject != "" && c.heldSubject != subject {
		// Tokens of another person never stand beside these.
		c.dropLocked()
	}
	c.heldSubject = subject
	for audience, t := range checked {
		c.held[audience] = t
		if timer := c.timers[audience]; timer != nil {
			timer.Stop()
		}
		c.timers[audience] = time.AfterFunc(t.ExpiresAt.Add(-credentialsMargin).Sub(now), func() {
			c.mu.Lock()
			f := c.onNeed
			c.mu.Unlock()
			if f != nil {
				f()
			}
		})
	}
	close(c.changed)
	c.changed = make(chan struct{})
	return nil
}

// Wait blocks until a usable token for audience is held, with no deadline,
// and answers the context's error when it ends first. It is for a step that
// cannot go on without the person's sign-in, such as minting this
// computer's key, which waits for the client however long that takes.
func (c *Credentials) Wait(ctx context.Context, audience string) error {
	for {
		c.mu.Lock()
		t, ok := c.held[audience]
		changed := c.changed
		c.mu.Unlock()
		if ok && c.now().Add(credentialsMargin).Before(t.ExpiresAt) {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Token answers a usable token for audience, waiting for a client to hand
// one when none is held, up to the store's wait.
func (c *Credentials) Token(ctx context.Context, audience string) (string, error) {
	deadline := time.NewTimer(c.wait)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		t, ok := c.held[audience]
		changed := c.changed
		c.mu.Unlock()
		if ok && c.now().Add(credentialsMargin).Before(t.ExpiresAt) {
			return t.Token, nil
		}
		select {
		case <-changed:
		case <-deadline.C:
			return "", fmt.Errorf("%w: %s", ErrCredentialsNeeded, audience)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// State is, for each audience, whether a usable token is held.
func (c *Credentials) State() CredentialsState {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	state := func(audience string) string {
		if t, ok := c.held[audience]; ok && now.Add(credentialsMargin).Before(t.ExpiresAt) {
			return CredentialValid
		}
		return CredentialNeeded
	}
	return CredentialsState{Cella: state(AudienceCella), Platform: state(c.platform)}
}

// checkToken checks a handed token by the claims read reads, without
// verifying its signature, which the service it is presented to does: it
// must be addressed to the audience, name a person, be in that person's own
// context rather than an organization's (a device is personal), and expire
// more than a minute from now. It answers its expiry, the earlier of its
// own and the one the client gave, and the person it names.
func checkToken(audience string, t HeldToken, now time.Time, read TokenReader) (time.Time, string, error) {
	if strings.TrimSpace(t.Token) == "" {
		return time.Time{}, "", &CredentialsError{Reason: "the " + audience + " token is empty"}
	}
	if read == nil {
		return time.Time{}, "", &CredentialsError{Reason: "this engine reads no token a client hands"}
	}
	claims, err := read(t.Token)
	if err != nil {
		return time.Time{}, "", &CredentialsError{Reason: "the " + audience + " token does not read: " + err.Error()}
	}
	if !slices.Contains(claims.Audiences, audience) {
		return time.Time{}, "", &CredentialsError{Reason: fmt.Sprintf("the %s token is addressed to %v, not to %s", audience, claims.Audiences, audience)}
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return time.Time{}, "", &CredentialsError{Reason: "the " + audience + " token names no person"}
	}
	if !claims.Personal {
		return time.Time{}, "", &CredentialsError{Code: CodeCredentialsOrg, Reason: "the " + audience + " token is in the context of the organization " + claims.Context + ", and a device is personal"}
	}
	expiry := t.ExpiresAt
	if !claims.Expiry.IsZero() && (expiry.IsZero() || claims.Expiry.Before(expiry)) {
		expiry = claims.Expiry
	}
	if expiry.IsZero() {
		return time.Time{}, "", &CredentialsError{Reason: "the " + audience + " token names no expiry"}
	}
	if !now.Add(credentialsMargin).Before(expiry) {
		return time.Time{}, "", &CredentialsError{Reason: fmt.Sprintf("the %s token expires at %s, within a minute", audience, expiry.UTC().Format(time.RFC3339))}
	}
	return expiry.UTC(), claims.Subject, nil
}
