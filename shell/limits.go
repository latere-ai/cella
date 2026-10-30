// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// Limits bound what one command line takes of the process it runs in,
// whatever the line holds. A line that reaches one stops where it is,
// with a message as the shell's own and status 2, and what it changed
// before stays changed. A field left zero takes its default.
type Limits struct {
	// Steps bounds the statements, commands and loop turns the line
	// runs; DefaultSteps when zero.
	Steps int
	// Time bounds how long the line runs, apart from the caller's
	// context; DefaultTime when zero.
	Time time.Duration
	// Value bounds one variable's value and one word of a command;
	// DefaultValue when zero.
	Value int
	// Values bounds the line's variables together and one command's
	// words together; DefaultValues when zero.
	Values int
	// Input bounds what the line's programs hold at once: what they read
	// from standard input, the files sort sorts, the output a program
	// keeps until it is done, a redirection's file until its command is
	// done, and one line written into a pipe; DefaultInput when zero.
	Input int
	// Read bounds the bytes of files the line reads; DefaultRead when
	// zero.
	Read int64
	// Names bounds the directory entries the line lists, by its patterns
	// and its programs together; DefaultNames when zero.
	Names int
}

// The defaults of Limits.
const (
	DefaultSteps  = 50000
	DefaultTime   = 10 * time.Second
	DefaultValue  = 256 << 10
	DefaultValues = 1 << 20
	DefaultInput  = 16 << 20
	DefaultRead   = 64 << 20
	DefaultNames  = 50000
)

// The bounds Check holds a line to before it runs, which it refuses as
// ReasonSize: the parser's stack grows with how deeply a line nests, one
// command's expansions decide how much it can make of the line's values
// before its words are checked, and every pipe, here document and input
// redirection is an open pipe of the process while its command runs.
const (
	// MaxLine is the longest line, in bytes.
	MaxLine = 64 << 10
	// MaxDepth bounds how deeply a line nests its brackets and its
	// compound commands, with the && || and | of one list.
	MaxDepth = 64
	// MaxExpansions bounds the parameter expansions and patterns in the
	// words of one command with its redirections and here documents, in
	// a for loop's list, and in a case clause's word and one item's
	// patterns.
	MaxExpansions = 16
	// MaxPipes bounds a line's pipes, here documents and input
	// redirections together.
	MaxPipes = 32
)

func (l Limits) withDefaults() Limits {
	if l.Steps <= 0 {
		l.Steps = DefaultSteps
	}
	if l.Time <= 0 {
		l.Time = DefaultTime
	}
	if l.Value <= 0 {
		l.Value = DefaultValue
	}
	if l.Values <= 0 {
		l.Values = DefaultValues
	}
	if l.Input <= 0 {
		l.Input = DefaultInput
	}
	if l.Read <= 0 {
		l.Read = DefaultRead
	}
	if l.Names <= 0 {
		l.Names = DefaultNames
	}
	return l
}

// errStopped is what a handler answers once the line reached a bound;
// the interpreter ends the line on it.
var errStopped = errors.New("shell: the line reached a bound")

// budget is what a line has used of its limits, and the first bound it
// reached.
type budget struct {
	lim    Limits
	cancel context.CancelFunc
	out    *output
	line   *atomic.Int64

	steps atomic.Int64
	held  atomic.Int64
	read  atomic.Int64
	names atomic.Int64

	done    atomic.Bool
	mu      sync.Mutex
	message string
	at      int64
}

// stop ends the line on the message, said at the line of the command
// running. The first bound reached is the one the line reports, and
// nothing written after it reaches the output.
func (b *budget) stop(format string, args ...any) {
	b.mu.Lock()
	if b.message == "" {
		b.message = fmt.Sprintf(format, args...)
		b.at = b.line.Load()
		b.out.close()
		b.done.Store(true)
	}
	b.mu.Unlock()
	b.cancel()
}

