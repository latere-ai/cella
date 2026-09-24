// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtimetest

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/runtime"
)

// hostDriver is a driver whose sandboxes share the host's network namespace,
// as the native driver's do: a sandbox's listen or echo command binds its
// port on the host's loopback, a bind that fails fails the sandbox, and the
// probe and the dial reach the host's loopback. It is what shows a case
// cannot be decided by a port another process on the machine holds.
type hostDriver struct {
	Nop
	mu    sync.Mutex
	boxes map[string]*hostBox
}

// hostBox is one sandbox: its phase, its declared ports, and the listener its
// command bound, nil where the bind failed.
type hostBox struct {
	phase    string
	ports    []runtime.Port
	listener net.Listener
}

func newHostDriver() *hostDriver { return &hostDriver{boxes: map[string]*hostBox{}} }

func (d *hostDriver) Capabilities() runtime.Capabilities { return runtime.Capabilities{Dial: true} }

func (d *hostDriver) Create(_ context.Context, s runtime.CreateSpec) (runtime.Ref, error) {
	box := &hostBox{phase: runtime.Running, ports: s.Ports}
	if len(s.Command) == 2 {
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", s.Command[1]))
		if err != nil {
			// The command exited on a port it could not bind, which is a
			// main process that ended with an error.
			box.phase = "Failed"
		} else {
			box.listener = l
			go serveHostEcho(l)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.boxes[s.ID] = box
	return runtime.Ref{ID: s.ID}, nil
}

// serveHostEcho writes back every byte each connection sends.
func serveHostEcho(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			_, _ = io.Copy(conn, conn)
		}()
	}
}

func (d *hostDriver) Inspect(_ context.Context, id string) (runtime.State, error) {
	d.mu.Lock()
	box, ok := d.boxes[id]
	d.mu.Unlock()
	if !ok {
		return runtime.State{}, runtime.ErrNotFound
	}
	state := runtime.State{ID: id, Phase: box.phase}
	if box.phase != runtime.Running {
		return state, nil
	}
	for _, p := range box.ports {
		port := runtime.PortState{Name: p.Name, Port: p.Port, State: runtime.PortClosed}
		if conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Port)), time.Second); err == nil {
			_ = conn.Close()
			port.State = runtime.PortListening
		}
		state.Ports = append(state.Ports, port)
	}
	return state, nil
}

func (d *hostDriver) Stop(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if box, ok := d.boxes[id]; ok {
		box.phase = runtime.Stopped
		if box.listener != nil {
			_ = box.listener.Close()
		}
	}
	return nil
}

func (d *hostDriver) Delete(ctx context.Context, id string) error {
	if err := d.Stop(ctx, id); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.boxes, id)
	return nil
}

func (d *hostDriver) Dial(ctx context.Context, id string, port int) (net.Conn, error) {
	d.mu.Lock()
	box, ok := d.boxes[id]
	d.mu.Unlock()
	switch {
	case !ok:
		return nil, runtime.ErrNotFound
	case box.phase != runtime.Running:
		return nil, runtime.ErrNotRunning
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// TestThePortCasesBindNoFixedHostPort: the probe and dial cases pass a driver
// whose sandboxes bind the host's own loopback while another process on the
// machine holds the ports those cases once fixed, 18080, 18081 and 18090. A
// port the case names is one the host had free when the case began.
func TestThePortCasesBindNoFixedHostPort(t *testing.T) {
	for _, port := range []string{"18080", "18081", "18090"} {
		// A port that cannot be bound here is already held by somebody
		// else, which is the condition this test sets up.
		if l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port)); err == nil {
			t.Cleanup(func() { _ = l.Close() })
		}
	}
	opts := Options{
		Listen: func(port int) []string { return []string{"listen", strconv.Itoa(port)} },
		Echo:   func(port int) []string { return []string{"echo", strconv.Itoa(port)} },
	}
	for _, name := range []string{"PortsReportListening", "DialReachesAPort"} {
		t.Run(name, func(t *testing.T) {
			if r := runCase(t, name, newHostDriver(), opts); r.failed() || r.skipped() != "" {
				t.Fatalf("%s did not pass a driver on the host's network:\n%s", name, r.report())
			}
		})
	}
}
