// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"
)

// openPTY opens a pseudo-terminal pair: /dev/ptmx is the controlling end,
// TIOCPTYGRANT and TIOCPTYUNLK release the other end and TIOCPTYGNAME names
// it. The name buffer's length is encoded in the request itself.
func openPTY() (controlling, other *os.File, err error) {
	controlling, err = os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*os.File, *os.File, error) {
		_ = controlling.Close()
		return nil, nil, err
	}
	if err = ioctl(controlling.Fd(), syscall.TIOCPTYGRANT, 0); err != nil {
		return fail(err)
	}
	if err = ioctl(controlling.Fd(), syscall.TIOCPTYUNLK, 0); err != nil {
		return fail(err)
	}
	name := make([]byte, (syscall.TIOCPTYGNAME>>16)&0x1fff)
	if err = ioctl(controlling.Fd(), syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		return fail(err)
	}
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	other, err = os.OpenFile(string(name), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return fail(err)
	}
	return controlling, other, nil
}
