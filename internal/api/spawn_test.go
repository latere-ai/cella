// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/cella/internal/auth"
	v1 "latere.ai/x/cella/manifest/v1"
)

// spawnBody is one child manifest, with the spawn rights its author asks for.
func spawnBody(name string, budget, depth int) string {
	obj := map[string]any{
		"apiVersion": v1.APIVersion, "kind": "Sandbox",
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"mesh": map[string]any{"spawn": map[string]any{"budget": budget, "depth": depth}}},
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// rootBody is spawnBody with a compute envelope, so a child that asks for
// more than its parent has a boundary to exceed.
func rootBody(name string, budget, depth int) string {
	obj := map[string]any{
		"apiVersion": v1.APIVersion, "kind": "Sandbox",
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{
			"resources": map[string]any{"cpu": "2"},
			"mesh":      map[string]any{"spawn": map[string]any{"budget": budget, "depth": depth}},
		},
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// tree is a root with spawn rights and the token a process inside it holds.
// The token is minted as the controller mints one, from the sandbox's own
// desired state.
func tree(t *testing.T, f *fixture, budget, depth int) (v1.Sandbox, string) {
	t.Helper()
	signer := signing(t, f, rows{})
	body := rootBody("planner", budget, depth)
	var parent v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, body, 201), &parent); err != nil {
		t.Fatal(err)
	}
	token, err := signer.MintWorkload(auth.Workload{
		Sandbox: parent.Status.ID, Environment: "default",
		Spawn: &auth.Spawn{Budget: parent.Status.Spawn.Budget, Depth: parent.Status.Spawn.Depth},
	})
	if err != nil {
		t.Fatal(err)
	}
	return parent, token.Value
}

// TestSpawnOverTheAPI: a workload's create is a spawn. The child carries its
// parent, the tree's root and the root's owner, and the budget runs out where
// the manifest said it would.
func TestSpawnOverTheAPI(t *testing.T) {
	f := setup(t, allowAll{})
	parent, token := tree(t, f, 2, 1)

	var first v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", token, spawnBody("worker", 0, 0), 201), &first); err != nil {
		t.Fatal(err)
	}
	if first.Status.Parent != parent.Status.ID || first.Status.Root != parent.Status.ID {
		t.Fatalf("the child is parent %q of root %q", first.Status.Parent, first.Status.Root)
	}
	if first.Status.Owner != parent.Status.Owner {
		t.Fatalf("the child's owner is %q, want the root's %q", first.Status.Owner, parent.Status.Owner)
	}
	f.request("POST", "/v1/sandboxes", token, spawnBody("second", 0, 0), 201)

	// The third exceeds the budget the root declared.
	body := f.request("POST", "/v1/sandboxes", token, spawnBody("third", 0, 0), 422)
	if !strings.Contains(string(body), "spawn_budget_exhausted") {
		t.Fatalf("the third spawn answered %s", body)
	}
}

