// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// The blocks read reads its input in: the first is small, so a read of a
// short line keeps little past it, and each next one is twice the last.
const (
	firstBlock = 4 << 10
	lastBlock  = 64 << 10
)

// read is the shell's read, run as one of the package's programs: it
// reads its input a block at a time where the interpreter's reads a byte
// at a time, and keeps what it read past its line for whatever reads the
// same input next. A line longer than the Value bound stops the line, as
// the variable it becomes would. Its options and its splitting are the
// interpreter's: -r keeps backslashes, the line is split by IFS among the
// names it is given, the last taking the rest, and with no name it is
// REPLY whole. A NUL byte is dropped, as dash drops it.
func (s *session) read(ctx context.Context, p *proc, args []string) error {
	// Check lets no option other than -r through.
	raw := false
	for len(args) > 0 && args[0] == "-r" {
		raw = true
		args = args[1:]
	}
	for _, name := range args {
		if !syntax.ValidName(name) {
			p.eprintf("read: invalid identifier %q\n", name)
			return status(2)
		}
	}
	lineCtx := uncounted(ctx)
	line, err := s.readLine(lineCtx, p.in, raw)
	if errors.Is(err, errStopped) || lineCtx.Err() != nil {
		return errStopped
	}
	hc := interp.HandlerCtx(ctx)
	var names, values []string
	if len(args) == 0 {
		names, values = []string{"REPLY"}, []string{unescape(string(line), raw)}
	} else {
		ifs := " \t\n"
		if vr := hc.Env.Get("IFS"); vr.IsSet() {
			ifs = vr.String()
		}
		names = args
		values = splitFields(ifs, string(line), len(args), raw)
	}
	var assign strings.Builder
	size := 0
	for i, name := range names {
		size += len(name) + 2
		if i < len(values) {
			size += quotedLen(values[i])
		} else {
			size += quotedLen("")
		}
	}
	assign.Grow(size)
	for i, name := range names {
		value := ""
		if i < len(values) {
			value = values[i]
		}
		if i > 0 {
			assign.WriteByte(' ')
		}
		assign.WriteString(name)
		assign.WriteByte('=')
		quoteAll(&assign, value)
	}
	// The interpreter sets a variable only by a statement it runs, so the
	// values are set by one it parses, quoted so it reads them as they are.
	// The statement is the package's, not the line's, so it counts no
	// step.
	if hc.Builtin(lineCtx, []string{"eval", assign.String()}) != nil {
		// The interpreter said why, as its read does for a variable it
		// cannot set.
		return status(1)
	}
	if err != nil {
		return status(1)
	}
	return nil
}

// readLine is the next line of in as read takes it, without its newline:
// a backslash and a newline join two lines unless raw. It reads through
// what an earlier read of in kept, and keeps what it reads past the line.
// It answers io.EOF with what it read of a last line that has no
// newline, and errStopped once the line is longer than the Value bound.
func (s *session) readLine(ctx context.Context, in *os.File, raw bool) ([]byte, error) {
	if in == nil {
		return nil, io.EOF
	}
	// A pipe's read waits for its writer, so a line that stops ends the
	// read by its deadline. Every pipe takes one; were one refused, the
	// read would end when its writer closes, as a stopped line's writers
	// do, so there is nothing else to do with the error.
	defer context.AfterFunc(ctx, func() { _ = in.SetReadDeadline(time.Now()) })()
	var line []byte
	esc := false
	limit := s.budget.lim.Value
	block := firstBlock
	for {
		chunk := s.takeRest(in)
		var err error
		if chunk == nil {
			buf := make([]byte, block)
			block = min(2*block, lastBlock)
			var n int
			n, err = in.Read(buf)
			chunk = buf[:n]
		}
		end := bytes.IndexByte(chunk, '\n')
		run := chunk
		if end >= 0 {
			run = chunk[:end]
		}
		// A run with no NUL, and no backslash unless raw, is the line's as
		// it is.
		if !esc && bytes.IndexByte(run, 0) < 0 && (raw || bytes.IndexByte(run, '\\') < 0) {
			line = append(line, run...)
			if end >= 0 {
				if len(line) > limit {
					return nil, s.budget.exceeded("read: a line longer than %d bytes", limit)
				}
				s.keepRest(in, chunk[end+1:])
				return line, nil
			}
			chunk = nil
		}
		for i, b := range chunk {
			switch {
			case b == 0:
			case !raw && b == '\\':
				line = append(line, b)
				esc = !esc
			case !raw && b == '\n' && esc:
				line = line[:len(line)-1]
				esc = false
			case b == '\n':
				if len(line) > limit {
					return nil, s.budget.exceeded("read: a line longer than %d bytes", limit)
				}
				s.keepRest(in, chunk[i+1:])
				return line, nil
			default:
				line = append(line, b)
				esc = false
			}
		}
		if len(line) > limit {
			return nil, s.budget.exceeded("read: a line longer than %d bytes", limit)
		}
		switch {
		case err == nil:
		case errors.Is(err, os.ErrDeadlineExceeded):
			return nil, errStopped
		case errors.Is(err, io.EOF):
			return line, io.EOF
		default:
			return line, fmt.Errorf("read: %w", err)
		}
	}
}

