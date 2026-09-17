// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/internal/auth"
)

const defaultEnvironment = "default"

// policy is the owner policy with alice an admin and bob a plain
// subject, which is the shape every row below is read against.
func policy() *auth.OwnerPolicy {
	return &auth.OwnerPolicy{Admins: []string{alice}, DefaultEnvironment: defaultEnvironment}
}

// decide asks the owner policy one question.
func decide(t *testing.T, p *auth.OwnerPolicy, subject, action string, res authz.Resource) authz.Decision {
	t.Helper()
	d, err := p.Authorize(t.Context(), authz.Request{
		Subject: subject, Action: action, Resource: res, Claims: map[string]any{},
	})
	if err != nil {
		t.Fatalf("the owner policy answered an error: %v", err)
	}
	return d
}

// TestOwnerPolicy is spec 006's row: the rules hold for every kind and
// every action, including that only an admin creates an environment and
// only the default environment is usable by a non-admin.
func TestOwnerPolicy(t *testing.T) {
	p := policy()
	owned := map[string]authz.Resource{
		authorizer.KindSandbox:     auth.Sandbox{ID: "sbx_01J9", Owner: bob, Environment: "env_01J9"}.Resource(),
		authorizer.KindSecret:      auth.Secret{ID: "sec_01J9", Owner: bob}.Resource(),
		authorizer.KindVolume:      auth.Volume{ID: "vol_01J9", Owner: bob}.Resource(),
		authorizer.KindSandboxSet:  auth.Set{ID: "set_01J9", Owner: bob}.Resource(),
		authorizer.KindEnvironment: auth.Environment{ID: "env_01J9", Name: "private", Owner: bob}.Resource(),
	}
	others := map[string]authz.Resource{
		authorizer.KindSandbox:     auth.Sandbox{ID: "sbx_OTHER", Owner: alice}.Resource(),
		authorizer.KindSecret:      auth.Secret{ID: "sec_OTHER", Owner: alice}.Resource(),
		authorizer.KindVolume:      auth.Volume{ID: "vol_OTHER", Owner: alice}.Resource(),
		authorizer.KindSandboxSet:  auth.Set{ID: "set_OTHER", Owner: alice}.Resource(),
		authorizer.KindEnvironment: auth.Environment{ID: "env_OTHER", Name: "private", Owner: alice}.Resource(),
	}

	for _, a := range authorizer.Vocabulary().Actions {
		t.Run(a.Name, func(t *testing.T) {
			environment := a.Kind == authorizer.KindEnvironment
			isList := authz.IsList(a.Name)
			isCreate := a.Name == createOf(a.Kind)

			// An admin may do everything, on every object and every kind.
			if d := decide(t, p, alice, a.Name, owned[a.Kind]); !d.Allow {
				t.Errorf("an admin was denied %s: %s", a.Name, d.Reason)
			}

			switch {
			case environment && a.Name == authorizer.ActionEnvironmentUse:
				d := decide(t, p, bob, a.Name, auth.Environment{ID: "env_DEFAULT", Name: defaultEnvironment}.Resource())
				if !d.Allow {
					t.Errorf("a subject was denied the default environment: %s", d.Reason)
				}
				if d := decide(t, p, bob, a.Name, owned[a.Kind]); d.Allow || d.Reason != auth.ReasonAdminOnly {
					t.Errorf("a subject used an environment that is not the default: %+v", d)
				}
			case environment:
				// Create, key, read, update, delete and list are admins'.
				if d := decide(t, p, bob, a.Name, owned[a.Kind]); d.Allow || d.Reason != auth.ReasonAdminOnly {
					t.Errorf("a subject was allowed %s on an environment: %+v", a.Name, d)
				}
			case isList:
				d := decide(t, p, bob, a.Name, auth.List(a.Name))
				if !d.Allow {
					t.Errorf("a subject was denied %s: %s", a.Name, d.Reason)
				}
				if d.Filter == nil || len(d.Filter.Owners) != 1 || d.Filter.Owners[0] != bob {
					t.Errorf("%s answered the filter %+v; a list returns the subject's own objects", a.Name, d.Filter)
				}
			case isCreate:
				if d := decide(t, p, bob, a.Name, createResource(a.Kind)); !d.Allow {
					t.Errorf("a subject was denied %s: %s", a.Name, d.Reason)
				}
			default:
				if d := decide(t, p, bob, a.Name, owned[a.Kind]); !d.Allow {
					t.Errorf("the owner was denied %s on its own object: %s", a.Name, d.Reason)
				}
				if d := decide(t, p, bob, a.Name, others[a.Kind]); d.Allow || d.Reason != authz.ReasonNotOwner {
					t.Errorf("a subject was allowed %s on another subject's object: %+v", a.Name, d)
				}
			}
		})
	}
}

