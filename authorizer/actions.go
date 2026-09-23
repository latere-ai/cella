// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"slices"

	"latere.ai/x/pkg/authz"
)

// Core is the name the shared contract carries this vocabulary under,
// which an endpoint's refusal of an unknown action names.
const Core = "cella"

// The five resource kinds of the cella.latere.ai/v1 API group. They are
// strings here because the vocabulary is the half of the contract an
// endpoint needs without the manifest types; the manifest package of
// spec 003 declares the same five for the objects themselves, and a kind
// an action names never changes.
const (
	KindSandbox     = "Sandbox"
	KindSecret      = "Secret"
	KindVolume      = "Volume"
	KindSandboxSet  = "SandboxSet"
	KindEnvironment = "Environment"
)

// The actions of spec 006's table: every question cellad asks an
// authorizer, and with the resource kinds above the whole of what this
// module adds to the shared contract's envelope. A constant never
// changes its string and never disappears; a new action is a new row in
// that table first and a constant here second.
const (
	ActionSandboxCreate = "sandbox.create"
	ActionSandboxRead   = "sandbox.read"
	ActionSandboxUpdate = "sandbox.update"
	ActionSandboxDelete = "sandbox.delete"
	ActionSandboxExec   = "sandbox.exec"
	ActionSandboxToken  = "sandbox.token"
	ActionSandboxList   = "sandbox.list"

	ActionSecretCreate = "secret.create"
	ActionSecretRead   = "secret.read"
	ActionSecretUpdate = "secret.update"
	ActionSecretDelete = "secret.delete"
	ActionSecretList   = "secret.list"
	ActionSecretMount  = "secret.mount"

	ActionVolumeCreate   = "volume.create"
	ActionVolumeRead     = "volume.read"
	ActionVolumeUpdate   = "volume.update"
	ActionVolumeDelete   = "volume.delete"
	ActionVolumeList     = "volume.list"
	ActionVolumeAttach   = "volume.attach"
	ActionVolumeSnapshot = "volume.snapshot"

	ActionSetCreate = "set.create"
	ActionSetRead   = "set.read"
	ActionSetUpdate = "set.update"
	ActionSetDelete = "set.delete"
	ActionSetList   = "set.list"

	ActionEnvironmentCreate = "environment.create"
	ActionEnvironmentRead   = "environment.read"
	ActionEnvironmentUpdate = "environment.update"
	ActionEnvironmentDelete = "environment.delete"
	ActionEnvironmentList   = "environment.list"
	ActionEnvironmentKey    = "environment.key"
	ActionEnvironmentUse    = "environment.use"
)

// table is spec 006's action table in its order, one row per action, and
// the one place the pairing of an action with its kind is written. An
// action acts on exactly one kind, so set.* acts on SandboxSet and is
// the one row whose kind is not its prefix capitalized.
var table = []authz.Action{
	{Name: ActionSandboxCreate, Kind: KindSandbox},
	{Name: ActionSandboxRead, Kind: KindSandbox},
	{Name: ActionSandboxUpdate, Kind: KindSandbox},
	{Name: ActionSandboxDelete, Kind: KindSandbox},
	{Name: ActionSandboxExec, Kind: KindSandbox},
	{Name: ActionSandboxToken, Kind: KindSandbox},
	{Name: ActionSandboxList, Kind: KindSandbox},

	{Name: ActionSecretCreate, Kind: KindSecret},
	{Name: ActionSecretRead, Kind: KindSecret},
	{Name: ActionSecretUpdate, Kind: KindSecret},
	{Name: ActionSecretDelete, Kind: KindSecret},
	{Name: ActionSecretList, Kind: KindSecret},
	{Name: ActionSecretMount, Kind: KindSecret},

	{Name: ActionVolumeCreate, Kind: KindVolume},
	{Name: ActionVolumeRead, Kind: KindVolume},
	{Name: ActionVolumeUpdate, Kind: KindVolume},
	{Name: ActionVolumeDelete, Kind: KindVolume},
	{Name: ActionVolumeList, Kind: KindVolume},
	{Name: ActionVolumeAttach, Kind: KindVolume},
	{Name: ActionVolumeSnapshot, Kind: KindVolume},

	{Name: ActionSetCreate, Kind: KindSandboxSet},
	{Name: ActionSetRead, Kind: KindSandboxSet},
	{Name: ActionSetUpdate, Kind: KindSandboxSet},
	{Name: ActionSetDelete, Kind: KindSandboxSet},
	{Name: ActionSetList, Kind: KindSandboxSet},

	{Name: ActionEnvironmentCreate, Kind: KindEnvironment},
	{Name: ActionEnvironmentRead, Kind: KindEnvironment},
	{Name: ActionEnvironmentUpdate, Kind: KindEnvironment},
	{Name: ActionEnvironmentDelete, Kind: KindEnvironment},
	{Name: ActionEnvironmentList, Kind: KindEnvironment},
	{Name: ActionEnvironmentKey, Kind: KindEnvironment},
	{Name: ActionEnvironmentUse, Kind: KindEnvironment},
}

// labels is the name a person reads for each resource kind. A kind is a
// type name, and a person choosing what a personal access token may do
// picks a function under a heading (infrastructure/identity id-13): the
// picker groups by kind, because an action acts on exactly one kind and
// that grouping is the only correct one, and it names each group with
// the label here rather than with a word of its own. "SandboxSet" is the
// row that makes the point, and set.* is why the grouping is the kind
// and not the action's prefix.
var labels = map[string]string{
	KindSandbox:     "Sandboxes",
	KindSecret:      "Secrets",
	KindVolume:      "Volumes",
	KindSandboxSet:  "Sandbox sets",
	KindEnvironment: "Environments",
}

// Vocabulary is Cella's action table as the shared contract reads it:
// the client refuses an action outside it before the wire, the endpoint
// scaffold of latere.ai/x/pkg/authz/server answers a 400 for one, and
// the conformance suite drives a case per row, and a picker reads the
// heading of each kind off Label. The value is a fresh copy each call,
// so a caller that sorts or appends to it, or declares labels of its
// own, changes nothing here.
func Vocabulary() authz.Vocabulary {
	return authz.Vocabulary{Core: Core, Actions: slices.Clone(table)}.WithLabels(labels)
}

// Actions lists every action of the vocabulary, in the table's order.
func Actions() []string {
	out := make([]string, len(table))
	for i, a := range table {
		out[i] = a.Name
	}
	return out
}

// Kind is the resource kind an action acts on, and "" for a string
// outside the vocabulary.
func Kind(action string) string {
	for _, a := range table {
		if a.Name == action {
			return a.Kind
		}
	}
	return ""
}

// Known reports whether action is one of the vocabulary.
func Known(action string) bool { return Kind(action) != "" }
