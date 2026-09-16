// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
)

// The resource shapes of spec 006's table, one type per kind. They are
// here and not in latere.ai/x/cella/authorizer because they are what
// cellad sends rather than what an endpoint must import: the vocabulary
// package publishes the action and its kind, which is what an endpoint
// decides from, and the fields of an object are the manifest types of
// spec 003 once those exist. An endpoint reads a field off the flat
// object either way, resource.owner and not resource.fields.owner.
//
// A field the control plane does not know yet is absent rather than
// empty, so an authorizer that reads one can tell "no parent" from "a
// parent this version does not send".

// Sandbox is the resource of every sandbox.* action. A create carries no
// id, because the id does not exist until the create is allowed.
type Sandbox struct {
	ID          string
	Name        string
	Owner       string
	Environment string
	Parent      string
	Root        string
	Labels      map[string]string
}

// Resource renders the sandbox as the envelope carries it.
func (s Sandbox) Resource() authz.Resource {
	return authz.NewResource(authorizer.KindSandbox, s.ID, fields(
		field{"name", s.Name}, field{"owner", s.Owner}, field{"environment", s.Environment},
		field{"parent", s.Parent}, field{"root", s.Root}, labels(s.Labels),
	))
}

// Secret is the resource of every secret.* action, mount included, which
// is asked at resolve for every secret a manifest names.
type Secret struct {
	ID     string
	Name   string
	Owner  string
	Labels map[string]string
}

// Resource renders the secret as the envelope carries it.
func (s Secret) Resource() authz.Resource {
	return authz.NewResource(authorizer.KindSecret, s.ID, fields(
		field{"name", s.Name}, field{"owner", s.Owner}, labels(s.Labels),
	))
}

// Volume is the resource of every volume.* action, attach included.
type Volume struct {
	ID          string
	Name        string
	Owner       string
	Environment string
	Labels      map[string]string
}

// Resource renders the volume as the envelope carries it.
func (v Volume) Resource() authz.Resource {
	return authz.NewResource(authorizer.KindVolume, v.ID, fields(
		field{"name", v.Name}, field{"owner", v.Owner}, field{"environment", v.Environment}, labels(v.Labels),
	))
}

// Set is the resource of every set.* action.
type Set struct {
	ID          string
	Name        string
	Owner       string
	Environment string
	Labels      map[string]string
}

// Resource renders the set as the envelope carries it.
func (s Set) Resource() authz.Resource {
	return authz.NewResource(authorizer.KindSandboxSet, s.ID, fields(
		field{"name", s.Name}, field{"owner", s.Owner}, field{"environment", s.Environment}, labels(s.Labels),
	))
}

// Environment is the resource of every environment.* action, use
// included, which is asked at resolve for the environment a manifest
// names.
type Environment struct {
	ID        string
	Name      string
	Owner     string
	Isolation string
	Labels    map[string]string
}

// Resource renders the environment as the envelope carries it.
func (e Environment) Resource() authz.Resource {
	return authz.NewResource(authorizer.KindEnvironment, e.ID, fields(
		field{"name", e.Name}, field{"owner", e.Owner}, field{"isolation", e.Isolation}, labels(e.Labels),
	))
}

// List is the resource of a list action: the kind and nothing else. The
// answer is a decision whose filter narrows the page, not a page.
func List(action string) authz.Resource {
	return authz.NewResource(authorizer.Kind(action), "", nil)
}

// field is one member of a resource, left out when it is empty.
type field struct {
	name  string
	value any
}

// labels is the one member that is a map, left out when it holds
// nothing, so an object with no label sends no labels member.
func labels(m map[string]string) field {
	if len(m) == 0 {
		return field{}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return field{"labels", out}
}

// fields gathers the members an object carries, dropping the empty ones.
func fields(list ...field) map[string]any {
	out := map[string]any{}
	for _, f := range list {
		switch v := f.value.(type) {
		case nil:
			continue
		case string:
			if v == "" {
				continue
			}
		}
		out[f.name] = f.value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
