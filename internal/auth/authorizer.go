// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
)

// ProbeID is the reserved resource id every authorizer denies for every
// subject and every action. It is the shared contract's, not a value of
// Cella's own: one id is what lets one check command read one answer
// from every endpoint in the family.
const ProbeID = authz.ProbeID

// ClientOptions configures the client that asks the operator's endpoint.
// URL and Token are CELLA_AUTHORIZER_URL and CELLA_AUTHORIZER_TOKEN, and
// Timeout is CELLA_AUTHORIZER_TIMEOUT, which bounds one call, its retry
// included.
type ClientOptions struct {
	URL     string
	Token   string
	HTTP    *http.Client
	Timeout time.Duration
	Now     func() time.Time
	// Observe receives every call's result and its duration in seconds,
	// for the metric of spec 017. Optional.
	Observe func(result string, seconds float64)
}

// NewClient builds the authorizer client. It sends nothing. The cache,
// the one retry, the timeout and the failure rules are the shared
// package's, so an operator who wrote one endpoint for a sibling core
// runs it for Cella by pointing a second URL at it. The vocabulary rides
// along, so an action outside Cella's table costs no round trip.
func NewClient(o ClientOptions) (*authz.Client, error) {
	opts := authz.Options{
		URL: o.URL, Token: o.Token, HTTP: o.HTTP, Timeout: o.Timeout, Now: o.Now,
		Observe: o.Observe, Vocabulary: authorizer.Vocabulary(),
	}
	return authz.NewClient(opts)
}

// Decision is one answer as cellad reads it: the ceilings it granted,
// the filter it narrowed a list with, and how long it may be held. The
// allow itself is not a field, because a Decision only exists for one.
type Decision struct {
	Limits authorizer.Limits
	Filter *authz.Filter
	TTL    time.Duration
}

// Authorizer asks one question per request. It is the seam: an operator's
// endpoint behind the shared client, or the owner policy, and the rest of
// cellad cannot tell which.
type Authorizer struct {
	inner authz.Authorizer
}

// NewAuthorizer wraps whichever authorizer this deployment runs.
func NewAuthorizer(inner authz.Authorizer) *Authorizer {
	return &Authorizer{inner: inner}
}

// Envelope is what one call carries: the caller's subject and its claims
// verbatim, the action, the resource, and what is known about the
// request itself.
func Envelope(c Caller, info authz.Caller, action string, res authz.Resource) authz.Request {
	claims := c.Claims
	if claims == nil {
		claims = map[string]any{}
	}
	return authz.Request{
		Subject: c.Subject, Issuer: c.Issuer, Sub: c.Sub, Claims: claims,
		Workload: workloadOf(c), Action: action, Resource: res, Request: info,
	}
}

// workloadOf is the workload member of the envelope, set when the caller
// is a sandbox and absent otherwise. The members beyond the sandbox's own
// id are the store's ([[010-state]], [[022-mesh-and-spawn]]) and join it
// with the guard that reads them.
func workloadOf(c Caller) map[string]any {
	id, ok := c.Sandbox()
	if !ok {
		return nil
	}
	out := map[string]any{"id": id}
	if c.sandbox == nil {
		return out
	}
	s := c.sandbox
	out["parent"], out["root"] = s.Parent, s.Root
	out["environment"], out["mesh"] = s.Environment, s.Mesh
	out["spawn"] = map[string]any{"budget": s.Spawn.Budget, "used": s.Spawn.Used, "depth": s.Spawn.Depth}
	return out
}

// Decide asks one question and returns the answer for an allow. A deny
// on the request's own action is forbidden, with the endpoint's reason
// as the developer detail and never in the user sentence; a call that
// produced no decision is authorizer_unavailable and never an allow.
func (a *Authorizer) Decide(ctx context.Context, c Caller, info authz.Caller, action string, res authz.Resource) (Decision, error) {
	return a.decide(ctx, c, info, action, res, CodeForbidden)
}

// Lookup asks the question a resolve asks: may this caller use the
// object a manifest named. A deny is not_found, so a refused object and
// a missing one are the same answer and a manifest cannot be written to
// enumerate what somebody else owns.
func (a *Authorizer) Lookup(ctx context.Context, c Caller, info authz.Caller, action string, res authz.Resource) (Decision, error) {
	return a.decide(ctx, c, info, action, res, CodeNotFound)
}

func (a *Authorizer) decide(ctx context.Context, c Caller, info authz.Caller, action string, res authz.Resource, deny Code) (Decision, error) {
	d, err := a.inner.Authorize(ctx, Envelope(c, info, action, res))
	if err != nil {
		var unknown *authz.UnknownAction
		if errors.As(err, &unknown) {
			return Decision{}, err
		}
		return Decision{}, refuse(CodeAuthorizerUnavailable, "%s: %v", action, err)
	}
	if !d.Allow {
		return Decision{}, refuse(deny, "%s: %s", action, reasonOf(d))
	}
	limits, err := authorizer.DecodeLimits(d)
	if err != nil {
		// A ceiling the control plane cannot read is not a ceiling it
		// can hold, so the answer is no decision at all.
		return Decision{}, refuse(CodeAuthorizerUnavailable, "%s: the answer's limits: %v", action, err)
	}
	return Decision{Limits: limits, Filter: d.Filter, TTL: d.TTL}, nil
}

// reasonOf is the endpoint's reason, or a word when it named none, so a
// log line always says something.
func reasonOf(d authz.Decision) string {
	if d.Reason == "" {
		return "the authorizer named no reason"
	}
	return d.Reason
}

// Check sends the probe and reports an authorizer that allowed it, which
// is an endpoint that does not read the request. A deny is the one right
// answer, and every authorizer of the family gives it.
func (a *Authorizer) Check(ctx context.Context) error {
	return authz.Check(ctx, a.inner, authorizer.ActionSandboxRead, authorizer.KindSandbox)
}
