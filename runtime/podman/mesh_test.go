// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"net/http"
	"slices"
	"testing"

	driver "latere.ai/x/cella/runtime"
)

const testMesh = "msh_01j9zk2p7q8r9s0t1u2v3w4x5y"

// TestMeshNetwork is what spec 022 asks this driver for: one network per
// mesh, every member on it under its own name and its name in the mesh zone,
// and the network gone once the last member is deleted.
func TestMeshNetwork(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	network := driver.MeshObjectName(testMesh)

	create(t, d, driver.CreateSpec{ID: "sbx_one", Name: "planner", Mesh: driver.Mesh{ID: testMesh}})
	members, held := f.network(network)
	if !held {
		t.Fatalf("the first member made no network %s", network)
	}
	want := []string{"planner", "planner.mesh"}
	if got := members[containerName("sbx_one")]; !slices.Equal(got, want) {
		t.Fatalf("the member answers to %v, want %v", got, want)
	}

	// The second member joins the network the first made rather than
	// refusing on the name it already holds.
	create(t, d, driver.CreateSpec{ID: "sbx_two", Name: "worker", Parent: "sbx_one", Mesh: driver.Mesh{ID: testMesh}})
	members, _ = f.network(network)
	if len(members) != 2 {
		t.Fatalf("the network holds %d members, want 2", len(members))
	}

	// The stamped identity is what the filter reads, so a list answers the
	// mesh and the tree without reading every object.
	if got := ids(t, d, driver.Filter{MeshID: testMesh}); !slices.Equal(got, []string{"sbx_one", "sbx_two"}) {
		t.Errorf("the mesh holds %v, want both members", got)
	}
	if got := ids(t, d, driver.Filter{Parent: "sbx_one"}); !slices.Equal(got, []string{"sbx_two"}) {
		t.Errorf("the children of sbx_one are %v, want sbx_two", got)
	}

	// A mesh ends with its last member, and not before.
	if err := d.Delete(t.Context(), "sbx_one"); err != nil {
		t.Fatal(err)
	}
	if _, held = f.network(network); !held {
		t.Fatal("the network went while a member was still on it")
	}
	if err := d.Delete(t.Context(), "sbx_two"); err != nil {
		t.Fatal(err)
	}
	if _, held = f.network(network); held {
		t.Fatalf("the network %s outlived its last member", network)
	}
}

// TestMeshlessSandboxTouchesNoNetwork: a sandbox in no mesh asks the engine
// for no network at all, so the driver's one network per mesh is one network
// per mesh and not one per sandbox.
func TestMeshlessSandboxTouchesNoNetwork(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_alone", Name: "alone"})
	if err := d.Delete(t.Context(), "sbx_alone"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.networks) != 0 {
		t.Fatalf("the engine holds %v, want no network", f.networks)
	}
}

// TestMeshJoinFailureUndoesTheCreate: a member the engine would not attach
// leaves nothing behind, because a sandbox that is in a mesh by its manifest
// and off the network in the engine is a sandbox its peers cannot reach and
// the control plane believes they can.
func TestMeshJoinFailureUndoesTheCreate(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	f.fault("POST "+libpod+"/networks/create", http.StatusInternalServerError)
	_, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_one", Name: "planner", Image: "img", Mesh: driver.Mesh{ID: testMesh}})
	if err == nil {
		t.Fatal("a create whose mesh could not be made was accepted")
	}
	if _, err = d.Inspect(t.Context(), "sbx_one"); err == nil {
		t.Fatal("the refused create left a sandbox behind")
	}
}

// TestMeshConnectFailureUndoesTheCreate is the same rule one step later: the
// network was made and the member could not be put on it.
func TestMeshConnectFailureUndoesTheCreate(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	f.fault("POST "+libpod+"/networks/"+driver.MeshObjectName(testMesh)+"/connect", http.StatusInternalServerError)
	_, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_one", Name: "planner", Image: "img", Mesh: driver.Mesh{ID: testMesh}})
	if err == nil {
		t.Fatal("a create whose member could not join was accepted")
	}
	if _, err = d.Inspect(t.Context(), "sbx_one"); err == nil {
		t.Fatal("the refused create left a sandbox behind")
	}
}

// TestMeshNetworkAlreadyGone: a network another deletion already removed is
// not an error, so two members deleted at once do not leave one delete
// failing on the object the other took.
func TestMeshNetworkAlreadyGone(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_one", Name: "planner", Mesh: driver.Mesh{ID: testMesh}})
	f.mu.Lock()
	delete(f.networks, driver.MeshObjectName(testMesh))
	f.mu.Unlock()
	if err := d.Delete(t.Context(), "sbx_one"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