// exceeded stops the line on the message and answers the error a handler
// ends the line with.
func (b *budget) exceeded(format string, args ...any) error {
	b.stop(format, args...)
	return errStopped
}

// stopped is the bound the line reached and the line it was reached at,
// empty when it reached none.
func (b *budget) stopped() (string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.message, b.at
}

// hold takes n bytes of the Input bound for what who keeps, until it lets
// them go with release.
func (b *budget) hold(who string, n int) error {
	if b.held.Add(int64(n)) > int64(b.lim.Input) {
		return b.exceeded("%s: more than %d bytes held at once", who, b.lim.Input)
	}
	return nil
}

func (b *budget) release(n int) { b.held.Add(-int64(n)) }

// reading takes n bytes of the Read bound for a file the line read.
func (b *budget) reading(n int) error {
	if b.read.Add(int64(n)) > b.lim.Read {
		return b.exceeded("the line read more than %d bytes of files", b.lim.Read)
	}
	return nil
}

// listing takes n names of the Names bound for a directory the line
// listed.
func (b *budget) listing(n int) error {
	if b.names.Add(int64(n)) > int64(b.lim.Names) {
		return b.exceeded("the line listed more than %d names", b.lim.Names)
	}
	return nil
}

// step counts one step of the line against the Steps bound.
func (b *budget) step() {
	if b.steps.Add(1) > int64(b.lim.Steps) {
		b.stop("the line ran more than %d steps", b.lim.Steps)
	}
}

// command checks a command's words and the variables it runs with
// against the Value and Values bounds. A variable a subshell changed is
// counted with the value it has outside too.
func (b *budget) command(args []string, env expand.Environ) error {
	total := 0
	for _, a := range args {
		if len(a) > b.lim.Value {
			return b.exceeded("%s: argument longer than %d bytes", printable(args[0]), b.lim.Value)
		}
		total += len(a)
	}
	if total > b.lim.Values {
		return b.exceeded("%s: arguments longer than %d bytes together", printable(args[0]), b.lim.Values)
	}
	var err error
	total = 0
	env.Each(func(name string, vr expand.Variable) bool {
		size := len(vr.Str)
		if size > b.lim.Value {
			err = b.exceeded("%s: value longer than %d bytes", name, b.lim.Value)
			return false
		}
		total += size
		if total > b.lim.Values {
			err = b.exceeded("the line's variables hold more than %d bytes", b.lim.Values)
			return false
		}
		return true
	})
	return err
}

// printable is a command's name as a message says it: the package's own
// commands by the builtin they stand for.
func printable(name string) string {
	switch name {
	case internalEcho:
		return "echo"
	case internalPrintf:
		return "printf"
	case internalCheck:
		return "sh"
	}
	return name
}

// stepCtx is the context the interpreter runs the line under. The
// interpreter asks for its error before every statement, command, call
// and loop turn, so counting the questions counts the line's steps, the
// turns of a loop that calls nothing among them.
type stepCtx struct {
	context.Context
	b *budget
}

// countSteps is ctx with its questions counted as the line's steps.
func countSteps(ctx context.Context, b *budget) context.Context { return stepCtx{Context: ctx, b: b} }

func (c stepCtx) Err() error {
	c.b.step()
	return c.Context.Err()
}

// buffer is what a program keeps until it writes it, held against the
// Input bound while it is kept. Once the line has stopped it keeps
// nothing more, so its writes never fail.
type buffer struct {
	data bytes.Buffer
	b    *budget
	who  string
	held int
}

func (p *proc) buffer() *buffer { return &buffer{b: p.s.budget, who: p.shown()} }

func (b *buffer) take(n int) bool {
	if b.b.done.Load() || b.b.hold(b.who, n) != nil {
		return false
	}
	b.held += n
	return true
}

// Write makes the buffer the standard output of the programs that write
// as they go.
func (b *buffer) Write(d []byte) (int, error) {
	b.put(d)
	return len(d), nil
}

