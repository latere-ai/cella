// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"path"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Reason is why Check refused a command line.
type Reason string

// The reasons Check refuses a line for, a closed set a caller can count
// by.
const (
	// ReasonSyntax is a line that does not parse as POSIX sh.
	ReasonSyntax Reason = "syntax"
	// ReasonProgram is a program the package does not provide.
	ReasonProgram Reason = "program"
	// ReasonOption is an option of a program or a builtin the package does
	// not take.
	ReasonOption Reason = "option"
	// ReasonPath is a path no mount serves, a relative path while the
	// shell is in a directory no mount serves among them.
	ReasonPath Reason = "path"
	// ReasonExpansion is a command substitution, a variable the line does
	// not set, or a value known only once something has run where the
	// value decides a path or an option.
	ReasonExpansion Reason = "expansion"
	// ReasonSize is a line longer than MaxLine, nested deeper than
	// MaxDepth, with a command of more expansions than MaxExpansions, or
	// with more pipes than MaxPipes.
	ReasonSize Reason = "size"
	// ReasonConstruct is a construct the package does not run: a function,
	// a background job, a brace expansion, cd inside a loop or a pipe, a
	// redirection other than <, >, >>, >|, a descriptor's copy or a here
	// document.
	ReasonConstruct Reason = "construct"
)

// Plan is what Check decided.
type Plan struct {
	// Reason is why the line does not run here, empty when it does.
	Reason Reason
	// Program names the program or builtin of ReasonProgram and
	// ReasonOption.
	Program string
}

// OK reports whether the line runs here.
func (p Plan) OK() bool { return p.Reason == "" }

// builtins are the shell's own commands a line may use. Every other
// builtin of the interpreter, such as eval, exec, source or command, is a
// program the package does not provide.
var builtins = map[string]bool{
	"cd": true, "pwd": true, "echo": true, "printf": true, "test": true, "[": true,
	"true": true, "false": true, ":": true, "exit": true, "set": true, "read": true,
	"break": true, "continue": true,
}

// parseLine parses a command line as POSIX sh.
func parseLine(command string) (*syntax.File, error) {
	return syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
}

// Check decides, without running anything, whether a command line started
// in dir runs here over the mounts: every program it names is one the
// package provides with options it takes, every path it names, its
// redirections, the directories cd moves to and the directories its
// patterns read among them, is served, and every value that decides a path
// or an option is known before anything runs.
func Check(command, dir string, mounts []Mount) Plan {
	mounts, err := checkMounts(mounts)
	if err != nil {
		return Plan{Reason: ReasonPath}
	}
	if len(command) > MaxLine || nesting(command) > MaxDepth {
		return Plan{Reason: ReasonSize}
	}
	f, err := parseLine(command)
	if err != nil {
		return Plan{Reason: ReasonSyntax}
	}
	if !sized(f) {
		return Plan{Reason: ReasonSize}
	}
	start := ""
	if dir != "" && serves(mounts, dir) {
		start = path.Clean(dir)
	}
	c := &checker{mounts: mounts, vars: map[string][]alt{}}
	c.stmts(f.Stmts, []string{start})
	return c.plan
}

// checker walks a parsed line, keeping the set of directories the shell
// may be in at each command and the values of the variables the line set,
// and records the first refusal.
type checker struct {
	mounts []Mount
	vars   map[string][]alt
	// noCd counts the loops, pipes and case clauses the walk is in, where
	// a cd would move the shell by a count of runs the walk cannot know.
	noCd int
	plan Plan
}

func (c *checker) refuse(r Reason, program string) {
	if c.plan.OK() {
		c.plan = Plan{Reason: r, Program: program}
	}
}

func (c *checker) refused() bool { return !c.plan.OK() }

// dirs is a set of directories the shell may be in; "" is one no mount
// serves.
type dirs []string

