// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// Run runs a command line Check allows over the file system, and answers
// what it wrote, its exit status and the directory it ended in. A line
// Check refuses is a *Declined error, and nothing ran. A context that ends
// stops the line between operations, and Run answers what it had with the
// context's error. A line that reaches a bound of the request's Limits
// stops where it is and ends with the bound's message and status 2, with
// no error: it ran, and what it changed before stays changed.
func Run(ctx context.Context, fsys FS, mounts []Mount, r Request) (Result, error) {
	mounts, err := checkMounts(mounts)
	if err != nil {
		return Result{}, err
	}
	if p := Check(r.Command, r.Dir, mounts); !p.OK() {
		return Result{}, &Declined{Plan: p}
	}
	file, err := parseLine(r.Command)
	if err != nil {
		return Result{}, &Declined{Plan: Plan{Reason: ReasonSyntax}}
	}
	start := placeholder
	if r.Dir != "" && serves(mounts, r.Dir) {
		start = path.Clean(r.Dir)
	}
	now := r.Now
	if now.IsZero() {
		now = time.Now()
	}
	lim := r.Limits.withDefaults()
	// The line runs under its own deadline, which a bound it reaches
	// also ends.
	runCtx, cancel := context.WithTimeout(ctx, lim.Time)
	defer cancel()
	s := &session{
		name:  r.Name,
		owner: r.Owner,
		now:   now,
		out:   &output{max: r.MaxOutput},
	}
	s.budget = &budget{lim: lim, cancel: cancel, out: s.out, line: &s.line}
	s.files = newFiles(fsys, mounts, now, s.budget)
	// The time bound stops the line the moment it passes, so nothing the
	// line writes once it has comes before the bound's message.
	deadlineSeen := make(chan struct{})
	stopAtDeadline := context.AfterFunc(runCtx, func() {
		defer close(deadlineSeen)
		if ctx.Err() == nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			s.budget.stop("the line ran longer than %s", lim.Time)
		}
	})
	if s.name == "" {
		s.name = "sh"
	}
	if s.owner == "" {
		s.owner = "root"
	}
	if s.out.max <= 0 {
		s.out.max = DefaultMaxOutput
	}
	if r.Line > 1 {
		s.offset = int64(r.Line - 1)
	}
	s.line.Store(1 + s.offset)
	runner, err := interp.New(
		// The environment is the package's own, so no variable of the
		// calling process reaches the line, and the interpreter reads no
		// home directory of the host.
		interp.Env(expand.ListEnviron("HOME=/", "PATH=/usr/bin:/bin", "TMPDIR=/", "LC_ALL=en_US.UTF-8")),
		interp.StdIO(nil, s.out, s.out),
		interp.CallHandler(s.call),
		interp.ExecHandlers(func(interp.ExecHandlerFunc) interp.ExecHandlerFunc { return s.exec }),
		interp.OpenHandler(s.open),
		interp.ReadDirHandler2(s.readDir),
		interp.StatHandler(s.stat),
		interp.AccessHandler(s.access),
		// interp.Dir would stat the host's disk; the starting directory is
		// the file system's.
		func(r *interp.Runner) error { r.Dir = start; return nil },
	)
	if err != nil {
		return Result{}, fmt.Errorf("shell: start the interpreter: %w", err)
	}
	instrument(file)
	runErr := runner.Run(countSteps(runCtx, s.budget), file)
	if !stopAtDeadline() {
		// The line's context ended while it ran: the time bound has been
		// told from the caller's context and the other bounds once this
		// returns.
		<-deadlineSeen
	}
	res := Result{ExitCode: 0, Dir: runner.Dir}
	if res.Dir == placeholder {
		res.Dir = ""
	}
	if ctx.Err() != nil {
		res.Output, res.Truncated = s.out.result()
		return res, ctx.Err()
	}
	if msg, at := s.budget.stopped(); msg != "" {
		s.out.last("%s: %d: %s\n", s.name, at, msg)
		res.ExitCode = 2
		res.Output, res.Truncated = s.out.result()
		return res, nil
	}
	var status interp.ExitStatus
	switch {
	case runErr == nil:
	case errors.As(runErr, &status):
		res.ExitCode = int(status)
	default:
		// A handler's failure the line cannot report itself, which no
		// program of the package returns: the line ends as a shell ends
		// on an internal error.
		s.out.appendf("%s: %v\n", s.name, runErr)
		res.ExitCode = 2
	}
	if res.ExitCode == 1 && s.redirectFailed.Load() {
		// dash gives a command whose redirection failed status 2, the
		// interpreter 1. The line's status is the last command's, so a
		// line that ends on one says 2 as dash does; $? within the line
		// still reads 1.
		res.ExitCode = 2
	}
	if res.ExitCode == 0 && s.writeFailed.Load() {
		// The interpreter answers a statement's status before its
		// redirections' files are written; a line that ends on a write
		// the file system then refused fails, so a caller never reads it
		// as written.
		res.ExitCode = 1
	}
	res.Output, res.Truncated = s.out.result()
	return res, nil
}

