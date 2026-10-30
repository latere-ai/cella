# Run a command line over your own files

`latere.ai/x/cella/shell` runs a POSIX shell command line inside your
program, over files your program serves, with no sandbox. It is for a
program that keeps some files outside any sandbox, such as notes in a
files service, and gives an agent a command line to reach them:

```sh
cd /notes && ls && grep -rn todo . && cat plan.md
```

runs in a few milliseconds instead of waiting for a sandbox, and it
reads your files, which a sandbox would not see.

The package decides before anything runs. A line that names a program
the package does not provide, or a path you do not serve, is refused
with the reason, and nothing has run, so you run it in a sandbox as you
would have. A line never starts here and moves to a sandbox halfway.

## Serve your files

Implement five methods over your storage. Every name is absolute and
clean, and is under a directory you mount.

```go
type FS interface {
	ReadFile(ctx context.Context, name string) ([]byte, error)
	WriteFile(ctx context.Context, name string, data []byte) error
	Stat(ctx context.Context, name string) (fs.FileInfo, error)
	ReadDir(ctx context.Context, name string) ([]fs.FileInfo, error)
	Remove(ctx context.Context, name string) error
}
```

`shell.NewFileInfo` builds the `fs.FileInfo` values. Two methods are
optional:

| Interface | Without it |
|---|---|
| `Renamer` | `mv` writes the file at its new path, then removes the old one |
| `DirMaker` | a directory exists while a file is under it; one `mkdir` makes lasts for the rest of the line |

Return an error that wraps `fs.ErrNotExist`, `fs.ErrPermission` or a
`syscall.Errno`, and a program prints the system's text for it, such as
`No such file or directory`. Return a `*shell.Explained` to print your
own words instead, for example why a write was refused:

```go
return &shell.Explained{Words: "changed since it was last read; read it again before writing", Err: err}
```

## Mount and run

```go
mounts := []shell.Mount{
	{Dir: "/notes"},
	{Dir: "/shared", ReadOnly: true},
}

if plan := shell.Check(line, dir, mounts); !plan.OK() {
	// plan.Reason says why: "program", "path", "option", "expansion",
	// "construct", "size" or "syntax"; plan.Program names the program.
	return runInSandbox(line)
}

res, err := shell.Run(ctx, files, mounts, shell.Request{
	Command: line,
	Dir:     dir,       // where the line starts; may be a sandbox directory
	Name:    "/bin/sh", // the name the shell's messages begin with
})
// res.Output, res.ExitCode, res.Dir (where the line ended)
```

`Run` checks the line itself and answers a `*shell.Declined` for one it
refuses, so calling `Check` first is only needed to decide before you
start anything.

`dir` may be a directory you do not serve, such as a sandbox's working
directory. The line can then name no relative path until it has moved
into a mount with `cd`, and `res.Dir` is empty unless it did.

A read-only mount refuses every change with `Read-only file system`
before your file system is called.

## What a line can use

| | |
|---|---|
| Programs | `ls`, `cat`, `head`, `tail`, `wc`, `grep` (`egrep`, `fgrep`), `find`, `sort`, `uniq`, `cut`, `mkdir`, `rm`, `mv`, `cp`, `touch` |
| Builtins | `cd`, `pwd`, `echo`, `printf`, `test`, `[`, `true`, `false`, `:`, `exit`, `set`, `read`, `break`, `continue` |
| Shell | pipes, `&&`, `\|\|`, `;`, `if`, `for`, `while`, `case`, subshells, variables the line sets, arithmetic, patterns, here documents, redirections to and from files |

Each program takes the options people and models commonly use, and
prints what GNU coreutils, grep and findutils print, messages and exit
status included, in an `en_US.UTF-8` locale. The shell is dash, the
`/bin/sh` of Debian and Ubuntu: `echo` interprets backslash escapes,
and an error reads `/bin/sh: 1: cannot create x: Read-only file system`.

A line is refused when it needs anything else: another program, a path
you do not mount, a command substitution, a variable it did not set, an
option a program does not take, a background job, brace expansion, or
`set -x`. A line is refused as `size` when it is longer than 64 KiB,
nests deeper than 64, has a command with more than 16 expansions, has
more than 32 pipes, or has a pattern with more than 8 `*`.

Where the package differs from a sandbox's shell: `grep` has no
back-references, `find` and `grep -r` visit names in sorted order,
`touch` does not change an existing file's time, and `rm` and `mv`
refuse a directory a mount is at, or one that holds a mount, before
touching anything under it.

## Bounds

A line runs inside your process, so what it can take is bounded
whatever it holds. A line that reaches a bound stops where it is, with
the shell's kind of message and status 2:

```text
/bin/sh: 1: the line ran more than 50000 steps
```

`Run` answers that as a line that ran, with no error: what it changed
before the bound stays changed, and the command that reached it writes
no file. Set your own in `Request.Limits`; a
field left zero takes the default.

| Limit | Default | Bounds |
|---|---|---|
| `Steps` | 50 000 | the statements, commands and loop turns a line runs |
| `Time` | 10 s | how long a line runs, apart from your context |
| `Value` | 256 KiB | one variable's value, one word of a command |
| `Values` | 1 MiB | the line's variables together, one command's words together |
| `Input` | 16 MiB | what the line's programs hold at once: standard input, the files `sort` sorts, a program's output until it is done, a redirection's file, one line through a pipe |
| `Read` | 64 MiB | the bytes of files a line reads |
| `Names` | 50 000 | the directory entries a line lists |

Output past `Request.MaxOutput` (64 MiB by default) is dropped, and
`res.Truncated` says so. Pick it near what you show of a result.

## Safety

No program of the host runs and no file of the host is opened,
whatever the line holds: every file operation is a call of your `FS`,
for a path under a mount. The environment is the package's own, so no
variable of your process can be printed. A context that ends stops the
line between operations, and `Run` answers the context's error.