func union(a, b dirs) dirs {
	out := slices.Clone(a)
	for _, d := range b {
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// stmts walks a list run one after another, and answers the directories
// the shell may be in once the last succeeded or failed.
func (c *checker) stmts(list []*syntax.Stmt, in dirs) (ok, failed dirs) {
	ok = in
	for i, st := range list {
		if c.refused() {
			return in, in
		}
		if i > 0 {
			ok = union(ok, failed)
		}
		ok, failed = c.stmt(st, ok)
	}
	return ok, failed
}

func (c *checker) stmt(st *syntax.Stmt, in dirs) (ok, failed dirs) {
	if st.Background || st.Coprocess || st.Disown {
		c.refuse(ReasonConstruct, "")
		return in, in
	}
	for _, rd := range st.Redirs {
		c.redirect(rd, in)
	}
	if st.Cmd == nil {
		return in, in
	}
	ok, failed = c.command(st.Cmd, in)
	if st.Negated {
		ok, failed = failed, ok
	}
	return ok, failed
}

func (c *checker) command(cmd syntax.Command, in dirs) (ok, failed dirs) {
	switch x := cmd.(type) {
	case *syntax.CallExpr:
		return c.call(x, in)
	case *syntax.BinaryCmd:
		switch x.Op {
		case syntax.AndStmt:
			xo, xf := c.stmt(x.X, in)
			yo, yf := c.stmt(x.Y, xo)
			return yo, union(xf, yf)
		case syntax.OrStmt:
			xo, xf := c.stmt(x.X, in)
			yo, yf := c.stmt(x.Y, xf)
			return union(xo, yo), yf
		case syntax.Pipe:
			// Each stage runs in a subshell, so no cd of one moves the
			// shell; the walk refuses cd in a stage outright.
			c.noCd++
			c.stmt(x.X, in)
			c.stmt(x.Y, in)
			c.noCd--
			return in, in
		}
		c.refuse(ReasonConstruct, "")
	case *syntax.Subshell:
		c.stmts(x.Stmts, in)
		return in, in
	case *syntax.Block:
		return c.stmts(x.Stmts, in)
	case *syntax.IfClause:
		return c.ifClause(x, in)
	case *syntax.WhileClause:
		c.noCd++
		c.stmts(x.Cond, in)
		c.stmts(x.Do, in)
		c.noCd--
		return in, in
	case *syntax.ForClause:
		c.forClause(x, in)
		return in, in
	case *syntax.CaseClause:
		c.words([]*syntax.Word{x.Word}, in, wordArg)
		c.noCd++
		for _, it := range x.Items {
			for _, p := range it.Patterns {
				c.words([]*syntax.Word{p}, in, wordPattern)
			}
			c.stmts(it.Stmts, in)
		}
		c.noCd--
		return in, in
	default:
		c.refuse(ReasonConstruct, "")
	}
	return in, in
}

func (c *checker) ifClause(x *syntax.IfClause, in dirs) (ok, failed dirs) {
	if len(x.Cond) == 0 {
		// An else: its body runs on the directories the conditions before
		// it failed in.
		return c.stmts(x.Then, in)
	}
	co, cf := c.stmts(x.Cond, in)
	to, tf := c.stmts(x.Then, co)
	if x.Else == nil {
		// No branch ran: the if succeeds.
		return union(to, cf), tf
	}
	eo, ef := c.ifClause(x.Else, cf)
	return union(to, eo), union(tf, ef)
}

func (c *checker) forClause(x *syntax.ForClause, in dirs) {
	it, ok := x.Loop.(*syntax.WordIter)
	if !ok || x.Select || it.InPos == (syntax.Pos{}) {
		c.refuse(ReasonConstruct, "")
		return
	}
	fields := c.words(it.Items, in, wordArg)
	if c.refused() {
		return
	}
	// The variable takes each field in turn: a pattern's matches are paths
	// in the directory it reads, which words checked is served.
	var vals []alt
	for _, f := range fields {
		for _, a := range f.alts {
			switch a.kind {
			case kindGlob:
				for _, d := range c.globDirs(a.s, in) {
					vals = append(vals, alt{kind: kindUnder, s: d})
				}
			default:
				vals = append(vals, a)
			}
		}
	}
	if len(vals) == 0 {
		vals = []alt{{kind: kindExact}}
	}
	c.assign(it.Name.Value, vals)
	c.noCd++
	c.stmts(x.Do, in)
	c.noCd--
}

// assign records a variable's possible values, joining them to the ones
// it had: the walk does not know which branch of the line set it last.
func (c *checker) assign(name string, vals []alt) {
	cur, known := c.vars[name]
	if !known {
		c.vars[name] = vals
		return
	}
	for _, v := range vals {
		if !slices.Contains(cur, v) {
			cur = append(cur, v)
		}
	}
	if len(cur) > maxAlts {
		cur = []alt{{kind: kindDynamic}}
	}
	c.vars[name] = cur
}

func (c *checker) redirect(rd *syntax.Redirect, in dirs) {
	if rd.N != nil && (len(rd.N.Value) != 1 || rd.N.Value[0] < '0' || rd.N.Value[0] > '9') {
		c.refuse(ReasonConstruct, "")
		return
	}
	switch rd.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.RdrIn:
		fields := c.words([]*syntax.Word{rd.Word}, in, wordTarget)
		if c.refused() {
			return
		}
		if len(fields) != 1 || len(fields[0].alts) != 1 {
			c.refuse(ReasonExpansion, "")
			return
		}
		a := fields[0].alts[0]
		if a.kind == kindExact && a.s == devNull {
			return
		}
		c.pathAlt(a, in)
	case syntax.DplIn, syntax.DplOut:
		if v := rd.Word.Lit(); v != "-" && (len(v) != 1 || v[0] < '0' || v[0] > '9') {
			c.refuse(ReasonConstruct, "")
		}
	case syntax.Hdoc, syntax.DashHdoc:
		if rd.Hdoc != nil {
			c.words([]*syntax.Word{rd.Hdoc}, in, wordHeredoc)
		}
	default:
		c.refuse(ReasonConstruct, "")
	}
}

// devNull is the one path outside the mounts a redirection may name.
const devNull = "/dev/null"

func (c *checker) call(x *syntax.CallExpr, in dirs) (ok, failed dirs) {
	for _, as := range x.Assigns {
		if as.Append || as.Naked || as.Index != nil || as.Array != nil || as.Name == nil {
			c.refuse(ReasonConstruct, "")
			return in, in
		}
		var vals []alt
		if as.Value != nil {
			vals = c.assignValue(as.Value, in)
		} else {
			vals = []alt{{kind: kindExact}}
		}
		if c.refused() {
			return in, in
		}
		if len(x.Args) == 0 {
			c.assign(as.Name.Value, vals)
		}
	}
	if len(x.Args) == 0 {
		return in, in
	}
	fields := c.words(x.Args, in, wordArg)
	if c.refused() || len(fields) == 0 {
		return in, in
	}
	head := fields[0]
	if len(head.alts) != 1 || head.alts[0].kind != kindExact || head.splits {
		c.refuse(ReasonExpansion, "")
		return in, in
	}
	name := head.alts[0].s
	args := fields[1:]
	if builtins[name] {
		return c.builtin(name, args, in)
	}
	p, known := programs[name]
	if !known || strings.Contains(name, "/") {
		c.refuse(ReasonProgram, programName(name))
		return in, in
	}
	c.program(name, p, args, in)
	return in, in
}

// programName is a program's name as a refusal carries it: its last
// element, so a path to a program names the program alone.
func programName(name string) string {
	if name == "" {
		return ""
	}
	return path.Base(name)
}

// program checks a call of one of the package's programs: its options are
// read as the program reads them, with a value known only at run time in
// the place of an argument, and every argument it reads as a path, and the
// directory it reads when it names none, must be served.
func (c *checker) program(name string, p program, args []field, in dirs) {
	strs := make([]string, len(args))
	afterDashes := false
	for i, f := range args {
		if f.exact() {
			strs[i] = f.alts[0].s
			if strs[i] == "--" {
				afterDashes = true
			}
			continue
		}
		// A value that may split into more words, or that could read as an
		// option, decides the call's options at run time.
		if f.splits || (f.dynamic() && !afterDashes) {
			c.refuse(ReasonExpansion, "")
			return
		}
		strs[i] = placeholderArg
	}
	inv, err := p.parse(strs)
	if err != nil {
		c.refuse(c.parseFailure(p, strs), name)
		return
	}
	paths := inv.paths()
	for _, i := range paths {
		c.pathField(args[i], in)
	}
	if inv.cwd() {
		c.useCwd(in)
	}
	for i, f := range args {
		if slices.Contains(paths, i) {
			continue
		}
		// A value known only at run time is fine where the program reads
		// no path, such as a pattern; a pattern of the shell's own reads
		// directories wherever it stands.
		for _, a := range f.alts {
			if a.kind == kindGlob {
				c.pathAlt(a, in)
			}
		}
	}
}

// parseFailure is why a program's arguments did not read: what a value
// known only at run time expands to, when the arguments read with a plain
// number in its place, and otherwise an option the program does not take.
func (c *checker) parseFailure(p program, strs []string) Reason {
	if !slices.Contains(strs, placeholderArg) {
		return ReasonOption
	}
	plain := slices.Clone(strs)
	for i, s := range plain {
		if s == placeholderArg {
			plain[i] = "1"
		}
	}
	if _, err := p.parse(plain); err == nil {
		return ReasonExpansion
	}
	return ReasonOption
}

// placeholderArg stands for an argument whose value is known at run time:
// it reads as no option, and no program takes it as a number.
const placeholderArg = "\x00"

func (c *checker) builtin(name string, args []field, in dirs) (ok, failed dirs) {
	switch name {
	case "cd":
		return c.cd(args, in)
	case "pwd":
		for _, f := range args {
			if !f.exact() || (f.alts[0].s != "-L" && f.alts[0].s != "-P") {
				c.refuse(ReasonOption, name)
				return in, in
			}
		}
		c.useCwd(in)
	case "set":
		c.set(args)
	case "read":
		c.read(args)
	case "test", "[":
		c.test(args, in)
	case "echo", "printf":
		for _, f := range args {
			for _, a := range f.alts {
				if a.kind == kindGlob {
					c.pathAlt(a, in)
				}
			}
		}
	default:
		// true, false, :, exit, break and continue read no path; their
		// words were checked for what they expand.
		for _, f := range args {
			if f.splits {
				c.refuse(ReasonExpansion, "")
			}
		}
	}
	return in, in
}

func (c *checker) cd(args []field, in dirs) (ok, failed dirs) {
	if c.noCd > 0 {
		c.refuse(ReasonConstruct, "cd")
		return in, in
	}
	if len(args) > 0 && args[0].exact() && args[0].alts[0].s == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		// cd alone goes to $HOME, which is the machine's.
		c.refuse(ReasonExpansion, "")
		return in, in
	}
	if len(args) > 1 {
		c.refuse(ReasonOption, "cd")
		return in, in
	}
	f := args[0]
	if !f.exact() || f.splits {
		c.refuse(ReasonExpansion, "")
		return in, in
	}
	t := f.alts[0].s
	switch {
	case t == "-":
		// cd - goes to $OLDPWD.
		c.refuse(ReasonExpansion, "")
		return in, in
	case strings.HasPrefix(t, "-"):
		c.refuse(ReasonOption, "cd")
		return in, in
	}
	var to dirs
	sure := true
	for _, d := range in {
		p := clean(d, t)
		if !serves(c.mounts, p) {
			c.refuse(ReasonPath, "")
			return in, in
		}
		if !slices.Contains(to, p) {
			to = append(to, p)
		}
		if !c.isMountDir(p) {
			sure = false
		}
	}
	if sure {
		// A mount's directory always exists, so cd to one never fails.
		return to, nil
	}
	return to, in
}

