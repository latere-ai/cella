// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package porttest

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
)

// errStep is the failure a replaced call returns.
var errStep = errors.New("the step failed")

// TestAFailedStepIsReportedAndClosesTheSocket: whichever call after the
// socket fails, reserve names it, returns no port, and closes the socket it
// opened.
func TestAFailedStepIsReportedAndClosesTheSocket(t *testing.T) {
	for _, tc := range []struct {
		step string
		fail func()
	}{
		{"SO_REUSEPORT", func() {
			setsockopt = func(int, int, int, int) error { return errStep }
		}},
		{"bind", func() {
			bind = func(int, syscall.Sockaddr) error { return errStep }
		}},
		{"getsockname", func() {
			getsockname = func(int) (syscall.Sockaddr, error) { return nil, errStep }
		}},
		{"getsockname", func() {
			getsockname = func(int) (syscall.Sockaddr, error) { return &syscall.SockaddrInet6{}, nil }
		}},
	} {
		t.Run(tc.step, func(t *testing.T) {
			restore(t)
			var opened, closed []int
			socket = func(domain, typ, proto int) (int, error) {
				fd, err := syscall.Socket(domain, typ, proto)
				opened = append(opened, fd)
				return fd, err
			}
			closeSocket = func(fd int) error {
				closed = append(closed, fd)
				return syscall.Close(fd)
			}
			tc.fail()
			port, release, err := reserve()
			if err == nil || !strings.HasPrefix(err.Error(), tc.step+": ") {
				t.Fatalf("reserve answered %d, %v; want the error of %s", port, err, tc.step)
			}
			if port != 0 || release != nil {
				t.Fatalf("a failed reserve returned port %d and a release", port)
			}
			if fmt.Sprint(closed) != fmt.Sprint(opened) {
				t.Fatalf("the socket %v was opened and %v closed", opened, closed)
			}
		})
	}
}

// TestASocketThatCannotBeOpenedIsReported: Reserve reports the failure
// through the test and returns no port.
func TestASocketThatCannotBeOpenedIsReported(t *testing.T) {
	restore(t)
	socket = func(int, int, int) (int, error) { return -1, errStep }
	if _, _, err := reserve(); !errors.Is(err, errStep) || !strings.HasPrefix(err.Error(), "socket: ") {
		t.Fatalf("reserve answered %v", err)
	}
	r := &recorder{}
	if port := Reserve(r); port != 0 || !strings.Contains(r.fatal, "the step failed") || r.cleanups != 0 {
		t.Fatalf("Reserve answered %d, reported %q, and registered %d cleanups", port, r.fatal, r.cleanups)
	}
}

// TestASocketOptionThatCannotBeSetFailsTheListen: Listen binds nothing when
// the option cannot be set on the socket.
func TestASocketOptionThatCannotBeSetFailsTheListen(t *testing.T) {
	if err := reusePort("tcp", "127.0.0.1:0", refusingConn{}); !errors.Is(err, errStep) {
		t.Fatalf("reusePort over a socket that refuses the call answered %v", err)
	}
}

// restore puts every replaced call back when the test ends.
func restore(t *testing.T) {
	t.Helper()
	s, o, b, g, c := socket, setsockopt, bind, getsockname, closeSocket
	t.Cleanup(func() { socket, setsockopt, bind, getsockname, closeSocket = s, o, b, g, c })
}

// recorder is a T whose Fatalf returns, as a suite's own recorder's does.
type recorder struct {
	fatal    string
	cleanups int
}

func (*recorder) Helper() {}

func (r *recorder) Fatalf(format string, args ...any) { r.fatal = fmt.Sprintf(format, args...) }

func (r *recorder) Cleanup(func()) { r.cleanups++ }

// refusingConn is a raw connection whose every call fails.
type refusingConn struct{}

func (refusingConn) Control(func(uintptr)) error    { return errStep }
func (refusingConn) Read(func(uintptr) bool) error  { return errStep }
func (refusingConn) Write(func(uintptr) bool) error { return errStep }
