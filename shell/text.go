// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"mvdan.cc/sh/v3/expand"
)

// echo is dash's: -n alone as the first argument leaves out the newline,
// every other argument is printed, and backslash escapes are read, \c
// ending the output.
func echo(p *proc, args []string) int {
	newline := true
	if len(args) > 0 && args[0] == "-n" {
		newline = false
		args = args[1:]
	}
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		if stop := writeEscaped(&b, a); stop {
			p.write([]byte(b.String()))
			return 0
		}
	}
	if newline {
		b.WriteByte('\n')
	}
	p.write([]byte(b.String()))
	return 0
}

// printf is the shell's printf, run as one of the package's programs so
// its output passes the line's bounds: the format is applied to the
// arguments until they are used, as the interpreter's own applies it.
func printf(p *proc, args []string) int {
	if len(args) == 0 {
		p.eprintf("usage: printf format [arguments]\n")
		return 2
	}
	format, args := args[0], args[1:]
	// One pass of the format is made whole before it is written.
	if !p.hold(formatSize(format, args)) {
		return 1
	}
	for {
		s, n, err := expand.Format(nil, format, args)
		if err != nil {
			p.eprintf("%v\n", err)
			return 1
		}
		p.write([]byte(s))
		args = args[n:]
		if n == 0 || len(args) == 0 {
			return 0
		}
	}
}

// maxWidth is the widest field Go's fmt pads to; it prints a wider one's
// width as an error.
const maxWidth = 1e6

// formatSize bounds how long one pass of a printf format can be: the
// format, every argument, and every directive's width.
func formatSize(format string, args []string) int {
	n := len(format)
	for _, a := range args {
		n += len(a)
	}
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		j := i + 1
		for j < len(format) && strings.IndexByte("+- ", format[j]) >= 0 {
			j++
		}
		width := 0
		for ; j < len(format) && format[j] >= '0' && format[j] <= '9'; j++ {
			width = min(width*10+int(format[j]-'0'), maxWidth+1)
		}
		if width <= maxWidth {
			n += width
		}
	}
	return n
}

// writeEscaped writes s with dash echo's escapes read, and reports a \c.
func writeEscaped(b *strings.Builder, s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'c':
			return true
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '\\':
			b.WriteByte('\\')
		case '0':
			n, j := 0, i+1
			for ; j < len(s) && j < i+4 && s[j] >= '0' && s[j] <= '7'; j++ {
				n = n*8 + int(s[j]-'0')
			}
			b.WriteByte(byte(n))
			i = j - 1
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return false
}

// fileOps is an invocation whose operands are all paths, "-" being its
// standard input.
type fileOps struct {
	args     []string
	operands []int
}

func (f fileOps) paths() []int {
	var out []int
	for _, i := range f.operands {
		if f.args[i] != "-" {
			out = append(out, i)
		}
	}
	return out
}

func (fileOps) cwd() bool { return false }

func (f fileOps) names() []string {
	if len(f.operands) == 0 {
		return []string{"-"}
	}
	return operandStrings(f.args, f.operands)
}

// cat

type catInv struct {
	fileOps
	number, nonblank, squeeze, ends, tabs bool
}

var catSpec = optSpec{prog: "cat", short: "AbeEnstTuv", code: 1, long: map[string]bool{
	"number": false, "number-nonblank": false, "squeeze-blank": false, "show-ends": false, "show-tabs": false,
}}

func parseCat(args []string) (invocation, error) {
	ps, err := catSpec.parse(args)
	if err != nil {
		return nil, err
	}
	inv := &catInv{args: args, operands: ps.operands}
	for _, o := range ps.opts {
		switch o.name {
		case "n", "number":
			inv.number = true
		case "b", "number-nonblank":
			inv.nonblank = true
		case "s", "squeeze-blank":
			inv.squeeze = true
		case "E", "show-ends":
			inv.ends = true
		case "T", "show-tabs":
			inv.tabs = true
		case "e":
			inv.ends = true
		case "t":
			inv.tabs = true
		case "A":
			inv.ends, inv.tabs = true, true
		}
	}
	return inv, nil
}

func (c *catInv) run(p *proc) int {
	code := 0
	plain := !c.number && !c.nonblank && !c.squeeze && !c.ends && !c.tabs
	n := 0
	blank := false
	for _, name := range c.names() {
		data, err := p.readInput(name)
		if err != nil {
			p.errorf("%s: %s", quoteIfNeeded(name), errorText(err))
			code = 1
			continue
		}
		if plain {
			p.write(data)
			continue
		}
		b := p.buffer()
		for len(data) > 0 {
			line, rest, nl := bytes.Cut(data, []byte{'\n'})
			data = rest
			empty := len(line) == 0
			if c.squeeze && empty && nl && blank {
				continue
			}
			blank = empty && nl
			if c.nonblank && !empty || c.number && !c.nonblank {
				n++
				b.printf("%6d\t", n)
			}
			if c.tabs {
				line = bytes.ReplaceAll(line, []byte{'\t'}, []byte("^I"))
			}
			b.put(line)
			if nl {
				if c.ends {
					b.putByte('$')
				}
				b.putByte('\n')
			}
		}
		p.flush(b)
	}
	return code
}

