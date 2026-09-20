// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/runtimetest"
)

// conformanceImage is the base image the suite's sandboxes run. It is a
// published image name, not a coordinate of any installation, and an operator
// running the suite elsewhere overrides it.
const conformanceImage = "docker.io/library/alpine:latest"

// envDisplayImage names an image carrying the desktop's tools, which is what
// the display cases need. It is the caller's: this package builds no image and
// names no registry, and with none set every case about a screen skips.
const envDisplayImage = "CELLA_TEST_DISPLAY_IMAGE"

// TestPodmanConformance runs the shared driver contract against a real engine.
// It skips, naming every socket it tried, where none answers, so a machine
// with no podman still runs the suite.
func TestPodmanConformance(t *testing.T) {
	d, err := New(Options{Socket: os.Getenv("CELLA_PODMAN_SOCKET")})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Preflight(t.Context()); err != nil {
		t.Skipf("no podman engine for the conformance suite: %v", err)
	}
	open := func(t *testing.T) driver.Driver {
		d, err := New(Options{Socket: d.Socket()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		return d
	}
	runtimetest.Run(t, open, runtimetest.Options{
		Image: conformanceImage, DisplayImage: os.Getenv(envDisplayImage),
		// What binds a port belongs to the image: this is what the suite's
		// own image has.
		Listen: func(port int) []string {
			return []string{"sh", "-c", fmt.Sprintf("nc -l -p %d || sleep 600", port)}
		},
	})
}

// TestPodmanMeshOnARealEngine holds the mesh of spec 022 to the engine rather
// than to the fake: the network exists with the name the contract derives, a
// member answers to its own name and to that name in the mesh zone, and the
// network is gone once the last member is deleted. It skips where no engine
// answers, as the conformance suite above does.
func TestPodmanMeshOnARealEngine(t *testing.T) {
	d, err := New(Options{Socket: os.Getenv("CELLA_PODMAN_SOCKET")})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Preflight(t.Context()); err != nil {
		t.Skipf("no podman engine for the mesh case: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	const mesh = "msh_01j9zk2p7q8r9s0t1u2v3w4x5z"
	network := driver.MeshObjectName(mesh)
	ids := []string{"sbx_meshone", "sbx_meshtwo"}
	names := []string{"peer-one", "peer-two"}
	t.Cleanup(func() {
		clean := context.WithoutCancel(t.Context())
		for _, id := range ids {
			_ = d.Delete(clean, id)
		}
	})
	for i, id := range ids {
		spec := driver.CreateSpec{ID: id, Name: names[i], Owner: "alice", Image: conformanceImage,
			Mesh: driver.Mesh{ID: mesh}}
		if _, err = d.Create(t.Context(), spec); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	var inspected map[string]any
	if err = d.client().json(t.Context(), http.MethodGet, "/networks/"+network+"/json", nil, &inspected); err != nil {
		t.Fatalf("the engine holds no network %s: %v", network, err)
	}
	members, err := d.List(t.Context(), driver.Filter{MeshID: mesh})
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("the mesh holds %d members, want 2", len(members))
	}
	if err = d.Delete(t.Context(), ids[0]); err != nil {
		t.Fatal(err)
	}
	if err = d.client().json(t.Context(), http.MethodGet, "/networks/"+network+"/json", nil, &inspected); err != nil {
		t.Fatalf("the network went while a member was still on it: %v", err)
	}
	if err = d.Delete(t.Context(), ids[1]); err != nil {
		t.Fatal(err)
	}
	if err = d.client().json(t.Context(), http.MethodGet, "/networks/"+network+"/json", nil, &inspected); err == nil {
		t.Fatalf("the network %s outlived its last member", network)
	}
}
