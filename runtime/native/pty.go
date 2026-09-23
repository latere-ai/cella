// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package native

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"

	driver "latere.ai/x/cella/runtime"
)

// ptySupported says whether this build can open a pseudo-terminal, which is
// what Capabilities.Attach declares. The ioctls differ per operating system
// and the two files beside this one hold them.
const ptySupported = true

// winsize is the terminal window an ioctl reads and writes. The field order is
// the kernel's struct winsize on both operating systems.
type winsize struct {
	rows, cols, xpixel, ypixel uint16
}

// ioctl issues one request on fd. A PTY pair is four ioctls and an open, so
// the driver opens it with the standard library rather than a module.
func ioctl(fd, request, arg uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, arg); errno != 0 {
		return errno
	}
	return nil
}

// setWinsize sets the window on the controlling end of a pair. The process
// inside reads the new size and receives SIGWINCH.
//
// The descriptor is held for the ioctl through the file's raw connection
// rather than read with Fd: a session that ends closes the file while a
// resize may still be on its way, and Fd reads the descriptor with nothing
// ordering it against that close. A window for a terminal that has ended is
// ErrNotRunning, which is what the session is.
func setWinsize(f *os.File, cols, rows int) error {
	if cols <= 0 || rows <= 0 || cols > 0xffff || rows > 0xffff {
		return errInvalidWindow
	}
	ws := winsize{rows: uint16(rows), cols: uint16(cols)}
	conn, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("%w: the terminal has ended: %v", driver.ErrNotRunning, err)
	}
	var set error
	if err := conn.Control(func(fd uintptr) {
		set = ioctl(fd, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
	}); err != nil {
		return fmt.Errorf("%w: the terminal has ended: %v", driver.ErrNotRunning, err)
	}
	return set
}

// ptyReader is the controlling end of a pair read as a stream. Linux reports a
// terminal whose other end has closed as EIO and darwin as the end of the
// file; both mean the session ended, and the contract names one of them.
type ptyReader struct{ f *os.File }

func (r *ptyReader) Read(p []byte) (int, error) {
	n, err := r.f.Read(p)
	if err != nil && errors.Is(err, syscall.EIO) {
		err = io.EOF
	}
	return n, err
}
func (r *ptyReader) Close() error { return r.f.Close() }
