---
title: "A shell over served files: `latere.ai/x/cella/shell` runs a POSIX command line in the calling process over a file system the caller serves, when every program it names is one the package provides and every path it names is served, decided before anything runs"
status: testing
track: core
depends_on:
  - specs/001-architecture.md
  - specs/004-runtime-contract.md
affects: [shell/, arch_test.go, docs/shell.md, docs/README.md, CHANGELOG.md]
effort: medium
created: 2026-10-30
updated: 2026-10-30
author: changkun
---

# A shell over served files

## Overview

A program that runs an agent keeps some of its files outside any
sandbox: a person's notes in a files service, a project's records in a
database. The agent reaches them with the same tool it reaches a
sandbox with, a command line, because that is the tool it knows. Today
the only place a command line runs is a sandbox, so a line as simple as

```sh
cd /notes && ls && cat plan.md
```

starts a sandbox, which takes seconds and costs one, and the sandbox
cannot see `/notes` in the first place: the files live in the
program, not on its disk.

This spec adds a package that runs such a line in the calling process,
over a file system the caller serves, with no sandbox at all. The
caller says which directories it serves and which of them are read
only. The package reads the line before anything runs and answers
whether it can run there: every program a known one, every path served.
When it can, it runs it with the output, exit status and final
directory a sandbox's `/bin/sh` would give. When it cannot, nothing has
run, and the caller runs the line where it always did. A line never
starts in the process and moves to a sandbox halfway, since what it
already changed cannot be undone.

The package knows nothing of where the files come from. It holds the
interpreter, the decision, and the programs; the caller holds the file
system.

## Design

### The package

`latere.ai/x/cella/shell`, Apache-2.0, beside `cella/device` as a
package a program outside this module embeds. Its build list is the
standard library and `mvdan.cc/sh/v3` (BSD-3-Clause), the POSIX parser
and interpreter, with the `golang.org/x/term` and `golang.org/x/sys`
packages that interpreter is built with. It reaches no network, and
`arch_test.go` holds it to that list.

```go
type Mount struct {
	Dir      string // absolute and clean, never "/"
	ReadOnly bool
}

type FS interface {
	ReadFile(ctx context.Context, name string) ([]byte, error)
	WriteFile(ctx context.Context, name string, data []byte) error
	Stat(ctx context.Context, name string) (fs.FileInfo, error)
	ReadDir(ctx context.Context, name string) ([]fs.FileInfo, error)
	Remove(ctx context.Context, name string) error
}

type Renamer interface{ Rename(ctx context.Context, from, to string) error }
type DirMaker interface{ Mkdir(ctx context.Context, name string) error }

func Check(command, dir string, mounts []Mount) Plan
func Run(ctx context.Context, fsys FS, mounts []Mount, r Request) (Result, error)
```

A path is served when it is a mount's directory or under one; the
longest mount that holds it decides whether it is read only. The file
system is only ever called with served, absolute, clean names. A read-only
mount refuses every change before the file system is called, with
`Read-only file system`, as a disk mounted read only does.

`Renamer` and `DirMaker` are optional. Without `Renamer`, `mv` writes
the file at its new path and then removes the old one. Without
`DirMaker`, a directory exists while a file is under it, as in a files
service that keeps files by path, and a directory `mkdir` makes exists
for the rest of the line.

