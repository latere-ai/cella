// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/internal/egressd"
	driver "latere.ai/x/cella/runtime"
)

// TestSpawnTreeEndToEnd is spec 022 over one running node: a root with spawn
// rights, a process inside it reading its own token and creating children
// with it through the same route a person uses, the budget and the depth
// running out where the manifest said they would, a child that asks for one
// more host than its parent refused, and the root's delete taking the tree
// with it.
//
// It runs on the native environment, which declares no Mesh: spawn works
// there and the mesh is a status field nothing enforces, which is what makes
// the two capabilities separate.
func TestSpawnTreeEndToEnd(t *testing.T) {
	proxyAddr, reverseAddr := freePort(t), freePort(t)
	plane := startPlane(t, proxyAddr, reverseAddr)
	// An allowlist boundary is held by a gateway, so one runs: the root's
	// reach is what its children are held to.
	ready := make(chan struct{})
	startGateway(t, plane, egressd.Options{
		ProxyAddr: proxyAddr, ReverseAddr: reverseAddr, Ready: func() { close(ready) },
	})
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("the gateway never received its first snapshot")
	}

	root := spawnRoot(t, plane)
	if root.Status.Root != root.Status.ID || root.Status.Parent != "" {
		t.Fatalf("the root is parent %q of root %q", root.Status.Parent, root.Status.Root)
	}
	if root.Status.Spawn.Budget != 2 || root.Status.Spawn.Depth != 1 {
		t.Fatalf("the root's budget is %+v, want two children over one generation", root.Status.Spawn)
	}
	// The native environment connects no peers, so the mesh is a field the
	// status carries and nothing joins.
	if root.Status.Mesh != "" {
		t.Fatalf("a native root is in the mesh %q", root.Status.Mesh)
	}

	// The workload reads its own token the way the agent client does, from
	// the path the driver named in its environment.
	token := strings.TrimSpace(plane.exec(t, root.Status.ID, `cat "$`+driver.TokenFileEnv+`"`))
	if strings.Count(token, ".") != 2 {
		t.Fatalf("the projection holds %q, want a compact JWS", token)
	}

	// Two children, created by the workload through the same route.
	first := spawnChild(t, plane, token, "worker-one", "", http.StatusCreated)
	if first.Status.Parent != root.Status.ID || first.Status.Root != root.Status.ID {
		t.Fatalf("the child is parent %q of root %q", first.Status.Parent, first.Status.Root)
	}
	if first.Status.Owner != root.Status.Owner {
		t.Fatalf("the child's owner is %q, want the root's %q", first.Status.Owner, root.Status.Owner)
	}
	second := spawnChild(t, plane, token, "worker-two", "", http.StatusCreated)

	// The third is one more than the root declared, and the ledger is what
	// refuses it.
	refused := spawnRefusal(t, plane, token, "worker-three", "")
	if refused.Code != "spawn_budget_exhausted" {
		t.Fatalf("the third child answered %s", refused.Code)
	}

	// A grandchild is one generation more than the root declared. The
	// child's own token carries no depth, so the refusal is the boundary's
	// at the depth rather than the ledger's.
	childToken := strings.TrimSpace(plane.exec(t, first.Status.ID, `cat "$`+driver.TokenFileEnv+`"`))
	refused = spawnRefusal(t, plane, childToken, "grandchild", "")
	if refused.Code != "boundary_exceeded" || !slices.Contains(refused.Paths, "spec.mesh.spawn.depth") {
		t.Fatalf("the grandchild answered %s at %v", refused.Code, refused.Paths)
	}

	// The root's own budget is spent, so a wider child is refused at
	// resolve rather than at the debit: a boundary is read before a count.
	wider := spawnRoot(t, plane)
	widerToken := strings.TrimSpace(plane.exec(t, wider.Status.ID, `cat "$`+driver.TokenFileEnv+`"`))
	refused = spawnRefusal(t, plane, widerToken, "reaches-further",
		`"network":{"egress":{"mode":"allowlist","allowedHosts":["upstream.example.com","elsewhere.example.com"]}},`)
	if refused.Code != "boundary_exceeded" || !slices.Contains(refused.Paths, "spec.network.egress.allowedHosts") {
		t.Fatalf("a child reaching one more host answered %s at %v", refused.Code, refused.Paths)
	}

	// The parent's status carries what the ledger counted.
	read := readSandbox(t, plane, root.Status.ID)
	if read.Status.Spawn.Used != 2 {
		t.Fatalf("the root has used %d of its budget, want 2", read.Status.Spawn.Used)
	}

	// The tree selector answers the whole tree, the root included.
	tree := treeOf(t, plane, root.Status.ID)
	if len(tree) != 3 {
		t.Fatalf("the tree holds %d sandboxes, want the root and its two children", len(tree))
	}

	// Deleting the root deletes the tree below it.
	if status, answer := plane.do(t, http.MethodDelete, "/v1/sandboxes/"+root.Status.ID, nil); status != http.StatusAccepted {
		t.Fatalf("DELETE the root = %d %s", status, answer)
	}
	for _, id := range []string{root.Status.ID, first.Status.ID, second.Status.ID} {
		if status, answer := plane.do(t, http.MethodGet, "/v1/sandboxes/"+id, nil); status != http.StatusNotFound {
			t.Fatalf("%s outlived the root: %d %s", id, status, answer)
		}
	}
}

