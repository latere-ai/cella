// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"bytes"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

type grepInv struct {
	args     []string
	operands []int
	// patternArg is the operand that holds the pattern, -1 when -e gave
	// them.
	patternArg int
	patterns   []string

	syntax                                  byte // 'G', 'E' or 'F'
	ignoreCase, invert, word, whole         bool
	number, count, listMatch, listNoMatch   bool
	only, quiet, noMessages, text, noBinary bool
	recursive                               bool
	names                                   int // 0 by count of files, 1 -H, -1 -h
	maxCount                                int64
	before, after                           int
	include, exclude, excludeDir            []string
}

var grepSpec = optSpec{prog: "grep", short: "0123456789A:B:C:EFGHILRace:hilm:noqrsvwxy", code: 2, long: map[string]bool{
	"extended-regexp": false, "fixed-strings": false, "basic-regexp": false, "regexp": true,
	"ignore-case": false, "no-ignore-case": false, "word-regexp": false, "line-regexp": false,
	"invert-match": false, "count": false, "files-with-matches": false, "files-without-match": false,
	"only-matching": false, "quiet": false, "silent": false, "no-messages": false,
	"line-number": false, "with-filename": false, "no-filename": false,
	"recursive": false, "dereference-recursive": false, "max-count": true,
	"after-context": true, "before-context": true, "context": true, "text": false,
	"include": true, "exclude": true, "exclude-dir": true, "color": false, "colour": false,
}}

func parseGrep(args []string) (invocation, error) {
	// --color=WHEN is read here: getopt's table holds options with a
	// value or without, and --color takes one optionally.
	read := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "--color=") || strings.HasPrefix(a, "--colour=") {
			if v := a[strings.IndexByte(a, '=')+1:]; v != "never" && v != "auto" {
				return nil, &usageError{prog: "grep", msg: "invalid argument " + quote(v) + " for '--color'", code: 2}
			}
			a = "--color"
		}
		read = append(read, a)
	}
	ps, err := grepSpec.parse(read)
	if err != nil {
		return nil, err
	}
	g := &grepInv{args: args, syntax: 'G', maxCount: -1, patternArg: -1}
	var context strings.Builder
	gotPattern := false
	for _, o := range ps.opts {
		switch o.name {
		case "E", "extended-regexp":
			g.syntax = 'E'
		case "F", "fixed-strings":
			g.syntax = 'F'
		case "G", "basic-regexp":
			g.syntax = 'G'
		case "e", "regexp":
			g.patterns = append(g.patterns, strings.Split(o.val, "\n")...)
			gotPattern = true
		case "i", "y", "ignore-case":
			g.ignoreCase = true
		case "no-ignore-case":
			g.ignoreCase = false
		case "w", "word-regexp":
			g.word = true
		case "x", "line-regexp":
			g.whole = true
		case "v", "invert-match":
			g.invert = true
		case "c", "count":
			g.count = true
		case "l", "files-with-matches":
			g.listMatch, g.listNoMatch = true, false
		case "L", "files-without-match":
			g.listNoMatch, g.listMatch = true, false
		case "o", "only-matching":
			g.only = true
		case "q", "quiet", "silent":
			g.quiet = true
		case "s", "no-messages":
			g.noMessages = true
		case "n", "line-number":
			g.number = true
		case "H", "with-filename":
			g.names = 1
		case "h", "no-filename":
			g.names = -1
		case "r", "R", "recursive", "dereference-recursive":
			g.recursive = true
		case "a", "text":
			g.text = true
		case "I":
			g.noBinary = true
		case "m", "max-count":
			n, err := strconv.ParseInt(o.val, 10, 64)
			if err != nil {
				return nil, &usageError{prog: "grep", msg: "invalid max count", code: 2, noHint: true}
			}
			g.maxCount = n
		case "A", "after-context", "B", "before-context", "C", "context":
			n, err := strconv.Atoi(o.val)
			if err != nil || n < 0 {
				return nil, &usageError{prog: "grep", msg: o.val + ": invalid context length argument", code: 2, noHint: true}
			}
			switch o.name {
			case "A", "after-context":
				g.after = n
			case "B", "before-context":
				g.before = n
			default:
				g.before, g.after = n, n
			}
		case "include":
			g.include = append(g.include, o.val)
		case "exclude":
			g.exclude = append(g.exclude, o.val)
		case "exclude-dir":
			g.excludeDir = append(g.excludeDir, o.val)
		case "color", "colour":
		default:
			// -NUM is -C NUM.
			if len(o.name) == 1 && o.name[0] >= '0' && o.name[0] <= '9' {
				context.WriteString(o.name)
				n, err := strconv.Atoi(context.String())
				if err != nil {
					return nil, &usageError{prog: "grep", msg: context.String() + ": invalid context length argument", code: 2, noHint: true}
				}
				g.before, g.after = n, n
			}
		}
	}
	g.operands = ps.operands
	if !gotPattern {
		if len(g.operands) == 0 {
			return nil, &usageError{prog: "grep", line: "Usage: grep [OPTION]... PATTERNS [FILE]...", code: 2}
		}
		g.patternArg = g.operands[0]
		g.patterns = strings.Split(args[g.patternArg], "\n")
		g.operands = g.operands[1:]
	}
	return g, nil
}

