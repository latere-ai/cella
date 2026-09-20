// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli

import (
	"context"
	"errors"

	"latere.ai/x/cella/internal/cellaclient"
)

// The exit codes of design 011. One scheme, by the response's status class
// with named exceptions, so a code the client does not know still has an
// exit.
const (
	// exitOK is success, and under a session a child that exited 0.
	exitOK = 0
	// exitFailed is a 5xx, including every *_unavailable, and a stream that
	// ended before its end.
	exitFailed = 1
	// exitUsage is a flag, a reference or a document the command could not
	// read.
	exitUsage = 2
	// exitRefused is 400, 401, 403, 413, 415, 422 and 429.
	exitRefused = 3
	// exitNotFound is 404.
	exitNotFound = 4
	// exitConflict is 409, version_conflict and phase_conflict included.
	exitConflict = 5
	// exitUnreachable is a dial, a TLS handshake or a timeout before any
	// status.
	exitUnreachable = 7
	// exitSessionFailed is, under a session, any failure that outside one
	// would be 1, 3, 4 or 5.
	exitSessionFailed = 125
	// exitCannotStart is, under a session, a command that never began: the
	// capability is missing, or an error frame arrived before the first
	// byte.
	exitCannotStart = 126
	// exitSessionUnreachable is, under a session, the server unreachable.
	exitSessionUnreachable = 127
)

// exitFor maps one failure to its exit code. underSession says the failure
// happened under exec or attach, where design 011's second column applies,
// and started whether the command had written a byte by then.
func exitFor(err error, underSession, started bool) int {
	if err == nil {
		return exitOK
	}
	var usage usageError
	if errors.As(err, &usage) {
		return exitUsage
	}
	var gone *cellaclient.Unreachable
	if errors.As(err, &gone) || errors.Is(err, context.DeadlineExceeded) {
		if underSession {
			return exitSessionUnreachable
		}
		return exitUnreachable
	}
	var refusal *cellaclient.Error
	if !errors.As(err, &refusal) {
		// Anything else is a transfer that ended early or a failure of the
		// caller's own file, which outside a session is the same class as a
		// server that failed.
		if underSession {
			return exitSessionFailed
		}
		return exitFailed
	}
	if underSession {
		if !started && refusal.Code == "capability_unsupported" {
			return exitCannotStart
		}
		return exitSessionFailed
	}
	return exitForStatus(refusal.Status)
}

// exitForStatus is the class rule: a code this client has never seen still
// exits by the status its response carried.
func exitForStatus(status int) int {
	switch {
	case status == 404:
		return exitNotFound
	case status == 409:
		return exitConflict
	case status/100 == 4:
		return exitRefused
	case status/100 == 5:
		return exitFailed
	default:
		return exitFailed
	}
}
