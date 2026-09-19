// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package v1 defines the implemented Sandbox API contract.
package v1

import "time"

const APIVersion = "cella.latere.ai/v1beta1"

// Sandbox describes a workspace and its server-owned runtime state.
type Sandbox struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   Metadata      `json:"metadata"`
	Spec       SandboxSpec   `json:"spec"`
	Status     SandboxStatus `json:"status,omitzero"`
}
type Metadata struct {
	Name        string            `json:"name,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// SandboxSpec currently supports a direct native workspace. Additional
// boundaries must be implemented before their fields can be accepted.
type SandboxSpec struct {
	Environment string            `json:"environment,omitempty"`
	Image       string            `json:"image,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Workdir     string            `json:"workdir,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}
type SandboxStatus struct {
	ID             string    `json:"id"`
	Owner          string    `json:"owner"`
	Environment    string    `json:"environment"`
	Driver         string    `json:"driver"`
	Isolation      string    `json:"isolation"`
	Phase          string    `json:"phase"`
	CreatedAt      time.Time `json:"createdAt"`
	StartedAt      time.Time `json:"startedAt,omitzero"`
	StoppedAt      time.Time `json:"stoppedAt,omitzero"`
	LastActivityAt time.Time `json:"lastActivityAt,omitzero"`
	Reason         string    `json:"reason,omitempty"`
}