// TestClaimsAreTheGrant: a token minted with a budget larger than the store's
// spawns exactly as far as the store says. The claim is the grant at mint and
// the ledger is the enforcement.
func TestClaimsAreTheGrant(t *testing.T) {
	f := setup(t, allowAll{})
	signer := signing(t, f, rows{})
	var parent v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, rootBody("planner", 1, 1), 201), &parent); err != nil {
		t.Fatal(err)
	}
	forged, err := signer.MintWorkload(auth.Workload{
		Sandbox: parent.Status.ID, Environment: "default",
		Spawn: &auth.Spawn{Budget: 99, Depth: 9},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.request("POST", "/v1/sandboxes", forged.Value, spawnBody("one", 0, 0), 201)
	body := f.request("POST", "/v1/sandboxes", forged.Value, spawnBody("two", 0, 0), 422)
	if !strings.Contains(string(body), "spawn_budget_exhausted") {
		t.Fatalf("a forged budget raised the ledger: %s", body)
	}
}

// TestSpawnBoundaryOverTheAPI: a child asking for more than its parent has is
// boundary_exceeded naming the path, and a grandchild from a parent with no
// generations left is refused at the depth.
func TestSpawnBoundaryOverTheAPI(t *testing.T) {
	f := setup(t, allowAll{})
	_, token := tree(t, f, 2, 1)

	// The root's compute envelope is the boundary here, because it needs no
	// gateway to be enforced and the rule is the same containment.
	wider := `{"apiVersion":"` + v1.APIVersion + `","kind":"Sandbox","metadata":{"name":"wider"},` +
		`"spec":{"resources":{"cpu":"8"}}}`
	body := f.request("POST", "/v1/sandboxes", token, wider, 422)
	var refusal struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Paths []string `json:"paths"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Error.Code != "boundary_exceeded" {
		t.Fatalf("a wider child answered %s", body)
	}
	if !slices.Contains(refusal.Error.Details.Paths, "spec.resources.cpu") {
		t.Fatalf("the refusal names %v", refusal.Error.Details.Paths)
	}

	// The child that does fit gets a depth of zero, so its own token creates
	// nothing: the grandchild is refused at the depth.
	var first v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", token, spawnBody("worker", 0, 0), 201), &first); err != nil {
		t.Fatal(err)
	}
	signer := signing(t, f, rows{})
	childToken, err := signer.MintWorkload(auth.Workload{Sandbox: first.Status.ID, Environment: "default"})
	if err != nil {
		t.Fatal(err)
	}
	body = f.request("POST", "/v1/sandboxes", childToken.Value, spawnBody("grandchild", 0, 0), 422)
	if err = json.Unmarshal(body, &refusal); err != nil {
		t.Fatal(err)
	}
	if refusal.Error.Code != "boundary_exceeded" || !slices.Contains(refusal.Error.Details.Paths, "spec.mesh.spawn.depth") {
		t.Fatalf("a grandchild from a parent with no depth answered %s", body)
	}
}

// TestOnlyWorkloadsSpawn: status is the server's, so a person naming a parent
// on apply is applying a root and not a child.
func TestOnlyWorkloadsSpawn(t *testing.T) {
	f := setup(t, allowAll{})
	parent, _ := tree(t, f, 2, 1)
	body := `{"apiVersion":"` + v1.APIVersion + `","kind":"Sandbox","metadata":{"name":"claimed"},` +
		`"spec":{},"status":{"parent":"` + parent.Status.ID + `","root":"` + parent.Status.ID + `"}}`
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, body, 201), &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Status.Parent != "" || obj.Status.Root != obj.Status.ID {
		t.Fatalf("a person applied a child: parent %q of root %q", obj.Status.Parent, obj.Status.Root)
	}
	// The parent's budget was not spent by it.
	var read v1.Sandbox
	if err := json.Unmarshal(f.request("GET", "/v1/sandboxes/"+parent.Status.ID, f.alice, "", 200), &read); err != nil {
		t.Fatal(err)
	}
	if read.Status.Spawn.Used != 0 {
		t.Fatalf("the named parent has used %d of its budget", read.Status.Spawn.Used)
	}
}

// TestRootQuery: the tree selector of design 008 returns one root's
// sandboxes, the root included, and every other selector still narrows it.
func TestRootQuery(t *testing.T) {
	f := setup(t, allowAll{})
	parent, token := tree(t, f, 2, 1)
	f.request("POST", "/v1/sandboxes", token, spawnBody("worker", 0, 0), 201)
	// A sandbox of another tree is not in this one's page.
	f.request("POST", "/v1/sandboxes", f.alice, spawnBody("stranger", 0, 0), 201)

	var page struct {
		Items []v1.Sandbox `json:"items"`
	}
	if err := json.Unmarshal(f.request("GET", "/v1/sandboxes?root="+parent.Status.ID, f.alice, "", 200), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("the tree holds %d sandboxes, want the root and its child", len(page.Items))
	}
	for _, obj := range page.Items {
		if obj.Status.Root != parent.Status.ID {
			t.Errorf("%s is in the page with root %q", obj.Metadata.Name, obj.Status.Root)
		}
	}
	// The other selectors compose with it.
	if err := json.Unmarshal(f.request("GET", "/v1/sandboxes?root="+parent.Status.ID+"&environment=other", f.alice, "", 200), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("the tree on another environment holds %d sandboxes", len(page.Items))
	}
}

// TestCascadeOverTheAPI: deleting a root deletes the tree below it, so a
// workload cannot outlive the sandbox that made it.
func TestCascadeOverTheAPI(t *testing.T) {
	f := setup(t, allowAll{})
	parent, token := tree(t, f, 2, 1)
	var first v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", token, spawnBody("worker", 0, 0), 201), &first); err != nil {
		t.Fatal(err)
	}
	f.request("DELETE", "/v1/sandboxes/"+parent.Status.ID, f.alice, "", 202)
	f.request("GET", "/v1/sandboxes/"+first.Status.ID, f.alice, "", 404)
	f.request("GET", "/v1/sandboxes/"+parent.Status.ID, f.alice, "", 404)
}
