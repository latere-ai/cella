// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// probeTimeout bounds one connection the port probe opens. A port on loopback
// answers or refuses at once, so a probe that waits longer is reading a host
// under load and reports the port closed rather than holding Inspect.
const probeTimeout = 250 * time.Millisecond

// Dial connects to a port of a running sandbox. The driver confines nothing:
// the sandbox's processes run on the host, so its port is the host's loopback
// port of that number, and a caller allowed to dial it is one already allowed
// to run a command there.
func (d *Driver) Dial(ctx context.Context, id string, port int) (net.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("%w: port %d is outside 1 to 65535", driver.ErrInvalid, port)
	}
	d.mu.Lock()
	r, err := d.load(id)
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if r.State.Phase != driver.Running {
		return nil, driver.ErrNotRunning
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", loopback(port))
	if err != nil {
		return nil, fmt.Errorf("native: dialing port %d of %s: %w", port, id, err)
	}
	return conn, nil
}

// probePorts reports each declared port as listening where a connection to it
// on loopback opens, and closed otherwise.
func probePorts(ctx context.Context, ports []driver.Port) []driver.PortState {
	if len(ports) == 0 {
		return nil
	}
	out := make([]driver.PortState, len(ports))
	for i, p := range ports {
		state := driver.PortClosed
		dialer := net.Dialer{Timeout: probeTimeout}
		if conn, err := dialer.DialContext(ctx, "tcp", loopback(p.Port)); err == nil {
			_ = conn.Close()
			state = driver.PortListening
		}
		out[i] = driver.PortState{Name: p.Name, Port: p.Port, State: state}
	}
	return out
}

func loopback(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }
