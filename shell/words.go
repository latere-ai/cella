// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"slices"
	"strings"

	"mvdan.cc/sh/v3/pattern"
	"mvdan.cc/sh/v3/syntax"
)

// kind is what the walk knows of one value of a word.
type kind int

const (
	// kindExact is a value known in full.
	kindExact kind = iota
	// kindGlob is a pattern the shell expands against the directories it
	// reads; s is the pattern.
	kindGlob
	// kindUnder is a path in the directory s, a match of a pattern.
	kindUnder
	// kindDynamic is a value known only once something has run.
	kindDynamic
)

// alt is one value a word may take.
type alt struct {
	kind kind
	s    string
}

// maxAlts bounds the values the walk keeps for one word; past it the word
// is a value known only at run time.
const maxAlts = 32

// field is one word after expansion, with every value it may take.
type field struct {
	alts []alt
	// splits is set when an unquoted expansion may make the word more
	// words or none at run time.
	splits bool
}

func (f field) exact() bool {
	return len(f.alts) == 1 && f.alts[0].kind == kindExact && !f.splits
}

func (f field) dynamic() bool {
	for _, a := range f.alts {
		if a.kind == kindDynamic {
			return true
		}
	}
	return false
}

// wordMode is where a word stands, which decides how it expands.
type wordMode int

const (
	// wordArg is an argument: split and globbed.
	wordArg wordMode = iota
	// wordTarget is a redirection's file: neither split nor globbed.
	wordTarget
	// wordPattern is a case pattern: matched, never globbed.
	wordPattern
	// wordHeredoc is a here document's body: expanded, never a path.
	wordHeredoc
)

// words expands the words of one command as far as the walk can, and
// records a refusal for what it cannot.
func (c *checker) words(ws []*syntax.Word, in dirs, mode wordMode) []field {
	var out []field
	for _, w := range ws {
		if c.refused() {
			return nil
		}
		out = append(out, c.word(w, in, mode)...)
	}
	return out
}

func (c *checker) word(w *syntax.Word, in dirs, mode wordMode) []field {
	if w == nil {
		return nil
	}
	if mode == wordArg {
		cp := *w
		if syntax.SplitBraces(&cp) {
			// The interpreter expands braces as bash does; dash, which a
			// machine's /bin/sh is, leaves them.
			c.refuse(ReasonConstruct, "")
			return nil
		}
	}
	if len(w.Parts) > 0 && mode != wordHeredoc {
		if l, ok := w.Parts[0].(*syntax.Lit); ok && strings.HasPrefix(l.Value, "~") {
			// A tilde is a home directory, the machine's.
			c.refuse(ReasonExpansion, "")
			return nil
		}
	}
	single := len(w.Parts) == 1
	if mode == wordArg && !single {
		for _, part := range w.Parts {
			if l, ok := part.(*syntax.Lit); ok && openBracket(l.Value) {
				// A set may close in a quoted part: whether the word is a
				// pattern is the interpreter's to decide.
				c.refuse(ReasonExpansion, "")
				return nil
			}
		}
	}
	alts := []alt{{kind: kindExact}}
	splits := false
	for _, part := range w.Parts {
		vals, sp, ok := c.part(part, in, mode, false)
		if !ok {
			return nil
		}
		splits = splits || sp
		alts = concat(alts, vals)
	}
	if mode == wordArg && single {
		if _, ok := w.Parts[0].(*syntax.ParamExp); ok && len(alts) == 1 && alts[0].kind == kindExact {
			// An unquoted variable alone is split on blanks, and is no
			// word at all when empty.
			fs := strings.Fields(alts[0].s)
			out := make([]field, 0, len(fs))
			for _, s := range fs {
				out = append(out, field{alts: unquotedAlts(s)})
			}
			return out
		}
	}
	if mode != wordArg {
		for i, a := range alts {
			if a.kind == kindGlob {
				// A redirection's file and a case pattern are not
				// expanded against any directory.
				alts[i] = alt{kind: kindExact, s: unescapePattern(a.s)}
			}
		}
		splits = false
	}
	return []field{{alts: alts, splits: splits}}
}

// unquotedAlts is the value of an unquoted variable: a pattern when it
// holds a wildcard, which the shell then expands.
func unquotedAlts(s string) []alt {
	if hasMeta(s) {
		return []alt{{kind: kindGlob, s: s}}
	}
	return []alt{{kind: kindExact, s: s}}
}

