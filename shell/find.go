// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/pattern"
)

// findExpr is one node of find's expression.
type findExpr interface {
	eval(e findEntry) bool
}

// findEntry is a path find visits.
type findEntry struct {
	path  string
	name  string
	info  fs.FileInfo
	depth int
	empty bool
}

type (
	findAnd   struct{ x, y findExpr }
	findOr    struct{ x, y findExpr }
	findNot   struct{ x findExpr }
	findConst bool
	findName  struct {
		re    *regexp.Regexp
		whole bool
	}
	findType  string
	findEmpty struct{}
	findPrint struct{ out *strings.Builder }
)

func (a findAnd) eval(e findEntry) bool { return a.x.eval(e) && a.y.eval(e) }
func (o findOr) eval(e findEntry) bool  { return o.x.eval(e) || o.y.eval(e) }
func (n findNot) eval(e findEntry) bool { return !n.x.eval(e) }
func (c findConst) eval(findEntry) bool { return bool(c) }

func (n findName) eval(e findEntry) bool {
	if n.whole {
		return n.re.MatchString(e.path)
	}
	return n.re.MatchString(e.name)
}

func (t findType) eval(e findEntry) bool {
	for c := range strings.SplitSeq(string(t), ",") {
		switch {
		case c == "d" && e.info.IsDir(), c == "f" && e.info.Mode().IsRegular():
			return true
		}
	}
	return false
}

func (findEmpty) eval(e findEntry) bool {
	if e.info.IsDir() {
		return e.empty
	}
	return e.info.Size() == 0
}

func (p findPrint) eval(e findEntry) bool {
	p.out.WriteString(e.path)
	p.out.WriteByte('\n')
	return true
}

type findInv struct {
	args     []string
	starts   []int
	expr     findExpr
	print    bool // the expression has no action: every true path is printed
	maxDepth int
	minDepth int
	depth    bool
	out      *strings.Builder
	usesDir  bool // -empty reads every directory it visits
}

// findError is a find expression find refuses, said as findutils says it.
func findError(format string, args ...any) error {
	return &usageError{prog: "find", msg: fmt.Sprintf(format, args...), code: 1, noHint: true}
}

func parseFind(args []string) (invocation, error) {
	inv := &findInv{args: args, maxDepth: -1, out: &strings.Builder{}}
	i := 0
	for i < len(args) && (args[i] == "-H" || args[i] == "-L" || args[i] == "-P") {
		i++
	}
	for ; i < len(args); i++ {
		a := args[i]
		if (strings.HasPrefix(a, "-") && len(a) > 1) || a == "(" || a == "!" || a == ")" {
			break
		}
		inv.starts = append(inv.starts, i)
	}
	fp := &findParser{args: args, pos: i, inv: inv}
	if fp.pos < len(args) {
		expr, err := fp.or()
		if err != nil {
			return nil, err
		}
		if fp.pos < len(args) {
			if args[fp.pos] == ")" {
				return nil, findError("invalid expression; you have too many ')'")
			}
			return nil, findError("paths must precede expression: %s", quote(args[fp.pos]))
		}
		inv.expr = expr
	} else {
		inv.expr = findConst(true)
	}
	inv.print = !fp.action
	return inv, nil
}

// findParser reads find's expression: an or of ands of possibly negated
// primaries, an and written by juxtaposition as well.
type findParser struct {
	args   []string
	pos    int
	inv    *findInv
	action bool
}

func (p *findParser) peek() string {
	if p.pos < len(p.args) {
		return p.args[p.pos]
	}
	return ""
}

func (p *findParser) or() (findExpr, error) {
	x, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.peek() == "-o" || p.peek() == "-or" {
		p.pos++
		y, err := p.and()
		if err != nil {
			return nil, err
		}
		x = findOr{x, y}
	}
	return x, nil
}

func (p *findParser) and() (findExpr, error) {
	x, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		switch p.peek() {
		case "", "-o", "-or", ")":
			return x, nil
		case "-a", "-and":
			p.pos++
		}
		y, err := p.unary()
		if err != nil {
			return nil, err
		}
		x = findAnd{x, y}
	}
}

func (p *findParser) unary() (findExpr, error) {
	switch p.peek() {
	case "!", "-not":
		p.pos++
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return findNot{x}, nil
	case "(":
		p.pos++
		x, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.peek() != ")" {
			return nil, findError("invalid expression; I was expecting to find a ')' somewhere but did not see one.")
		}
		p.pos++
		return x, nil
	case "":
		return nil, findError("invalid expression; you have used a binary operator '%s' with nothing before it.", p.args[p.pos-1])
	}
	return p.primary()
}