func (o *output) result() ([]byte, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return bytes.Clone(o.buf), o.truncated
}

// session is one line's run.
type session struct {
	files *files
	name  string
	owner string
	now   time.Time
	out   *output
	// line is the line of the command running, which the shell's own
	// messages carry as dash's do, and offset what the request's first
	// line adds to the parser's count.
	line   atomic.Int64
	offset int64
	// writeFailed is a redirection's file the file system refused once its
	// command was done, with no command run since.
	writeFailed atomic.Bool
	// redirectFailed is a redirection that could not be opened, with no
	// command run since.
	redirectFailed atomic.Bool
	// inputs are the files redirections opened for reading.
	inputs sync.Map
	// budget is what the line has used of its limits.
	budget *budget
	// pipes are the line's pipes a program wrote to, each with the
	// length of the line it ends on.
	pipesMu sync.Mutex
	pipes   map[*os.File]*atomic.Int64
	// rest is what a read read of an input past its line, which the next
	// read of the input reads first.
	restMu sync.Mutex
	rest   map[*os.File][]byte
}

// internal names of the commands the call handler swaps a builtin for:
// dash's echo, printf, whose output then passes the line's bounds, read,
// which reads its input a block at a time, and a cd that failed. No word
// of a line can hold a NUL, so no line can name them.
const (
	internalEcho   = "\x00echo"
	internalPrintf = "\x00printf"
	internalRead   = "\x00read"
	internalCdFail = "\x00cd"
	// internalStatus ends with the status it is given.
	internalStatus = "\x00status"
)

// call runs before every command: it records the command's line, swaps
// echo, printf and read for the package's, and answers a cd that cannot
// succeed with dash's words and status.
func (s *session) call(ctx context.Context, args []string) ([]string, error) {
	hc := interp.HandlerCtx(ctx)
	if hc.Pos.IsValid() {
		s.line.Store(int64(hc.Pos.Line()) + s.offset)
	}
	if err := s.budget.command(args, hc.Env); err != nil {
		return nil, err
	}
	if args[0] == internalCheck {
		// The check of an assignment or a read, which is done: the
		// statement's status stands.
		return []string{internalStatus, strconv.Itoa(hc.LastExitStatus)}, nil
	}
	s.redirectFailed.Store(false)
	s.writeFailed.Store(false)
	switch args[0] {
	case "echo":
		return append([]string{internalEcho}, args[1:]...), nil
	case "printf":
		return append([]string{internalPrintf}, args[1:]...), nil
	case "read":
		return append([]string{internalRead}, args[1:]...), nil
	case "cd":
		target := ""
		rest := args[1:]
		if len(rest) > 0 && rest[0] == "--" {
			rest = rest[1:]
		}
		if len(rest) == 1 {
			target = rest[0]
		}
		if target == "" {
			return args, nil
		}
		if !s.files.dirExists(uncounted(ctx), clean(hc.Dir, target)) {
			return []string{internalCdFail, target}, nil
		}
		// The interpreter's cd takes no "--"; dash's does.
		return []string{"cd", target}, nil
	}
	if interp.IsBuiltin(args[0]) && !builtins[args[0]] {
		// Check refuses every other builtin; one reached here is a line
		// Check let through by mistake, which must not run.
		return nil, fmt.Errorf("the builtin %s is not run here", args[0])
	}
	return args, nil
}

