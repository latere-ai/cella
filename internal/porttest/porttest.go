// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package porttest holds a loopback TCP port for a test from the moment the
// test picks it until the test ends.
//
// A port picked by listening on port 0 and closing the listener is free only
// at that moment. Another process, or the local end of an outgoing
// connection, can take it before the process the test hands it to binds it,
// and that bind then fails with the address in use.
//
// Reserve binds a socket on a port the kernel picks and never listens on it.
// While it is held, the kernel gives the port to no other bind of port 0 and
// to no outgoing connection, and a bind of it on loopback is refused (on
// Linux, a bind on every address is). The one bind it admits is one that
// sets SO_REUSEPORT, as Listen does, so a server the test starts can bind and
// serve the reserved port.
//
// A reserved port is not a closed one. Where no server listens on it, Linux
// refuses a dial and macOS drops it unanswered, so a test that wants a dial
// refused does not reserve the port it dials.
package porttest

import (
	"context"
	"net"
	"strconv"
)

// T is the part of testing.TB that Reserve uses.
type T interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// Reserve holds a loopback port the kernel picks until the test ends, and
// returns it.
func Reserve(t T) int {
	t.Helper()
	port, release, err := reserve()
	if err != nil {
		// A T other than *testing.T may return from Fatalf.
		t.Fatalf("reserving a loopback port: %v", err)
		return 0
	}
	t.Cleanup(release)
	return port
}

// Listen binds the loopback port with SO_REUSEPORT and listens on it, which
// a reserved port admits.
func Listen(ctx context.Context, port int) (net.Listener, error) {
	lc := net.ListenConfig{Control: reusePort}
	return lc.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}