func (b *buffer) put(d []byte) {
	if b.take(len(d)) {
		b.data.Write(d)
	}
}

func (b *buffer) puts(s string) {
	if b.take(len(s)) {
		b.data.WriteString(s)
	}
}

func (b *buffer) putByte(c byte) {
	if b.take(1) {
		b.data.WriteByte(c)
	}
}

func (b *buffer) printf(format string, args ...any) {
	b.put(fmt.Appendf(nil, format, args...))
}

// Bytes is what the buffer kept.
func (b *buffer) Bytes() []byte { return b.data.Bytes() }

// free lets go of what the buffer held.
func (b *buffer) free() {
	b.b.release(b.held)
	b.held = 0
}

// flush writes what the buffer kept as the program's output and lets go
// of it.
func (p *proc) flush(b *buffer) {
	b.free()
	p.write(b.Bytes())
}

// pipeWriter is the end of a pipe a program, or a redirection's file,
// writes: whatever reads a pipe holds a line of it whole, read's value
// among them, so no line through a pipe is longer than the Input bound.
type pipeWriter struct {
	f   *os.File
	b   *budget
	who string
	// where says what the pipe is to the line, for the bound's message.
	where string
	// run is the length of the line the pipe's writes so far end on.
	run *atomic.Int64
}

func (w pipeWriter) Write(d []byte) (int, error) {
	if w.b.done.Load() {
		return 0, errStopped
	}
	run := w.run.Load()
	longest := run
	for rest := d; ; {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			run += int64(len(rest))
			longest = max(longest, run)
			break
		}
		longest = max(longest, run+int64(i))
		run = 0
		rest = rest[i+1:]
	}
	w.run.Store(run)
	if longest > int64(w.b.lim.Input) {
		return 0, w.b.exceeded("%s: a line longer than %d bytes %s", w.who, w.b.lim.Input, w.where)
	}
	return w.f.Write(d)
}

// internalCheck is the command the line's assignments are followed by:
// an assignment alone calls no handler, so the check of the line's
// variables runs after it. No word of a line can hold a NUL, so no line
// can name it.
const internalCheck = "\x00check"

// instrument follows every statement that only assigns with the check
// of the line's variables, in a block so the assignment stays the
// shell's own. An assignment's status is 0 in every line Check allows,
// since it allows no command substitution, and the check's is too.
func instrument(f *syntax.File) {
	syntax.Walk(f, func(n syntax.Node) bool {
		st, ok := n.(*syntax.Stmt)
		if !ok {
			return true
		}
		call, ok := st.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) > 0 || len(call.Assigns) == 0 {
			return true
		}
		pos := call.Pos()
		check := &syntax.CallExpr{Args: []*syntax.Word{{Parts: []syntax.WordPart{&syntax.Lit{ValuePos: pos, ValueEnd: pos, Value: internalCheck}}}}}
		st.Cmd = &syntax.Block{
			Lbrace: pos,
			Rbrace: pos,
			Stmts: []*syntax.Stmt{
				{Position: pos, Cmd: call},
				{Position: pos, Cmd: check},
			},
		}
		// An assignment's words hold no statement, so nothing in the
		// block needs a check of its own.
		return false
	})
}

// nesting is how deeply a line nests, read before the line is parsed,
// since the parser's stack grows with it: its brackets, its compound
// commands' words, and the && || and | of one list. Quotes are not read,
// so a quoted word counts as one outside them would.
func nesting(command string) int {
	depth, chain, deepest := 0, 0, 0
	word := 0
	end := func(i int) {
		switch command[word:i] {
		case "if", "while", "until", "for", "case":
			depth++
		case "fi", "done", "esac":
			depth = max(depth-1, 0)
		}
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch c {
		case ' ', '\t', '\n', ';', '&', '|', '(', ')', '{', '}', '<', '>':
			end(i)
			word = i + 1
		default:
			continue
		}
		switch c {
		case '(', '{':
			depth++
		case ')', '}':
			depth = max(depth-1, 0)
		case '|':
			chain++
		case '&':
			if i+1 < len(command) && command[i+1] == '&' {
				chain++
				i++
				word = i + 1
			} else {
				chain = 0
			}
		case ';', '\n':
			chain = 0
		}
		deepest = max(deepest, depth+chain)
	}
	end(len(command))
	return max(deepest, depth)
}

