// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package fileshell is what the container drivers run inside a sandbox for
// the file operations of design 033: one shell program per operation, the
// exit codes they report a refusal with, and the parser of what they print.
//
// The programs are shared because the two container drivers differ only in
// how they carry an argv and a stream into the sandbox. Three properties make
// them safe to run on a name the caller chose:
//
//   - Every name reaches the program as a positional parameter and is never
//     interpolated into the program text.
//   - A record is terminated by NUL, because a filename may hold a newline, a
//     pipe or a carriage return and may not hold NUL.
//   - Every refusal is a fixed exit code, so the driver answers with the
//     contract's error rather than with whatever the utility printed.
package fileshell

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// The exit codes the programs refuse with. Anything else is the operation's
// own failure and reaches the caller as the driver's error.
const (
	// ExitOutside is a path that leaves the workspace, lexically or through a
	// symbolic link.
	ExitOutside = 10
	// ExitAbsent is a path that names nothing.
	ExitAbsent = 11
	// ExitKind is a path that is the wrong kind for the operation: a listing
	// of a file, a move onto a directory, a directory where a file goes.
	ExitKind = 12
	// ExitTooLarge is a body past the bound the write carried.
	ExitTooLarge = 13
)

// Shell is the interpreter every program is handed to: POSIX sh. Beyond its
// builtins the programs use realpath, stat, cat, head, wc, chmod, mkdir, mv
// and rm, which both GNU coreutils and BusyBox carry, and an image without
// one of them cannot serve the file operations. Every name but realpath's
// rides after "--"; BusyBox's realpath has no such separator, and the paths
// it is handed are absolute, so no name of one can read as an option.
const Shell = "/bin/sh"

// preamble is what every program starts with: the record printer, the
// containment rule, and the resolved workspace root the rule is against.
//
// contain takes the root, the path, and whether the last component is
// followed. An operation that reads through a name follows it; one that
// replaces or drops the name does not, because a link is the caller's own
// name and what it points at is not what the operation touches. The probe
// walks up to the deepest ancestor that exists, because a write names a file
// that is not there yet.
const preamble = `stat_record() {
  name="$1"
  shift
  metadata=$(stat -c '%a|%F|%s|%Y' "$@" -- "$name") || exit 11
  printf '%s|%s\000' "$metadata" "$name"
}
contain() {
  probe="$2"
  [ "$3" = 1 ] || probe="${probe%/*}"
  [ -n "$probe" ] || probe=/
  while [ ! -e "$probe" ] && [ ! -L "$probe" ]; do
    next="${probe%/*}"
    [ -n "$next" ] || next=/
    if [ "$next" = "$probe" ]; then break; fi
    probe="$next"
  done
  real=$(realpath "$probe") || exit 10
  case "$real" in
    "$1"|"$1"/*) ;;
    *) exit 10 ;;
  esac
}
root=$(realpath "$1") || exit 10
`

// The programs. Each reads the workspace root as $1 and its own arguments
// after it.
const (
	statProgram = preamble + `contain "$root" "$2" 1
[ -e "$2" ] || exit 11
stat_record "$2" -L`

	readDirProgram = preamble + `contain "$root" "$2" 1
[ -e "$2" ] || exit 11
[ -d "$2" ] || exit 12
cd "$2" || exit 11
for f in * .[!.]* ..?*; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  stat_record "$f"
done`

	catProgram = preamble + `contain "$root" "$2" 1
[ -e "$2" ] || exit 11
if [ -d "$2" ]; then exit 12; fi
cat -- "$2"`

	mkdirProgram = preamble + `contain "$root" "$2" 0
if [ -e "$2" ] && [ ! -d "$2" ]; then exit 12; fi
mkdir -p -- "$2" || exit 12`

	removeProgram = preamble + `contain "$root" "$2" 0
rm -rf -- "$2"`

	moveProgram = preamble + `contain "$root" "$2" 0
contain "$root" "$3" 0
if [ ! -e "$2" ] && [ ! -L "$2" ]; then exit 11; fi
if [ -d "$3" ]; then exit 12; fi
parent="${3%/*}"
[ -n "$parent" ] || parent=/
mkdir -p -- "$parent" || exit 12
mv -f -- "$2" "$3"`

	// writeBodyProgram stages the body beside its destination and prints the
	// bytes it holds. Nothing is committed here: the caller's own copy may
	// still fail, and a body that ended early must leave the previous file
	// where it is. $2 is the staged name, $3 one byte past the bound or 0,
	// and $4 the bound or 0.
	writeBodyProgram = preamble + `contain "$root" "$2" 0
parent="${2%/*}"
[ -n "$parent" ] || parent=/
mkdir -p -- "$parent" || exit 12
if [ "$3" -gt 0 ]; then head -c "$3" > "$2"; else cat > "$2"; fi
size=$(wc -c < "$2")
size=$((size))
if [ "$4" -gt 0 ] && [ "$size" -gt "$4" ]; then rm -f -- "$2"; exit 13; fi
printf '%s' "$size"`

	// commitProgram puts the staged file in place under its mode. $2 is the
	// staged name, $3 the destination, $4 the octal mode.
	commitProgram = preamble + `contain "$root" "$2" 0
contain "$root" "$3" 0
[ -e "$2" ] || exit 11
if [ -d "$3" ]; then rm -f -- "$2"; exit 12; fi
chmod -- "$4" "$2" || { rm -f -- "$2"; exit 1; }
mv -f -- "$2" "$3" || { rm -f -- "$2"; exit 1; }`

	discardProgram = preamble + `contain "$root" "$2" 0
rm -f -- "$2"`
)

