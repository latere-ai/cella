// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"latere.ai/x/cella/runtime"
)

// dialDriver declares Dial and implements no Dialer, which is a driver whose
// capability set and whose methods disagree. It embeds the interface rather
// than the native driver so no method is promoted by accident.
type dialDriver struct{ runtime.Driver }

func (dialDriver) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{Files: true, Dial: true}
}

// TestDialGate proves the dial route is registered and answers the gate:
// design 004's capability decides, and an environment without it reads the
// refusal rather than the mux's not-found.
func TestDialGate(t *testing.T) {
	t.Run("undeclared", func(t *testing.T) {
		f := setup(t, nil)
		obj := f.sandbox("plain")
		body := f.request("GET", "/v1/sandboxes/"+obj.Status.ID+"/dial/8080", f.alice, "", 422)
		if !strings.Contains(string(body), "capability_unsupported") {
			t.Fatalf("the gate answered %q", body)
		}
		if !strings.Contains(string(body), "reaches no port") {
			t.Fatalf("the developer detail does not name the reason: %q", body)
		}
	})
	t.Run("declared and unimplemented", func(t *testing.T) {
		f := setupDriver(t, nil, func(d runtime.Driver) runtime.Driver { return dialDriver{d} })
		obj := f.sandbox("plain")
		body := f.request("GET", "/v1/sandboxes/"+obj.Status.ID+"/dial/8080", f.alice, "", 422)
		if !strings.Contains(string(body), "serves no dial stream") {
			t.Fatalf("the developer detail does not name what the environment declared: %q", body)
		}
	})
	t.Run("read and authorize before the gate", func(t *testing.T) {
		f := setup(t, nil)
		obj := f.sandbox("plain")
		f.request("GET", "/v1/sandboxes/"+obj.Status.ID+"/dial/8080", f.bob, "", 403)
		f.request("GET", "/v1/sandboxes/sbx_01j0000000000000000000000/dial/8080", f.alice, "", 404)
	})
}
