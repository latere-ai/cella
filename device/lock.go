// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Lock is the one engine's hold on this computer, an exclusive advisory
// lock on a file in the state directory. The operating system releases it
// when the process ends, however it ends, so a crash leaves no stale lock.
type Lock struct{ f *os.File }

// TakeLock takes the lock at path, or refuses with device_already_served
// while another process holds it.
func TakeLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, Fail(CodeServeFailed, "", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, Fail(CodeServeFailed, "", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, Fail(CodeServeFailed, "", err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := "another process"
		if raw, rerr := os.ReadFile(path); rerr == nil {
			if pid := strings.TrimSpace(string(raw)); pid != "" {
				holder = "process " + pid
			}
		}
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, Fail(CodeAlreadyServed, fmt.Sprintf("%s holds the lock %s", holder, path), err)
		}
		return nil, Fail(CodeServeFailed, "", err)
	}
	if err = f.Truncate(0); err == nil {
		_, err = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	if err != nil {
		_ = f.Close()
		return nil, Fail(CodeServeFailed, "", err)
	}
	return &Lock{f: f}, nil
}

// Release lets the lock go.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
