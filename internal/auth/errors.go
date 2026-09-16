// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package auth is cellad's side of spec 006: who a caller is, and who
// decides what that caller may do.
//
// Who is [Verifier]. A bearer is verified against one of the issuers
// CELLA_OIDC_ISSUERS lists, through latere.ai/x/pkg/authkit/jwt, and
// becomes a [Caller]: the rendered subject, its two halves apart, and
// every claim of the token verbatim. No claim is read for meaning here.
// An issuer's organisation, role or group claim means something to the
// authorizer that reads it and nothing to the control plane.
//
// What is the authorizer. cellad asks one endpoint per request through
// latere.ai/x/pkg/authz's client, with the vocabulary of
// latere.ai/x/cella/authorizer, and caches, retries and fails closed by
// that package's rules. With no endpoint configured the owner policy of
// this package answers instead, built on authz.Policy.
//
// Nothing here calls an issuer while a request is served. The discovery
// documents and the key sets are read at start and refreshed by the
// shared verifier on its own schedule.
package auth

import (
	"errors"
	"fmt"
)

// Code is why a request was refused, in the vocabulary spec 006 and
// spec 008 share. The HTTP envelope is spec 008's; this package names
// the reason and nothing about the status line.
type Code string

// The refusals of spec 006.
const (
	// CodeUnauthenticated is a request with no bearer, or one no listed
	// issuer could have signed. 401.
	CodeUnauthenticated Code = "unauthenticated"
	// CodeForbidden is a deny on the request's own action. 403.
	CodeForbidden Code = "forbidden"
	// CodeNotFound is a deny on an object reached through a lookup, so a
	// refused object and a missing one are one answer. 404.
	CodeNotFound Code = "not_found"
	// CodeAuthorizerUnavailable is a call that produced no decision. 503,
	// and never an allow.
	CodeAuthorizerUnavailable Code = "authorizer_unavailable"
)

// Error is a refusal: the code a caller is answered with and the detail
// a developer reads. The detail never reaches the user sentence, so an
// authorizer's reason is a log line and not a page.
type Error struct {
	Code   Code
	Detail string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Detail }

// refuse builds a refusal.
func refuse(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// CodeOf reads the refusal code of an error, and "" for an error that is
// not one of this package's.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
