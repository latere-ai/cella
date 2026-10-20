// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// psState is a process's state letter: Z for a zombie, which has ended and
// waits to be reaped.
func psState(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			return "", err
		}
		// The state follows the command's closing parenthesis.
		_, rest, ok := strings.Cut(string(raw), ") ")
		if !ok {
			return "", fmt.Errorf("unreadable /proc/%d/stat", pid)
		}
		return rest[:1], nil
	}
	out, err := exec.Command("/bin/ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// processesRunning answers the pids, as this computer names them, of the
// processes whose whole command line is line, its words joined by single
// spaces.
func processesRunning(line string) ([]int, error) {
	var pids []int
	if runtime.GOOS == "linux" {
		dirs, err := filepath.Glob("/proc/[0-9]*/cmdline")
		if err != nil {
			return nil, err
		}
		for _, path := range dirs {
			raw, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.ESRCH) {
				// The process ended, or is another user's, since the glob.
				continue
			}
			if err != nil {
				return nil, err
			}
			args := strings.Split(string(bytes.TrimRight(raw, "\x00")), "\x00")
			if strings.Join(args, " ") != line {
				continue
			}
			pid, err := strconv.Atoi(filepath.Base(filepath.Dir(path)))
			if err != nil {
				return nil, err
			}
			pids = append(pids, pid)
		}
		return pids, nil
	}
	out, err := exec.Command("/bin/ps", "-axo", "pid=,args=").Output()
	if err != nil {
		return nil, err
	}
	for row := range strings.Lines(string(out)) {
		pid, args, ok := strings.Cut(strings.TrimSpace(row), " ")
		if !ok || strings.TrimSpace(args) != line {
			continue
		}
		n, err := strconv.Atoi(pid)
		if err != nil {
			return nil, err
		}
		pids = append(pids, n)
	}
	return pids, nil
}
