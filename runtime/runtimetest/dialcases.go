// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtimetest

import (
	"bufio"
	"context"
	"net"
	"strings"
	"time"

	"latere.ai/x/cella/internal/porttest"
	"latere.ai/x/cella/runtime"
)

// dialRoundTrip bounds one line's trip to the echo server and back.
var dialRoundTrip = 2 * time.Second

// dialReachesAPort is the Dialer of spec 004: a connection to a port a process
// inside holds carries bytes both ways, two connections are open at once, and
// a sandbox that is not running or not there is refused with the contract's
// errors rather than dialed.
func dialReachesAPort(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	if !d.Capabilities().Dial {
		t.Skipf("the driver declares no Dial")
	}
	dialer, ok := d.(runtime.Dialer)
	need(t, ok, "the driver declares Dial and does not implement runtime.Dialer")
	if opts.Echo == nil {
		t.Skipf("Options.Echo is nil: this suite has no command that serves a port")
	}
	const id = "sbx_cnf_dial"
	dialPort := porttest.Reserve(t)
	create(t, d, opts, runtime.CreateSpec{
		ID: id, Name: "dial", Owner: "alice",
		Command: opts.Echo(dialPort),
		Ports:   []runtime.Port{{Name: "echo", Port: dialPort}},
	})
	ctx := context.Background()

	// The server inside comes up after the sandbox reads Running, so the
	// first connection is retried until a line makes the trip.
	first := dialUntilEcho(t, dialer, id, dialPort)
	defer func() { _ = first.Close() }()
	second, err := dialer.Dial(ctx, id, dialPort)
	must(t, err, "a second Dial while the first is open")
	defer func() { _ = second.Close() }()
	expect(t, echoes(second, "second") == nil, "the second connection carries no bytes back")
	expect(t, echoes(first, "first again") == nil, "the first connection stopped carrying bytes once a second opened")

	_, err = dialer.Dial(ctx, "sbx_cnf_absent", dialPort)
	wantErr(t, err, runtime.ErrNotFound, "Dial on an absent sandbox")
	must(t, d.Stop(ctx, id), "Stop")
	waitPhase(t, d, id, runtime.Stopped)
	conn, err := dialer.Dial(ctx, id, dialPort)
	if conn != nil {
		_ = conn.Close()
	}
	wantErr(t, err, runtime.ErrNotRunning, "Dial on a stopped sandbox")
}

// dialUntilEcho dials until one line comes back, or fails the case once the
// poll window has passed.
func dialUntilEcho(t tb, dialer runtime.Dialer, id string, port int) net.Conn {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for {
		conn, err := dialer.Dial(context.Background(), id, port)
		if err == nil {
			if err = echoes(conn, "first"); err == nil {
				return conn
			}
			_ = conn.Close()
		}
		need(t, time.Now().Before(deadline), "no line came back through Dial within %s: %v", pollTimeout, err)
		time.Sleep(50 * time.Millisecond)
	}
}

// echoes writes one line and reads it back.
func echoes(conn net.Conn, line string) error {
	if err := conn.SetDeadline(time.Now().Add(dialRoundTrip)); err != nil {
		return err
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		return err
	}
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSuffix(got, "\n") != line {
		return &echoMismatch{sent: line, got: got}
	}
	return nil
}

// echoMismatch is a line that came back as something else.
type echoMismatch struct{ sent, got string }

func (e *echoMismatch) Error() string {
	return "sent " + e.sent + " and read back " + strings.TrimSpace(e.got)
}

// closedPort is a loopback port nothing listened on a moment ago, for a
// port the probe reads closed. A port a sandbox's server binds is reserved
// instead (Options.Listen), so no other process the machine runs takes it
// before the server binds it. This one is not: a reserved port that nothing
// listens on is dropped unanswered by macOS rather than refused, and the
// probe would read it closed only once its own wait ran out.
func closedPort(t tb) int {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	must(t, err, "listening on loopback")
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	need(t, ok, "a loopback listener reports %T, not a TCP address", l.Addr())
	return addr.Port
}
