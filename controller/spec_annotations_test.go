// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// TestSpecOfCarriesAnnotations: the driver's create spec carries the
// manifest's annotations, so a worker's host policy sees what the caller
// wrote for people (spec 082).
func TestSpecOfCarriesAnnotations(t *testing.T) {
	obj := v1.Sandbox{Metadata: v1.Metadata{
		Name:        "ses-1",
		Annotations: map[string]string{"example.com/agent-name": "Site helper"},
	}}
	spec := specOf(obj, driver.Lifecycle{}, driver.Egress{}, nil, "")
	if got := spec.Annotations["example.com/agent-name"]; got != "Site helper" {
		t.Errorf("specOf carried annotation %q, want %q", got, "Site helper")
	}
}