// TestOwnerPolicyRefusesWhatIsNobody: the anonymous subject and an
// action outside the vocabulary are denied before any row is weighed.
func TestOwnerPolicyRefusesWhatIsNobody(t *testing.T) {
	p := policy()
	res := auth.Sandbox{ID: "sbx_01J9", Owner: bob}.Resource()
	if d := decide(t, p, "", authorizer.ActionSandboxRead, res); d.Allow || d.Reason != authz.ReasonAnonymous {
		t.Errorf("the anonymous subject answered %+v", d)
	}
	if d := decide(t, p, bob, "sandbox.explode", res); d.Allow || d.Reason != auth.ReasonUnknownAction {
		t.Errorf("an action outside the vocabulary answered %+v", d)
	}
	mismatched := auth.Secret{ID: "sec_01J9", Owner: bob}.Resource()
	if d := decide(t, p, bob, authorizer.ActionSandboxRead, mismatched); d.Allow || d.Reason != auth.ReasonUnknownAction {
		t.Errorf("an action on a kind it does not act on answered %+v", d)
	}
	env := auth.Environment{ID: "env_01J9"}.Resource()
	if d := decide(t, p, "environment:env_01J9", authorizer.ActionEnvironmentRead, env); d.Allow || d.Reason != auth.ReasonWorkerKey {
		t.Errorf("an environment key asked about an object and answered %+v", d)
	}
}

// TestProbeIdIsAlwaysDenied is spec 006's row: the probe is denied by
// the owner policy and by the stub authorizer, for every subject and
// every action, the admin and the anonymous subject included.
func TestProbeIdIsAlwaysDenied(t *testing.T) {
	p := policy()
	for _, a := range authorizer.Vocabulary().Actions {
		for _, subject := range []string{alice, bob, "", "sandbox:sbx_01J9", "environment:env_01J9"} {
			d := decide(t, p, subject, a.Name, authz.NewResource(a.Kind, auth.ProbeID, map[string]any{"owner": subject}))
			if d.Allow {
				t.Fatalf("the owner policy allowed the probe on %s for %q", a.Name, subject)
			}
			if d.Reason != authz.ReasonProbe {
				t.Errorf("the probe on %s for %q was denied %q, want %q", a.Name, subject, d.Reason, authz.ReasonProbe)
			}
		}
	}
	// The check command reads a deny as the one right answer, and an
	// allow as an endpoint that does not read the request.
	if err := auth.NewAuthorizer(p).Check(t.Context()); err != nil {
		t.Fatalf("the owner policy denies the probe and the check failed: %v", err)
	}
}

