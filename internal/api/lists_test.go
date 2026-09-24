// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/authorizer"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// tenantEnvironment is one worker environment carrying a tenant's label.
func tenantEnvironment(name, tenant string) string {
	return `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Environment",` +
		`"metadata":{"name":"` + name + `","labels":{"tenant":"` + tenant + `"}},` +
		`"spec":{"mode":"worker","isolation":"none","capacity":{"cpu":"8","memory":"16Gi","sandboxes":10}}}`
}

// labelOf reads one label off the resource the envelope carries.
func labelOf(res authz.Resource, key string) string {
	labels, _ := res.Fields["labels"].(map[string]any)
	value, _ := labels[key].(string)
	return value
}

// tenancy is an authorizer that narrows each environment list to a tenant,
// the way a plane that hosts several tenants on one control plane does. The
// subject "admin" may do everything and lists without a filter. Every other
// subject may create and apply, lists under the filter its row names, and
// reads an environment when it is the default or when its row's rule admits
// it. asked counts each environment read, by environment.
type tenancy struct {
	filters map[string]*authz.Filter
	reads   map[string]func(res authz.Resource, subject string) bool
	asked   map[string]int
}

func (p *tenancy) Authorize(_ context.Context, r authz.Request) (authz.Decision, error) {
	_, sub, _ := strings.Cut(r.Subject, "|")
	if sub == "admin" || r.Resource.Kind != authorizer.KindEnvironment {
		return authz.Decision{Allow: true}, nil
	}
	switch r.Action {
	case authorizer.ActionEnvironmentList:
		return authz.Decision{Allow: true, Filter: p.filters[sub]}, nil
	case authorizer.ActionEnvironmentRead:
		p.asked[r.Resource.ID]++
		if r.Resource.ID == "default" || p.reads[sub](r.Resource, r.Subject) {
			return authz.Decision{Allow: true}, nil
		}
		return authz.Decision{Reason: "not_in_tenant"}, nil
	}
	return authz.Decision{Allow: true}, nil
}

// listedEnvironments reads the environment list as one subject and returns
// the names it carries.
func listedEnvironments(t *testing.T, k *kinds, token string) []string {
	t.Helper()
	var page struct {
		Items []v1.Environment `json:"items"`
		Next  string           `json:"next"`
	}
	body, _ := k.send(http.MethodGet, "/v1/environments", token, "", nil, http.StatusOK)
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("the list did not decode: %v", err)
	}
	names := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		names = append(names, item.Metadata.Name)
	}
	return names
}

// tenantPlane stands up a control plane decided by the tenancy given, with
// three worker environments beside the default: tenant-a, applied by bob with
// the label tenant=a; tenant-a-ops, applied by the administrator with the
// same label; and tenant-b, applied by the administrator with tenant=b.
func tenantPlane(t *testing.T, policy *tenancy) *kinds {
	t.Helper()
	k := setupEnvironmentsWith(t, func(d runtime.Driver) runtime.Driver { return d }, policy)
	k.send(http.MethodPut, "/v1/environments/tenant-a", k.bob, tenantEnvironment("tenant-a", "a"), nil, http.StatusCreated)
	k.send(http.MethodPut, "/v1/environments/tenant-a-ops", k.alice, tenantEnvironment("tenant-a-ops", "a"), nil, http.StatusCreated)
	k.send(http.MethodPut, "/v1/environments/tenant-b", k.alice, tenantEnvironment("tenant-b", "b"), nil, http.StatusCreated)
	return k
}

// inTenantA is the read rule of a subject that reads every environment of the
// tenant a, and ownsInTenantA the rule of one that reads only its own there.
func inTenantA(res authz.Resource, _ string) bool { return labelOf(res, "tenant") == "a" }

func ownsInTenantA(res authz.Resource, subject string) bool {
	return inTenantA(res, subject) && res.String("owner") == subject
}

// TestEnvironmentListAppliesTheFilter is the list rule over environments: the
// decision's filter narrows by owner and label, and environment.read decides
// each environment the filter admits. A member narrowed to its own
// environments of its tenant sees those; a subject narrowed by the tenant's
// label alone sees every environment carrying it; a filter that admits an
// environment the read then refuses leaves it out; and no filter lists every
// environment the read allows.
func TestEnvironmentListAppliesTheFilter(t *testing.T) {
	policy := &tenancy{asked: map[string]int{}}
	k := tenantPlane(t, policy)
	bob := k.issuerURL + "|bob"
	policy.filters = map[string]*authz.Filter{
		"bob":   {Owners: []string{bob}, Labels: map[string]string{"tenant": "a"}},
		"carol": {Labels: map[string]string{"tenant": "a"}},
		"dave":  {Labels: map[string]string{"tenant": "a"}},
	}
	policy.reads = map[string]func(authz.Resource, string) bool{
		"bob": ownsInTenantA, "carol": inTenantA, "dave": ownsInTenantA,
	}
	carol := k.issuer.Mint(issuertest.Claims{Sub: "carol"})
	dave := k.issuer.Mint(issuertest.Claims{Sub: "dave"})

	for _, tc := range []struct {
		name  string
		token string
		want  []string
	}{
		{"a member narrowed to its own in its tenant", k.bob, []string{"default", "tenant-a"}},
		{"a subject narrowed by the tenant's label alone", carol, []string{"default", "tenant-a", "tenant-a-ops"}},
		{"a filter that admits what the read refuses", dave, []string{"default"}},
		{"no filter at all", k.alice, []string{"default", "tenant-a", "tenant-a-ops", "tenant-b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := listedEnvironments(t, k, tc.token); !slices.Equal(got, tc.want) {
				t.Errorf("the list carries %v, want %v", got, tc.want)
			}
		})
	}
	// The filter is applied before the read, so an environment it excludes
	// costs no question at the authorizer: tenant-b was read only for the
	// administrator, whose list carries no filter.
	if n := policy.asked["tenant-b"]; n != 0 {
		t.Errorf("an environment outside every filter was read %d times for a filtered caller", n)
	}
}