// part is the values one part of a word may take, whether it may split,
// and false when the walk refused it.
func (c *checker) part(part syntax.WordPart, in dirs, mode wordMode, quoted bool) ([]alt, bool, bool) {
	switch x := part.(type) {
	case *syntax.Lit:
		if quoted || mode == wordHeredoc {
			return []alt{{kind: kindExact, s: unescapeQuoted(x.Value, mode == wordHeredoc)}}, false, true
		}
		if mode == wordArg && hasMeta(x.Value) {
			return []alt{{kind: kindGlob, s: x.Value}}, false, true
		}
		return []alt{{kind: kindExact, s: unescapeLit(x.Value)}}, false, true
	case *syntax.SglQuoted:
		if x.Dollar {
			c.refuse(ReasonConstruct, "")
			return nil, false, false
		}
		return []alt{{kind: kindExact, s: x.Value}}, false, true
	case *syntax.DblQuoted:
		if x.Dollar {
			c.refuse(ReasonConstruct, "")
			return nil, false, false
		}
		alts := []alt{{kind: kindExact}}
		for _, p := range x.Parts {
			vals, _, ok := c.part(p, in, mode, true)
			if !ok {
				return nil, false, false
			}
			alts = concat(alts, vals)
		}
		return alts, false, true
	case *syntax.ParamExp:
		vals, ok := c.param(x, in)
		if !ok {
			return nil, false, false
		}
		if quoted || mode != wordArg {
			return vals, false, true
		}
		out := make([]alt, 0, len(vals))
		splits := false
		for _, v := range vals {
			switch v.kind {
			case kindExact:
				if strings.ContainsAny(v.s, " \t\n") || v.s == "" {
					splits = true
				}
				out = append(out, unquotedAlts(v.s)...)
			case kindUnder, kindDynamic:
				splits = true
				out = append(out, v)
			default:
				out = append(out, v)
			}
		}
		return out, splits, true
	case *syntax.ArithmExp:
		if !c.arithm(x) {
			return nil, false, false
		}
		return []alt{{kind: kindDynamic}}, false, true
	case *syntax.CmdSubst, *syntax.ProcSubst:
		c.refuse(ReasonExpansion, "")
		return nil, false, false
	}
	c.refuse(ReasonConstruct, "")
	return nil, false, false
}

// param is the values a parameter expansion may take.
func (c *checker) param(x *syntax.ParamExp, in dirs) ([]alt, bool) {
	if x.Param == nil || x.NestedParam != nil || x.Excl || x.Index != nil || x.Names != 0 || x.Width || x.Flags != nil {
		c.refuse(ReasonConstruct, "")
		return nil, false
	}
	name := x.Param.Value
	plain := !x.Length && x.Slice == nil && x.Repl == nil && x.Exp == nil
	var vals []alt
	switch {
	case name == "?" || name == "#" || name == "@" || name == "*" || (name != "0" && isDigits(name)):
		vals = []alt{{kind: kindDynamic}}
	case name == "PWD":
		if slices.Contains(in, "") {
			// The shell's directory is the machine's.
			c.refuse(ReasonExpansion, "")
			return nil, false
		}
		if len(in) == 1 {
			vals = []alt{{kind: kindExact, s: in[0]}}
		} else {
			vals = []alt{{kind: kindDynamic}}
		}
	default:
		v, ok := c.vars[name]
		if !ok {
			// A variable the line does not set is the machine's:
			// HOME, PATH and the rest.
			c.refuse(ReasonExpansion, "")
			return nil, false
		}
		vals = v
	}
	if x.Exp != nil && x.Exp.Word != nil {
		// ${a:-word} and its kin may expand the word.
		if c.word(x.Exp.Word, in, wordHeredoc); c.refused() {
			return nil, false
		}
	}
	if !plain {
		return []alt{{kind: kindDynamic}}, true
	}
	return vals, true
}