// TestWorkloadIsLeastPrivileged is spec 006's row: a sandbox's token
// reads and execs itself, reads its descendants, cannot read a sibling
// or delete itself, and cannot mount a secret its parent did not.
func TestWorkloadIsLeastPrivileged(t *testing.T) {
	p := policy()
	const self = "sandbox:sbx_SELF"

	t.Run("it reads and execs itself", func(t *testing.T) {
		res := auth.Sandbox{ID: "sbx_SELF", Owner: bob, Root: "sbx_SELF"}.Resource()
		for _, action := range []string{authorizer.ActionSandboxRead, authorizer.ActionSandboxExec} {
			if d := decide(t, p, self, action, res); !d.Allow {
				t.Errorf("a sandbox was denied %s on itself: %s", action, d.Reason)
			}
		}
	})

	t.Run("it reads its descendants", func(t *testing.T) {
		child := auth.Sandbox{ID: "sbx_CHILD", Owner: bob, Parent: "sbx_SELF", Root: "sbx_SELF"}.Resource()
		grandchild := auth.Sandbox{ID: "sbx_DEEP", Owner: bob, Parent: "sbx_CHILD", Root: "sbx_SELF"}.Resource()
		for _, res := range []authz.Resource{child, grandchild} {
			if d := decide(t, p, self, authorizer.ActionSandboxRead, res); !d.Allow {
				t.Errorf("a sandbox was denied a read of %s in the tree it roots: %s", res.ID, d.Reason)
			}
		}
	})

	t.Run("it cannot read a sibling", func(t *testing.T) {
		sibling := auth.Sandbox{ID: "sbx_SIBLING", Owner: bob, Parent: "sbx_PARENT", Root: "sbx_PARENT"}.Resource()
		if d := decide(t, p, self, authorizer.ActionSandboxRead, sibling); d.Allow {
			t.Error("a sandbox read a sibling, which is outside the tree it roots")
		}
	})

	t.Run("it cannot delete or update itself", func(t *testing.T) {
		res := auth.Sandbox{ID: "sbx_SELF", Owner: bob, Root: "sbx_SELF"}.Resource()
		for _, action := range []string{authorizer.ActionSandboxDelete, authorizer.ActionSandboxUpdate, authorizer.ActionSandboxToken} {
			if d := decide(t, p, self, action, res); d.Allow {
				t.Errorf("a sandbox was allowed %s on itself", action)
			}
		}
	})

	t.Run("it creates a child and nothing else", func(t *testing.T) {
		child := auth.Sandbox{Name: "child", Parent: "sbx_SELF"}.Resource()
		if d := decide(t, p, self, authorizer.ActionSandboxCreate, child); !d.Allow {
			t.Errorf("a sandbox was denied a child: %s", d.Reason)
		}
		elsewhere := auth.Sandbox{Name: "orphan", Parent: "sbx_OTHER"}.Resource()
		if d := decide(t, p, self, authorizer.ActionSandboxCreate, elsewhere); d.Allow {
			t.Error("a sandbox created a child of another sandbox")
		}
		rootless := auth.Sandbox{Name: "orphan"}.Resource()
		if d := decide(t, p, self, authorizer.ActionSandboxCreate, rootless); d.Allow {
			t.Error("a sandbox created a sandbox with no parent at all")
		}
	})

	t.Run("it mounts no secret and touches no other kind", func(t *testing.T) {
		for _, tc := range []struct {
			action string
			res    authz.Resource
		}{
			{authorizer.ActionSecretMount, auth.Secret{ID: "sec_01J9", Owner: bob}.Resource()},
			{authorizer.ActionSecretRead, auth.Secret{ID: "sec_01J9", Owner: bob}.Resource()},
			{authorizer.ActionVolumeAttach, auth.Volume{ID: "vol_01J9", Owner: bob}.Resource()},
			{authorizer.ActionEnvironmentUse, auth.Environment{ID: "env_DEFAULT", Name: defaultEnvironment}.Resource()},
			{authorizer.ActionSetCreate, auth.Set{Name: "fleet"}.Resource()},
		} {
			if d := decide(t, p, self, tc.action, tc.res); d.Allow {
				t.Errorf("a sandbox was allowed %s", tc.action)
			}
		}
	})

	t.Run("its list is an allow the API narrows", func(t *testing.T) {
		d := decide(t, p, self, authorizer.ActionSandboxList, auth.List(authorizer.ActionSandboxList))
		if !d.Allow {
			t.Fatalf("a sandbox was denied its own list: %s", d.Reason)
		}
		if d.Filter != nil {
			t.Errorf("the list carried the filter %+v; descendants are not an owner or a label", d.Filter)
		}
	})
}

