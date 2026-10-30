// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/expand"
)

func TestReadTakesOneLine(t *testing.T) {
	m := filesFS()
	m.put("/memory/user/two", "a\nb\n", day)
	m.put("/memory/user/spaced", " a  b  c \n", day)
	m.put("/memory/user/joined", "a\\\nb c\\ d\n", day)
	m.put("/memory/user/nul", "a\x00b\n", day)
	m.put("/memory/user/odd", "it's $x `y` \\ \"q\" \x01\xff\té\n", day)
	m.put("/memory/user/nonl", "last", day)
	many := strings.Repeat("0123456789abcdef\n", 1<<11)
	m.put("/memory/user/many", many, day)
	for _, tc := range []struct{ line, want string }{
		// What a read leaves of its input is the next reader's.
		{"{ read -r a; cat; } < two", "b\n"},
		{"{ read -r a; read -r b; echo \"$b$a\"; } < two", "ba\n"},
		{"cat two | { read -r a; wc -l; }", "1\n"},
		{"{ read -r a; cat | wc -l; } < many", "2047\n"},
		{"n=0; while read -r l; do n=$((n+1)); done < many; echo $n $l", "2048\n"},
		{"cat many | { while read -r l; do echo \"$l\"; done; } | wc -c", "34816\n"},
		{"i=0; while [ $i -lt 3 ]; do read -r l < two; echo $l; i=$((i+1)); done", "a\na\na\n"},
		// Splitting, backslashes and the last name taking the rest.
		{"read x y < spaced; echo \"<$x> <$y>\"", "<a> <b  c>\n"},
		{"read x y z w < spaced; echo \"<$x> <$y> <$z> <$w>\"", "<a> <b> <c> <>\n"},
		{"IFS=; read -r x < spaced; echo \"<$x>\"", "< a  b  c >\n"},
		{"read x y < joined; echo \"<$x> <$y>\"", "<ab> <c d>\n"},
		{"read -r x y < joined; echo \"<$x> <$y>\"", "<a\\> <>\n"},
		// A NUL is dropped, as dash drops it, and every other byte kept.
		{"read -r x < nul; echo \"$x\"", "ab\n"},
		{"IFS=; read -r x < odd; printf '%s\\n' \"$x\"", "it's $x `y` \\ \"q\" \x01\xff\té\n"},
		{"read -r x y < odd; printf '%s|%s\\n' \"$x\" \"$y\"", "it's|$x `y` \\ \"q\" \x01\xff\té\n"},
		// A last line with no newline is read, with status 1.
		{"read -r x < nonl; echo $? $x", "1 last\n"},
		{"read -r x; echo $? \"<$x>\"", "1 <>\n"},
		{"read -r a -r < two; echo $?", "read: invalid identifier \"-r\"\n2\n"},
	} {
		res := run(t, m, Request{Command: tc.line, Dir: "/memory/user"})
		if string(res.Output) != tc.want {
			t.Errorf("%q: got exit %d and %q, want %q", tc.line, res.ExitCode, res.Output, tc.want)
		}
	}
}

// TestReadKeepsEveryByte checks that a value read is the line's bytes as
// they are, whichever way it is quoted to be set.
func TestReadKeepsEveryByte(t *testing.T) {
	var all []byte
	for c := 1; c < 256; c++ {
		if c != '\n' {
			all = append(all, byte(c))
		}
	}
	for _, value := range []string{"it's", "a'b'c 'd'", "é'", "plain é", string(all), "\\'\\\\''"} {
		m := filesFS()
		m.put("/memory/user/in", value+"\n", day)
		res := run(t, m, Request{Command: "IFS=; read -r x < in; printf '%s' \"$x\" > out", Dir: "/memory/user"})
		if got := string(m.files["/memory/user/out"].data); res.ExitCode != 0 || got != value {
			t.Errorf("read %q: got exit %d and %q, wrote %q", value, res.ExitCode, res.Output, got)
		}
	}
}

// TestAClosedInputIsLetGo checks what keepRest answers for an input once
// the interpreter closed it: nothing more is kept of it.
func TestAClosedInputIsLetGo(t *testing.T) {
	s := &session{}
	r1, w1, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w1.Close()
	r2, w2, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	defer w2.Close()
	s.keepRest(r1, []byte("rest"))
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	s.keepRest(r2, []byte("more"))
	if _, ok := s.rest[r1]; ok || len(s.rest) != 1 {
		t.Errorf("kept %d inputs, the closed one among them: %v", len(s.rest), ok)
	}
	if got := string(s.takeRest(r2)); got != "more" || len(s.rest) != 0 {
		t.Errorf("took %q, and %d kept", got, len(s.rest))
	}
}

// TestFieldsAreTheInterpreters checks that read splits a line as the
// interpreter's read splits it, for every line of UTF-8.
func TestFieldsAreTheInterpreters(t *testing.T) {
	pieces := []string{" ", " ", "\t", "\n", ":", "a", "bc", "é", "\\", "\\ ", "\\:", "\\\\", "x"}
	ifss := []string{" \t\n", "", ":", " :", "é", "\\", " ", ":\t"}
	r := rand.New(rand.NewPCG(3, 4))
	for range 50000 {
		var line strings.Builder
		for range r.IntN(10) {
			line.WriteString(pieces[r.IntN(len(pieces))])
		}
		ifs := ifss[r.IntN(len(ifss))]
		n := r.IntN(4)
		if n == 0 {
			n = -1
		}
		raw := r.IntN(2) == 0
		env := expand.ListEnviron("IFS=" + ifs)
		want := expand.ReadFields(&expand.Config{Env: env}, line.String(), n, raw)
		if got := splitFields(ifs, line.String(), n, raw); !slices.Equal(got, want) {
			t.Fatalf("splitFields(%q, %q, %d, %v) = %q, the interpreter's %q", ifs, line.String(), n, raw, got, want)
		}
	}
}