// sized reports whether a parsed line keeps to MaxDepth, MaxExpansions
// and MaxPipes.
func sized(f *syntax.File) bool {
	pipes := 0
	ok := true
	words := func(ws ...*syntax.Word) {
		n := 0
		for _, w := range ws {
			n += expansions(w)
		}
		if n > MaxExpansions {
			ok = false
		}
	}
	depth := 0
	var walk func(syntax.Node) bool
	walk = func(n syntax.Node) bool {
		if !ok {
			return false
		}
		switch x := n.(type) {
		case *syntax.Stmt:
			var ws []*syntax.Word
			for _, rd := range x.Redirs {
				ws = append(ws, rd.Word, rd.Hdoc)
				switch rd.Op {
				case syntax.RdrIn, syntax.Hdoc, syntax.DashHdoc:
					pipes++
				}
			}
			if call, isCall := x.Cmd.(*syntax.CallExpr); isCall {
				for _, as := range call.Assigns {
					ws = append(ws, as.Value)
				}
				ws = append(ws, call.Args...)
			}
			words(ws...)
		case *syntax.BinaryCmd:
			if x.Op == syntax.Pipe || x.Op == syntax.PipeAll {
				pipes++
			}
		case *syntax.ForClause:
			if it, isIter := x.Loop.(*syntax.WordIter); isIter {
				words(it.Items...)
			}
		case *syntax.CaseClause:
			words(x.Word)
			for _, it := range x.Items {
				words(it.Patterns...)
			}
		}
		switch n.(type) {
		case *syntax.BinaryCmd, *syntax.Block, *syntax.Subshell, *syntax.IfClause,
			*syntax.WhileClause, *syntax.ForClause, *syntax.CaseClause:
			depth++
			if depth > MaxDepth {
				ok = false
				return false
			}
			for _, child := range children(n) {
				syntax.Walk(child, walk)
			}
			depth--
			return false
		}
		return true
	}
	syntax.Walk(f, walk)
	return ok && pipes <= MaxPipes
}

// children are the statements and words directly in a compound command.
func children(n syntax.Node) []syntax.Node {
	var out []syntax.Node
	stmts := func(list []*syntax.Stmt) {
		for _, st := range list {
			out = append(out, st)
		}
	}
	switch x := n.(type) {
	case *syntax.BinaryCmd:
		out = append(out, x.X, x.Y)
	case *syntax.Block:
		stmts(x.Stmts)
	case *syntax.Subshell:
		stmts(x.Stmts)
	case *syntax.IfClause:
		stmts(x.Cond)
		stmts(x.Then)
		if x.Else != nil {
			out = append(out, x.Else)
		}
	case *syntax.WhileClause:
		stmts(x.Cond)
		stmts(x.Do)
	case *syntax.ForClause:
		stmts(x.Do)
	case *syntax.CaseClause:
		for _, it := range x.Items {
			stmts(it.Stmts)
		}
	}
	return out
}

// expansions counts a word's parameter expansions, and its patterns: a
// word with an unquoted wildcard is one.
func expansions(w *syntax.Word) int {
	if w == nil {
		return 0
	}
	n := 0
	for _, part := range w.Parts {
		if lit, isLit := part.(*syntax.Lit); isLit {
			if hasMeta(lit.Value) {
				n++
			}
			continue
		}
		syntax.Walk(part, func(x syntax.Node) bool {
			if _, isParam := x.(*syntax.ParamExp); isParam {
				n++
			}
			return true
		})
	}
	return n
}