// createOf is the action of a kind that makes one.
func createOf(kind string) string {
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

// createResource is the manifest's half of a create: fields, no id and
// no owner, because there is no object yet.
func createResource(kind string) authz.Resource {
	switch kind {
	case authorizer.KindSandbox:
		return auth.Sandbox{Name: "dev", Environment: "env_01J9"}.Resource()
	case authorizer.KindSecret:
		return auth.Secret{Name: "token"}.Resource()
	case authorizer.KindVolume:
		return auth.Volume{Name: "data"}.Resource()
	case authorizer.KindSandboxSet:
		return auth.Set{Name: "fleet"}.Resource()
	case authorizer.KindEnvironment:
		return auth.Environment{Name: "private"}.Resource()
	}
	return authz.Resource{}
}

// scoped is one question from a personal access token: the claims a
// verified token carries verbatim, the credential class in token_use and
// the grants its holder chose in authorization_details.
func scoped(t *testing.T, p *auth.OwnerPolicy, subject, action string, res authz.Resource, grants ...any) authz.Decision {
	t.Helper()
	claims := map[string]any{"token_use": "pat"}
	if grants != nil {
		claims["authorization_details"] = grants
	}
	d, err := p.Authorize(t.Context(), authz.Request{
		Subject: subject, Action: action, Resource: res, Claims: claims,
	})
	if err != nil {
		t.Fatalf("the owner policy answered an error: %v", err)
	}
	return d
}

// entry is one authorization_details entry, as auth mints it: an action
// set qualified by its core, paired with a resource selector. An empty id
// selects every resource of the kind.
func entry(kind, id string, actions ...string) any {
	e := map[string]any{
		"type": "latere-authz", "actions": actions,
		"datatypes": []any{kind}, "locations": []any{"https://api.latere.ai"},
	}
	if id != "" {
		e["identifier"] = id
	}
	return e
}

// TestOwnerPolicyNarrowsAPersonalAccessToken is id-13's rule at Cella's
// decision point: the policy answers about the person, the grants answer
// about the credential, and the allow is the conjunction. A person's key
// narrowed to one sandbox reaches that sandbox and is refused everywhere
// else, with reason "grant".
func TestOwnerPolicyNarrowsAPersonalAccessToken(t *testing.T) {
	p := policy()
	mine := auth.Sandbox{ID: "sbx_01J9MINE", Owner: bob}.Resource()
	other := auth.Sandbox{ID: "sbx_01J9OTHER", Owner: bob}.Resource()
	read := entry(authorizer.KindSandbox, "sbx_01J9MINE", "cella:sandbox.read")

	for _, tc := range []struct {
		name    string
		res     authz.Resource
		action  string
		grants  []any
		allowed bool
	}{{
		name: "the action and the resource its own grant names",
		res:  mine, action: authorizer.ActionSandboxRead, grants: []any{read}, allowed: true,
	}, {
		name: "another action on the granted resource",
		res:  mine, action: authorizer.ActionSandboxDelete, grants: []any{read},
	}, {
		name: "the granted action on another resource",
		res:  other, action: authorizer.ActionSandboxRead, grants: []any{read},
	}, {
		name: "a selector naming every sandbox",
		res:  other, action: authorizer.ActionSandboxRead,
		grants: []any{entry(authorizer.KindSandbox, "", "cella:sandbox.read")}, allowed: true,
	}, {
		name: "an action of another core, which names no Cella action at all",
		res:  mine, action: authorizer.ActionSandboxRead,
		grants: []any{entry(authorizer.KindSandbox, "", "origo:repo.read")},
	}, {
		// A grant is a restriction and never authority. The policy says
		// the subject does not own this object, and a grant that names
		// the action does not make it theirs.
		name:   "a grant on an object the person does not own",
		res:    auth.Sandbox{ID: "sbx_01J9MINE", Owner: "someone-else"}.Resource(),
		action: authorizer.ActionSandboxRead, grants: []any{read},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			d := scoped(t, p, bob, tc.action, tc.res, tc.grants...)
			if d.Allow != tc.allowed {
				t.Fatalf("the answer is %+v, want allow %v", d, tc.allowed)
			}
			if tc.allowed || tc.name == "a grant on an object the person does not own" {
				return
			}
			if d.Reason != authz.ReasonGrant {
				t.Errorf("the deny names %q, want %q: the credential says what it may do, and this is not it", d.Reason, authz.ReasonGrant)
			}
		})
	}
}

