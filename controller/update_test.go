// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"slices"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// TestUpdateReplacesTheSpecificationAndKeepsTheStatus: an apply writes what
// the sandbox is asked to be and never what the controller recorded it as.
func TestUpdateReplacesTheSpecificationAndKeepsTheStatus(t *testing.T) {
	d := &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist, v1.EgressOpen}}
	gw := &gateway{}
	c := openController(t, Options{Driver: d, Egress: gw})
	created, err := c.Create(t.Context(), bounded(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	next := bounded()
	next.Status = created.Status
	next.Metadata.Labels = map[string]string{"team": "research"}
	next.Spec.Network.Egress.AllowedHosts = []string{"api.example.com", "pypi.org"}
	updated, err := c.Update(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status.ID != created.Status.ID || updated.Status.Phase != created.Status.Phase {
		t.Fatalf("the update rewrote the status: %+v", updated.Status)
	}
	if updated.Status.CreatedAt != created.Status.CreatedAt {
		t.Fatal("the update moved the creation instant")
	}
	if updated.Metadata.Labels["team"] != "research" {
		t.Fatalf("the update did not land: %v", updated.Metadata.Labels)
	}
	// The boundary reached a gateway, so what a gateway holds is what desired
	// state says.
	sent := gw.maps()
	if len(sent) != 2 {
		t.Fatalf("%d maps pushed, want the create's and the update's", len(sent))
	}
	if !slices.Equal(sent[1].Allow, []string{"api.example.com", "pypi.org"}) {
		t.Fatalf("the second map allows %v", sent[1].Allow)
	}
	// A read of the sandbox answers what the update wrote.
	read, err := c.Get(t.Context(), created.Status.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if read.Metadata.Labels["team"] != "research" {
		t.Fatalf("a read after the update answers %v", read.Metadata.Labels)
	}
	// The identity and the boundary's own record are the control plane's, so
	// an answer carries neither.
	if updated.Status.EgressState != nil || updated.Status.TokenState != nil {
		t.Fatal("the answer carries the control plane's own record")
	}
}

// TestUpdateOfAnObjectThatIsNotThere: an id this controller does not hold is
// not found, which is the answer a read of it gives.
func TestUpdateOfAnObjectThatIsNotThere(t *testing.T) {
	c := openController(t, Options{Driver: &gatewayDriver{modes: []v1.EgressMode{v1.EgressOpen}}})
	obj := workspace()
	obj.Status.ID = "sbx_01j0000000000000000000000"
	if _, err := c.Update(t.Context(), obj); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the update answered %v", err)
	}
}

// TestUpdateWhoseBoundaryNoGatewayWillHold: a map a gateway refuses is a
// refusal with desired state unchanged, and the boundary the object already
// had is put back, so every map a gateway holds is a map desired state has.
func TestUpdateWhoseBoundaryNoGatewayWillHold(t *testing.T) {
	d := &gatewayDriver{modes: []v1.EgressMode{v1.EgressAllowlist}}
	gw := &gateway{}
	c := openController(t, Options{Driver: d, Egress: gw})
	created, err := c.Create(t.Context(), bounded(), "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	gw.sendErr = errors.New("the gateway is not there")
	next := bounded()
	next.Status = created.Status
	next.Spec.Network.Egress.AllowedHosts = []string{"elsewhere.example.com"}
	if _, err := c.Update(t.Context(), next); err == nil {
		t.Fatal("an update whose map no gateway held was accepted")
	}
	read, err := c.Get(t.Context(), created.Status.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(read.Spec.Network.Egress.AllowedHosts, []string{"api.example.com"}) {
		t.Fatalf("a refused update changed the object: %v", read.Spec.Network.Egress.AllowedHosts)
	}
}
