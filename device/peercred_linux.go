// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID is the user id of the process on the other end of a Unix socket,
// which Linux reports through SO_PEERCRED.
func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		cred *unix.Ucred
		cerr error
	)
	if err = raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if cerr != nil {
		return 0, cerr
	}
	return int(cred.Uid), nil
}