func (c *checker) isMountDir(p string) bool {
	return slices.ContainsFunc(c.mounts, func(m Mount) bool { return m.Dir == p })
}

// setFlags are the options of set a line may change: the ones that change
// how a failure or an unset variable is answered. The trace is not one:
// it would print the commands the package runs for its bounds.
var setFlags = map[string]bool{
	"-e": true, "+e": true, "-u": true, "+u": true, "+x": true,
	"-eu": true, "-ue": true,
}

var setNames = map[string]bool{"errexit": true, "nounset": true}

func (c *checker) set(args []field) {
	for i := 0; i < len(args); i++ {
		f := args[i]
		if !f.exact() {
			c.refuse(ReasonExpansion, "")
			return
		}
		s := f.alts[0].s
		switch {
		case setFlags[s]:
		case (s == "-o" || s == "+o") && i+1 < len(args) && args[i+1].exact() && setNames[args[i+1].alts[0].s]:
			i++
		default:
			c.refuse(ReasonOption, "set")
			return
		}
	}
}

func (c *checker) read(args []field) {
	for _, f := range args {
		if !f.exact() {
			c.refuse(ReasonExpansion, "")
			return
		}
		s := f.alts[0].s
		switch {
		case s == "-r":
		case syntax.ValidName(s):
			c.assign(s, []alt{{kind: kindDynamic}})
		default:
			c.refuse(ReasonOption, "read")
			return
		}
	}
}