// TestTheDefaultEnvironmentIsListedByItsRead: a filter that names a tenant
// cannot name the default environment, which carries no tenant's label and is
// owned by the control plane, so the default is decided by environment.read
// alone. It is listed when that read allows it and left out when it does not,
// the same answer the read by id gives.
func TestTheDefaultEnvironmentIsListedByItsRead(t *testing.T) {
	denied := errors.New("the read of the default is denied")
	var refuseDefault bool
	policy := decisionFunc(func(_ context.Context, r authz.Request) (authz.Decision, error) {
		switch {
		case r.Action == authorizer.ActionEnvironmentList:
			return authz.Decision{Allow: true, Filter: &authz.Filter{
				Owners: []string{r.Subject}, Labels: map[string]string{"tenant": "a"},
			}}, nil
		case r.Action == authorizer.ActionEnvironmentRead && r.Resource.ID == "default" && refuseDefault:
			return authz.Decision{Reason: denied.Error()}, nil
		}
		return authz.Decision{Allow: true}, nil
	})
	k := setupEnvironmentsWith(t, func(d runtime.Driver) runtime.Driver { return d }, policy)
	k.send(http.MethodPut, "/v1/environments/tenant-b", k.alice, tenantEnvironment("tenant-b", "b"), nil, http.StatusCreated)
	if got := listedEnvironments(t, k, k.bob); !slices.Equal(got, []string{"default"}) {
		t.Errorf("a member narrowed to its tenant lists %v, want the default alone", got)
	}
	refuseDefault = true
	if got := listedEnvironments(t, k, k.bob); len(got) != 0 {
		t.Errorf("the default was listed to a caller who may not read it: %v", got)
	}
	k.send(http.MethodGet, "/v1/environments/default", k.bob, "", nil, http.StatusForbidden)
}

// TestListsRefuseWhatTheyCannotApply: a list whose decision this server
// cannot carry out refuses the page rather than answering around the
// decision, on the environment list as on the sandbox list. A read of one row
// that produced no decision is authorizer_unavailable for the whole list, and
// a list allowed with a rate limit this server does not hold is
// capability_unsupported.
func TestListsRefuseWhatTheyCannotApply(t *testing.T) {
	var failRead, rateLimited bool
	policy := decisionFunc(func(_ context.Context, r authz.Request) (authz.Decision, error) {
		switch {
		case failRead && (r.Action == authorizer.ActionEnvironmentRead || r.Action == authorizer.ActionSandboxRead):
			return authz.Decision{}, errors.New("the authorizer is down")
		case rateLimited && authz.IsList(r.Action):
			return authz.Decision{Allow: true, Limits: json.RawMessage(`{"requests_per_minute":10}`)}, nil
		}
		return authz.Decision{Allow: true}, nil
	})
	k := setupEnvironmentsWith(t, func(d runtime.Driver) runtime.Driver { return d }, policy)
	k.send(http.MethodPost, "/v1/sandboxes?wait=1", k.alice, createBody, nil, http.StatusCreated)
	for _, path := range []string{"/v1/environments", "/v1/sandboxes"} {
		failRead, rateLimited = true, false
		body, _ := k.send(http.MethodGet, path, k.alice, "", nil, http.StatusServiceUnavailable)
		if !strings.Contains(string(body), "authorizer_unavailable") {
			t.Errorf("%s with a read that produced no decision answered %s", path, body)
		}
		failRead, rateLimited = false, true
		body, _ = k.send(http.MethodGet, path, k.alice, "", nil, http.StatusUnprocessableEntity)
		if !strings.Contains(string(body), "capability_unsupported") {
			t.Errorf("%s allowed with a rate limit answered %s", path, body)
		}
	}
}

// TestSecretListAppliesTheWholeFilter: the secret list holds each secret to
// the filter's labels as well as its owners, as every list does.
func TestSecretListAppliesTheWholeFilter(t *testing.T) {
	policy := decisionFunc(func(_ context.Context, r authz.Request) (authz.Decision, error) {
		if r.Action == authorizer.ActionSecretList {
			return authz.Decision{Allow: true, Filter: &authz.Filter{
				Owners: []string{r.Subject}, Labels: map[string]string{"team": "a"},
			}}, nil
		}
		return authz.Decision{Allow: true}, nil
	})
	f := setupSealed(t, policy)
	secret := func(name, team string) string {
		return `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Secret","metadata":{"name":"` + name +
			`","labels":{"team":"` + team + `"}},"spec":{"scope":{"hosts":["api.example.com"]},"value":"v"}}`
	}
	f.request(http.MethodPost, "/v1/secrets", f.alice, secret("in-team", "a"), http.StatusCreated)
	f.request(http.MethodPost, "/v1/secrets", f.alice, secret("other-team", "b"), http.StatusCreated)
	var page struct {
		Items []v1.Secret `json:"items"`
	}
	if err := json.Unmarshal(f.request(http.MethodGet, "/v1/secrets", f.alice, "", http.StatusOK), &page); err != nil {
		t.Fatalf("the list did not decode: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Metadata.Name != "in-team" {
		t.Errorf("the list carries %+v, want the secret whose label the filter names", page.Items)
	}
}