// arithm reports whether an arithmetic expansion names only variables the
// line set, and refuses it otherwise.
func (c *checker) arithm(x *syntax.ArithmExp) bool {
	ok := true
	syntax.Walk(x.X, func(n syntax.Node) bool {
		if !ok {
			return false
		}
		switch n := n.(type) {
		case *syntax.ParamExp:
			if n.Param == nil {
				ok = false
				break
			}
			if _, known := c.vars[n.Param.Value]; !known && !isDigits(n.Param.Value) && n.Param.Value != "?" && n.Param.Value != "#" {
				ok = false
			}
		case *syntax.Lit:
			if syntax.ValidName(n.Value) {
				if _, known := c.vars[n.Value]; !known {
					ok = false
				}
			}
		case *syntax.CmdSubst:
			ok = false
		case *syntax.BinaryArithm:
			// An assignment changes a variable the walk has read already.
			switch n.Op {
			case syntax.Assgn, syntax.AddAssgn, syntax.SubAssgn, syntax.MulAssgn, syntax.QuoAssgn,
				syntax.RemAssgn, syntax.AndAssgn, syntax.OrAssgn, syntax.XorAssgn, syntax.ShlAssgn,
				syntax.ShrAssgn, syntax.AndBoolAssgn, syntax.OrBoolAssgn, syntax.XorBoolAssgn, syntax.PowAssgn:
				ok = false
			}
		case *syntax.UnaryArithm:
			if n.Op == syntax.Inc || n.Op == syntax.Dec {
				ok = false
			}
		}
		return true
	})
	if !ok {
		c.refuse(ReasonExpansion, "")
	}
	return ok
}

// assignValue is the values an assignment's word may take: an assignment
// is neither split nor globbed.
func (c *checker) assignValue(w *syntax.Word, in dirs) []alt {
	fields := c.word(w, in, wordTarget)
	if c.refused() {
		return nil
	}
	if len(fields) == 0 {
		return []alt{{kind: kindExact}}
	}
	return fields[0].alts
}

// concat is every value of a word followed by every value of its next
// part.
func concat(a, b []alt) []alt {
	out := make([]alt, 0, len(a)*len(b))
	for _, x := range a {
		for _, y := range b {
			out = append(out, join(x, y))
			if len(out) > maxAlts {
				return []alt{{kind: kindDynamic}}
			}
		}
	}
	return out
}

func join(x, y alt) alt {
	switch {
	case x.kind == kindDynamic || y.kind == kindDynamic:
		return alt{kind: kindDynamic}
	case x.kind == kindExact && y.kind == kindExact:
		return alt{kind: kindExact, s: x.s + y.s}
	case x.kind == kindExact && x.s == "":
		return y
	case y.kind == kindExact && y.s == "":
		return x
	case x.kind == kindExact && y.kind == kindGlob:
		return alt{kind: kindGlob, s: pattern.QuoteMeta(x.s, 0) + y.s}
	case x.kind == kindGlob && y.kind == kindExact:
		return alt{kind: kindGlob, s: x.s + pattern.QuoteMeta(y.s, 0)}
	case x.kind == kindGlob && y.kind == kindGlob:
		return alt{kind: kindGlob, s: x.s + y.s}
	case x.kind == kindUnder && y.kind == kindExact && !climbs(y.s):
		// A match followed by text stays in the match's directory unless
		// the text climbs out of it.
		return x
	}
	return alt{kind: kindDynamic}
}

// climbs reports whether text appended to a path has a ".." element.
func climbs(s string) bool {
	return strings.Contains("/"+s+"/", "/../")
}

// hasMeta reports whether an unquoted literal holds a wildcard not escaped
// with a backslash.
func hasMeta(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '*', '?':
			return true
		case '[':
			if bracketCloses(s, i) {
				return true
			}
		}
	}
	return false
}

// bracketCloses reports whether the bracket at s[i] opens a set a later
// bracket closes; one that none closes is a plain character.
func bracketCloses(s string, i int) bool {
	j := i + 1
	if j < len(s) && (s[j] == '!' || s[j] == '^') {
		j++
	}
	// A closing bracket first in the set is a member of it.
	return strings.IndexByte(s[min(j+1, len(s)):], ']') >= 0
}

// openBracket reports whether s has a bracket no later bracket in s
// closes, which a bracket in another part of the word might.
func openBracket(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '[':
			if !bracketCloses(s, i) {
				return true
			}
		}
	}
	return false
}

// unescapeLit is an unquoted literal's value: each backslash quotes the
// character after it, and a backslash before a newline joins the lines.
func unescapeLit(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// unescapeQuoted is a literal's value inside double quotes, or in a here
// document's body: a backslash quotes only $, `, \, a newline and, in
// quotes, ".
func unescapeQuoted(s string, heredoc bool) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch n := s[i+1]; {
			case n == '$' || n == '`' || n == '\\' || (n == '"' && !heredoc):
				i++
			case n == '\n':
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// unescapePattern is a pattern's text as a literal path.
func unescapePattern(s string) string { return unescapeLit(s) }

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
