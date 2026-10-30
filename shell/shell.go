// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package shell runs a POSIX shell command line in the calling process,
// over a file system the caller serves, when every program the line names
// is one the package provides and every path it names is in a directory the
// file system serves.
//
// A program that keeps files somewhere other than a disk, such as a files
// service, serves them here as an FS under one or more Mounts. A command
// line such as `cd /notes && ls -l && grep -rn todo .` then runs with no
// machine at all: the interpreter is mvdan.cc/sh, the programs are this
// package's own, and every file operation is a call of the FS.
//
// Check reads a line before anything runs and answers whether it can run
// here, and why not: a program the package does not provide, a path no
// mount serves, an expansion whose value is known only once something has
// run, or a construct the package does not run. A caller that gets a
// refusal runs the line where it would have run otherwise; Run never starts
// a line Check refuses, so a line never runs half here and half elsewhere.
//
// The language is POSIX sh as dash, the /bin/sh of Debian and Ubuntu, runs
// it: echo interprets backslash escapes, and the shell's own messages read
// as dash writes them. The programs are ls, cat, head, tail, wc, grep, find,
// sort, uniq, cut, mkdir, rm, mv, cp and touch, with the options models and
// people commonly use, the output, messages and exit codes of GNU coreutils,
// grep and findutils, and names ordered as en_US.UTF-8 orders them. The
// builtins are cd, pwd, echo, printf, test and [, true, false, :, exit,
// set, read, break and continue.
//
// Nothing reaches the host the program runs on: no program is executed, no
// file of the host is opened, and the environment is the package's own, so
// no variable of the calling process can be printed.
package shell

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Mount is a directory the file system serves, with everything under it.
// A path is served when it is a mount's directory or under one; the
// longest mount that holds a path decides whether it is read only.
type Mount struct {
	// Dir is an absolute, clean directory.
	Dir string
	// ReadOnly refuses every change under Dir, before the file system is
	// called, as a read-only file system does.
	ReadOnly bool
}

// FS is the file system a command line runs over. Every name is absolute,
// clean and served by a mount. An error that is, or wraps, fs.ErrNotExist,
// fs.ErrExist, fs.ErrPermission or a syscall.Errno is printed as the
// system's own text for it, and an *Explained error with its words.
type FS interface {
	// ReadFile is a file's whole content.
	ReadFile(ctx context.Context, name string) ([]byte, error)
	// WriteFile replaces a file's content, creating the file when it is
	// absent. The parent directory exists.
	WriteFile(ctx context.Context, name string, data []byte) error
	// Stat describes a file or a directory.
	Stat(ctx context.Context, name string) (fs.FileInfo, error)
	// ReadDir lists the entries directly in a directory.
	ReadDir(ctx context.Context, name string) ([]fs.FileInfo, error)
	// Remove removes a file.
	Remove(ctx context.Context, name string) error
}

// Renamer is the optional interface of a file system that moves a file in
// one step. Without it, mv writes the file at its new path and then
// removes the old one.
type Renamer interface {
	Rename(ctx context.Context, from, to string) error
}

// DirMaker is the optional interface of a file system that keeps empty
// directories. Without it a directory exists while a file is under it,
// and a directory mkdir makes exists for the rest of the command line.
type DirMaker interface {
	Mkdir(ctx context.Context, name string) error
}

// Explained is an error a file system answers with words of its own,
// which a program prints after the path in the place of the system's text,
// such as why a write was refused.
type Explained struct {
	Words string
	Err   error
}

func (e *Explained) Error() string { return e.Words }

func (e *Explained) Unwrap() error { return e.Err }

// DefaultMaxOutput is the most output Run keeps of one command line, the
// bound a machine's own command has; what is written past it is dropped.
const DefaultMaxOutput = 64 << 20

// Request is one command line to run.
type Request struct {
	// Command is the line, as `/bin/sh -c` would be given it.
	Command string
	// Dir is the directory the line starts in. Empty, or a directory no
	// mount serves, is a directory the line may not use: Check refuses a
	// line that names a relative path while it is there.
	Dir string
	// Name is $0, which the shell's own messages begin with; "sh" when
	// empty.
	Name string
	// Line is the number the shell's messages give the command's first
	// line, for a caller whose shell runs lines of its own before the
	// command; 1 when zero.
	Line int
	// Owner is the owner and group ls -l prints; "root" when empty.
	Owner string
	// Now is the time ls -l tells a recent file by; the current time when
	// zero.
	Now time.Time
	// MaxOutput bounds the output kept; DefaultMaxOutput when zero.
	MaxOutput int
}

