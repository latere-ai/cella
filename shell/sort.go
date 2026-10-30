// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// sort

// sortKey is one -k key, or the whole line under the global options.
type sortKey struct {
	startField, startChar int
	endField, endChar     int // endField 0 is the line's end
	// numeric is the number a key is read as: 'n', 'g' or 'h', or 0 for
	// text.
	numeric       byte
	reverse, fold bool
	// startBlanks and endBlanks skip the blanks before the key's start
	// and its end character, as b does on either position.
	startBlanks, endBlanks bool
}

func (k sortKey) plain() bool {
	return k.numeric == 0 && !k.reverse && !k.fold && !k.startBlanks && !k.endBlanks
}

type sortInv struct {
	args     []string
	operands []int
	output   int // the argument -o names, -1 for none
	keys     []sortKey
	sep      string
	global   sortKey
	unique   bool
	stable   bool
}

var sortSpec = optSpec{prog: "sort", short: "bfghk:nmo:rst:u", code: 2, long: map[string]bool{
	"ignore-leading-blanks": false, "ignore-case": false, "numeric-sort": false, "general-numeric-sort": false,
	"human-numeric-sort": false, "key": true, "merge": false, "output": true, "reverse": false, "stable": false,
	"field-separator": true, "unique": false,
}}

func parseSort(args []string) (invocation, error) {
	ps, err := sortSpec.parse(args)
	if err != nil {
		return nil, err
	}
	inv := &sortInv{args: args, operands: ps.operands, output: -1}
	for _, o := range ps.opts {
		switch o.name {
		case "b", "ignore-leading-blanks":
			inv.global.startBlanks, inv.global.endBlanks = true, true
		case "f", "ignore-case":
			inv.global.fold = true
		case "n", "numeric-sort":
			inv.global.numeric = 'n'
		case "g", "general-numeric-sort":
			inv.global.numeric = 'g'
		case "h", "human-numeric-sort":
			inv.global.numeric = 'h'
		case "r", "reverse":
			inv.global.reverse = true
		case "u", "unique":
			inv.unique = true
		case "s", "stable":
			inv.stable = true
		case "m", "merge":
		case "t", "field-separator":
			if len(o.val) != 1 {
				return nil, &usageError{prog: "sort", msg: "multi-character tab " + quoteLocale(o.val), code: 2, noHint: true}
			}
			inv.sep = o.val
		case "k", "key":
			k, err := readKey(o.val)
			if err != nil {
				return nil, err
			}
			inv.keys = append(inv.keys, k)
		case "o", "output":
			if o.attached {
				// A file named inside the option's own argument is not a
				// path Check can read; such a call runs elsewhere.
				return nil, &usageError{prog: "sort", msg: "the output file must be its own argument", code: 2}
			}
			inv.output = o.at
		}
	}
	return inv, nil
}

func readKey(s string) (sortKey, error) {
	invalid := func(why string) error {
		return &usageError{prog: "sort", msg: why + ": invalid field specification " + quoteLocale(s), code: 2, noHint: true}
	}
	var k sortKey
	readPos := func(p string) (field, char int, opts string, err error) {
		i := 0
		for i < len(p) && p[i] >= '0' && p[i] <= '9' {
			i++
		}
		f, err := strconv.Atoi(p[:i])
		if err != nil {
			return 0, 0, "", invalid("invalid number at field start")
		}
		rest := p[i:]
		if strings.HasPrefix(rest, ".") {
			j := 1
			for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
				j++
			}
			c, err := strconv.Atoi(rest[1:j])
			if err != nil {
				return 0, 0, "", invalid("invalid number after '.'")
			}
			char, rest = c, rest[j:]
		}
		return f, char, rest, nil
	}
	start, end, hasEnd := strings.Cut(s, ",")
	f, c, all, err := readPos(start)
	switch {
	case err != nil:
		return k, err
	case f == 0:
		return k, invalid("field number is zero")
	case c == 0 && strings.Contains(start, "."):
		return k, invalid("character offset is zero")
	}
	k.startField, k.startChar = f, c
	if err := k.options(all, &k.startBlanks, invalid); err != nil {
		return k, err
	}
	if hasEnd {
		f, c, opts, err := readPos(end)
		switch {
		case err != nil:
			return k, err
		case f == 0:
			return k, invalid("field number is zero")
		}
		k.endField, k.endChar = f, c
		if err := k.options(opts, &k.endBlanks, invalid); err != nil {
			return k, err
		}
	}
	return k, nil
}

