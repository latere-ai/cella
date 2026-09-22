// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/manifest"
)

// dial serves GET /v1/sandboxes/{id}/dial/{port}: design 008's raw byte
// stream to a port inside the sandbox, behind design 004's Dial capability.
//
// The route is registered so that the gate answers and not the mux. An
// environment without the capability owes the caller 422 and the code that
// names the reason, where a 404 from an unregistered pattern would say the
// sandbox is missing, which it is not.
//
// Design 004 declares Dialer and no driver in this repository implements it,
// so the byte pump behind the gate is not served either: a socket that
// carries nothing tells a caller less than a refusal that says what the
// environment cannot provide.
func (h *handler) dial(w http.ResponseWriter, r *http.Request) {
	obj, err := h.authorizedObject(r, authorizer.ActionSandboxExec)
	if err != nil {
		respondError(w, err)
		return
	}
	detail := "the environment reaches no port inside the sandbox"
	if h.Controller.CapabilitiesOf(obj.Status.Environment).Dial {
		detail = "the " + h.Controller.DriverNameOf(obj.Status.Environment) + " driver declares Dial and this server serves no dial stream"
	}
	respondError(w, &manifest.Error{Code: "capability_unsupported", Detail: detail})
}
