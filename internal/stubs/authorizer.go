// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package stubs

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/cella/authorizer"
)

// DenyHeader refuses one action for one request, without changing what
// the endpoint answers the next caller. The rule table is the way to
// refuse for a whole run; this is the way a table-driven test in another
// process refuses one row.
const DenyHeader = "X-Stub-Deny"

// The failure modes every stub role produces, which are the forms of
// unavailability specs 006 and 007 name.
const (
	// FailTimeout never answers, so the caller's deadline decides.
	FailTimeout = "timeout"
	// FailMalformed answers 200 with a body that is not JSON.
	FailMalformed = "malformed"
	// FailNoAllow answers 200 with a body that parses and carries no
	// verdict.
	FailNoAllow = "no-allow"
	// FailStatusPrefix is "status:", followed by the code to answer.
	FailStatusPrefix = "status:"
)

// AuthorizerOptions configures the authorizer role. The endpoint itself
// is the shared stub of latere.ai/x/pkg/authz/stub, so what a core's tier
// drives here is what every core in the family drives, the reserved probe
// id denied for every subject included.
type AuthorizerOptions struct {
	// Addr is the listen address, empty to turn the role off.
	Addr string
	// Token is the bearer the endpoint requires. Empty takes the shared
	// stub's own default.
	Token string
	// Deny names actions refused for every subject.
	Deny []string
	// Limits and Filter are JSON objects returned on every allow: the
	// ceilings of spec 007 and the narrowing of spec 006.
	Limits string
	Filter string
	// TTL is how long an allow may be cached, in seconds. Zero leaves the
	// answer without one, so the caller applies its own default.
	TTL int
	// Fail is one of the modes above.
	Fail string
}

// newAuthorizer builds the authorizer's handler. The core's vocabulary is
// handed over, so an action outside spec 006's table is refused with the
// 400 a conforming endpoint answers and never decided from the table.
func newAuthorizer(o AuthorizerOptions) (http.Handler, func(), error) {
	token := o.Token
	if token == "" {
		token = stub.DefaultToken
	}
	s := stub.NewHandler(stub.WithToken(token), stub.WithVocabulary(authorizer.Vocabulary()))
	allow := stub.Rule{Allow: true, TTL: o.TTL}
	if o.Limits != "" {
		if err := json.Unmarshal([]byte(o.Limits), &allow.Limits); err != nil {
			return nil, nil, fmt.Errorf("the limits are not a JSON object: %w", err)
		}
	}
	if o.Filter != "" {
		if err := json.Unmarshal([]byte(o.Filter), &allow.Filter); err != nil {
			return nil, nil, fmt.Errorf("the filter is not a JSON object: %w", err)
		}
	}
	rules := []stub.Rule{allow}
	for _, action := range o.Deny {
		if action = strings.TrimSpace(action); action != "" {
			rules = append(rules, stub.Rule{Action: action, Reason: "the stub is told to deny " + action})
		}
	}
	// A later rule wins, so the allow that carries the ceilings is first
	// and each refusal stands over it.
	s.SetRules(rules...)
	if err := failAuthorizer(s, o.Fail); err != nil {
		return nil, nil, err
	}
	return denyOneAction(s.Handler(), token), s.Close, nil
}

// failAuthorizer puts one outage mode in force.
func failAuthorizer(s *stub.Server, mode string) error {
	switch mode = strings.TrimSpace(mode); {
	case mode == "":
	case mode == FailTimeout:
		s.Hang()
	case mode == FailMalformed:
		s.FailBody(stub.BodyMalformed)
	case mode == FailNoAllow:
		s.FailBody(stub.BodyNoAllow)
	case strings.HasPrefix(mode, FailStatusPrefix):
		code, err := statusOf(mode)
		if err != nil {
			return err
		}
		s.Fail(code)
	default:
		return fmt.Errorf("the failure mode is %q; %s, %s, %s or %s<code>", mode, FailTimeout, FailMalformed, FailNoAllow, FailStatusPrefix)
	}
	return nil
}

// statusOf reads the code out of "status:<code>".
func statusOf(mode string) (int, error) {
	raw := strings.TrimPrefix(mode, FailStatusPrefix)
	code, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || code < 100 || code > 599 {
		return 0, fmt.Errorf("the failure mode is %q; the status after %s is an HTTP code", mode, FailStatusPrefix)
	}
	return code, nil
}

// denyOneAction refuses the action the request's own header names and
// passes everything else to the endpoint. The bearer is checked first,
// because a stub that refused a decision to a caller it never
// authenticated would answer a question nobody was allowed to ask.
func denyOneAction(next http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := strings.TrimSpace(r.Header.Get(DenyHeader))
		bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if action == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(bearer)), []byte(token)) != 1 {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req authz.Request
		if err := json.Unmarshal(body, &req); err == nil && req.Action == action {
			writeJSON(w, http.StatusOK, map[string]any{"allow": false, "reason": "the request asked for " + action + " to be denied"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