// fileTests are the operators of test that read a path.
var fileTests = map[string]bool{
	"-e": true, "-f": true, "-d": true, "-s": true, "-r": true, "-w": true, "-x": true,
	"-L": true, "-h": true, "-p": true, "-S": true, "-b": true, "-c": true, "-g": true,
	"-u": true, "-k": true, "-O": true, "-G": true, "-N": true,
}

var fileCompares = map[string]bool{"-nt": true, "-ot": true, "-ef": true}

func (c *checker) test(args []field, in dirs) {
	for i, f := range args {
		if f.splits {
			c.refuse(ReasonExpansion, "")
			return
		}
		if !f.exact() {
			continue
		}
		s := f.alts[0].s
		switch {
		case fileTests[s] && i+1 < len(args):
			c.pathField(args[i+1], in)
		case fileCompares[s] && i > 0 && i+1 < len(args):
			c.pathField(args[i-1], in)
			c.pathField(args[i+1], in)
		}
	}
	for _, f := range args {
		for _, a := range f.alts {
			if a.kind == kindGlob {
				c.pathAlt(a, in)
			}
		}
	}
}

// useCwd checks a command that reads the directory the shell is in.
func (c *checker) useCwd(in dirs) {
	if slices.Contains(in, "") {
		c.refuse(ReasonPath, "")
		return
	}
}

