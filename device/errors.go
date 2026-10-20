// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"errors"
	"strings"
)

// Error is a refusal of the engine: one code to branch on, one fixed
// sentence for the person, the next step when there is one, and the
// developer detail of this occurrence. The sentences and next steps are
// the latere command's words, the defaults; an embedder with its own
// surface shows its own words for each code.
type Error struct {
	Code    string
	Message string
	Hint    string
	Detail  string
	// Err is the underlying failure, kept for errors.Is and errors.As.
	Err error
}

// Error renders the refusal as a terminal prints it: the code and the
// sentence, the next step, and the detail on its own line.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Code + ": " + e.Message)
	if e.Hint != "" {
		b.WriteString("\n" + e.Hint)
	}
	if e.Detail != "" {
		b.WriteString("\ndetail: " + e.Detail)
	}
	return b.String()
}

// Unwrap is the underlying failure.
func (e *Error) Unwrap() error { return e.Err }

// CodeOf is the code of an engine refusal, or "" for any other error.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// The codes, each with its fixed sentence and next step in refusals.
const (
	CodeRunsAsRoot         = "device_runs_as_root"
	CodeAlreadyServed      = "device_already_served"
	CodeOrgContext         = "device_org_context"
	CodeNoSignIn           = "device_no_sign_in"
	CodeNotConfirmed       = "device_not_confirmed"
	CodePolicyInvalid      = "device_policy_invalid"
	CodeNotConnected       = "device_not_connected"
	CodeAlreadyConnected   = "device_already_connected"
	CodeRemoved            = "device_removed"
	CodeWorkerLimit        = "device_worker_limit"
	CodeWorkerReplaced     = "device_worker_replaced"
	CodeSignInExpired      = "device_sign_in_expired"
	CodeSandboxUnavailable = "device_sandbox_unavailable"
	CodeControlUnavailable = "device_control_unavailable"
	CodeNotServing         = "device_not_serving"
	CodeKeyUnavailable     = "device_key_unavailable"
	CodeRefused            = "device_refused"
	CodeUnsupported        = "device_unsupported"
	CodeServeFailed        = "device_serve_failed"
	CodeRemoveFailed       = "device_remove_failed"
	CodeRenameFailed       = "device_rename_failed"
	CodeCredentialsNeeded  = "device_credentials_needed"

	// The platform's own refusals of a device's create (platform spec 220),
	// which reach the CLI as Cella's forbidden with the reason in its
	// detail. The CLI answers each with the platform's code.
	CodePersonalOnly   = "device_personal_only"
	CodeSignInRequired = "sign_in_required"
	CodeKeyRefused     = "device_key_refused"
	CodeLimit          = "device_limit"
	CodeInvalid        = "device_invalid"
)

// refusals is the one table of sentences and next steps, by code.
var refusals = map[string]struct{ message, hint string }{
	CodeRunsAsRoot: {
		"Devices refuse to run as root.",
		"Run `latere devices connect` as the person whose folders this computer shares.",
	},
	CodeAlreadyServed: {
		"This computer is already being served.",
		"Stop the other `latere devices connect` or `latere devices serve` first.",
	},
	CodeOrgContext: {
		"A device is personal, and you are signed in to an organization.",
		"Run `latere org --personal`, then connect again.",
	},
	CodeNoSignIn: {
		"You are not signed in, and connecting this computer needs your sign-in.",
		"Run `latere login`, then connect again.",
	},
	CodeNotConfirmed: {
		"Nothing was connected.",
		"Run `latere devices connect` again and type yes to connect this computer.",
	},
	CodePolicyInvalid: {
		"The host policy file cannot be used.",
		"Fix it, then run `latere devices policy` to check it.",
	},
	CodeNotConnected: {
		"This computer is not connected.",
		"Run `latere devices connect` to connect it.",
	},
	CodeAlreadyConnected: {
		"This computer is already connected; its folders and network are in the host policy file.",
		"Edit the file, then run `latere devices connect` with no flags. `latere devices policy --session-start` changes how sessions start.",
	},
	CodeRemoved: {
		"This computer was removed from your devices.",
		"Run `latere devices connect` to connect it again.",
	},
	CodeWorkerLimit: {
		"Another computer is serving with this device's key right now.",
		"If that is not you, remove this device with `latere devices remove`.",
	},
	CodeWorkerReplaced: {
		"Another process connected with this device's key and took over.",
		"If that was not you, remove this device with `latere devices remove`.",
	},
	CodeSignInExpired: {
		"Your sign-in has expired, and so has this computer's key.",
		"Sign in again with `latere login`, then connect again.",
	},
	CodeSandboxUnavailable: {
		"This computer cannot run sessions in its sandbox.",
		"The detail names what to install.",
	},
	CodeControlUnavailable: {
		"The control socket could not be opened.",
		"",
	},
	CodeNotServing: {
		"This computer is not serving right now, so no session is using it.",
		"Start it with `latere devices connect`.",
	},
	CodeKeyUnavailable: {
		"This computer's key could not be read or kept.",
		"",
	},
	CodeRefused: {
		"The platform refused to connect this computer.",
		"",
	},
	CodeUnsupported: {
		"Devices run on macOS and Linux.",
		"",
	},
	CodeServeFailed: {
		"This computer stopped serving.",
		"",
	},
	CodeRemoveFailed: {
		"The device could not be removed.",
		"Try again, or remove it from the console.",
	},
	CodeCredentialsNeeded: {
		"This computer needs your sign-in from the application that serves it.",
		"The application hands it again on its own; if this goes on, sign in to the application again.",
	},
	CodeRenameFailed: {
		"The device could not be renamed, so its name is unchanged.",
		"Try again.",
	},
	CodePersonalOnly: {
		"A device is personal, and this sign-in is in an organization's context.",
		"Run `latere org --personal`, then connect again.",
	},
	CodeSignInRequired: {
		"Connecting a computer needs your own sign-in on it.",
		"Run `latere login`, then connect again.",
	},
	CodeKeyRefused: {
		"Connecting a computer needs your own sign-in, not a key.",
		"Run `latere login` without --token, then connect again.",
	},
	CodeLimit: {
		"You have connected as many computers as one person may.",
		"See them with `latere devices list`, and remove one with `latere devices remove ID`.",
	},
	CodeInvalid: {
		"The platform refused this computer's request to connect.",
		"",
	},
}

// Fail builds the refusal for code with the developer detail of this
// occurrence and the error behind it, which may be nil.
func Fail(code, detail string, err error) *Error {
	r := refusals[code]
	if detail == "" && err != nil {
		detail = err.Error()
	}
	return &Error{Code: code, Message: r.message, Hint: r.hint, Detail: detail, Err: err}
}
