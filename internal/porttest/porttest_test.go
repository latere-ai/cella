// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package porttest_test

import (
	"bufio"
	"errors"
	"net"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"latere.ai/x/cella/internal/porttest"
)

// TestAReservedPortIsHeld: while a port is reserved, a listener of the kind
// any other process opens is refused the bind on loopback, and on Linux on
// every address, and a dial connects to nothing.
func TestAReservedPortIsHeld(t *testing.T) {
	port := porttest.Reserve(t)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	binds := []string{address}
	if runtime.GOOS == "linux" {
		binds = append(binds, net.JoinHostPort("", strconv.Itoa(port)))
	}
	for _, bind := range binds {
		ln, err := net.Listen("tcp", bind)
		if err == nil {
			_ = ln.Close()
			t.Fatalf("a plain listener bound %s while the port was reserved", bind)
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("a plain listener on %s failed with %v, want the address in use", bind, err)
		}
	}
	answersNothing(t, address)
}

// answersNothing dials an address nothing listens on: Linux refuses the
// dial, and macOS drops it, so the wait there is short.
func answersNothing(t *testing.T, address string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("a dial of %s connected, with nothing listening", address)
	}
	if runtime.GOOS == "linux" && !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("a dial of %s failed with %v, want a refusal", address, err)
	}
}

// TestListenServesAReservedPort: Listen binds a reserved port beside the
// reservation and receives every connection to it, and once it closes
// nothing answers the port and the reservation still holds it.
func TestListenServesAReservedPort(t *testing.T) {
	port := porttest.Reserve(t)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ln, err := porttest.Listen(t.Context(), port)
	if err != nil {
		t.Fatalf("Listen on a reserved port: %v", err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, err := bufio.NewReader(conn).ReadString('\n')
			if err == nil {
				_, _ = conn.Write([]byte(line))
			}
			_ = conn.Close()
		}
	}()
	for n := range 20 {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			t.Fatalf("dial %d of the served port: %v", n, err)
		}
		if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte("hello\n")); err != nil {
			t.Fatal(err)
		}
		got, err := bufio.NewReader(conn).ReadString('\n')
		_ = conn.Close()
		if err != nil || got != "hello\n" {
			t.Fatalf("dial %d read back %q, %v: a connection reached something other than the listener", n, got, err)
		}
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	<-served
	answersNothing(t, address)
	if ln, err := net.Listen("tcp", address); err == nil {
		_ = ln.Close()
		t.Fatal("a plain listener bound the port after Listen's listener closed, while the reservation held it")
	}
}

// TestReservedPortsAreDistinct: each reservation holds a port of its own.
func TestReservedPortsAreDistinct(t *testing.T) {
	seen := map[int]bool{}
	for range 50 {
		port := porttest.Reserve(t)
		if seen[port] {
			t.Fatalf("port %d was reserved twice", port)
		}
		seen[port] = true
	}
}
