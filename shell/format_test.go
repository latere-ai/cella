// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"math/rand/v2"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/expand"
)

// TestFormatIsTheInterpreters checks that printf's formatter makes what
// the interpreter's makes for every format whose widths Go's fmt pads, and
// pads a field where the interpreter's prints fmt's error for its width.
// A string with a character of more than a byte is padded in bytes, as
// GNU's printf pads it, which TestPrintfIsBounded checks.
func TestFormatIsTheInterpreters(t *testing.T) {
	pieces := []string{
		"%", "%", "%", "+", "-", " ", "0", "1", "5", "9", "12", "999999", "1000000",
		"s", "b", "c", "d", "i", "u", "o", "x", "q", "f", ".", "#",
		"a", "é", "\\", "\\n", "\\t", "\\x41", "\\x", "\\u00e9", "\\U0001F600", "\\U", "\\0101", "\\8", "\\c", "\\'", "\\%",
		"\n", "\xff",
	}
	// The interpreter pads a string to its width in characters, GNU's
	// printf in bytes, so every value is ASCII.
	values := []string{"", "0", "7", "-42", "+3", "0x1f", "077", "abc", "-x", " 5", "99999999999999999999", "a\\nb", "%d", "\\x41\\c"}
	// Fields: a flag, a width and a verb, among the pieces.
	flags := []string{"", "", "+", "-", " "}
	widths := []string{"", "0", "1", "3", "05", "8", "012", "40", "0100"}
	verbs := []string{"s", "d", "i", "u", "o", "x", "c", "b", "%"}
	cases := 40000
	if raceEnabled {
		cases = 4000
	}
	r := rand.New(rand.NewPCG(1, 2))
	for i := range cases {
		var format strings.Builder
		for range r.IntN(8) + 1 {
			if i%2 == 0 {
				format.WriteString(pieces[r.IntN(len(pieces))])
				continue
			}
			format.WriteString("%" + flags[r.IntN(len(flags))] + widths[r.IntN(len(widths))] + verbs[r.IntN(len(verbs))])
			if r.IntN(3) == 0 {
				format.WriteString(pieces[r.IntN(len(pieces))])
			}
		}
		args := []string{}
		for range r.IntN(4) {
			args = append(args, values[r.IntN(len(values))])
		}
		want, wantN, wantErr := expand.Format(nil, format.String(), args)
		var f formatted
		n, err := f.format(format.String(), args, true)
		if strings.Contains(want, "%!(NOVERB)") {
			// fmt refused a width past a million.
			if err != nil || n != wantN || f.size(1e6) <= 1e6 {
				t.Fatalf("format %q %q = size %d, %d, %v; the interpreter's = %.80q, %d, %v", format.String(), args, f.size(1e6), n, err, want, wantN, wantErr)
			}
			continue
		}
		got := ""
		if err == nil {
			got = string(f.render())
		}
		if (err == nil) != (wantErr == nil) || err != nil && err.Error() != wantErr.Error() || n != wantN || got != want {
			t.Fatalf("format %q %q = %.200q, %d, %v; the interpreter's = %.200q, %d, %v", format.String(), args, got, n, err, want, wantN, wantErr)
		}
	}
}