// Result is a command line that ran.
type Result struct {
	// Output is standard output and standard error, in the order written.
	Output []byte
	// ExitCode is the line's exit status.
	ExitCode int
	// Dir is the shell's directory when the line ended: the request's when
	// the line changed none, and empty when that was empty or unserved and
	// the line did not leave it.
	Dir string
	// Truncated is set when output past MaxOutput was dropped.
	Truncated bool
}

// Declined is the error of Run for a line Check refuses: nothing ran.
type Declined struct {
	Plan Plan
}

func (d *Declined) Error() string {
	if d.Plan.Program != "" {
		return fmt.Sprintf("shell: the command line does not run here: %s (%s)", d.Plan.Reason, d.Plan.Program)
	}
	return "shell: the command line does not run here: " + string(d.Plan.Reason)
}

// placeholder is the directory the interpreter starts in when the line
// starts in one it may not use: Check refuses every use of it, and no
// mount serves "/" alone, since a mount is never the root.
const placeholder = "/"

// errorText is the text glibc's strerror gives the errors a program
// prints, which GNU's programs and dash print after a path.
func errorText(err error) string {
	var ex *Explained
	if errors.As(err, &ex) {
		return ex.Words
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if t, ok := errnoText[errno]; ok {
			return t
		}
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errnoText[syscall.ENOENT]
	case errors.Is(err, fs.ErrExist):
		return errnoText[syscall.EEXIST]
	case errors.Is(err, fs.ErrPermission):
		return errnoText[syscall.EACCES]
	}
	return errnoText[syscall.EIO]
}

var errnoText = map[syscall.Errno]string{
	syscall.ENOENT:    "No such file or directory",
	syscall.EEXIST:    "File exists",
	syscall.EACCES:    "Permission denied",
	syscall.EPERM:     "Operation not permitted",
	syscall.EROFS:     "Read-only file system",
	syscall.EISDIR:    "Is a directory",
	syscall.ENOTDIR:   "Not a directory",
	syscall.ENOTEMPTY: "Directory not empty",
	syscall.EBUSY:     "Device or resource busy",
	syscall.EFBIG:     "File too large",
	syscall.ENOSPC:    "No space left on device",
	syscall.EINVAL:    "Invalid argument",
	syscall.EIO:       "Input/output error",
}

// output is the line's standard output and standard error in one buffer,
// written by the stages of a pipe at once, bounded by max.
type output struct {
	mu        sync.Mutex
	buf       []byte
	max       int
	truncated bool
}

// appendf writes a message of the shell's own.
func (o *output) appendf(format string, args ...any) {
	o.add(fmt.Appendf(nil, format, args...))
}

// Write never fails: what is written past the bound is dropped and
// recorded.
func (o *output) Write(p []byte) (int, error) {
	o.add(p)
	return len(p), nil
}

func (o *output) add(p []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	room := o.max - len(o.buf)
	if room < len(p) {
		o.truncated = true
		if room > 0 {
			o.buf = append(o.buf, p[:room]...)
		}
		return
	}
	o.buf = append(o.buf, p...)
}

// clean is p made absolute against dir and cleaned; a relative p against
// an empty dir stays relative, which no mount serves.
func clean(dir, p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	if dir == "" {
		return p
	}
	return path.Join(dir, p)
}

// within reports whether p is dir or under it.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// mountOf is the longest mount holding p, false for none.
func mountOf(mounts []Mount, p string) (Mount, bool) {
	var best Mount
	found := false
	for _, m := range mounts {
		if within(p, m.Dir) && (!found || len(m.Dir) > len(best.Dir)) {
			best, found = m, true
		}
	}
	return best, found
}

// serves reports whether a mount holds p, an absolute clean path.
func serves(mounts []Mount, p string) bool {
	if !path.IsAbs(p) {
		return false
	}
	_, ok := mountOf(mounts, path.Clean(p))
	return ok
}

// checkMounts is the mounts made clean, an error for one that is not an
// absolute directory below the root.
func checkMounts(mounts []Mount) ([]Mount, error) {
	out := make([]Mount, 0, len(mounts))
	for _, m := range mounts {
		if !path.IsAbs(m.Dir) || path.Clean(m.Dir) == "/" {
			return nil, fmt.Errorf("shell: the mount %q is not an absolute directory below the root", m.Dir)
		}
		out = append(out, Mount{Dir: path.Clean(m.Dir), ReadOnly: m.ReadOnly})
	}
	return out, nil
}
