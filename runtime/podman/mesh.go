// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"context"
	"fmt"
	"net/http"

	driver "latere.ai/x/cella/runtime"
)

// MeshDomain is the zone a mesh's members resolve each other in: a peer is
// reached at <sandbox-name>.mesh on a port it declared. The driver owns the
// zone inside its members and nothing outside the mesh resolves it.
const MeshDomain = "mesh"

// networkCreate is the body libpod makes a network from. The driver is the
// engine's own bridge, which is what gives the members one subnet and the
// engine's DNS over it, and internal keeps that subnet off the host's
// uplink: a mesh is peers reaching each other, never a second way out.
type networkCreate struct {
	Name     string `json:"name"`
	Driver   string `json:"driver"`
	Internal bool   `json:"internal"`
	DNS      bool   `json:"dns_enabled"`
}

// networkConnect is the body libpod attaches one container to one network
// with. The aliases are the names the engine's DNS answers for this member
// inside that network.
type networkConnect struct {
	Container      string          `json:"container"`
	EndpointConfig *networkAliases `json:"endpoint_config,omitempty"`
}

type networkAliases struct {
	Aliases []string `json:"aliases,omitempty"`
}

// joinMesh puts one container on its mesh's network under the two names its
// peers resolve it by, making the network first where this is the mesh's
// first member.
//
// The mesh network is additional: the container keeps the network it was
// created on, so the route to the gateway of spec 018 is the one it already
// had and a member reaches the network through its own map as before.
func (d *Driver) joinMesh(ctx context.Context, id, name, meshID string) error {
	if meshID == "" {
		return nil
	}
	network := driver.MeshObjectName(meshID)
	if err := d.createNetwork(ctx, network); err != nil {
		return err
	}
	body := networkConnect{
		Container:      containerName(id),
		EndpointConfig: &networkAliases{Aliases: meshAliases(name)},
	}
	err := d.client().json(ctx, http.MethodPost, "/networks/"+network+"/connect", body, nil)
	if err != nil {
		return fmt.Errorf("podman: putting %s on the mesh network %s: %w", id, network, err)
	}
	return nil
}

// meshAliases are the names a member answers to inside its mesh: its own
// name, which the engine's DNS serves inside the network, and the same name
// under the mesh zone, which is what spec 022 fixes as the address a peer
// dials.
func meshAliases(name string) []string {
	if name == "" {
		return nil
	}
	return []string{name, name + "." + MeshDomain}
}

// createNetwork makes one mesh's network. A network another member already
// made is not an error: the mesh is the set of members and the network is one
// object under it, whichever member arrived first.
func (d *Driver) createNetwork(ctx context.Context, network string) error {
	body := networkCreate{Name: network, Driver: "bridge", Internal: true, DNS: true}
	err := d.client().json(ctx, http.MethodPost, "/networks/create", body, nil)
	if err == nil || conflict(err) {
		return nil
	}
	return fmt.Errorf("podman: creating the mesh network %s: %w", network, err)
}

// leaveMesh removes the mesh's network once the sandbox being deleted was its
// last member. A mesh ends with its last member, so the object the engine
// holds ends with it; a network another member is still on is left alone, and
// one another deletion already removed is not an error.
func (d *Driver) leaveMesh(ctx context.Context, id, meshID string) error {
	if meshID == "" {
		return nil
	}
	members, err := d.List(ctx, driver.Filter{MeshID: meshID})
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.ID != id {
			return nil
		}
	}
	network := driver.MeshObjectName(meshID)
	err = d.client().json(ctx, http.MethodDelete, "/networks/"+network, nil, nil)
	if err == nil || notFound(err) {
		return nil
	}
	return fmt.Errorf("podman: removing the mesh network %s: %w", network, err)
}