func (g *grepInv) paths() []int {
	var out []int
	for _, i := range g.operands {
		if g.args[i] != "-" {
			out = append(out, i)
		}
	}
	return out
}

func (g *grepInv) cwd() bool { return g.recursive && len(g.operands) == 0 }

// regexp is the patterns as one Go expression, matched leftmost longest as
// POSIX matches.
func (g *grepInv) regexp() (*regexp.Regexp, error) {
	alts := make([]string, 0, len(g.patterns))
	for _, p := range g.patterns {
		var e string
		switch g.syntax {
		case 'F':
			e = regexp.QuoteMeta(p)
		case 'E':
			e = fromERE(p)
		default:
			e = fromBRE(p)
		}
		switch {
		case g.whole:
			e = "^(?:" + e + ")$"
		case g.word:
			e = `\b(?:` + e + `)\b`
		}
		alts = append(alts, e)
	}
	expr := strings.Join(alts, "|")
	if g.ignoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, err
	}
	re.Longest()
	return re, nil
}

// fromERE is a POSIX extended expression as Go reads one: GNU's word
// boundaries become \b.
func fromERE(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' && i+1 < len(p) {
			switch p[i+1] {
			case '<', '>':
				b.WriteString(`\b`)
			default:
				b.WriteByte('\\')
				b.WriteByte(p[i+1])
			}
			i++
			continue
		}
		if p[i] == '[' {
			j := bracketEnd(p, i)
			b.WriteString(p[i:j])
			i = j - 1
			continue
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// fromBRE is a POSIX basic expression as Go reads one: ( ) { } | + ? are
// literal unless escaped, as GNU reads them, and * at the start is
// literal.
func fromBRE(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\' && i+1 < len(p):
			n := p[i+1]
			i++
			switch n {
			case '(', ')', '{', '}', '|', '+', '?':
				b.WriteByte(n)
			case '<', '>':
				b.WriteString(`\b`)
			default:
				b.WriteByte('\\')
				b.WriteByte(n)
			}
		case c == '[':
			j := bracketEnd(p, i)
			b.WriteString(p[i:j])
			i = j - 1
		case strings.IndexByte("(){}|+?", c) >= 0:
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '*' && (i == 0 || p[i-1] == '^' && i == 1):
			b.WriteString(`\*`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// bracketEnd is the index after a bracket expression starting at i.
func bracketEnd(p string, i int) int {
	j := i + 1
	if j < len(p) && p[j] == '^' {
		j++
	}
	if j < len(p) && p[j] == ']' {
		j++
	}
	for j < len(p) && p[j] != ']' {
		if p[j] == '[' && j+1 < len(p) && (p[j+1] == ':' || p[j+1] == '.' || p[j+1] == '=') {
			end := strings.Index(p[j+2:], string(p[j+1])+"]")
			if end >= 0 {
				j += end + 4
				continue
			}
		}
		j++
	}
	if j < len(p) {
		j++
	}
	return j
}

// grepRun is one grep's state across its files.
type grepRun struct {
	g         *grepInv
	p         *proc
	re        *regexp.Regexp
	showNames bool
	matched   bool
	failed    bool
	out       *buffer
}

func (g *grepInv) run(p *proc) int {
	re, err := g.regexp()
	if err != nil {
		p.errorf("%s", strings.TrimPrefix(err.Error(), "error parsing regexp: "))
		return 2
	}
	r := &grepRun{g: g, p: p, re: re, out: p.buffer()}
	names := operandStrings(g.args, g.operands)
	implicit := false
	if len(names) == 0 {
		if g.recursive {
			names, implicit = []string{"."}, true
		} else {
			names = []string{"-"}
		}
	}
	r.showNames = g.names == 1 || (g.names == 0 && (len(names) > 1 || g.recursive))
	for _, name := range names {
		if r.done() {
			break
		}
		if name == "-" {
			data, err := p.readStdin()
			if err != nil {
				r.fail("(standard input)", err)
				continue
			}
			r.file("(standard input)", data)
			continue
		}
		abs := p.abs(name)
		info, err := p.s.files.stat(p.ctx, abs)
		if err != nil {
			r.fail(name, err)
			continue
		}
		if info.IsDir() {
			if !g.recursive {
				r.fail(name, syscall.EISDIR)
				continue
			}
			label := name
			if implicit {
				label = ""
			}
			r.walk(abs, label)
			continue
		}
		if !r.included(name) {
			continue
		}
		data, err := p.s.files.read(p.ctx, abs)
		if err != nil {
			r.fail(name, err)
			continue
		}
		r.file(name, data)
	}
	p.flush(r.out)
	switch {
	case r.failed && (!g.quiet || !r.matched):
		return 2
	case r.matched:
		return 0
	}
	return 1
}

func (r *grepRun) done() bool { return r.g.quiet && r.matched }

func (r *grepRun) fail(name string, err error) {
	r.failed = true
	if !r.g.noMessages {
		r.p.errorf("%s: %s", name, errorText(err))
	}
}

// included reports whether --include and --exclude keep a file.
func (r *grepRun) included(name string) bool {
	base := path.Base(name)
	for _, x := range r.g.exclude {
		if ok, _ := path.Match(x, base); ok {
			return false
		}
	}
	if len(r.g.include) == 0 {
		return true
	}
	for _, x := range r.g.include {
		if ok, _ := path.Match(x, base); ok {
			return true
		}
	}
	return false
}

// walk searches a directory's files, in the order ls lists them.
func (r *grepRun) walk(abs, label string) {
	entries, err := r.p.s.files.readDir(r.p.ctx, abs)
	if err != nil {
		r.fail(label, err)
		return
	}
	sortInfos(entries, r.p.byteOrder())
	for _, e := range entries {
		if r.done() {
			return
		}
		name := e.Name()
		child := name
		if label != "" {
			child = strings.TrimSuffix(label, "/") + "/" + name
		}
		if e.IsDir() {
			skip := false
			for _, x := range r.g.excludeDir {
				if ok, _ := path.Match(x, name); ok {
					skip = true
				}
			}
			if !skip {
				r.walk(path.Join(abs, name), child)
			}
			continue
		}
		if !r.included(name) {
			continue
		}
		data, err := r.p.s.files.read(r.p.ctx, path.Join(abs, name))
		if err != nil {
			r.fail(child, err)
			continue
		}
		r.file(child, data)
	}
}

// file searches one file's content.
func (r *grepRun) file(name string, data []byte) {
	g := r.g
	binary := !g.text && bytes.IndexByte(data, 0) >= 0
	if binary && g.noBinary {
		return
	}
	prefix := func(n int, sep byte) {
		if r.showNames {
			r.out.puts(name)
			r.out.putByte(sep)
		}
		if g.number {
			r.out.puts(strconv.Itoa(n))
			r.out.putByte(sep)
		}
	}
	lines := splitLines(data)
	var count int64
	lastPrinted := -1
	pendingAfter := 0
	printedAny := false
	quietOut := g.quiet || g.count || g.listMatch || g.listNoMatch || binary
	for i, line := range lines {
		if g.maxCount >= 0 && count >= g.maxCount {
			// Context after the last match is still printed.
			if pendingAfter > 0 && !quietOut && !g.only {
				prefix(i+1, '-')
				r.out.put(line)
				r.out.putByte('\n')
				lastPrinted = i
				pendingAfter--
				continue
			}
			break
		}
		hit := r.re.Match(line) != g.invert
		if !hit {
			if pendingAfter > 0 && !quietOut && !g.only {
				prefix(i+1, '-')
				r.out.put(line)
				r.out.putByte('\n')
				lastPrinted = i
				pendingAfter--
			}
			continue
		}
		count++
		r.matched = true
		if g.quiet {
			return
		}
		if quietOut {
			continue
		}
		if (g.before > 0 || g.after > 0) && printedAny && lastPrinted < i-1-g.before {
			r.out.puts("--\n")
		}
		if !g.only {
			for j := max(i-g.before, lastPrinted+1); j < i; j++ {
				prefix(j+1, '-')
				r.out.put(lines[j])
				r.out.putByte('\n')
			}
		}
		if g.only {
			if !g.invert {
				for _, m := range r.re.FindAll(line, -1) {
					if len(m) == 0 {
						continue
					}
					prefix(i+1, ':')
					r.out.put(m)
					r.out.putByte('\n')
				}
			}
		} else {
			prefix(i+1, ':')
			r.out.put(line)
			r.out.putByte('\n')
		}
		lastPrinted, pendingAfter, printedAny = i, g.after, true
	}
	switch {
	case g.count:
		if r.showNames {
			r.out.printf("%s:%d\n", name, count)
		} else {
			r.out.printf("%d\n", count)
		}
	case g.listMatch:
		if count > 0 {
			r.out.puts(name + "\n")
		}
	case g.listNoMatch:
		if count == 0 {
			r.out.puts(name + "\n")
		}
	case binary && count > 0:
		r.out.printf("%s: %s: binary file matches\n", r.p.name, name)
	}
}

// splitLines is data's lines without their newlines; a last line without
// one is a line.
func splitLines(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	lines := bytes.Split(data, []byte{'\n'})
	if len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	return lines
}