// program renders one argv: the shell, the program, the placeholder $0, the
// workspace root, and the operation's own arguments.
func program(text, root string, args ...string) []string {
	return append([]string{Shell, "-c", text, "_", root}, args...)
}

// Stat describes one path, following the last component.
func Stat(root, path string) []string { return program(statProgram, root, path) }

// ReadDir lists the immediate entries of a directory.
func ReadDir(root, path string) []string { return program(readDirProgram, root, path) }

// Cat streams one file.
func Cat(root, path string) []string { return program(catProgram, root, path) }

// Mkdir creates a directory and the missing parents.
func Mkdir(root, path string) []string { return program(mkdirProgram, root, path) }

// Remove deletes a file or a directory tree.
func Remove(root, path string) []string { return program(removeProgram, root, path) }

// Move renames from onto the exact path to.
func Move(root, from, to string) []string { return program(moveProgram, root, from, to) }

// WriteBody stages the body it reads on standard input beside the
// destination, bounded, and prints the bytes it holds.
func WriteBody(root, staged string, max int64) []string {
	bound, past := "0", "0"
	if max > 0 {
		bound, past = strconv.FormatInt(max, 10), strconv.FormatInt(max+1, 10)
	}
	return program(writeBodyProgram, root, staged, past, bound)
}

// Commit puts the staged file in place under mode. It runs only once the
// caller's own copy of the body has finished, which is what leaves the
// previous file whole when the body does not.
func Commit(root, staged, path string, mode fs.FileMode) []string {
	return program(commitProgram, root, staged, path, strconv.FormatUint(uint64(mode.Perm()), 8))
}

// Discard drops a staged file the caller is not going to commit.
func Discard(root, staged string) []string { return program(discardProgram, root, staged) }

// Staged is where a write holds the body until it is whole: beside the
// destination, so the commit is a rename inside one directory, and under a
// name no caller can collide with.
func Staged(path, token string) string {
	dir := path[:strings.LastIndex(path, "/")+1]
	return dir + ".cella-write-" + token
}

// Classify turns one program's exit into the contract's error. A code the
// programs do not use is the operation's own failure, reported with what the
// program wrote to its error stream.
func Classify(code int, path, diagnostic string) error {
	switch code {
	case 0:
		return nil
	case ExitOutside:
		return fmt.Errorf("%w: %s leaves the workspace", driver.ErrInvalid, path)
	case ExitAbsent:
		return fmt.Errorf("%w: %s", driver.ErrNotFound, path)
	case ExitKind:
		return fmt.Errorf("%w: %s is not what this operation takes", driver.ErrInvalid, path)
	case ExitTooLarge:
		return fmt.Errorf("%w: the body is past the bound the write carried", driver.ErrTooLarge)
	}
	if diagnostic = strings.TrimSpace(diagnostic); diagnostic != "" {
		return fmt.Errorf("file operation on %s exited %d: %s", path, code, diagnostic)
	}
	return fmt.Errorf("file operation on %s exited %d", path, code)
}

// Size reads the byte count a staged write printed.
func Size(out string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the staged write reported %q, not a byte count", out)
	}
	return n, nil
}

// Parse turns the NUL-terminated records a program printed into entries. dir
// is what a listed name hangs under; for one path it is that path's own
// directory. The records carry the name last and unescaped, so a name holding
// a pipe, a newline or a percent sign arrives as it is.
func Parse(out, dir string) ([]driver.FileInfo, error) {
	if out == "" {
		return nil, nil
	}
	body, terminated := strings.CutSuffix(out, "\x00")
	if !terminated {
		return nil, errors.New("a file record was cut short")
	}
	var entries []driver.FileInfo
	for record := range strings.SplitSeq(body, "\x00") {
		parts := strings.SplitN(record, "|", 5)
		if len(parts) != 5 {
			return nil, fmt.Errorf("a file record reads %q", record)
		}
		bits, err := strconv.ParseUint(parts[0], 8, 32)
		if err != nil {
			return nil, fmt.Errorf("a file record's mode reads %q", parts[0])
		}
		size, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("a file record's size reads %q", parts[2])
		}
		epoch, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("a file record's modification time reads %q", parts[3])
		}
		name := parts[4]
		if cut := strings.LastIndex(name, "/"); cut >= 0 {
			name = name[cut+1:]
		}
		isDir := parts[1] == "directory"
		mode := fs.FileMode(bits).Perm()
		if isDir {
			mode |= fs.ModeDir
			size = 0
		}
		entries = append(entries, driver.FileInfo{
			Name:    name,
			Path:    strings.TrimSuffix(dir, "/") + "/" + name,
			Size:    size,
			Mode:    mode,
			ModTime: time.Unix(epoch, 0).UTC(),
			IsDir:   isDir,
		})
	}
	return entries, nil
}

// One reads a single path's record, which is what Stat prints.
func One(out, path string) (driver.FileInfo, error) {
	dir := path[:max(strings.LastIndex(path, "/"), 0)]
	entries, err := Parse(out, dir)
	if err != nil {
		return driver.FileInfo{}, err
	}
	if len(entries) != 1 {
		return driver.FileInfo{}, fmt.Errorf("a stat of %s printed %d records", path, len(entries))
	}
	entry := entries[0]
	entry.Path = path
	return entry, nil
}