// exec runs a program of the package.
func (s *session) exec(ctx context.Context, args []string) error {
	hc := interp.HandlerCtx(ctx)
	if hc.Pos.IsValid() {
		s.line.Store(int64(hc.Pos.Line()) + s.offset)
	}
	p := &proc{
		ctx:    uncounted(ctx),
		s:      s,
		name:   args[0],
		dir:    hc.Dir,
		env:    hc.Env,
		stdout: hc.Stdout,
		stderr: hc.Stderr,
	}
	// The interpreter's standard input is a file or none.
	if in, ok := hc.Stdin.(*os.File); ok {
		p.in = in
		p.stdin = stdinReader{s: s, in: in}
	} else {
		p.stdin = bytes.NewReader(nil)
	}
	p.stdout = s.pipe(p.stdout, p.shown())
	p.stderr = s.pipe(p.stderr, p.shown())
	defer p.free()
	switch args[0] {
	case internalEcho:
		return status(echo(p, args[1:]))
	case internalPrintf:
		return status(printf(p, args[1:]))
	case internalRead:
		return s.read(ctx, p, args[1:])
	case internalStatus:
		code, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("shell: the status %q of a check: %w", args[1], err)
		}
		return status(code)
	case internalCdFail:
		p.eprintf("%s: %d: cd: can't cd to %s\n", s.name, s.line.Load(), args[1])
		return status(2)
	}
	prog, ok := programs[args[0]]
	if !ok {
		p.eprintf("%s: %d: %s: not found\n", s.name, s.line.Load(), args[0])
		return status(127)
	}
	inv, err := prog.parse(args[1:])
	if err != nil {
		var u *usageError
		if errors.As(err, &u) {
			return status(u.report(p))
		}
		p.eprintf("%s: %v\n", args[0], err)
		return status(1)
	}
	if args[0] == "cat" {
		return status(p.done(inv.run(p)))
	}
	// GNU's programs other than cat buffer what they write to a pipe or a
	// file, so their errors come before their output.
	stdout := p.stdout
	buf := p.buffer()
	p.stdout = buf
	code := inv.run(p)
	p.stdout = stdout
	p.flush(buf)
	return status(p.done(code))
}

// pipe is a program's standard output or error as it writes it: a pipe
// of the line is written through a pipeWriter, and anything else as it
// is.
func (s *session) pipe(w io.Writer, who string) io.Writer {
	f, ok := w.(*os.File)
	if !ok {
		return w
	}
	s.pipesMu.Lock()
	defer s.pipesMu.Unlock()
	if s.pipes == nil {
		s.pipes = map[*os.File]*atomic.Int64{}
	}
	run, ok := s.pipes[f]
	if !ok {
		run = new(atomic.Int64)
		s.pipes[f] = run
	}
	return pipeWriter{f: f, b: s.budget, who: who, where: "through a pipe", run: run}
}

// shown is the program's name as its messages say it.
func (p *proc) shown() string { return printable(p.name) }

// hold takes n bytes of the Input bound for what the program keeps until
// it is done, false once the line stopped.
func (p *proc) hold(n int) bool {
	err := p.s.budget.hold(p.shown(), n)
	p.held += n
	return err == nil
}

// release lets go of n bytes the program held.
func (p *proc) release(n int) {
	p.s.budget.release(n)
	p.held -= n
}

// free lets go of what the program held.
func (p *proc) free() {
	p.s.budget.release(p.held)
	p.held = 0
}

// done is a program's status once its output is written: a program
// whose output could not be written fails.
func (p *proc) done(code int) int {
	if p.werr != nil && code == 0 {
		return 1
	}
	return code
}

func status(code int) error {
	if code == 0 {
		return nil
	}
	return interp.ExitStatus(uint8(code))
}

