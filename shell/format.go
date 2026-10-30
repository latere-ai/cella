// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"fmt"
	"strconv"
	"unicode/utf8"
)

// maxPad bounds the width of one printf field as it is read, so reading
// it cannot overflow.
const maxPad = 1 << 30

// formatted is one pass of a printf format over its arguments, as the
// interpreter's printf makes it, with the padding of its fields kept
// apart until the pass is written: a field is as wide as its width says,
// however wide, and the pass is held whole before it is made. Go's fmt,
// which makes the fields, refuses a width much past a million, so it is
// given none and the field is padded here.
type formatted struct {
	out  []byte
	pads []pad
}

// pad is n bytes c at the offset at of out.
type pad struct {
	at int
	c  byte
	n  int
}

// size is how long the pass is, or limit+1 when it is longer than limit.
func (f *formatted) size(limit int) int {
	n := len(f.out)
	for _, p := range f.pads {
		if n > limit-p.n {
			return limit + 1
		}
		n += p.n
	}
	return min(n, limit+1)
}

// render is the pass as it is written.
func (f *formatted) render() []byte {
	size := len(f.out)
	for _, p := range f.pads {
		size += p.n
	}
	b := make([]byte, 0, size)
	last := 0
	for _, p := range f.pads {
		b = append(b, f.out[last:p.at]...)
		start := len(b)
		b = b[:start+p.n]
		for i := start; i < len(b); i++ {
			b[i] = p.c
		}
		last = p.at
	}
	return append(b, f.out[last:]...)
}

// format makes one pass of format over args and answers how many of them
// it used. Its escapes, directives and errors are those of the
// interpreter's printf: a field takes one flag of + - and space, then a
// width, and one of the verbs s b c d i u o x and %. Without directives
// only the escapes are read, as for the argument of %b.
func (f *formatted) format(format string, args []string, directives bool) (int, error) {
	initial := len(args)
	var spec []byte
	for i := 0; i < len(format); i++ {
		// digits reads up to n digits from i, octal or hexadecimal.
		digits := func(n int, hex bool) string {
			j := 0
			for j < n && i+j < len(format) && isDigit(format[i+j], hex) {
				j++
			}
			d := format[i : i+j]
			i += j - 1
			return d
		}
		c := format[i]
		switch {
		case c == '\\':
			i++
			if i >= len(format) {
				f.out = append(f.out, '\\')
				break
			}
			switch c = format[i]; c {
			case 'a':
				f.out = append(f.out, '\a')
			case 'b':
				f.out = append(f.out, '\b')
			case 'e', 'E':
				f.out = append(f.out, '\x1b')
			case 'f':
				f.out = append(f.out, '\f')
			case 'n':
				f.out = append(f.out, '\n')
			case 'r':
				f.out = append(f.out, '\r')
			case 't':
				f.out = append(f.out, '\t')
			case 'v':
				f.out = append(f.out, '\v')
			case '\\', '\'', '"', '?':
				f.out = append(f.out, c)
			case '0', '1', '2', '3', '4', '5', '6', '7':
				// Three digits at most, as strconv reads them: 0xff past a
				// byte, and 0 with an 8 or a 9.
				n, _ := strconv.ParseUint(digits(3, false), 8, 8)
				f.out = append(f.out, byte(n))
			case 'x', 'u', 'U':
				i++
				n := 2
				switch c {
				case 'u':
					n = 4
				case 'U':
					n = 8
				}
				if d := digits(n, true); d != "" {
					// Hexadecimal digits, which strconv reads; eight past
					// 32 bits are the largest, an invalid rune.
					v, _ := strconv.ParseUint(d, 16, 32)
					if c == 'x' {
						f.out = append(f.out, byte(v))
					} else {
						f.out = utf8.AppendRune(f.out, rune(v))
					}
					break
				}
				f.out = append(f.out, '\\', c)
			default:
				f.out = append(f.out, '\\', c)
			}
		case len(spec) > 0:
			switch c {
			case '%':
				f.out = append(f.out, '%')
				spec = nil
			case 'c':
				var b byte
				if len(args) > 0 {
					if args[0] != "" {
						b = args[0][0]
					}
					args = args[1:]
				}
				f.out = append(f.out, b)
				spec = nil
			case '+', '-', ' ':
				if len(spec) > 1 {
					return 0, fmt.Errorf("invalid format char: %c", c)
				}
				spec = append(spec, c)
			case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
				spec = append(spec, c)
			case 's', 'b', 'd', 'i', 'u', 'o', 'x':
				arg := ""
				if len(args) > 0 {
					arg, args = args[0], args[1:]
				}
				if c == 'b' {
					if _, err := f.format(arg, nil, false); err != nil {
						return 0, err
					}
				} else {
					f.field(spec, c, arg)
				}
				spec = nil
			default:
				return 0, fmt.Errorf("invalid format char: %c", c)
			}
		case directives && c == '%':
			spec = []byte{c}
		default:
			f.out = append(f.out, c)
		}
	}
	if len(spec) > 0 {
		return 0, fmt.Errorf("missing format char")
	}
	return initial - len(args), nil
}

// isDigit reports whether c is a decimal digit, or a hexadecimal one when
// hex.
func isDigit(c byte, hex bool) bool {
	switch {
	case '0' <= c && c <= '9':
		return true
	case hex:
		return 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
	}
	return false
}

// field makes one field of the verb s d i u o or x from its directive's
// flag and width: a number is read as C reads it, d and i signed and the
// others not, and the field is padded to its width in bytes as GNU's
// printf pads it, with spaces on the left, on the right for -, and with
// zeros for a width that starts with 0, after a number's sign. fmt pads a
// string to its width in characters, so its padding is not used.
func (f *formatted) field(spec []byte, verb byte, arg string) {
	flag := byte(0)
	width := spec[1:]
	if len(width) > 0 && (width[0] == '+' || width[0] == '-' || width[0] == ' ') {
		flag, width = width[0], width[1:]
	}
	var value any = arg
	if verb != 's' {
		n, _ := strconv.ParseInt(arg, 0, 0)
		if verb == 'd' || verb == 'i' {
			value = int(n)
		} else {
			value = uint(n)
		}
		if verb == 'i' || verb == 'u' {
			verb = 'd'
		}
	}
	directive := []byte{'%'}
	if flag == '+' || flag == ' ' {
		directive = append(directive, flag)
	}
	at := len(f.out)
	f.out = fmt.Appendf(f.out, string(append(directive, verb)), value)
	w := 0
	for _, d := range width {
		if w > maxPad/10 {
			w = maxPad
			break
		}
		w = w*10 + int(d-'0')
	}
	n := w - (len(f.out) - at)
	if n <= 0 {
		return
	}
	zero := len(width) > 0 && width[0] == '0'
	switch {
	case flag == '-':
		f.pads = append(f.pads, pad{at: len(f.out), c: ' ', n: n})
	case zero && verb != 's' && (f.out[at] == '+' || f.out[at] == '-' || f.out[at] == ' '):
		f.pads = append(f.pads, pad{at: at + 1, c: '0', n: n})
	case zero:
		f.pads = append(f.pads, pad{at: at, c: '0', n: n})
	default:
		f.pads = append(f.pads, pad{at: at, c: ' ', n: n})
	}
}
