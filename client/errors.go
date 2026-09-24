// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"latere.ai/x/pkg/httpjson"
)

// Error is one refusal of the API, decoded from the envelope of design 008:
// the code a caller decides on, the fixed user sentence, the request id the
// server stamped, and the details the code carries. The status is kept beside
// the code, because a caller that meets a code it does not know can still
// decide by the status's class.
type Error struct {
	// Status is the response's status: 500 for a failure reported inside a
	// stream or a socket, which carries no status of its own.
	Status int
	// Code is the API's code, which is what a caller decides on.
	Code string
	// Message is the API's sentence for the code, written for a person.
	Message string
	// RequestID is the response's own id, which is what identifies the call
	// in the server's journal. It is the server's and not the one the
	// request carried.
	RequestID string
	// Paths are the fields a code that names fields named.
	Paths []string
	// Detail is the developer sentence, which says what about this request
	// the code refers to.
	Detail string
	// Details is the envelope's details object as it was decoded, the three
	// fields above included, so a code that carries more reaches the
	// caller whole.
	Details map[string]any
	// RetryAfter is the header a 429 carries.
	RetryAfter string
}

// Error is the API's sentence, or the status where the answer carried none.
func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the server answered %d", e.Status)
}

// Unreachable is a request that reached no status: the dial, the TLS
// handshake, or the wait for the first response byte failed. It is a
// separate type because a server that refused and a server that was not
// there call for different decisions.
type Unreachable struct {
	// Op is the method and the path of the request.
	Op string
	// Err is the transport's failure.
	Err error
}

// Error names the request and the failure.
func (e *Unreachable) Error() string { return e.Op + ": " + e.Err.Error() }

// Unwrap is the transport's failure.
func (e *Unreachable) Unwrap() error { return e.Err }

// CodeOf is the API's code for an error, or the empty string for anything
// that is not a refusal.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// errorFrom reads a refusal. The body is the envelope where the handler
// wrote one; a body that is not an envelope, which is what a proxy or a
// listener that is not this API answers, still becomes an Error carrying
// the status, so every failure has a code path and an exit.
func errorFrom(resp *http.Response, body []byte) *Error {
	e := &Error{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
	var envelope httpjson.ErrorEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Code != "" {
		e.Code = envelope.Error.Code
		e.Message = envelope.Error.Message
		e.RequestID, _ = envelope.Error.Details["request_id"].(string)
		e.Detail, _ = envelope.Error.Details["detail"].(string)
		e.Paths = append(e.Paths, list(envelope.Error.Details["paths"])...)
		e.Details = envelope.Error.Details
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
	}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	if e.RequestID == "" {
		e.RequestID = resp.Header.Get("X-Request-ID")
	}
	return e
}

// list reads the envelope's paths, which JSON decodes as a list of any.
func list(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
