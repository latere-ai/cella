// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package cellacli_test

import (
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"latere.ai/x/cella/internal/cellacli"
)

// The pseudo-terminal below is opened the way the native driver opens one
// (runtime/native/pty*.go), because the seam under test is the ioctls
// themselves and a fake would prove nothing about them.

// window is the kernel's struct winsize.
type window struct {
	rows, cols, xpixel, ypixel uint16
}

func ioctl(t *testing.T, fd, request, argument uintptr) {
	t.Helper()
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, argument); errno != 0 {
		t.Fatalf("ioctl: %v", errno)
	}
}

// TestTheCallersTerminalIsReadSetRawAndRestored drives the real seam over a
// pseudo-terminal: the window is read, raw mode clears the line discipline,
// and the restore puts back exactly what was there.
func TestTheCallersTerminalIsReadSetRawAndRestored(t *testing.T) {
	controlling, inside, err := openPTY()
	if err != nil {
		t.Skipf("this machine opens no pseudo-terminal: %v", err)
	}
	defer func() { _ = controlling.Close() }()
	defer func() { _ = inside.Close() }()

	terminal := cellacli.OSTerminal(inside)
	if terminal == nil {
		t.Fatal("a pseudo-terminal was not read as a terminal")
	}
	// A file that is no terminal is none, which is what makes exec on a pipe
	// a command rather than a session.
	plain, err := os.CreateTemp(t.TempDir(), "notaterminal")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }()
	if cellacli.OSTerminal(plain) != nil {
		t.Error("an ordinary file was read as a terminal")
	}
	if cellacli.OSTerminal(nil) != nil {
		t.Error("no file was read as a terminal")
	}

	ws := window{rows: 40, cols: 100}
	ioctl(t, controlling.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
	cols, rows, err := terminal.Size()
	if err != nil {
		t.Fatal(err)
	}
	if cols != 100 || rows != 40 {
		t.Fatalf("the window reads %dx%d, want 100x40", cols, rows)
	}

	var before syscall.Termios
	ioctl(t, inside.Fd(), attributesRequest(), uintptr(unsafe.Pointer(&before)))
	restore, err := terminal.MakeRaw()
	if err != nil {
		t.Fatal(err)
	}
	var raw syscall.Termios
	ioctl(t, inside.Fd(), attributesRequest(), uintptr(unsafe.Pointer(&raw)))
	if raw.Lflag&syscall.ICANON != 0 || raw.Lflag&syscall.ECHO != 0 || raw.Lflag&syscall.ISIG != 0 {
		t.Fatalf("raw mode left the line discipline on: %x", raw.Lflag)
	}
	if raw.Oflag&syscall.OPOST != 0 {
		t.Errorf("raw mode left output processing on: %x", raw.Oflag)
	}
	if raw.Cc[syscall.VMIN] != 1 || raw.Cc[syscall.VTIME] != 0 {
		t.Errorf("a raw read returns %d byte(s) after %d", raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME])
	}
	if err = restore(); err != nil {
		t.Fatal(err)
	}
	var after syscall.Termios
	ioctl(t, inside.Fd(), attributesRequest(), uintptr(unsafe.Pointer(&after)))
	if after.Lflag != before.Lflag || after.Iflag != before.Iflag || after.Oflag != before.Oflag {
		t.Fatalf("the restored terminal is %x %x %x and it was %x %x %x",
			after.Iflag, after.Oflag, after.Lflag, before.Iflag, before.Oflag, before.Lflag)
	}
	// Restoring twice is the ordinary case: a session restores on its way
	// out and again from a deferred call.
	if err = restore(); err != nil {
		t.Fatal(err)
	}
}

// TestAWindowChangeReachesTheWatch: SIGWINCH is what a terminal sends when
// its window changed, and the watch ends when the session does.
func TestAWindowChangeReachesTheWatch(t *testing.T) {
	controlling, inside, err := openPTY()
	if err != nil {
		t.Skipf("this machine opens no pseudo-terminal: %v", err)
	}
	defer func() { _ = controlling.Close() }()
	defer func() { _ = inside.Close() }()
	terminal := cellacli.OSTerminal(inside)
	if terminal == nil {
		t.Fatal("a pseudo-terminal was not read as a terminal")
	}
	changed, stop := terminal.Resized()
	defer stop()
	if err = syscall.Kill(os.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("a window change reached no watch")
	}
	stop()
	// The watch is ended once and a second end is the deferred one.
}
