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
prints, the time `ls -l` tells a recent file by, the most output
kept (64 MiB by default, a machine's bound), and the `Limits` the line
runs under (below). `Result` is the output,
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
| `size` | a line past the sizes the package runs (below): longer than 64 KiB, nested deeper than 64, a command of more than 16 expansions, more than 32 pipes, or a pattern of more than 8 wildcards |

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
`false`, `:`, `exit`, `set` (`-e`, `-u`, `+x`, and `-o` with `errexit`
and `nounset`), `read` (`-r`), `break` and `continue`. The trace, `set
-x`, is refused: it would print the commands the bounds below add to a
line. `if`,
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

### What a line takes of the process

A line runs inside the caller's process, which serves other lines and
other callers at the same time, so what a line can take of it is
bounded whatever the line holds. A sandbox bounds a line by its own
memory and CPU; here the bounds are the package's. A line that reaches
one stops where it is, with a message in the shell's form (`$0: LINE:
the line ran more than 50000 steps`) and status 2, and `Run` returns no
error: the line ran, and what it changed before the bound stays
changed. The command that reached the bound writes nothing: a
redirection's file is written when its command is done, and a stopped
command is not done. It is never handed elsewhere halfway, for the reason a line
never moves to a sandbox halfway. A caller whose context ends still gets
the context's error.

Before anything runs, `Check` refuses a line past these sizes as
`size`; they bound what the parser and the interpreter take before any
other bound is checked:

| Size | Bound | Why |
|---|---|---|
| `MaxLine` | 64 KiB | everything below grows with the line |
| `MaxDepth` | 64 | the parser's stack grows with nesting: 50 000 nested `$((` take 256 MB of stack, 50 000 `(` 66 MB, a 64 KiB line of nested `if` 8 MB. Brackets, `if`, `while`, `until`, `for`, `case` and the `&&`, `||` and `\|` of one list are counted on the text before it is parsed, and compound commands again on the parsed line |
| `MaxExpansions` | 16 | one command's words are made in full before any bound sees them: 16 expansions of a value at the value bound is 4 MiB. Counted over a command's words, assignments, redirections and here documents, a `for` list, and a `case` clause's word and patterns together; a word with an unquoted wildcard counts as one |
| `MaxPipes` | 32 | every pipe, here document and `<` is a pipe of the process while its command runs |
| `MaxStars` | 8 | a pattern is matched as a regular expression, whose cost is its wildcards times the length of what it is matched against: a pattern of 1 000 `*` takes 4 s over a 256 KiB value. Counted in every word the shell globs, every `case` pattern, and the pattern of `${x#p}`, `${x##p}`, `${x%p}` and `${x%%p}`, here documents' expansions among them |

While a line runs, `Limits` bound it; a caller passes its own or takes
the defaults:

| Limit | Default | What it bounds |
|---|---|---|
| `Steps` | 50 000 | statements, commands, calls and loop turns. The interpreter asks its context before each, so the count is taken there, and a loop that calls nothing, `while x=1; do y=2; done`, is counted too. `while :; do :; done` stops in about 7 ms |
| `Time` | 10 s | the line's own deadline, apart from the caller's context, so a line stuck on a slow file system stops too |
| `Value` | 256 KiB | one variable's value and one word of a command |
| `Values` | 1 MiB | the line's variables together, and one command's words together |
| `Input` | 16 MiB | what the line's programs hold at once: their standard input, the files `sort` sorts, the output a program keeps until it is done, a redirection's file until its command is done, one pass of a `printf` format, and one line through a pipe or a `<`, which whatever reads it holds whole |
| `Read` | 64 MiB | the bytes of files the line reads |
| `Names` | 50 000 | the directory entries the line lists, by its patterns and its programs |

The words of every command are checked before it runs, and the
variables with them. An assignment alone calls no command, and `read`
sets its variables after its call, so `Run` follows each statement that
only assigns, and each `read`, with a check of its own, which answers
the status of the statement before it, so `while read` and `read ||`
read as they do in dash; that is why the trace is refused. A negated
`read` is not followed, since under `set -e` the check would end the
line on the status the negation turned; the next command checks its
value. `printf` runs as one of the package's programs rather than the
interpreter's builtin, so its output is counted like any program's.
`read` takes a line byte by byte, about a megabyte a second, so a line
read whole into a variable is bounded by `Input` through the pipe or
the `<`, and by `Time`.

`Time` and `Steps` are taken between statements; one statement runs to
its end. The sizes bound what one statement can cost: the worst found
is a `case` whose word holds 8 expansions at the value bound and whose
8 patterns hold 8 wildcards each, about 2.5 s, so a line ends at most a
few seconds past `Time`.

The defaults are sized for folders of notes: a line over a folder of
2 000 files of up to a megabyte each lists, reads and sorts them well
inside every bound. `x=a; for i in
1 … 25; do x=$x$x; done` stops at `x` past 256 KiB, having allocated
under 2 MB; without the bounds it allocated 110 MB in 17 ms.

The alternatives considered:

- **Send loops to a sandbox.** `Check` could refuse every loop, so the
  only unbounded construct runs where it always did. It bounds nothing
  else: one command's expansions, a pipe into `sort`, a pattern over a
  large folder and a nested line take memory without a loop, and a
  `for` over a folder's files is the commonest loop a line over notes
  has. The bounds cover all of them and keep the loop.
- **Only the caller's deadline.** A context bounds time, not memory:
  the doubling above reaches 110 MB long before any tool timeout.
- **A memory limit on the process.** Go has a soft limit for the whole
  process, not for one line, and a line that reaches it slows every
  other line instead of stopping.
- **Counting steps in the call handler alone.** A loop that only
  assigns calls no handler; the interpreter's context is asked at every
  statement and loop turn, so the count is taken there.

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
- A line stops on a bound of the section above where a sandbox would
  have run on until its memory or the tool's timeout ran out; `set -x`
  is refused.

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
9. Each of these stops within a few seconds, with the bound's message,
   status 2 and a bounded allocation: a value doubled by assignment,
   straight and in a loop that calls nothing; a value doubled by `read`
   from a here document, and a long line read through a pipe and a `<`;
   a pipe into `sort` and into `uniq`; an endless loop, and one past the
   time bound; a pattern over a folder of 2 000 files in a loop; a
   redirection that grows; words and variables past their bounds; and a
   `printf` of wide fields. A line nested past `MaxDepth`, wider than
   `MaxExpansions`, with a pattern past `MaxStars` or past `MaxLine` is
   refused as `size` in under a second. A `read` keeps its status in
   `while`, `||`, `!` and under `set -e`.