// head and tail

type headInv struct {
	fileOps
	tail    bool
	lines   bool
	count   int64
	fromEnd bool // head: all but the last count; tail: from the count-th
	quiet   bool
	verbose bool
}

// count reads a count with an optional sign: head's - means all but the
// last, tail's + means from that one on.
func readCount(prog, flag, s string, tail bool) (n int64, signed bool, err error) {
	v := s
	if strings.HasPrefix(v, "+") && tail {
		signed, v = true, v[1:]
	} else if strings.HasPrefix(v, "-") && !tail {
		signed, v = true, v[1:]
	} else if strings.HasPrefix(v, "-") || strings.HasPrefix(v, "+") {
		v = v[1:]
	}
	n, err = readSize(v)
	if err != nil || n < 0 {
		what := "number of lines"
		if flag == "c" {
			what = "number of bytes"
		}
		return 0, false, &usageError{prog: prog, msg: fmt.Sprintf("invalid %s: %s", what, quote(s)), code: 1, noHint: true}
	}
	return n, signed, nil
}

// readSize is a count with GNU's multiplier suffixes: b for 512, K or k
// for 1024 and M, G, T and on for its powers, KB and the like for powers
// of 1000, and KiB and the like as plain K.
func readSize(s string) (int64, error) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return 0, err
	}
	suffix := s[i:]
	if suffix == "" {
		return n, nil
	}
	if suffix == "b" {
		return n * 512, nil
	}
	unit := suffix[0]
	switch unit {
	case 'k':
		unit = 'K'
	case 'm':
		unit = 'M'
	}
	// Past E a count no longer fits in 64 bits.
	p := strings.IndexByte("KMGTPE", unit)
	if p < 0 {
		return 0, strconv.ErrSyntax
	}
	base := int64(1024)
	switch suffix[1:] {
	case "", "iB":
	case "B":
		base = 1000
	default:
		return 0, strconv.ErrSyntax
	}
	for range p + 1 {
		n *= base
	}
	return n, nil
}

func parseHeadTail(prog string, args []string, tail bool) (invocation, error) {
	spec := optSpec{prog: prog, short: "c:n:qv", code: 1, long: map[string]bool{
		"bytes": true, "lines": true, "quiet": false, "silent": false, "verbose": false,
	}}
	// The obsolete -N, and tail's +N, are -n N: the one argument is read as
	// two, and the operands' indices moved back to the arguments given.
	read := args
	moved := 0
	if len(args) > 0 {
		a := args[0]
		if len(a) > 1 && (a[0] == '-' || (tail && a[0] == '+')) && isDigits(a[1:]) {
			n := a[1:]
			if a[0] == '+' {
				n = "+" + n
			}
			read = append([]string{"-n", n}, args[1:]...)
			moved = 1
		}
	}
	ps, err := spec.parse(read)
	if err != nil {
		return nil, err
	}
	inv := &headInv{tail: tail, lines: true, count: 10, args: args, operands: shift(ps.operands, moved)}
	for _, o := range ps.opts {
		switch o.name {
		case "n", "lines", "c", "bytes":
			flag := "n"
			if o.name == "c" || o.name == "bytes" {
				flag = "c"
			}
			n, signed, err := readCount(prog, flag, o.val, tail)
			if err != nil {
				return nil, err
			}
			inv.lines, inv.count, inv.fromEnd = flag == "n", n, signed
		case "q", "quiet", "silent":
			inv.quiet, inv.verbose = true, false
		case "v", "verbose":
			inv.verbose, inv.quiet = true, false
		}
	}
	return inv, nil
}

func parseHead(args []string) (invocation, error) { return parseHeadTail("head", args, false) }
func parseTail(args []string) (invocation, error) { return parseHeadTail("tail", args, true) }

func (h *headInv) run(p *proc) int {
	code := 0
	names := h.names()
	headers := h.verbose || (len(names) > 1 && !h.quiet)
	for i, name := range names {
		data, err := p.readInput(name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || !isDirErr(err) {
				p.errorf("cannot open %s for reading: %s", quote(name), errorText(err))
			} else {
				p.errorf("error reading %s: %s", quote(name), errorText(err))
			}
			code = 1
			continue
		}
		if headers {
			if i > 0 {
				p.printf("\n")
			}
			label := name
			if name == "-" {
				label = "standard input"
			}
			p.printf("==> %s <==\n", label)
		}
		p.write(h.slice(data))
	}
	return code
}

func isDirErr(err error) bool {
	return errorText(err) == errnoText[syscall.EISDIR]
}