// pathField checks a word a program reads as a path.
func (c *checker) pathField(f field, in dirs) {
	if f.splits {
		c.useCwd(in)
	}
	for _, a := range f.alts {
		c.pathAlt(a, in)
	}
}

// pathAlt checks one value of a path: an exact path resolved against each
// directory the shell may be in, a pattern by the directory it reads, a
// match of a pattern by the directory checked when it was read, and a value
// known only at run time not at all.
func (c *checker) pathAlt(a alt, in dirs) {
	switch a.kind {
	case kindExact:
		for _, d := range in {
			if !serves(c.mounts, clean(d, a.s)) {
				c.refuse(ReasonPath, "")
				return
			}
		}
	case kindGlob:
		c.globDirs(a.s, in)
	case kindUnder:
		// Checked where the pattern was read.
	default:
		c.refuse(ReasonExpansion, "")
	}
}

// globDirs checks a pattern by the directory it reads, the part before its
// first element with a wildcard, resolved against each directory the
// shell may be in, and that the part after it climbs out of no directory.
// It answers those directories.
func (c *checker) globDirs(pattern string, in dirs) []string {
	elems := strings.Split(pattern, "/")
	fixed := 0
	for fixed < len(elems) && !hasMeta(elems[fixed]) {
		fixed++
	}
	if slices.Contains(elems[fixed:], "..") {
		c.refuse(ReasonPath, "")
		return nil
	}
	base := strings.Join(elems[:fixed], "/")
	if fixed == 0 {
		base = ""
	} else if base == "" {
		base = "/"
	}
	var out []string
	for _, d := range in {
		dir := d
		if base != "" {
			dir = clean(d, unescapePattern(base))
		}
		if dir == "" || !serves(c.mounts, dir) {
			c.refuse(ReasonPath, "")
			return nil
		}
		out = append(out, dir)
	}
	return out
}
