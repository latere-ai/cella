// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package admission is the client half of spec 007's admission webhook:
// one POST per Sandbox apply to the endpoint an operator wrote, which
// returns the manifest to continue with or a refusal. It is where a
// platform's image catalog, plan shapes and policy profiles reach the
// control plane, and the control plane learns none of them: it learns
// that a manifest came back or that a refusal did.
//
//	Resolve stage 3            the operator's endpoint
//	---------------            -----------------------
//	POST {CELLA_ADMISSION_URL}
//	Authorization: Bearer ---> the defaulted manifest, the actor, the action
//	<--- 200 {"allow", "manifest", "reason", "warnings"}
//
// There is no retry. The authorizer of spec 006 retries once on a
// connection that failed before a response line arrived, because its call
// is a pure read; an admission call may rewrite the manifest, so a resend
// could apply one mutation twice. Everything that is not a parsed 200
// carrying allow is admission_unavailable and never a pass.
package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// DefaultTimeout bounds one call. Spec 007 fixes it, and MinTimeout and
// MaxTimeout bound what an operator may set it to: a deadline under a
// tenth of a second refuses every call that crosses a network, and one
// over half a minute holds an apply open longer than a caller waits.
const (
	DefaultTimeout = 3 * time.Second
	MinTimeout     = 100 * time.Millisecond
	MaxTimeout     = 30 * time.Second
)

// MaxResponseBytes caps the answer. Spec 007 fixes it: a decision is a
// manifest and a few sentences, and a body past the cap is no decision.
const MaxResponseBytes = 1 << 20

// The warning bounds of spec 007: at most eight sentences of at most 256
// characters, appended to the resolved manifest's status.warnings.
const (
	MaxWarnings     = 8
	MaxWarningRunes = 256
)

// The results Observe is called with, one per outcome an operator's
// dashboard separates: a manifest came back, a policy refused, or the
// endpoint gave no decision at all.
const (
	ResultAllow   = "allow"
	ResultRefused = "refused"
	ResultError   = "error"
)

// Options configures the client.
type Options struct {
	// URL is CELLA_ADMISSION_URL and Token the bearer CELLA_ADMISSION_TOKEN
	// it requires. Both are required: an endpoint that decides what a
	// caller may run is never open.
	URL   string
	Token string
	// HTTP sends the calls. An instrumented client is built when none is
	// given, so every outbound call of this repository carries a span.
	HTTP *http.Client
	// Timeout bounds one call. DefaultTimeout when zero.
	Timeout time.Duration
	// Observe receives every call's result and its duration in seconds,
	// for the metric of spec 017. Optional.
	Observe func(result string, seconds float64)
}

// Client asks one endpoint per apply.
type Client struct {
	url     string
	token   string
	http    *http.Client
	timeout time.Duration
	observe func(string, float64)
}

// New builds the client. It sends nothing. A URL without a bearer is
// refused here rather than at the first apply, so a deployment learns at
// start-up that its endpoint would have been called unauthenticated.
func New(o Options) (*Client, error) {
	if o.URL == "" {
		return nil, errors.New("CELLA_ADMISSION_URL is unset; there is no endpoint to ask")
	}
	if o.Token == "" {
		return nil, errors.New("CELLA_ADMISSION_TOKEN is unset while CELLA_ADMISSION_URL is set, and the endpoint requires a bearer")
	}
	c := &Client{url: o.URL, token: o.Token, http: o.HTTP, timeout: o.Timeout, observe: o.Observe}
	if c.http == nil {
		c.http = &http.Client{Transport: otel.Transport(nil), CheckRedirect: noRedirect}
	}
	if c.timeout == 0 {
		c.timeout = DefaultTimeout
	}
	if c.observe == nil {
		c.observe = func(string, float64) {}
	}
	return c, nil
}

// URL is the endpoint the client asks.
func (c *Client) URL() string { return c.url }