func (p *findParser) arg(name string) (string, error) {
	if p.pos >= len(p.args) {
		return "", findError("missing argument to `%s'", name)
	}
	v := p.args[p.pos]
	p.pos++
	return v, nil
}

func (p *findParser) primary() (findExpr, error) {
	name := p.args[p.pos]
	p.pos++
	switch name {
	case "-name", "-iname", "-path", "-ipath", "-wholename", "-iwholename":
		v, err := p.arg(name)
		if err != nil {
			return nil, err
		}
		mode := pattern.EntireString | pattern.NoGlobStar | pattern.GlobLeadingDot
		if strings.HasPrefix(name, "-i") {
			mode |= pattern.NoGlobCase
		}
		expr, err := pattern.Regexp(v, mode)
		if err != nil {
			return nil, findError("invalid pattern %s", quote(v))
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, findError("invalid pattern %s", quote(v))
		}
		return findName{re: re, whole: !strings.HasSuffix(name, "name") || strings.HasSuffix(name, "wholename")}, nil
	case "-type":
		v, err := p.arg(name)
		if err != nil {
			return nil, err
		}
		for c := range strings.SplitSeq(v, ",") {
			if len(c) != 1 || !strings.Contains("bcdpflsD", c) {
				return nil, findError("Unknown argument to -type: %s", c)
			}
		}
		return findType(v), nil
	case "-maxdepth", "-mindepth":
		v, err := p.arg(name)
		if err != nil {
			return nil, err
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, findError("Expected a positive decimal integer argument to %s, but got `%s'", name, v)
		}
		if name == "-maxdepth" {
			p.inv.maxDepth = n
		} else {
			p.inv.minDepth = n
		}
		return findConst(true), nil
	case "-depth":
		p.inv.depth = true
		return findConst(true), nil
	case "-print":
		p.action = true
		return findPrint{out: p.inv.out}, nil
	case "-true":
		return findConst(true), nil
	case "-false":
		return findConst(false), nil
	case "-empty":
		p.inv.usesDir = true
		return findEmpty{}, nil
	}
	return nil, findError("unknown predicate `%s'", name)
}

func (f *findInv) paths() []int { return f.starts }

func (f *findInv) cwd() bool { return len(f.starts) == 0 }

func (f *findInv) run(p *proc) int {
	starts := operandStrings(f.args, f.starts)
	if len(starts) == 0 {
		starts = []string{"."}
	}
	code := 0
	for _, s := range starts {
		abs := p.abs(s)
		info, err := p.s.files.stat(p.ctx, abs)
		if err != nil {
			p.errorf("%s: %s", quoteLocale(s), errorText(err))
			code = 1
			continue
		}
		if !f.visit(p, s, abs, info, 0) {
			code = 1
		}
	}
	p.write([]byte(f.out.String()))
	return code
}

// visit tests one path and, for a directory, what is under it, in the
// order ls lists them; false when a directory could not be read.
func (f *findInv) visit(p *proc, shown, abs string, info fs.FileInfo, depth int) bool {
	ok := true
	var entries []fs.FileInfo
	if info.IsDir() && (f.maxDepth < 0 || depth < f.maxDepth || f.usesDir) {
		var err error
		entries, err = p.s.files.readDir(p.ctx, abs)
		if err != nil {
			p.errorf("%s: %s", quoteLocale(shown), errorText(err))
			ok = false
		}
		sortInfos(entries, p.byteOrder())
	}
	e := findEntry{path: shown, name: path.Base(shown), info: info, depth: depth, empty: len(entries) == 0}
	if shown == "/" {
		e.name = "/"
	}
	test := func() {
		if depth >= f.minDepth && f.expr.eval(e) && f.print {
			f.out.WriteString(shown)
			f.out.WriteByte('\n')
		}
	}
	if !f.depth {
		test()
	}
	if info.IsDir() && (f.maxDepth < 0 || depth < f.maxDepth) {
		for _, c := range entries {
			child := strings.TrimSuffix(shown, "/") + "/" + c.Name()
			if !f.visit(p, child, path.Join(abs, c.Name()), c, depth+1) {
				ok = false
			}
		}
	}
	if f.depth {
		test()
	}
	return ok
}