An error the file system answers is printed as the system's own text
for it: `fs.ErrNotExist` as `No such file or directory`, a
`syscall.Errno` as glibc's `strerror`, anything else as `Input/output
error`. An `*Explained` error is printed with its own words, which is
how a caller says why a write was refused in terms the agent can act
on.

`Request` carries the line, the directory it starts in, `$0` (the name
the shell's messages begin with), the number of the line's first line
for a caller whose shell runs a line of its own first, the owner `ls -l`
prints, the time `ls -l` tells a recent file by, and the most output
kept (64 MiB by default, a machine's bound). `Result` is the output,
standard output and standard error in the order written, the exit
status, the directory the line ended in, and whether output was dropped.

### The decision

`Check` parses the line as POSIX sh and walks it, carrying the set of
directories the shell may be in at each point. A `cd` that may fail
leaves the set holding both; `&&` and `||` carry the directories of
success and failure apart, so `cd /notes && ls` lists `/notes` and
`cd /notes; ls` may list the start. A mount's directory is always there,
so a `cd` into one cannot fail. The start directory may be one no mount
serves, such as a sandbox's working directory: the line may then name
no relative path until it has moved into a mount.

It answers a `Plan` with an empty reason when the line can run, and
otherwise one of:

| Reason | When |
|---|---|
| `syntax` | the line does not parse as POSIX sh |
| `program` | a program the package does not provide, or a builtin it does not run (`eval`, `exec`, `.`, `source`, `command`, functions' calls), named in `Program` |
| `option` | an option or argument the program does not take, named in `Program` |
| `path` | a path no mount serves, or a relative path while the directory is not served |
| `expansion` | a value known only once something has run: command substitution, a variable the line does not set, `~`, `$1`, a value that may split, or a value in an option's place |
| `construct` | a construct the package does not run: brace expansion, which dash leaves as it is; a background job; a function definition; `<>`; `$'…'`; a `cd` inside a loop, a pipe or a `case` |

A program's arguments are read as the program reads them, with a
placeholder for a value known only at run time, so its paths are told
from its options and patterns. `/dev/null` is served to redirections
only. A pattern of the shell's own is a path, judged by the directory it
reads, and refused when a part after its first wildcard climbs with
`..`.

`Run` calls `Check` first and returns `*Declined` for a refused line
with nothing run.

### The interpreter

`Run` uses `mvdan.cc/sh/v3/interp` with every handler replaced: opening
a file, listing a directory, stat, access, calling a builtin, and
running a program. No program of the host is executed and no file of
the host is opened, whatever the line holds. The environment is the
package's own (`HOME`, `PATH`, `TMPDIR`, and `LC_ALL=en_US.UTF-8`), so
no variable of the calling process can be printed. A context that ends
stops the line between operations, and `Run` answers what it had with
the context's error.

The language is dash's, the `/bin/sh` of Debian and Ubuntu images:
`echo` takes only `-n` and interprets backslash escapes, `set -o
pipefail` does not exist, and the shell's own messages read as dash
writes them, `$0: LINE: …`.

The builtins are `cd`, `pwd`, `echo`, `printf`, `test` and `[`, `true`,
`false`, `:`, `exit`, `set` (`-e`, `-u`, `-x`, and `-o` with `errexit`,
`nounset`, `xtrace`), `read` (`-r`), `break` and `continue`. `if`,
`for`, `while`, `until`, `case`, `&&`, `||`, `!`, pipes, subshells and
groups run as the interpreter runs them.

### The programs

Each program has the output, messages and exit status of GNU coreutils
9.4, grep 3.11 and findutils 4.9 in an `en_US.UTF-8` locale, and orders
names and lines as glibc's collation does: letters and digits first,
case and accents set aside, then accents, then case, lower first, then
the punctuation the first levels skipped. Every program but `cat`
writes what it prints in one piece at its end, as GNU's do into a pipe,
so a program's errors come before its output.

| Program | Options |
|---|---|
| `ls` | `-1 -a -A -d -F -G -h -l -n -p -R -r -S -t -U`, `--color=never\|auto` |
| `cat` | `-A -b -e -E -n -s -t -T -u -v` |
| `head`, `tail` | `-n`, `-c` with GNU's suffixes, `-N`, `+N` for tail, `-q -v` |
| `wc` | `-c -l -L -m -w` |
| `grep`, `egrep`, `fgrep` | `-E -F -G -i -y -v -w -x -c -l -L -n -h -H -o -q -s -r -R -m -A -B -C -NUM -e -a -I`, `--include --exclude --exclude-dir --color=never\|auto` |
| `find` | `-H -L -P`, `-name -iname -path -ipath -wholename -type -maxdepth -mindepth -depth -empty -print -true -false`, `! -not -a -and -o -or ( )` |
| `sort` | `-b -f -g -h -k -m -n -o -r -s -t -u` |
| `uniq` | `-c -d -i -u`, an output operand |
| `cut` | `-b -c -f -d -s --complement --output-delimiter` |
| `mkdir` | `-m -p -v` |
| `rm` | `-d -f -r -R -v` |
| `mv` | `-f -n -v` |
| `cp` | `-a -f -n -p -r -R -v` |
| `touch` | `-a -c -m` |

Redirections `>`, `>>`, `>|`, `<`, here documents, `N>&M` and `N>&-`
run over the file system. A file a redirection writes is written once,
when its command is done.

### What differs from a sandbox's shell

Each of these is a deliberate limit, and none reaches outside the
mounts:

- `grep` patterns are Go's RE2: no back-references.
- `find` and `grep -r` visit a directory in collated order; a disk's is
  the order its directory holds, which no listing reproduces.
- `touch` leaves an existing file's time as it is: the file system
  keeps the time of its last write.
- A directory a served folder is mounted at, or one holding such a
  directory, is refused by `rm` and `mv` before anything under it is
  touched, with `Device or resource busy`. A disk refuses only the
  directory itself, after removing what is in it.
- dash gives a command whose redirection could not be opened status 2,
  the interpreter 1. The line's status is 2 when it ends on such a
  command; `$?` inside the line reads 1.
- A write the file system refuses when a redirection's command is done
  is printed with the shell's words and fails the line when it is the
  line's last command. dash would have refused it before the command
  ran, so `echo a > x && echo done` runs `echo done` here.
- `ls -l` shows a directory's size and link count as a disk does,
  4096 and two plus its subdirectories, and the owner the request
  names.

### Tested against GNU

`shell/testdata/gnu.golden` holds what `/bin/sh -c` answered, output and
exit status, for over four hundred lines over one set of files, in an
Ubuntu 24.04 image with the `en_US.UTF-8` locale, the files on an ext4
file system so sizes and blocks are a disk's, with `/memory` and one
folder mounted read only. The test runs each line here over the same
files and compares. Recording it again needs podman, a privileged
container and a free loop device:

```sh
SHELL_GOLDEN_IMAGE=<ubuntu 24.04 image with en_US.UTF-8> go test -run TestGNU ./shell/
```

A host whose kernel runs SELinux labels every file, which `ls -l`
marks with a dot after the mode; the recording leaves the mark out,
since a sandbox's file system has none.

### The environment side of a shared directory

The first caller serves files that never exist on a sandbox's disk. The
follow-on is a sandbox whose workspace is served the same way: the
files kept by a service, the sandbox's disk a cache of them, so a line
that only reads and writes files runs in the calling process and a line
that needs a program runs in a sandbox that sees the same files. The
package already decides between the two; what that needs is on Cella's
side, and it is not built here:

- A volume source that is a files service rather than an image or an
  empty disk ([[019-volumes]]), filled when a sandbox starts and written
  back as it changes.
- Who writes last when the process and a running sandbox change one
  file, and how the sandbox learns of a change it did not make.
- Whether a write back is a version the service keeps, as the first
  caller's files are, with the same If-Match.

## Acceptance criteria

1. `Check` refuses, with the reason above, each of: a program the
   package does not provide, an option a program does not take, a path
   no mount serves, a relative path while the directory is not served,
   command substitution, an unset variable, brace expansion, a
   background job, and a `cd` inside a loop; it allows
   `cd /notes && ls && cat plan.md` from a directory no mount serves.
2. `Run` never starts a line `Check` refuses, and answers `*Declined`.
3. No line reaches a file outside the mounts or a program of the host;
   `/dev/null` is a redirection's only.
4. A read-only mount refuses every change with `Read-only file system`
   and the file system is not called.
5. Every line of `gnu.golden` gives the recorded output and status.
6. A file system without `DirMaker` or `Renamer` gets writes then
   removes for `mv`, and a directory `mkdir` makes lasts for the line.
7. An `*Explained` error is printed with its words.
8. The package's build list is the standard library and the rows of
   `arch_test.go`.
