// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

// Environment names where sandboxes run and what that place can enforce. A
// manifest resolves against the environment it names: the isolation class is
// what the driver actually provides, and the capabilities are what its
// operations honour. Registration, capacity, scheduling and pools are the
// worker contract's and are not declared here yet.
type Environment struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   Metadata          `json:"metadata"`
	Spec       EnvironmentSpec   `json:"spec"`
	Status     EnvironmentStatus `json:"status,omitzero"`
}

// EnvironmentSpec is what the operator declared.
type EnvironmentSpec struct {
	Isolation Isolation `json:"isolation"`
}

// EnvironmentStatus is what the control plane observed of the driver serving
// the environment.
type EnvironmentStatus struct {
	ID           string       `json:"id,omitempty"`
	Driver       string       `json:"driver,omitempty"`
	Isolation    Isolation    `json:"isolation,omitempty"`
	Capabilities Capabilities `json:"capabilities,omitzero"`
}