// TestOwnerPolicyDeniesAGrantlessPAT: a personal access token carrying no
// grant at all is a token nobody wrote a grant for, and the intersection
// of an allow with an empty set is a deny. The claim is a restriction, so
// an absent one is not full authority.
func TestOwnerPolicyDeniesAGrantlessPAT(t *testing.T) {
	d := scoped(t, policy(), bob, authorizer.ActionSandboxRead, auth.Sandbox{ID: "sbx_01J9", Owner: bob}.Resource())
	if d.Allow || d.Reason != authz.ReasonGrant {
		t.Fatalf("a grantless personal access token answered %+v, want a %q deny", d, authz.ReasonGrant)
	}
}

// TestGrantsNarrowNothingButAPAT: the claim narrows one credential class.
// A person's session token, and a token cellad minted for a sandbox, are
// decided by the policy alone whatever authorization_details says.
func TestGrantsNarrowNothingButAPAT(t *testing.T) {
	p := policy()
	res := auth.Sandbox{ID: "sbx_01J9", Owner: bob}.Resource()
	claims := map[string]any{
		"token_use":             "access",
		"authorization_details": []any{entry(authorizer.KindSandbox, "sbx_01J9OTHER", "cella:sandbox.read")},
	}
	d, err := p.Authorize(t.Context(), authz.Request{
		Subject: bob, Action: authorizer.ActionSandboxRead, Resource: res, Claims: claims,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allow {
		t.Fatalf("a token that is no personal access token was narrowed by a claim: %+v", d)
	}
	if d := decide(t, p, bob, authorizer.ActionSandboxRead, res); !d.Allow {
		t.Fatalf("a token carrying no claim at all was narrowed: %+v", d)
	}
}

// TestAClaimNobodyCanParseIsADeny: the owner policy is also the endpoint
// a self-hoster serves from it, where the claims are whatever the calling
// enforcement point sent rather than what cellad's own verifier passed. A
// claim that does not read as grants is a deny, and not an error: an
// error is a decision point that produced no decision, which a core reads
// as an outage, and there is no outage here. There is a credential whose
// reach nobody can read, and the closed answer is the only safe one.
func TestAClaimNobodyCanParseIsADeny(t *testing.T) {
	d, err := policy().Authorize(t.Context(), authz.Request{
		Subject: bob, Action: authorizer.ActionSandboxRead,
		Resource: auth.Sandbox{ID: "sbx_01J9", Owner: bob}.Resource(),
		Claims:   map[string]any{"token_use": "pat", "authorization_details": "nope"},
	})
	if err != nil {
		t.Fatalf("the policy answered an error, which a core reads as an authorizer outage: %v", err)
	}
	if d.Allow || d.Reason != authz.ReasonGrant {
		t.Fatalf("the answer is %+v, want a %q deny", d, authz.ReasonGrant)
	}
}
