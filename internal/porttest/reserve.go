// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package porttest

import (
	"fmt"
	"syscall"
)

// reserve binds a socket that sets SO_REUSEPORT to 127.0.0.1 on a port the
// kernel picks, and leaves it bound without listening. A bind of the same
// port admits only another socket that sets SO_REUSEPORT. A socket that
// never listens is in no listening table, so every connection to the port
// reaches the listener that bound it beside the reservation, or is refused
// where there is none.
func reserve() (int, func(), error) {
	// The descriptor is made close-on-exec under the fork lock, as the net
	// package makes its own, so a process the test starts meanwhile does not
	// inherit the reservation and hold it past the test.
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return 0, nil, fmt.Errorf("socket: %w", err)
	}
	fail := func(step string, err error) (int, func(), error) {
		_ = syscall.Close(fd)
		return 0, nil, fmt.Errorf("%s: %w", step, err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1); err != nil {
		return fail("SO_REUSEPORT", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		return fail("bind", err)
	}
	name, err := syscall.Getsockname(fd)
	if err != nil {
		return fail("getsockname", err)
	}
	inet, ok := name.(*syscall.SockaddrInet4)
	if !ok {
		return fail("getsockname", fmt.Errorf("the socket is bound to a %T", name))
	}
	return inet.Port, func() { _ = syscall.Close(fd) }, nil
}

// reusePort sets SO_REUSEPORT on a socket before the net package binds it.
func reusePort(_, _ string, conn syscall.RawConn) error {
	var set error
	if err := conn.Control(func(fd uintptr) {
		set = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return set
}
