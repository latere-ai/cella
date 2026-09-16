// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"slices"
	"strings"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
)

// The reasons the owner policy adds to the shared frame's. The frame
// names the probe, the anonymous subject and the object nobody owns;
// these are the rows spec 006 writes on top of it.
const (
	// ReasonAdminOnly is an action only CELLA_ADMIN_SUBJECTS may take:
	// creating, keying, updating or deleting an environment, and using
	// one that is not the default.
	ReasonAdminOnly = "admin_only"
	// ReasonUnknownAction is an action outside Cella's vocabulary. The
	// client refuses one before the wire, so reaching this is a caller
	// inside the process that skipped it.
	ReasonUnknownAction = "unknown_action"
	// ReasonNotItsOwn is a workload asking for something outside the tree
	// it is the root of.
	ReasonNotItsOwn = "not_its_own"
	// ReasonWorkerKey is an environment key asking a question about an
	// object. A key authorizes a worker's own routes and nothing else.
	ReasonWorkerKey = "worker_key"
)

// OwnerPolicy is the policy cellad applies with CELLA_AUTHORIZER_URL
// unset. It is a policy with tests, not the absence of one: the rows of
// spec 006 in front of the shared frame of latere.ai/x/pkg/authz, which
// decides the probe, the anonymous subject, the admin, the owner and the
// create.
//
// The object's owner is read off the resource, because the control plane
// built that resource from its own state before it asked. An object that
// carries no owner is one that does not exist yet, which is what makes a
// create the one action allowed on it.
type OwnerPolicy struct {
	// Admins are the rendered subjects of CELLA_ADMIN_SUBJECTS.
	Admins []string
	// DefaultEnvironment is CELLA_DEFAULT_ENVIRONMENT, the one
	// environment every subject may use.
	DefaultEnvironment string
}

// Authorize answers one request, so the owner policy and an operator's
// endpoint are one seam to everything above.
func (p *OwnerPolicy) Authorize(_ context.Context, req authz.Request) (authz.Decision, error) {
	kind := authorizer.Kind(req.Action)
	switch {
	case strings.EqualFold(req.Resource.ID, authz.ProbeID):
		return authz.Decision{Reason: authz.ReasonProbe}, nil
	case req.Subject == "":
		return authz.Decision{Reason: authz.ReasonAnonymous}, nil
	case kind == "" || kind != req.Resource.Kind:
		return authz.Decision{Reason: ReasonUnknownAction}, nil
	}

	// A token cellad minted is not a person, and the rows that follow are
	// about people and the objects they own.
	if id, ok := strings.CutPrefix(req.Subject, SandboxPrefix); ok && id != "" {
		return workloadDecision(req, id), nil
	}
	if _, ok := strings.CutPrefix(req.Subject, EnvironmentPrefix); ok {
		return authz.Decision{Reason: ReasonWorkerKey}, nil
	}

	admin := slices.Contains(p.Admins, req.Subject)
	if !admin {
		// Every environment but the default is admins': theirs to make,
		// to key, to change, to remove, and theirs alone to use.
		if kind == authorizer.KindEnvironment {
			if req.Action != authorizer.ActionEnvironmentUse || !p.isDefault(req.Resource) {
				return authz.Decision{Reason: ReasonAdminOnly}, nil
			}
			return authz.Decision{Allow: true}, nil
		}
	}
	if authz.IsList(req.Action) {
		if admin {
			return authz.Decision{Allow: true}, nil
		}
		// A list is an allow the filter narrows to the caller's own.
		return authz.Decision{Allow: true, Filter: &authz.Filter{Owners: []string{req.Subject}}}, nil
	}
	frame := authz.Policy{Admins: p.Admins, Create: createAction(kind)}
	return frame.Decide(req, object(req.Resource)), nil
}

// Decide is the same answer under the name latere.ai/x/pkg/authz/server
// calls a decider by, so the policy cellad runs in process is also the
// endpoint an operator serves from it, with no second implementation
// between the two.
func (p *OwnerPolicy) Decide(ctx context.Context, req authz.Request) (authz.Decision, error) {
	return p.Authorize(ctx, req)
}

// isDefault reports whether the resource is the environment
// CELLA_DEFAULT_ENVIRONMENT names, by the name an operator wrote or by
// the id the control plane gave it.
func (p *OwnerPolicy) isDefault(res authz.Resource) bool {
	if p.DefaultEnvironment == "" {
		return false
	}
	return res.String("name") == p.DefaultEnvironment || res.ID == p.DefaultEnvironment
}

// workloadDecision is spec 006's least-privileged workload. A sandbox may
// read and exec itself, may read any sandbox of the tree it roots, and
// may create a child. Nothing else: it does not delete itself, does not
// read a sibling, and mounts no secret.
//
// The tree is decided from resource.parent and resource.root, both of
// which the request carries, so the policy walks nothing.
func workloadDecision(req authz.Request, self string) authz.Decision {
	switch req.Action {
	case authorizer.ActionSandboxRead, authorizer.ActionSandboxExec:
		if req.Resource.ID == self {
			return authz.Decision{Allow: true}
		}
	case authorizer.ActionSandboxCreate:
		if req.Resource.String("parent") == self {
			return authz.Decision{Allow: true}
		}
		return authz.Decision{Reason: ReasonNotItsOwn}
	case authorizer.ActionSandboxList:
		// The page is the caller's descendants. The contract's filter
		// narrows by owner and label, which no descendant shares, so the
		// narrowing is the API's own, from the workload member of the
		// request ([[008-api]]).
		return authz.Decision{Allow: true}
	default:
		return authz.Decision{Reason: ReasonNotItsOwn}
	}
	// A read of another sandbox is allowed when that sandbox is in the
	// tree this one roots: its parent is this one, or its root is.
	if req.Action == authorizer.ActionSandboxRead &&
		(req.Resource.String("parent") == self || req.Resource.String("root") == self) {
		return authz.Decision{Allow: true}
	}
	return authz.Decision{Reason: ReasonNotItsOwn}
}

// createAction is the action of a kind that makes one, which is the one
// action the frame allows on an object that does not exist.
func createAction(kind string) string {
	switch kind {
	case authorizer.KindSandbox:
		return authorizer.ActionSandboxCreate
	case authorizer.KindSecret:
		return authorizer.ActionSecretCreate
	case authorizer.KindVolume:
		return authorizer.ActionVolumeCreate
	case authorizer.KindSandboxSet:
		return authorizer.ActionSetCreate
	case authorizer.KindEnvironment:
		return authorizer.ActionEnvironmentCreate
	}
	return ""
}

// object is what the control plane found when it looked the resource up,
// read off the resource it built. An object with an owner exists; one
// without is a create's, or a name that resolved to nothing.
func object(res authz.Resource) authz.Object {
	owner := res.String("owner")
	return authz.Object{Exists: owner != "", Owner: owner}
}
