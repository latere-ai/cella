// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// openPTY opens a pseudo-terminal pair: /dev/ptmx is the controlling end,
// TIOCSPTLCK unlocks the other end and TIOCGPTN names it under /dev/pts.
func openPTY() (controlling, other *os.File, err error) {
	controlling, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var unlock int32
	if err = ioctl(controlling.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		_ = controlling.Close()
		return nil, nil, err
	}
	var number uint32
	if err = ioctl(controlling.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); err != nil {
		_ = controlling.Close()
		return nil, nil, err
	}
	other, err = os.OpenFile("/dev/pts/"+strconv.Itoa(int(number)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = controlling.Close()
		return nil, nil, err
	}
	return controlling, other, nil
}