// slice is the part of data head or tail prints.
func (h *headInv) slice(data []byte) []byte {
	n := int(h.count)
	if !h.lines {
		switch {
		case !h.tail && !h.fromEnd:
			return data[:min(n, len(data))]
		case !h.tail:
			return data[:max(len(data)-n, 0)]
		case h.fromEnd:
			return data[min(max(n-1, 0), len(data)):]
		}
		return data[max(len(data)-n, 0):]
	}
	starts := lineStarts(data)
	lines := len(starts)
	cut := func(from, to int) []byte {
		from, to = max(min(from, lines), 0), max(min(to, lines), 0)
		if from >= to {
			return nil
		}
		end := len(data)
		if to < lines {
			end = starts[to]
		}
		return data[starts[from]:end]
	}
	switch {
	case !h.tail && !h.fromEnd:
		return cut(0, n)
	case !h.tail:
		return cut(0, lines-n)
	case h.fromEnd:
		return cut(max(n-1, 0), lines)
	}
	return cut(lines-n, lines)
}

// lineStarts are the offsets the lines of data start at; a last line
// without a newline is a line.
func lineStarts(data []byte) []int {
	var out []int
	for i := 0; i < len(data); {
		out = append(out, i)
		j := bytes.IndexByte(data[i:], '\n')
		if j < 0 {
			break
		}
		i += j + 1
	}
	return out
}

// wc

type wcInv struct {
	fileOps
	lines, words, chars, bytes, longest bool
}

var wcSpec = optSpec{prog: "wc", short: "clLmw", code: 1, long: map[string]bool{
	"bytes": false, "chars": false, "lines": false, "words": false, "max-line-length": false,
}}

func parseWc(args []string) (invocation, error) {
	ps, err := wcSpec.parse(args)
	if err != nil {
		return nil, err
	}
	inv := &wcInv{args: args, operands: ps.operands}
	for _, o := range ps.opts {
		switch o.name {
		case "l", "lines":
			inv.lines = true
		case "w", "words":
			inv.words = true
		case "m", "chars":
			inv.chars = true
		case "c", "bytes":
			inv.bytes = true
		case "L", "max-line-length":
			inv.longest = true
		}
	}
	if !inv.lines && !inv.words && !inv.chars && !inv.bytes && !inv.longest {
		inv.lines, inv.words, inv.bytes = true, true, true
	}
	return inv, nil
}

type wcCounts struct{ lines, words, chars, bytes, longest int64 }

func countText(data []byte) wcCounts {
	var c wcCounts
	c.bytes = int64(len(data))
	inWord := false
	var width int64
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		data = data[size:]
		c.chars++
		switch r {
		case '\n':
			c.lines++
			c.longest = max(c.longest, width)
			width = 0
		case '\t':
			width += 8 - width%8
		default:
			width++
		}
		if unicode.IsSpace(r) {
			inWord = false
		} else if !inWord {
			inWord = true
			c.words++
		}
	}
	c.longest = max(c.longest, width)
	return c
}

func (w *wcInv) run(p *proc) int {
	names := w.names()
	type row struct {
		c    wcCounts
		name string
		ok   bool
	}
	rows := make([]row, 0, len(names))
	code := 0
	regularOnly := true
	var total wcCounts
	var sizes int64
	for _, name := range names {
		if name == "-" && !p.stdinIsFile() {
			regularOnly = false
		}
		data, err := p.readInput(name)
		if err != nil {
			p.errorf("%s: %s", quoteIfNeeded(name), errorText(err))
			code = 1
			if isDirErr(err) {
				regularOnly = false
				rows = append(rows, row{name: name, ok: true})
			}
			continue
		}
		c := countText(data)
		sizes += int64(len(data))
		total.lines += c.lines
		total.words += c.words
		total.chars += c.chars
		total.bytes += c.bytes
		total.longest = max(total.longest, c.longest)
		label := name
		if name == "-" && len(w.operands) == 0 {
			label = ""
		}
		rows = append(rows, row{c: c, name: label, ok: true})
	}
	counted := 0
	for _, b := range []bool{w.lines, w.words, w.chars, w.bytes, w.longest} {
		if b {
			counted++
		}
	}
	width := 1
	if len(names) > 1 || counted > 1 {
		width = len(strconv.FormatInt(sizes, 10))
		if !regularOnly {
			width = max(width, 7)
		}
	}
	print := func(c wcCounts, name string) {
		var parts []string
		for _, f := range []struct {
			on bool
			n  int64
		}{{w.lines, c.lines}, {w.words, c.words}, {w.chars, c.chars}, {w.bytes, c.bytes}, {w.longest, c.longest}} {
			if f.on {
				parts = append(parts, fmt.Sprintf("%*d", width, f.n))
			}
		}
		line := strings.Join(parts, " ")
		if name != "" {
			line += " " + name
		}
		p.printf("%s\n", line)
	}
	for _, r := range rows {
		if r.ok {
			print(r.c, r.name)
		}
	}
	if len(names) > 1 {
		print(total, "total")
	}
	return code
}
