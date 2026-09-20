// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli_test

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"
)

// attributesRequest is the ioctl that reads a terminal's mode on this
// operating system.
func attributesRequest() uintptr { return syscall.TIOCGETA }

// openPTY opens a pseudo-terminal pair: /dev/ptmx is the controlling end,
// TIOCPTYGRANT and TIOCPTYUNLK release the other end and TIOCPTYGNAME names
// it. It is the native driver's own sequence (runtime/native/pty_darwin.go).
func openPTY() (controlling, inside *os.File, err error) {
	controlling, err = os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*os.File, *os.File, error) {
		_ = controlling.Close()
		return nil, nil, err
	}
	call := func(request uintptr, argument uintptr) error {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, controlling.Fd(), request, argument); errno != 0 {
			return errno
		}
		return nil
	}
	if err = call(syscall.TIOCPTYGRANT, 0); err != nil {
		return fail(err)
	}
	if err = call(syscall.TIOCPTYUNLK, 0); err != nil {
		return fail(err)
	}
	name := make([]byte, (syscall.TIOCPTYGNAME>>16)&0x1fff)
	if err = call(syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		return fail(err)
	}
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	inside, err = os.OpenFile(string(name), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return fail(err)
	}
	return controlling, inside, nil
}
