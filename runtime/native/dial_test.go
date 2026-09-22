// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// freePort is a loopback port nothing held a moment ago, for a sandbox to
// serve. A fixed number would collide with whatever else the host runs.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err = ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// echoOnce writes one line on a fresh connection and reads it back.
func echoOnce(d *Driver, id string, port int, line string) error {
	conn, err := d.Dial(context.Background(), id, port)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err = conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	if _, err = conn.Write([]byte(line + "\n")); err != nil {
		return err
	}
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return err
	}
	if got != line+"\n" {
		return errors.New("read back " + got)
	}
	return nil
}

// TestNativeDial is the native Dialer: a running sandbox's port is the host's
// loopback port, and everything else is refused with the contract's errors.
func TestNativeDial(t *testing.T) {
	d, _ := fresh(t)
	ctx := t.Context()
	if !d.Capabilities().Dial {
		t.Fatal("the native driver declares no Dial")
	}
	port := freePort(t)
	_, err := d.Create(ctx, driver.CreateSpec{ID: "sbx_dial", Name: "dial", Owner: "alice",
		Command: echoCommand(port), Ports: []driver.Port{{Name: "echo", Port: port}}})
	check(t, err)
	t.Cleanup(func() { _ = d.Delete(context.WithoutCancel(ctx), "sbx_dial") })

	deadline := time.Now().Add(5 * time.Second)
	for err = echoOnce(d, "sbx_dial", port, "hello"); err != nil; err = echoOnce(d, "sbx_dial", port, "hello") {
		if time.Now().After(deadline) {
			t.Fatalf("no line came back through Dial: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, tc := range []struct {
		name string
		id   string
		port int
		want error
	}{
		{"a port below the range", "sbx_dial", 0, driver.ErrInvalid},
		{"a port above the range", "sbx_dial", 65536, driver.ErrInvalid},
		{"an id outside the syntax", "../escape", port, driver.ErrInvalid},
		{"an absent sandbox", "sbx_absent", port, driver.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := d.Dial(ctx, tc.id, tc.port)
			if conn != nil {
				_ = conn.Close()
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Dial answered %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("a port nothing holds", func(t *testing.T) {
		conn, err := d.Dial(ctx, "sbx_dial", freePort(t))
		if conn != nil {
			_ = conn.Close()
		}
		if err == nil || errors.Is(err, driver.ErrNotRunning) {
			t.Fatalf("a dial of a closed port answered %v, want the refusal", err)
		}
	})

	check(t, d.Stop(ctx, "sbx_dial"))
	conn, err := d.Dial(ctx, "sbx_dial", port)
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("a dial of a stopped sandbox answered %v, want ErrNotRunning", err)
	}
}

// TestNativeProbesDeclaredPorts: Inspect reads every declared port of a
// running sandbox as listening or closed, in the order declared, and reports
// no port of a sandbox that is not running.
func TestNativeProbesDeclaredPorts(t *testing.T) {
	d, root := fresh(t)
	ctx := t.Context()
	bound, idle := freePort(t), freePort(t)
	_, err := d.Create(ctx, driver.CreateSpec{ID: "sbx_ports", Name: "ports", Owner: "alice",
		Command: echoCommand(bound), Ports: []driver.Port{{Name: "bound", Port: bound}, {Name: "idle", Port: idle}}})
	check(t, err)
	t.Cleanup(func() { _ = d.Delete(context.WithoutCancel(ctx), "sbx_ports") })

	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := d.Inspect(ctx, "sbx_ports")
		check(t, err)
		if len(state.Ports) != 2 || state.Ports[0].Name != "bound" || state.Ports[1].Name != "idle" {
			t.Fatalf("the probe reports %+v, want the two declared in order", state.Ports)
		}
		if state.Ports[1].State != driver.PortClosed {
			t.Fatalf("a port nothing holds reads %q", state.Ports[1].State)
		}
		if state.Ports[0].State == driver.PortListening {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the bound port stayed %q", state.Ports[0].State)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The declaration is the record's, so a second driver over the root
	// probes what the first was told.
	check(t, d.Stop(ctx, "sbx_ports"))
	again, err := New(root)
	check(t, err)
	t.Cleanup(func() { _ = again.Close() })
	stopped, err := again.Inspect(ctx, "sbx_ports")
	check(t, err)
	if stopped.Ports != nil {
		t.Fatalf("a stopped sandbox reports ports %+v", stopped.Ports)
	}
	record, err := again.load("sbx_ports")
	check(t, err)
	if len(record.Ports) != 2 {
		t.Fatalf("the record keeps %+v", record.Ports)
	}

	_, err = d.Create(ctx, driver.CreateSpec{ID: "sbx_noports", Name: "plain", Owner: "alice"})
	check(t, err)
	plain, err := d.Inspect(ctx, "sbx_noports")
	check(t, err)
	if plain.Ports != nil {
		t.Fatalf("a sandbox that declared no port reports %+v", plain.Ports)
	}
}