// sandboxRead is the half of a sandbox this case reads back.
type sandboxRead struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		ID, Owner, Parent, Root, Mesh string
		Spawn                         struct {
			Budget, Used, Depth int
		} `json:"spawn"`
	} `json:"status"`
}

// refusal is the error envelope of design 008 as this case reads it.
type refusal struct {
	Code  string
	Paths []string
}

// spawnRoot applies a root with an allowlist boundary and spawn rights: two
// children over one generation.
func spawnRoot(t *testing.T, p *plane) sandboxRead {
	t.Helper()
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","spec":{` +
		`"network":{"egress":{"mode":"allowlist","allowedHosts":["upstream.example.com"]}},` +
		`"mesh":{"spawn":{"budget":2,"depth":1}}}}`
	status, answer := p.do(t, http.MethodPost, "/v1/sandboxes?wait=1", strings.NewReader(body))
	if status != http.StatusCreated {
		t.Fatalf("POST the root = %d %s", status, answer)
	}
	var obj sandboxRead
	if err := json.Unmarshal([]byte(answer), &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

// spawnChild applies one child manifest with the workload's own token, which
// is the same route and the same verb a person uses.
func spawnChild(t *testing.T, p *plane, token, name, extraSpec string, want int) sandboxRead {
	t.Helper()
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"` + name + `"},` +
		`"spec":{` + extraSpec + `"mesh":{"spawn":{"budget":0,"depth":0}}}}`
	status, answer := asWorkload(t, p, token, body)
	if status != want {
		t.Fatalf("POST the child %s = %d %s, want %d", name, status, answer, want)
	}
	var obj sandboxRead
	if err := json.Unmarshal([]byte(answer), &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

// spawnRefusal applies a child that must be refused and reads the envelope.
func spawnRefusal(t *testing.T, p *plane, token, name, extraSpec string) refusal {
	t.Helper()
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"` + name + `"},` +
		`"spec":{` + extraSpec + `"mesh":{"spawn":{"budget":0,"depth":0}}}}`
	status, answer := asWorkload(t, p, token, body)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("POST the child %s = %d %s, want 422", name, status, answer)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Paths []string `json:"paths"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(answer), &envelope); err != nil {
		t.Fatal(err)
	}
	return refusal{Code: envelope.Error.Code, Paths: envelope.Error.Details.Paths}
}

// asWorkload calls the control plane with a sandbox's own token rather than
// the person's bearer.
func asWorkload(t *testing.T, p *plane, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, p.url+"/v1/sandboxes?wait=1", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(answer)
}

func readSandbox(t *testing.T, p *plane, id string) sandboxRead {
	t.Helper()
	var obj sandboxRead
	if err := json.Unmarshal([]byte(p.get(t, "/v1/sandboxes/"+id)), &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func treeOf(t *testing.T, p *plane, root string) []sandboxRead {
	t.Helper()
	var page struct {
		Items []sandboxRead `json:"items"`
	}
	if err := json.Unmarshal([]byte(p.get(t, "/v1/sandboxes?root="+root)), &page); err != nil {
		t.Fatal(err)
	}
	return page.Items
}
