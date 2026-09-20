// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package cellacli

import (
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"unsafe"
)

// The ioctls that read and write a terminal's mode and window. They differ
// per operating system and the build tag above names the two this command
// has them for; elsewhere OSTerminal answers that there is no terminal, and
// every command but a session works the same.
const (
	getAttributes = syscall.TIOCGETA
	setAttributes = syscall.TIOCSETA
)

// winsize is the window an ioctl reads. The field order is the kernel's
// struct winsize.
type winsize struct {
	rows, cols, xpixel, ypixel uint16
}

// osTerminal is the caller's own terminal.
type osTerminal struct {
	file *os.File
}

// OSTerminal is the terminal behind a file, or nil where the file is not
// one. A command whose input is a pipe therefore has no terminal, which is
// what makes exec on a pipe a command and not a session.
func OSTerminal(f *os.File) Terminal {
	if f == nil {
		return nil
	}
	var state syscall.Termios
	if err := ioctl(f.Fd(), getAttributes, uintptr(unsafe.Pointer(&state))); err != nil {
		return nil
	}
	return &osTerminal{file: f}
}

// Size is the window in columns and rows.
func (t *osTerminal) Size() (int, int, error) {
	var ws winsize
	if err := ioctl(t.file.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); err != nil {
		return 0, 0, err
	}
	if ws.cols == 0 || ws.rows == 0 {
		return 0, 0, errors.New("the terminal reports no window")
	}
	return int(ws.cols), int(ws.rows), nil
}

// MakeRaw puts the terminal in raw mode: every byte reaches the command
// inside, including the ones the line discipline would have interpreted. The
// returned function restores exactly what was read here.
func (t *osTerminal) MakeRaw() (func() error, error) {
	var state syscall.Termios
	if err := ioctl(t.file.Fd(), getAttributes, uintptr(unsafe.Pointer(&state))); err != nil {
		return nil, err
	}
	previous := state
	// The rule of a raw terminal: no line editing, no echo, no signal
	// characters, no input translation, and a read that returns one byte as
	// soon as it is there.
	state.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	state.Oflag &^= syscall.OPOST
	state.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	state.Cflag &^= syscall.CSIZE | syscall.PARENB
	state.Cflag |= syscall.CS8
	state.Cc[syscall.VMIN] = 1
	state.Cc[syscall.VTIME] = 0
	if err := ioctl(t.file.Fd(), setAttributes, uintptr(unsafe.Pointer(&state))); err != nil {
		return nil, err
	}
	restored := false
	return func() error {
		if restored {
			return nil
		}
		restored = true
		return ioctl(t.file.Fd(), setAttributes, uintptr(unsafe.Pointer(&previous)))
	}, nil
}

// Resized delivers one value per SIGWINCH, which is how a terminal says its
// window changed.
func (t *osTerminal) Resized() (<-chan struct{}, func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	changed := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(changed)
		for {
			select {
			case <-signals:
				select {
				case changed <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	// Ending the watch twice is the ordinary case: a session ends it on its
	// way out and again from a deferred call, and the second end does
	// nothing rather than closing a channel that is already closed.
	var once sync.Once
	return changed, func() {
		once.Do(func() {
			signal.Stop(signals)
			close(done)
		})
	}
}

// ioctl issues one request on a file descriptor.
func ioctl(fd, request, argument uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, argument); errno != 0 {
		return errno
	}
	return nil
}