// splitFields splits a line among n names as the interpreter's read splits
// it, keeping its bytes as they are where the interpreter's makes a byte
// that is no UTF-8 a replacement character: a field is a run of
// characters not in ifs, the space, tab and newline of ifs around fields
// are dropped, a backslash escapes the next character unless raw and is
// dropped, and the last field takes the rest of the line, ifs within it.
func splitFields(ifs, s string, n int, raw bool) []string {
	inIFS := func(r rune) bool { return strings.ContainsRune(ifs, r) }
	// A space, tab or newline of ifs is one byte.
	white := func(c byte) bool { return (c == ' ' || c == '\t' || c == '\n') && inIFS(rune(c)) }
	type span struct{ start, end int }
	var spans []span
	// out is the line with its escaping backslashes dropped, which spans
	// index.
	out := make([]byte, 0, len(s))
	infield, esc := false, false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if infield {
			if inIFS(r) && (raw || !esc) {
				spans[len(spans)-1].end = len(out)
				infield = false
			}
		} else if !inIFS(r) && (raw || !esc) {
			spans = append(spans, span{start: len(out), end: -1})
			infield = true
		}
		if r == '\\' {
			if raw || esc {
				out = append(out, '\\')
			}
			esc = !esc
			i += size
			continue
		}
		out = append(out, s[i:i+size]...)
		esc = false
		i += size
	}
	if len(spans) == 0 {
		return nil
	}
	if infield {
		spans[len(spans)-1].end = len(out)
	}
	switch {
	case n == 1:
		// The one field is the line without the ifs whitespace around it;
		// what is outside the fields is ifs.
		lo, hi := 0, len(out)
		for lo < spans[0].start && white(out[lo]) {
			lo++
		}
		for hi > spans[len(spans)-1].end && white(out[hi-1]) {
			hi--
		}
		spans = []span{{lo, hi}}
	case n != -1 && n < len(spans):
		spans[n-1].end = spans[len(spans)-1].end
		spans = spans[:n]
	}
	fields := make([]string, len(spans))
	for i, sp := range spans {
		fields[i] = string(out[sp.start:sp.end])
	}
	return fields
}

// unescape is a line as a read with no name keeps it: whole, with a
// backslash dropped before the character it escapes unless raw.
func unescape(line string, raw bool) string {
	if raw || !strings.Contains(line, `\`) {
		return line
	}
	var b strings.Builder
	esc := false
	for i := range len(line) {
		if line[i] == '\\' && !esc {
			esc = true
			continue
		}
		b.WriteByte(line[i])
		esc = false
	}
	return b.String()
}

// quoteAll writes value as the shell reads it back byte for byte: in
// single quotes when it is UTF-8 with no quote, which the shell reads as
// it is, and otherwise in $'...', a quote and a backslash escaped and
// every other byte but a printable ASCII character as \xHH, since the
// shell's parser reads no other.
func quoteAll(b *strings.Builder, value string) {
	if plainQuote(value) {
		b.WriteByte('\'')
		b.WriteString(value)
		b.WriteByte('\'')
		return
	}
	const hex = "0123456789abcdef"
	b.WriteString("$'")
	for i := range len(value) {
		switch c := value[i]; {
		case c == '\'' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c >= ' ' && c <= '~':
			b.WriteByte(c)
		default:
			b.WriteString(`\x`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		}
	}
	b.WriteByte('\'')
}

// quotedLen is how long quoteAll writes value.
func quotedLen(value string) int {
	if plainQuote(value) {
		return len(value) + 2
	}
	n := 3
	for i := range len(value) {
		switch c := value[i]; {
		case c == '\'' || c == '\\':
			n += 2
		case c >= ' ' && c <= '~':
			n++
		default:
			n += 4
		}
	}
	return n
}

// plainQuote reports whether value is read back as it is in single
// quotes.
func plainQuote(value string) bool {
	return !strings.Contains(value, "'") && utf8.ValidString(value)
}

// takeRest is what a read kept of in past its line, which is no longer
// kept; nil when there is none.
func (s *session) takeRest(in *os.File) []byte {
	s.restMu.Lock()
	defer s.restMu.Unlock()
	rest, ok := s.rest[in]
	if !ok {
		return nil
	}
	delete(s.rest, in)
	return rest
}

// keepRest keeps what a read read of in past its line for the next read
// of in. An input the interpreter has closed is read no more, so what was
// kept of it is let go.
func (s *session) keepRest(in *os.File, rest []byte) {
	if len(rest) == 0 {
		return
	}
	s.restMu.Lock()
	defer s.restMu.Unlock()
	if s.rest == nil {
		s.rest = map[*os.File][]byte{}
	}
	for f := range s.rest {
		if _, err := f.Stat(); errors.Is(err, os.ErrClosed) {
			delete(s.rest, f)
		}
	}
	s.rest[in] = rest
}

// stdinReader is a program's standard input: what a read kept of the
// input past its line, then the input.
type stdinReader struct {
	s  *session
	in *os.File
}

func (r stdinReader) Read(b []byte) (int, error) {
	if rest := r.s.takeRest(r.in); rest != nil {
		n := copy(b, rest)
		r.s.keepRest(r.in, rest[n:])
		return n, nil
	}
	return r.in.Read(b)
}