// open answers a redirection's file: /dev/null, a file the line reads, or
// one it writes, which reaches the file system when the redirection's
// command is done with it.
func (s *session) open(ctx context.Context, name string, flag int, _ os.FileMode) (io.ReadWriteCloser, error) {
	hc := interp.HandlerCtx(ctx)
	ctx = uncounted(ctx)
	p := clean(hc.Dir, name)
	if p == devNull {
		return nullFile{}, nil
	}
	write := flag&(os.O_WRONLY|os.O_RDWR) != 0
	if !write {
		data, err := s.files.read(ctx, p)
		if err != nil {
			return nil, s.redirectError("open", name, err)
		}
		return s.input(name, data)
	}
	if err := s.files.writable(ctx, "create", p); err != nil {
		return nil, s.redirectError("create", name, err)
	}
	w := &writeFile{ctx: ctx, s: s, path: p, name: name}
	if flag&os.O_APPEND != 0 {
		data, err := s.files.read(ctx, p)
		switch {
		case err == nil:
			if _, err := w.Write(data); err != nil {
				return nil, err
			}
		case !errors.Is(err, fs.ErrNotExist):
			return nil, s.redirectError("create", name, err)
		}
	}
	return w, nil
}

// redirectError is a redirection that failed as dash says it: "cannot
// open" a file it reads and "cannot create" one it writes, with the
// system's text, which is "No such file" for an absent file it reads and
// "Directory nonexistent" for one in an absent directory.
func (s *session) redirectError(op, name string, err error) error {
	s.redirectFailed.Store(true)
	text := errorText(err)
	switch {
	case op == "open" && errors.Is(err, fs.ErrNotExist):
		text = "No such file"
	case op == "create" && errors.Is(err, fs.ErrNotExist):
		text = "Directory nonexistent"
	}
	return &fs.PathError{Op: fmt.Sprintf("%s: %d: cannot %s", s.name, s.line.Load(), op), Path: name, Err: errors.New(text)}
}

// writeFile is a file a redirection writes: it is written whole when the
// command is done, so the file system sees one write.
type writeFile struct {
	ctx  context.Context
	s    *session
	path string
	name string
	buf  bytes.Buffer
	held int
}

func (w *writeFile) Read([]byte) (int, error) { return 0, syscall.EBADF }

func (w *writeFile) Write(p []byte) (int, error) {
	if err := w.s.budget.hold(w.name, len(p)); err != nil {
		w.s.budget.release(len(p))
		return 0, err
	}
	w.held += len(p)
	return w.buf.Write(p)
}

func (w *writeFile) Close() error {
	defer w.s.budget.release(w.held)
	if w.s.budget.done.Load() {
		// The line stopped: its last command's output is not written.
		return nil
	}
	if err := w.s.files.write(w.ctx, w.path, w.buf.Bytes()); err != nil {
		w.s.out.appendf("%s: %d: cannot create %s: %s\n", w.s.name, w.s.line.Load(), w.name, errorText(err))
		w.s.writeFailed.Store(true)
	}
	return nil
}

// input is a file a redirection reads, as a pipe the file's content is
// written to: the interpreter hands a program its input as an *os.File,
// and one it was given stays the same file, so a program can tell a file
// from a pipe as GNU's do. The pipe is closed with the redirection, which
// ends a write its command did not read.
func (s *session) input(name string, data []byte) (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("shell: open a redirection's input: %w", err)
	}
	s.inputs.Store(r, true)
	pw := pipeWriter{f: w, b: s.budget, who: name, where: "read with <", run: new(atomic.Int64)}
	go func() {
		// A command that stops reading closes the read end, which fails
		// the write; either way the input is done.
		_, _ = pw.Write(data)
		_ = w.Close()
	}()
	return r, nil
}

type nullFile struct{}

func (nullFile) Read([]byte) (int, error)    { return 0, io.EOF }
func (nullFile) Write(p []byte) (int, error) { return len(p), nil }
func (nullFile) Close() error                { return nil }

// readDir lists a directory a pattern reads.
func (s *session) readDir(ctx context.Context, name string) ([]fs.DirEntry, error) {
	infos, err := s.files.readDir(uncounted(ctx), name)
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, len(infos))
	for i, info := range infos {
		out[i] = fs.FileInfoToDirEntry(info)
	}
	return out, nil
}

func (s *session) stat(ctx context.Context, name string, _ bool) (fs.FileInfo, error) {
	return s.files.stat(uncounted(ctx), name)
}

