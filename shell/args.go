// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"fmt"
	"strings"
)

// program is one of the package's programs: parse reads its arguments as
// the program does, which Check uses to tell its paths and its options
// from the rest, and the run uses to run it.
type program struct {
	parse func(args []string) (invocation, error)
}

// invocation is a program's arguments, read.
type invocation interface {
	// paths are the indices of the arguments the program reads or writes
	// as paths.
	paths() []int
	// cwd reports whether the program reads the directory it starts in,
	// as ls and find do when they name no path.
	cwd() bool
	// run runs the program and answers its exit status.
	run(p *proc) int
}

// programs are the package's programs by name; init fills it, since a
// program's parse refers to the table through Check.
var programs map[string]program

func init() {
	programs = map[string]program{
		"ls":    {parse: parseLs},
		"cat":   {parse: parseCat},
		"head":  {parse: parseHead},
		"tail":  {parse: parseTail},
		"wc":    {parse: parseWc},
		"grep":  {parse: parseGrep},
		"egrep": {parse: func(a []string) (invocation, error) { return parseGrep(append([]string{"-E"}, a...)) }},
		"fgrep": {parse: func(a []string) (invocation, error) { return parseGrep(append([]string{"-F"}, a...)) }},
		"find":  {parse: parseFind},
		"sort":  {parse: parseSort},
		"uniq":  {parse: parseUniq},
		"cut":   {parse: parseCut},
		"mkdir": {parse: parseMkdir},
		"rm":    {parse: parseRm},
		"mv":    {parse: parseMv},
		"cp":    {parse: parseCp},
		"touch": {parse: parseTouch},
	}
}

// usageError is an argument a program does not take, said as GNU's
// programs say it.
type usageError struct {
	prog string
	msg  string
	// line, when set, is printed in the place of "prog: msg".
	line string
	code int
	// noHint leaves out the line that points to --help.
	noHint bool
}

func (u *usageError) Error() string { return u.prog + ": " + u.msg }

func (u *usageError) report(p *proc) int {
	if u.line != "" {
		p.eprintf("%s\n", u.line)
	} else {
		p.eprintf("%s: %s\n", u.prog, u.msg)
	}
	if !u.noHint {
		p.eprintf("Try '%s --help' for more information.\n", u.prog)
	}
	return u.code
}

// optSpec is how a program's options are read, as GNU getopt_long reads
// them: short options alone or together, a value attached or in the next
// argument, long options with = or the next argument, options anywhere
// among the operands, and -- ending them.
type optSpec struct {
	prog string
	// short holds the short options; a letter followed by ':' takes a
	// value.
	short string
	// long maps each long option to whether it takes a value.
	long map[string]bool
	// code is the exit status of an option the program does not take.
	code int
}

// opt is one option read: a letter, or a long option's name.
type opt struct {
	name string
	val  string
	// at is the index of the argument the value was read from, and
	// attached reports whether it shares that argument with the option.
	at       int
	attached bool
}

// parsed is a program's arguments read.
type parsed struct {
	opts     []opt
	operands []int
}

func (s optSpec) usage(msg string) error {
	return &usageError{prog: s.prog, msg: msg, code: s.code}
}

func (s optSpec) parse(args []string) (parsed, error) {
	var out parsed
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			for j := i + 1; j < len(args); j++ {
				out.operands = append(out.operands, j)
			}
			return out, nil
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a[2:], "=")
			at := i
			takes, ok := s.long[name]
			if !ok {
				return out, s.usage(fmt.Sprintf("unrecognized option '%s'", a))
			}
			switch {
			case takes && !hasVal:
				if i+1 >= len(args) {
					return out, s.usage(fmt.Sprintf("option '--%s' requires an argument", name))
				}
				i++
				at = i
				val = args[i]
			case !takes && hasVal:
				return out, s.usage(fmt.Sprintf("option '--%s' doesn't allow an argument", name))
			}
			out.opts = append(out.opts, opt{name: name, val: val, at: at, attached: hasVal})
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				c := a[j]
				k := strings.IndexByte(s.short, c)
				if k < 0 || c == ':' {
					return out, s.usage(fmt.Sprintf("invalid option -- '%c'", c))
				}
				if k+1 < len(s.short) && s.short[k+1] == ':' {
					val := a[j+1:]
					attached := val != ""
					if !attached {
						if i+1 >= len(args) {
							return out, s.usage(fmt.Sprintf("option requires an argument -- '%c'", c))
						}
						i++
						val = args[i]
					}
					out.opts = append(out.opts, opt{name: string(c), val: val, at: i, attached: attached})
					break
				}
				out.opts = append(out.opts, opt{name: string(c), at: i})
			}
		default:
			out.operands = append(out.operands, i)
		}
	}
	return out, nil
}

// operandStrings are the operands' values.
func operandStrings(args []string, idx []int) []string {
	out := make([]string, len(idx))
	for i, j := range idx {
		out[i] = args[j]
	}
	return out
}

// shift is the indices moved by n, for arguments a program's parse
// prepended to.
func shift(idx []int, n int) []int {
	out := make([]int, 0, len(idx))
	for _, i := range idx {
		if i-n >= 0 {
			out = append(out, i-n)
		}
	}
	return out
}