// options reads the letters after a key's position; b sets blanks, the
// skip of the position it follows.
func (k *sortKey) options(letters string, blanks *bool, invalid func(string) error) error {
	for _, o := range letters {
		switch o {
		case 'n', 'g', 'h':
			k.numeric = byte(o)
		case 'r':
			k.reverse = true
		case 'f':
			k.fold = true
		case 'b':
			*blanks = true
		default:
			return invalid("stray character in field spec")
		}
	}
	return nil
}

func (s *sortInv) paths() []int {
	var out []int
	for _, i := range s.operands {
		if s.args[i] != "-" {
			out = append(out, i)
		}
	}
	if s.output >= 0 {
		out = append(out, s.output)
	}
	return out
}

func (s *sortInv) cwd() bool { return false }

func (s *sortInv) run(p *proc) int {
	names := operandStrings(s.args, s.operands)
	if len(names) == 0 {
		names = []string{"-"}
	}
	var lines []string
	for _, name := range names {
		data, err := p.readInput(name)
		if err != nil {
			if isDirErr(err) {
				p.errorf("read failed: %s: %s", name, errorText(err))
			} else {
				p.errorf("cannot read: %s: %s", name, errorText(err))
			}
			return 2
		}
		for _, l := range splitLines(data) {
			lines = append(lines, string(l))
		}
	}
	byteOrder := p.byteOrder()
	keys := slices.Clone(s.keys)
	for i := range keys {
		if keys[i].plain() {
			start, end := keys[i], keys[i]
			keys[i] = s.global
			keys[i].startField, keys[i].startChar = start.startField, start.startChar
			keys[i].endField, keys[i].endChar = end.endField, end.endChar
		}
	}
	if len(keys) == 0 {
		// The global options order the whole line, as a key does.
		whole := s.global
		whole.startField = 1
		keys = []sortKey{whole}
	}
	compareKeys := func(a, b string) int {
		for _, k := range keys {
			if c := compareText(s.field(a, k), s.field(b, k), k, byteOrder); c != 0 {
				return c
			}
		}
		return 0
	}
	compare := func(a, b string) int {
		if c := compareKeys(a, b); c != 0 || s.stable || s.unique {
			return c
		}
		// Lines whose keys are equal are ordered by the whole line, the
		// way GNU sort's last resort orders them.
		c := compareNames(a, b, byteOrder)
		if s.global.reverse {
			c = -c
		}
		return c
	}
	slices.SortStableFunc(lines, compare)
	var b strings.Builder
	for i, l := range lines {
		if s.unique && i > 0 && compareKeys(lines[i-1], l) == 0 {
			continue
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if s.output >= 0 {
		name := s.args[s.output]
		if err := p.s.files.write(p.ctx, p.abs(name), []byte(b.String())); err != nil {
			p.errorf("open failed: %s: %s", name, errorText(err))
			return 2
		}
		return 0
	}
	p.write([]byte(b.String()))
	return 0
}

// field is the part of a line a key covers, found as GNU sort finds it:
// the start skips startField-1 fields, then startChar-1 characters; the end
// skips endField-1 fields and then endChar characters, or to the end of
// endField when endChar is zero, or to the line's end when there is no end
// field. Neither stops at a field's end, only at the line's. Without -t a
// field holds the blanks before it, which b skips.
func (s *sortInv) field(line string, k sortKey) string {
	from := s.skipFields(line, k.startField-1, false)
	if k.startBlanks {
		from = skipBlanks(line, from)
	}
	from = min(len(line), from+max(k.startChar-1, 0))
	to := len(line)
	if k.endField > 0 {
		if k.endChar == 0 {
			to = s.skipFields(line, k.endField, true)
		} else {
			to = s.skipFields(line, k.endField-1, false)
			if k.endBlanks {
				to = skipBlanks(line, to)
			}
			to = min(len(line), to+k.endChar)
		}
	}
	if from >= to {
		return ""
	}
	return line[from:to]
}

// skipFields is the offset past n fields of a line. With -t, the
// separator after the last one is skipped too unless atEnd.
func (s *sortInv) skipFields(line string, n int, atEnd bool) int {
	ptr := 0
	for ; ptr < len(line) && n > 0; n-- {
		if s.sep != "" {
			for ptr < len(line) && line[ptr] != s.sep[0] {
				ptr++
			}
			if ptr < len(line) && (n > 1 || !atEnd) {
				ptr++
			}
			continue
		}
		ptr = skipBlanks(line, ptr)
		for ptr < len(line) && line[ptr] != ' ' && line[ptr] != '\t' {
			ptr++
		}
	}
	return ptr
}

func skipBlanks(line string, i int) int {
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return i
}

func compareText(a, b string, k sortKey, byteOrder bool) int {
	var c int
	switch {
	case k.numeric != 0:
		c = cmp.Compare(sortNumber(a, k.numeric), sortNumber(b, k.numeric))
	case k.fold:
		c = compareNames(strings.ToUpper(a), strings.ToUpper(b), byteOrder)
	default:
		c = compareNames(a, b, byteOrder)
	}
	if k.reverse {
		return -c
	}
	return c
}

// sortNumber is the number a key starts with after blanks, as -n, -g or
// -h reads it; a key with none reads as 0, or for -g as below every
// number.
func sortNumber(s string, kind byte) float64 {
	s = strings.TrimLeft(s, " \t")
	if kind == 'g' {
		end := 0
		for end < len(s) && s[end] != ' ' && s[end] != '\t' {
			end++
		}
		for ; end > 0; end-- {
			if n, err := strconv.ParseFloat(s[:end], 64); err == nil {
				return n
			}
		}
		return math.Inf(-1)
	}
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	if kind == 'h' && i < len(s) {
		suffix := s[i]
		if suffix == 'k' {
			suffix = 'K'
		}
		if p := strings.IndexByte("KMGTPEZYRQ", suffix); p >= 0 {
			n *= math.Pow(1024, float64(p+1))
		}
	}
	return n
}

// uniq

type uniqInv struct {
	args                    []string
	operands                []int
	count, repeated, unique bool
	fold                    bool
}

var uniqSpec = optSpec{prog: "uniq", short: "cdiu", code: 1, long: map[string]bool{
	"count": false, "repeated": false, "ignore-case": false, "unique": false,
}}

func parseUniq(args []string) (invocation, error) {
	ps, err := uniqSpec.parse(args)
	if err != nil {
		return nil, err
	}
	if len(ps.operands) > 2 {
		return nil, &usageError{prog: "uniq", msg: "extra operand " + quote(args[ps.operands[2]]), code: 1}
	}
	inv := &uniqInv{args: args, operands: ps.operands}
	for _, o := range ps.opts {
		switch o.name {
		case "c", "count":
			inv.count = true
		case "d", "repeated":
			inv.repeated = true
		case "u", "unique":
			inv.unique = true
		case "i", "ignore-case":
			inv.fold = true
		}
	}
	return inv, nil
}

func (u *uniqInv) paths() []int {
	var out []int
	for _, i := range u.operands {
		if u.args[i] != "-" {
			out = append(out, i)
		}
	}
	return out
}

func (u *uniqInv) cwd() bool { return false }

func (u *uniqInv) run(p *proc) int {
	in := "-"
	if len(u.operands) > 0 {
		in = u.args[u.operands[0]]
	}
	data, err := p.readInput(in)
	if err != nil {
		p.errorf("%s: %s", quoteIfNeeded(in), errorText(err))
		return 1
	}
	lines := splitLines(data)
	var b bytes.Buffer
	same := func(x, y []byte) bool {
		if u.fold {
			return bytes.EqualFold(x, y)
		}
		return bytes.Equal(x, y)
	}
	for i := 0; i < len(lines); {
		j := i + 1
		for j < len(lines) && same(lines[i], lines[j]) {
			j++
		}
		n := j - i
		if (!u.repeated || n > 1) && (!u.unique || n == 1) {
			if u.count {
				fmt.Fprintf(&b, "%7d ", n)
			}
			b.Write(lines[i])
			b.WriteByte('\n')
		}
		i = j
	}
	if len(u.operands) > 1 && u.args[u.operands[1]] != "-" {
		name := u.args[u.operands[1]]
		if err := p.s.files.write(p.ctx, p.abs(name), b.Bytes()); err != nil {
			p.errorf("%s: %s", quoteIfNeeded(name), errorText(err))
			return 1
		}
		return 0
	}
	p.write(b.Bytes())
	return 0
}

// cut

type cutInv struct {
	fileOps
	fields     bool
	ranges     [][2]int // 1-based, inclusive; 0 end is the line's end
	delim      string
	onlyDelim  bool
	complement bool
	outDelim   string
	hasOut     bool
}

var cutSpec = optSpec{prog: "cut", short: "b:c:d:f:ns", code: 1, long: map[string]bool{
	"bytes": true, "characters": true, "delimiter": true, "fields": true, "only-delimited": false,
	"complement": false, "output-delimiter": true,
}}

func parseCut(args []string) (invocation, error) {
	ps, err := cutSpec.parse(args)
	if err != nil {
		return nil, err
	}
	inv := &cutInv{args: args, operands: ps.operands, delim: "\t"}
	list := ""
	lists := 0
	for _, o := range ps.opts {
		switch o.name {
		case "b", "c", "bytes", "characters":
			list, inv.fields = o.val, false
			lists++
		case "f", "fields":
			list, inv.fields = o.val, true
			lists++
		case "d", "delimiter":
			if len(o.val) != 1 {
				return nil, &usageError{prog: "cut", msg: "the delimiter must be a single character", code: 1}
			}
			inv.delim = o.val
		case "s", "only-delimited":
			inv.onlyDelim = true
		case "complement":
			inv.complement = true
		case "output-delimiter":
			inv.outDelim, inv.hasOut = o.val, true
		}
	}
	switch {
	case lists == 0:
		return nil, &usageError{prog: "cut", msg: "you must specify a list of bytes, characters, or fields", code: 1}
	case lists > 1:
		return nil, &usageError{prog: "cut", msg: "only one list may be specified", code: 1}
	}
	for part := range strings.SplitSeq(list, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		var r [2]int
		var err1, err2 error
		switch {
		case !isRange:
			r[0], err1 = strconv.Atoi(lo)
			r[1] = r[0]
		default:
			r[0], r[1] = 1, 0
			if lo != "" {
				r[0], err1 = strconv.Atoi(lo)
			}
			if hi != "" {
				r[1], err2 = strconv.Atoi(hi)
			}
		}
		if err1 != nil || err2 != nil || r[0] < 1 || (r[1] != 0 && r[1] < r[0]) {
			return nil, &usageError{prog: "cut", msg: "invalid field value " + quote(part), code: 1}
		}
		inv.ranges = append(inv.ranges, r)
	}
	if !inv.hasOut {
		inv.outDelim = inv.delim
		if !inv.fields {
			inv.outDelim = ""
		}
	}
	return inv, nil
}

func (c *cutInv) selected(i int) bool {
	in := false
	for _, r := range c.ranges {
		if i >= r[0] && (r[1] == 0 || i <= r[1]) {
			in = true
			break
		}
	}
	return in != c.complement
}

func (c *cutInv) run(p *proc) int {
	code := 0
	var b bytes.Buffer
	for _, name := range c.names() {
		data, err := p.readInput(name)
		if err != nil {
			p.errorf("%s: %s", quoteIfNeeded(name), errorText(err))
			code = 1
			continue
		}
		for _, line := range splitLines(data) {
			if !c.fields {
				// Bytes are written as they stand, with the output
				// delimiter, when there is one, between separate ranges.
				last := -2
				for i := range line {
					if !c.selected(i + 1) {
						continue
					}
					if last >= 0 && last != i-1 {
						b.WriteString(c.outDelim)
					}
					b.WriteByte(line[i])
					last = i
				}
				b.WriteByte('\n')
				continue
			}
			fs := bytes.Split(line, []byte(c.delim))
			if len(fs) == 1 {
				if !c.onlyDelim {
					b.Write(line)
					b.WriteByte('\n')
				}
				continue
			}
			var parts [][]byte
			for i, f := range fs {
				if c.selected(i + 1) {
					parts = append(parts, f)
				}
			}
			b.Write(bytes.Join(parts, []byte(c.outDelim)))
			b.WriteByte('\n')
		}
	}
	p.write(b.Bytes())
	return code
}