// access answers test -r, -w and -x and cd's check: a served path is
// readable and searchable, and writable unless its mount is read only;
// no file of the file system is executable.
func (s *session) access(ctx context.Context, name string, mode interp.AccessMode) error {
	info, err := s.files.stat(uncounted(ctx), name)
	if err != nil {
		return err
	}
	switch {
	case mode&interp.AccessWrite != 0 && s.files.readOnly(name):
		return &fs.PathError{Op: "access", Path: name, Err: syscall.EROFS}
	case mode&interp.AccessExec != 0 && !info.IsDir():
		return &fs.PathError{Op: "access", Path: name, Err: syscall.EACCES}
	}
	return nil
}

// proc is one program's run.
type proc struct {
	ctx    context.Context
	s      *session
	name   string
	dir    string
	env    expand.Environ
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	// in is the file standard input reads, nil when there is none.
	in *os.File
	// werr is the first write to stdout or stderr that failed.
	werr error
	// held is what the program holds of the Input bound.
	held int
}

// errorf writes a message as the program's own, "name: message".
func (p *proc) errorf(format string, args ...any) {
	p.eprintf(p.name+": "+format+"\n", args...)
}

// printf and write are the program's standard output, and eprintf its
// standard error. A write that fails, to a pipe whose reader has gone, is
// kept, and the program's status says so as GNU's "write error" does.
func (p *proc) printf(format string, args ...any) {
	if _, err := fmt.Fprintf(p.stdout, format, args...); err != nil && p.werr == nil {
		p.werr = err
	}
}

func (p *proc) write(b []byte) {
	if _, err := p.stdout.Write(b); err != nil && p.werr == nil {
		p.werr = err
	}
}

func (p *proc) eprintf(format string, args ...any) {
	if _, err := fmt.Fprintf(p.stderr, format, args...); err != nil && p.werr == nil {
		p.werr = err
	}
}

// abs is a path the program names, made absolute against its directory.
func (p *proc) abs(name string) string { return clean(p.dir, name) }

// readInput is a file the program reads, or its standard input for "-".
func (p *proc) readInput(name string) ([]byte, error) {
	if name == "-" {
		return p.readStdin()
	}
	return p.s.files.read(p.ctx, p.abs(name))
}

// readStdin is the program's standard input, held until the program is
// done.
func (p *proc) readStdin() ([]byte, error) {
	var data []byte
	chunk := make([]byte, 32<<10)
	for {
		n, err := p.stdin.Read(chunk)
		if n > 0 {
			if !p.hold(n) {
				return nil, errStopped
			}
			data = append(data, chunk[:n]...)
		}
		if errors.Is(err, io.EOF) {
			return data, nil
		}
		if err != nil {
			return data, err
		}
	}
}

// stdinIsFile reports whether the program's standard input is a file a
// redirection opened, whose size a program can know before reading it.
func (p *proc) stdinIsFile() bool {
	if p.in == nil {
		return false
	}
	_, ok := p.s.inputs.Load(p.in)
	return ok
}

// byteOrder reports whether the program's environment asks for the C
// locale's order rather than en_US.UTF-8's.
func (p *proc) byteOrder() bool {
	for _, v := range []string{"LC_ALL", "LC_COLLATE", "LANG"} {
		if s := p.env.Get(v).String(); s != "" {
			return s == "C" || s == "POSIX" || s == "C.UTF-8" || s == "C.utf8"
		}
	}
	return false
}

// quote is a name as GNU's programs quote one in a message: in single
// quotes, an embedded quote closed, escaped and opened again.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// quoteLocale is a name as mkdir and find quote one under a UTF-8 locale.
func quoteLocale(s string) string { return "‘" + s + "’" }

// quoteIfNeeded is a name as cat, wc, cut and the like print one: as it
// is, or quoted when it holds a character a shell would read.
func quoteIfNeeded(s string) string {
	for _, r := range s {
		switch {
		case r <= ' ', r == '\'', r == '"', r == '\\', r == '$', r == '`', r == '*', r == '?', r == '[', r == '&', r == ';', r == '|', r == '<', r == '>', r == '(', r == ')', r == '!', r == '#', r == '~', r == '{', r == '}':
			return quote(s)
		}
	}
	return s
}
