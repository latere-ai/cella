// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"

	driver "latere.ai/x/cella/runtime"
)

// publishHost is the host address every declared port is published on. It is
// loopback, so what reaches a port is this host's own processes, the control
// plane among them, and no other machine.
const publishHost = "127.0.0.1"

// portMapping is libpod's publication of one container port on the host. An
// absent host port is one the engine picks, so two sandboxes that declare the
// same port never ask for the same host port.
type portMapping struct {
	ContainerPort uint16 `json:"container_port"`
	HostIP        string `json:"host_ip,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
}

// hostBinding is one host address the engine bound a container port to, as
// the container's inspect reports it.
type hostBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// publishedPorts is what a create asks the engine to publish: each declared
// port on loopback. A container's network namespace is its own and the libpod
// API has no way into it, so the publication is the route Dial takes.
func publishedPorts(ports []driver.Port) []portMapping {
	if len(ports) == 0 {
		return nil
	}
	out := make([]portMapping, len(ports))
	for i, p := range ports {
		out[i] = portMapping{ContainerPort: uint16(p.Port), HostIP: publishHost, Protocol: "tcp"}
	}
	return out
}

// Dial connects to a declared port of a running sandbox through the host
// port the engine published it on. The mapping is read from the engine at
// every dial, so a second driver over the same engine reaches what the first
// created. An undeclared port was never published and is not found.
//
// Where a user-space forwarder stands in front of the engine, as the rootless
// port proxy and a machine's forwarder do, the host side accepts even when
// nothing listens inside, and a closed port reads as a connection the far end
// closes at once.
func (d *Driver) Dial(ctx context.Context, id string, port int) (net.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("%w: port %d is outside 1 to 65535", driver.ErrInvalid, port)
	}
	if err := d.exists(ctx, id); err != nil {
		return nil, err
	}
	var ci containerInspect
	err := d.client().json(ctx, http.MethodGet, "/containers/"+containerName(id)+"/json", nil, &ci)
	if notFound(err) {
		// A stopped sandbox keeps its volumes and may have no container.
		return nil, driver.ErrNotRunning
	}
	if err != nil {
		return nil, fmt.Errorf("podman: reading the ports of %s: %w", id, err)
	}
	if phase, _ := phaseOf(status{present: true, state: ci.State.Status}, false); phase != driver.Running {
		return nil, driver.ErrNotRunning
	}
	bindings := ci.NetworkSettings.Ports[strconv.Itoa(port)+"/tcp"]
	if len(bindings) == 0 || bindings[0].HostPort == "" {
		return nil, fmt.Errorf("%w: port %d of %s is not published; only a declared port is", driver.ErrNotFound, port, id)
	}
	host := bindings[0].HostIP
	if host == "" || host == "0.0.0.0" {
		host = publishHost
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, bindings[0].HostPort))
	if err != nil {
		return nil, fmt.Errorf("podman: dialling port %d of %s: %w", port, id, err)
	}
	return conn, nil
}
