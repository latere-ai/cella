// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin && !linux

package porttest

import (
	"errors"
	"syscall"
)

// reserve needs SO_REUSEPORT and a socket that can be bound without
// listening, which this package uses on Linux and macOS alone.
func reserve() (int, func(), error) {
	return 0, nil, errors.ErrUnsupported
}

func reusePort(_, _ string, _ syscall.RawConn) error {
	return errors.ErrUnsupported
}