// noRedirect hands a 3xx back as the answer instead of following it. A
// redirect is a second request carrying the manifest and the bearer to an
// address the operator did not configure, which is both the retry this
// contract forbids and a bearer where it does not belong. The redirect is
// then no decision, like any other status that is not 200.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Admit is the AdmitFunc stage 3 calls. It returns the object to continue
// with and the endpoint's warnings, a manifest error carrying
// admission_refused with the endpoint's own code as the developer detail,
// or one carrying admission_unavailable.
func (c *Client) Admit(ctx context.Context, in *v1.Sandbox, req manifest.AdmitRequest) (*v1.Sandbox, []string, error) {
	started := time.Now()
	out, warnings, err := c.admit(ctx, in, req)
	c.observe(resultOf(err), time.Since(started).Seconds())
	return out, warnings, err
}

func resultOf(err error) string {
	var known *manifest.Error
	switch {
	case err == nil:
		return ResultAllow
	case errors.As(err, &known) && known.Code == manifest.CodeAdmissionRefused:
		return ResultRefused
	default:
		return ResultError
	}
}

func (c *Client) admit(ctx context.Context, in *v1.Sandbox, req manifest.AdmitRequest) (*v1.Sandbox, []string, error) {
	body, err := envelopeOf(in, req)
	if err != nil {
		return nil, nil, unavailable("the request could not be built: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	post, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, unavailable("the request could not be built: %v", err)
	}
	post.Header.Set("Authorization", "Bearer "+c.token)
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("Accept", "application/json")
	if req.RequestID != "" {
		post.Header.Set("X-Request-Id", req.RequestID)
	}
	res, err := c.http.Do(post)
	if err != nil {
		return nil, nil, unavailable("%v", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, res.Body); _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, nil, unavailable("the endpoint answered %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, nil, unavailable("the answer could not be read: %v", err)
	}
	if len(raw) > MaxResponseBytes {
		return nil, nil, unavailable("the answer is larger than %d bytes", MaxResponseBytes)
	}
	var answer response
	if err = json.Unmarshal(raw, &answer); err != nil {
		return nil, nil, unavailable("the answer is not JSON: %v", err)
	}
	// An answer that names no allow is no decision. Reading an absent
	// member as false would turn an endpoint's bug into a refusal every
	// caller sees and nobody can act on.
	if answer.Allow == nil {
		return nil, nil, unavailable("the answer names no allow")
	}
	if !*answer.Allow {
		return nil, nil, refused(answer.Reason)
	}
	return c.allowed(in, answer)
}

// allowed reads the manifest an allow carries. An absent one leaves the
// input unchanged; one that is there is decoded the way a caller's
// manifest is decoded, strictly, so an endpoint that writes a field the
// schema does not know is unknown_field naming it, and not a field the
// data plane silently drops.
func (c *Client) allowed(in *v1.Sandbox, answer response) (*v1.Sandbox, []string, error) {
	warnings := boundWarnings(answer.Warnings)
	if len(answer.Manifest) == 0 || string(answer.Manifest) == "null" {
		out := *in
		return &out, warnings, nil
	}
	out, err := manifest.Decode(answer.Manifest, "application/json")
	if err != nil {
		return nil, nil, err
	}
	return &out, warnings, nil
}

// boundWarnings holds the endpoint's warnings to spec 007's bounds. A
// sentence past the bound is truncated rather than dropped: what it says
// is the endpoint's, and a caller reads as much of it as the contract
// carries.
func boundWarnings(warnings []string) []string {
	if len(warnings) > MaxWarnings {
		warnings = warnings[:MaxWarnings]
	}
	out := make([]string, 0, len(warnings))
	for _, w := range warnings {
		if runes := []rune(w); len(runes) > MaxWarningRunes {
			w = string(runes[:MaxWarningRunes])
		}
		out = append(out, w)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// unavailable is every outcome that is not a decision: spec 007 gives the
// one code, so a caller reads one sentence and an operator reads which of
// the failures it was in the developer detail.
func unavailable(format string, args ...any) error {
	return &manifest.Error{
		Code:   manifest.CodeAdmissionUnavailable,
		Detail: "the admission endpoint gave no decision: " + fmt.Sprintf(format, args...),
	}
}

// refused carries the endpoint's reason verbatim. The reason is a stable
// code, optionally followed by a colon and a figure, and the client
// neither parses it nor changes its case: an endpoint that answers
// ceiling_exceeded is quoted and not paraphrased.
func refused(reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "the endpoint named no reason"
	}
	return &manifest.Error{Code: manifest.CodeAdmissionRefused, Detail: reason}
}
