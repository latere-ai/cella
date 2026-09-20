// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli_test

import (
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// attributesRequest is the ioctl that reads a terminal's mode on this
// operating system.
func attributesRequest() uintptr { return syscall.TCGETS }

// openPTY opens a pseudo-terminal pair: /dev/ptmx is the controlling end,
// TIOCSPTLCK unlocks the other end and TIOCGPTN names it under /dev/pts. It
// is the native driver's own sequence (runtime/native/pty_linux.go).
func openPTY() (controlling, inside *os.File, err error) {
	controlling, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	call := func(request uintptr, argument uintptr) error {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, controlling.Fd(), request, argument); errno != 0 {
			return errno
		}
		return nil
	}
	var unlock int32
	if err = call(syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		_ = controlling.Close()
		return nil, nil, err
	}
	var number uint32
	if err = call(syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); err != nil {
		_ = controlling.Close()
		return nil, nil, err
	}
	inside, err = os.OpenFile("/dev/pts/"+strconv.Itoa(int(number)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = controlling.Close()
		return nil, nil, err
	}
	return controlling, inside, nil
}
