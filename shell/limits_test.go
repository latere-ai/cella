// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// deadline is how long a line that reaches a bound may take to stop.
func deadline() time.Duration {
	if raceEnabled {
		return 20 * time.Second
	}
	return 3 * time.Second
}

// bounded runs a line and fails the test when it took longer than the
// deadline or allocated more than maxAlloc bytes on the way; zero
// allocates without a bound, for a line the time bound stops.
func bounded(t *testing.T, fsys FS, r Request, maxAlloc uint64) Result {
	t.Helper()
	if r.Dir == "" {
		r.Dir = "/memory/user"
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	res, err := Run(context.Background(), fsys, folders, r)
	took := time.Since(start)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("Run(%.80q): %v", r.Command, err)
	}
	if took > deadline() {
		t.Errorf("Run(%.80q) took %v", r.Command, took)
	}
	if raceEnabled {
		// The race detector drops what sync.Pool keeps, so regexp
		// allocates a machine for every line grep matches.
		maxAlloc *= 32
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; maxAlloc > 0 && alloc > maxAlloc {
		t.Errorf("Run(%.80q) allocated %d bytes, want at most %d", r.Command, alloc, maxAlloc)
	}
	return res
}

// stops checks that a line ended on a bound: status 2 and the bound's
// message as the line's last words.
func stops(t *testing.T, res Result, message string) {
	t.Helper()
	if res.ExitCode != 2 || !strings.HasSuffix(string(res.Output), message+"\n") {
		out := string(res.Output)
		if len(out) > 200 {
			out = "…" + out[len(out)-200:]
		}
		t.Errorf("got exit %d and %q, want exit 2 ending %q", res.ExitCode, out, message)
	}
}

// bigFS is the notes with a folder of 2000 files, a file of 4 MiB in
// lines of 64 bytes, and one of 4 MiB with no line break.
func bigFS() *memFS {
	m := filesFS()
	for i := range 2000 {
		m.put(fmt.Sprintf("/memory/user/many/note-%04d.md", i), "note\n", day)
	}
	m.put("/memory/user/lines", strings.Repeat(strings.Repeat("x", 63)+"\n", 1<<16), day)
	m.put("/memory/user/oneline", strings.Repeat("x", 4<<20), day)
	return m
}

func TestADoublingValueStops(t *testing.T) {
	for _, line := range []string{
		"x=a; for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25; do x=$x$x; done; echo done",
		// A loop that calls nothing.
		"x=a; while x=$x$x; do y=1; done; echo done",
		"x=a; until false; do x=$x$x$x$x$x$x$x$x$x$x$x$x$x$x$x$x; done; echo done",
		"x=a\nx=$x$x\nx=$x$x$x$x$x$x$x$x$x$x$x$x$x$x$x$x\nx=$x$x$x$x$x$x$x$x$x$x$x$x$x$x$x$x\nx=$x$x$x$x$x$x$x$x$x$x$x$x$x$x$x$x\nx=$x$x$x$x$x$x$x$x$x$x$x$x$x$x$x$x\nx=$x$x$x$x$x$x$x$x$x$x$x$x$x$x$x$x\necho done",
	} {
		res := bounded(t, filesFS(), Request{Command: line}, 32<<20)
		stops(t, res, fmt.Sprintf("x: value longer than %d bytes", DefaultValue))
		if strings.Contains(string(res.Output), "done") {
			t.Errorf("%q ran past the bound: %q", line, res.Output)
		}
	}
	res := bounded(t, filesFS(), Request{Command: "x=a\ny=b\nwhile :; do\n  x=$x$x\ndone"}, 32<<20)
	stops(t, res, fmt.Sprintf("sh: 4: x: value longer than %d bytes", DefaultValue))
}

func TestADoublingReadStops(t *testing.T) {
	long := fmt.Sprintf("read: a line longer than %d bytes", DefaultValue)
	line := "x=a\nwhile :; do\nread -r y <<EOF\n$x$x\nEOF\nx=$y\ndone"
	res := bounded(t, filesFS(), Request{Command: line}, 32<<20)
	stops(t, res, "sh: 3: "+long)

	// A line through a pipe is held whole by what reads it.
	res = bounded(t, bigFS(), Request{Command: "cat oneline | read -r y; echo done", Limits: Limits{Input: 1 << 20}}, 32<<20)
	stops(t, res, "cat: a line longer than 1048576 bytes through a pipe")
	res = bounded(t, bigFS(), Request{Command: "read -r y < oneline; echo $y", Limits: Limits{Input: 1 << 20}}, 32<<20)
	stops(t, res, "oneline: a line longer than 1048576 bytes read with <")
	m := bigFS()
	m.put("/memory/user/long", strings.Repeat("x", DefaultValue+16)+"\n", day)
	res = bounded(t, m, Request{Command: "read -r y < long; echo done"}, 32<<20)
	stops(t, res, long)
	m.put("/memory/user/value", strings.Repeat("x", DefaultValue)+"\n", day)
	res = bounded(t, m, Request{Command: "read -r y < value; echo ${#y}"}, 32<<20)
	if res.ExitCode != 0 || string(res.Output) != fmt.Sprintf("%d\n", DefaultValue) {
		t.Errorf("a line of the Value bound: got exit %d and %.80q", res.ExitCode, res.Output)
	}
}

// TestALongLineStopsItsRead checks that a read stops as soon as its line
// passes the Value bound, however long the line it is given: here one of
// 16 MiB, the Input bound, with no line break.
func TestALongLineStopsItsRead(t *testing.T) {
	m := bigFS()
	m.put("/memory/user/l16", strings.Repeat("x", DefaultInput), day)
	long := fmt.Sprintf("read: a line longer than %d bytes", DefaultValue)
	for _, line := range []string{
		"read -r x < l16; echo done",
		"read x < l16; echo done",
		"cat l16 | { read -r x; }; echo done",
		"cat l16 | read -r x; echo done",
		"while read -r x; do :; done < l16; echo done",
		"cat l16 | while read -r x; do :; done; echo done",
	} {
		start := time.Now()
		res := bounded(t, m, Request{Command: line}, 32<<20)
		if took := time.Since(start); took > time.Second && !raceEnabled {
			t.Errorf("%q took %v", line, took)
		}
		stops(t, res, long)
		if strings.Contains(string(res.Output), "done") {
			t.Errorf("%q ran past the bound: %q", line, res.Output)
		}
	}
}

func TestAReadKeepsItsStatus(t *testing.T) {
	m := filesFS()
	m.put("/memory/user/two", "a\nb\n", day)
	for _, tc := range []struct{ line, want string }{
		{"while read -r l; do echo \"<$l>\"; done < two; echo $?", "<a>\n<b>\n0\n"},
		{"read -r a < /dev/null || echo eof $?", "eof 1\n"},
		{"read -r a < /dev/null; echo $?", "1\n"},
		{"! read -r a < /dev/null && echo negated", "negated\n"},
		{"set -e; ! read -r a < two; echo after $a", "after a\n"},
		{"set -e; if read -r a < /dev/null; then echo yes; else echo no; fi", "no\n"},
		{"set -e; read -r a < /dev/null; echo unreached", ""},
		{"echo x | read -r a; echo $?", "0\n"},
	} {
		res := run(t, m, Request{Command: tc.line, Dir: "/memory/user"})
		if string(res.Output) != tc.want {
			t.Errorf("%q: got exit %d and %q, want %q", tc.line, res.ExitCode, res.Output, tc.want)
		}
	}
}

func TestAPipeIntoSortOrUniqStops(t *testing.T) {
	for _, line := range []string{
		"while :; do echo x; done | sort",
		"while :; do echo x; done | uniq -c",
		"while :; do printf 'x\\n'; done | sort -u",
	} {
		res := bounded(t, filesFS(), Request{Command: line}, 64<<20)
		stops(t, res, fmt.Sprintf("the line ran more than %d steps", DefaultSteps))
	}
	small := Limits{Input: 1 << 20}
	for _, tc := range []struct{ line, who string }{
		{"cat lines | sort", "sort"},
		{"cat lines | uniq", "uniq"},
		{"sort lines", "sort"},
		{"cat lines lines | grep -c x", "grep"},
		{"cat -n lines", "cat"},
		{"grep -rn x .", "grep"},
	} {
		res := bounded(t, bigFS(), Request{Command: tc.line, Limits: small}, 48<<20)
		stops(t, res, tc.who+": more than 1048576 bytes held at once")
	}
	// A line over files well inside the bounds still runs.
	m := bigFS()
	m.put("/memory/user/some", strings.Repeat(strings.Repeat("x", 63)+"\n", 4096), day)
	res := bounded(t, m, Request{Command: "cat some | sort | uniq -c"}, 32<<20)
	if res.ExitCode != 0 || string(res.Output) != fmt.Sprintf("%7d %s\n", 4096, strings.Repeat("x", 63)) {
		t.Errorf("got exit %d and %.80q", res.ExitCode, res.Output)
	}
}

func TestAnEndlessLoopStops(t *testing.T) {
	for _, line := range []string{
		"while true; do :; done",
		"until false; do :; done",
		"while x=1; do y=2; done",
		"while :; do while :; do :; done; done",
		"echo first; while :; do :; done; echo last",
	} {
		res := bounded(t, filesFS(), Request{Command: line}, 64<<20)
		stops(t, res, fmt.Sprintf("sh: 1: the line ran more than %d steps", DefaultSteps))
		if strings.Contains(string(res.Output), "last") {
			t.Errorf("%q ran past the bound", line)
		}
	}
	res := bounded(t, filesFS(), Request{Command: "echo first; while :; do :; done"}, 64<<20)
	if !strings.HasPrefix(string(res.Output), "first\n") {
		t.Errorf("what ran before the bound is gone: %q", res.Output)
	}
	// The time bound holds apart from the steps.
	start := time.Now()
	res = bounded(t, filesFS(), Request{Command: "while :; do :; done", Limits: Limits{Steps: 1 << 40, Time: 100 * time.Millisecond}}, 0)
	stops(t, res, "sh: 1: the line ran longer than 100ms")
	if took := time.Since(start); took > time.Second {
		t.Errorf("the time bound of 100ms stopped the line after %v", took)
	}
}

func TestDeepNestingIsRefused(t *testing.T) {
	for _, line := range []string{
		strings.Repeat("(", 30000) + "true" + strings.Repeat(")", 30000),
		strings.Repeat("{ ", 20000) + "true" + strings.Repeat("; }", 20000),
		"echo $((" + strings.Repeat("(", 20000) + "1" + strings.Repeat(")", 20000) + "))",
		strings.Repeat("if :; then ", 4500) + ":" + strings.Repeat(";fi", 4500),
		strings.Repeat("while :; do ", 4000) + "break" + strings.Repeat(";done", 4000),
		strings.Repeat("for i in a; do ", 3000) + ":" + strings.Repeat(";done", 3000),
		strings.Repeat(": && ", 12000) + ":",
		strings.Repeat(": || ", 12000) + ":",
		strings.Repeat("cat | ", 10000) + "cat",
		"if :; then :; " + strings.Repeat("elif :; then :; ", 3500) + "fi",
		strings.Repeat("echo a; ", 10000),
		strings.Repeat("cat < a; ", 33),
	} {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		p := Check(line, "/memory/user", folders)
		took := time.Since(start)
		runtime.ReadMemStats(&after)
		if p.Reason != ReasonSize {
			t.Errorf("Check(%.40q…) = %+v, want size", line, p)
		}
		if took > time.Second || after.TotalAlloc-before.TotalAlloc > 8<<20 {
			t.Errorf("Check(%.40q…) took %v and %d bytes", line, took, after.TotalAlloc-before.TotalAlloc)
		}
		if _, err := Run(context.Background(), filesFS(), folders, Request{Command: line, Dir: "/memory/user"}); err == nil {
			t.Errorf("Run(%.40q…) ran", line)
		}
	}
	for _, line := range []string{
		strings.Repeat("(", MaxDepth) + "true" + strings.Repeat(")", MaxDepth),
		strings.Repeat(": && ", MaxDepth-1) + ":",
		strings.Repeat("cat a | ", MaxPipes) + "cat",
		"if :; then echo if; fi; for f in a; do case $f in a) echo done;; esac; done",
	} {
		if p := Check(line, "/memory/user", folders); !p.OK() {
			t.Errorf("Check(%.40q…) = %+v, want it run", line, p)
		}
	}
}

func TestWideExpansionIsRefused(t *testing.T) {
	x17 := strings.Repeat("$x", MaxExpansions+1)
	for _, line := range []string{
		"x=a; echo " + x17,
		"x=a; echo " + strings.Repeat("$x ", MaxExpansions+1),
		"x=a; y=" + x17,
		"x=a; cat <<EOF\n" + x17 + "\nEOF",
		"x=a; for i in " + x17 + "; do :; done",
		"x=a; case " + x17 + " in a) :;; esac",
		"echo" + strings.Repeat(" /memory/user/*", MaxExpansions+1),
		// A case clause's word and patterns count together.
		"x=a; case $x in " + strings.Repeat("*a) ;; ", MaxExpansions) + "esac",
		// A pattern's wildcards.
		"echo /memory/user/" + strings.Repeat("*a", MaxStars+1),
		"x=a; case $x in " + strings.Repeat("*a", MaxStars+1) + ") ;; esac",
		"x=a; echo ${x%" + strings.Repeat("*a", MaxStars+1) + "}",
		"x=a; y=${x##" + strings.Repeat("*a", MaxStars+1) + "}",
		"x=a; cat <<EOF\n${x%" + strings.Repeat("*a", MaxStars+1) + "}\nEOF",
		"for f in /memory/user/" + strings.Repeat("*a", MaxStars+1) + "; do :; done",
	} {
		if p := Check(line, "/memory/user", folders); p.Reason != ReasonSize {
			t.Errorf("Check(%.40q…) = %+v, want size", line, p)
		}
	}
	for _, line := range []string{
		"x=a; echo " + strings.Repeat("$x", MaxExpansions),
		"echo" + strings.Repeat(" /memory/user/*", MaxExpansions),
		"echo '" + strings.Repeat("*", 100) + "'",
		"echo \\" + strings.Repeat("*\\", 20),
		"x=a; case $x in " + strings.Repeat("*a) ;; ", MaxExpansions-1) + "esac",
		"x=a; echo ${x%" + strings.Repeat("*a", MaxStars) + "}",
		"cat <<EOF\n" + strings.Repeat("* a list item\n", 20) + "EOF",
	} {
		if p := Check(line, "/memory/user", folders); !p.OK() {
			t.Errorf("Check(%.40q…) = %+v, want it run", line, p)
		}
	}
}

func TestAGiantGlobStops(t *testing.T) {
	m := bigFS()
	res := bounded(t, m, Request{Command: "echo many/* | wc -w; ls many | wc -l; find many -type f | wc -l"}, 32<<20)
	if res.ExitCode != 0 || string(res.Output) != "2000\n2000\n2000\n" {
		t.Errorf("got exit %d and %q", res.ExitCode, res.Output)
	}
	glob := strings.Repeat(" many/*", MaxExpansions)
	res = bounded(t, m, Request{Command: "while :; do echo" + glob + " > /dev/null; done"}, 64<<20)
	stops(t, res, fmt.Sprintf("the line listed more than %d names", DefaultNames))
	res = bounded(t, m, Request{Command: "while :; do ls -R > /dev/null; done"}, 64<<20)
	stops(t, res, fmt.Sprintf("the line listed more than %d names", DefaultNames))
	res = bounded(t, m, Request{Command: "for f in many/*; do cat \"$f\" many/*; done > /dev/null"}, 64<<20)
	stops(t, res, fmt.Sprintf("the line listed more than %d names", DefaultNames))
}

func TestARedirectionThatGrowsStops(t *testing.T) {
	small := Limits{Input: 1 << 20}
	grow := "x=a; for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17; do x=$x$x; done; "
	res := bounded(t, filesFS(), Request{Command: grow + "while :; do echo $x; done > out", Limits: small}, 32<<20)
	stops(t, res, "out: more than 1048576 bytes held at once")
	res = bounded(t, filesFS(), Request{Command: grow + "echo $x > out; while :; do cat out out >> out; done", Limits: small}, 48<<20)
	stops(t, res, "more than 1048576 bytes held at once")
	res = bounded(t, bigFS(), Request{Command: "while :; do cat lines > /dev/null; done", Limits: Limits{Read: 16 << 20}}, 48<<20)
	stops(t, res, "the line read more than 16777216 bytes of files")
}

func TestWordsAndVariablesAreBounded(t *testing.T) {
	grow := "x=a; for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17; do x=$x$x; done; "
	res := bounded(t, filesFS(), Request{Command: grow + "echo $x $x $x $x $x $x $x $x $x"}, 32<<20)
	stops(t, res, fmt.Sprintf("echo: arguments longer than %d bytes together", DefaultValues))
	res = bounded(t, filesFS(), Request{Command: grow + "a=$x; b=$x; c=$x; d=$x; e=$x; f=$x; g=$x; h=$x; echo done"}, 32<<20)
	stops(t, res, fmt.Sprintf("the line's variables hold more than %d bytes", DefaultValues))
	res = bounded(t, filesFS(), Request{Command: grow + "printf '%s%s%s%s%s%s%s%s%s' $x $x $x $x $x $x $x $x $x"}, 32<<20)
	stops(t, res, fmt.Sprintf("printf: arguments longer than %d bytes together", DefaultValues))
}

func TestPrintfIsBounded(t *testing.T) {
	res := bounded(t, filesFS(), Request{Command: "printf '" + strings.Repeat("%999999d", 40) + "' 1"}, 32<<20)
	stops(t, res, fmt.Sprintf("printf: more than %d bytes held at once", DefaultInput))
	res = bounded(t, filesFS(), Request{Command: "printf; printf '%z'; printf '%0100000000d\\n' 1 | wc -c; printf '%5d|%-3s|\\n' 42 a b"}, 32<<20)
	if want := "usage: printf format [arguments]\ninvalid format char: z\n26\n   42|a  |\n    0|   |\n"; res.ExitCode != 0 || string(res.Output) != want {
		t.Errorf("got exit %d and %q", res.ExitCode, res.Output)
	}
}

func TestAnAssignmentKeepsItsStatus(t *testing.T) {
	res := run(t, filesFS(), Request{Command: "false; x=1; echo $?; x=2 && echo and; ! y=1 || echo not; set -e; z=3; echo $x$z", Dir: "/memory/user"})
	if string(res.Output) != "0\nand\nnot\n23\n" || res.ExitCode != 0 {
		t.Errorf("got exit %d and %q", res.ExitCode, res.Output)
	}
}

// askingFS is a file system that asks its context for its error many
// times on every read, as an HTTP client may.
type askingFS struct{ *memFS }

func (a askingFS) ReadFile(ctx context.Context, name string) ([]byte, error) {
	for range 2 * DefaultSteps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return a.memFS.ReadFile(ctx, name)
}

func TestOnlyTheLineCountsSteps(t *testing.T) {
	res := run(t, askingFS{filesFS()}, Request{Command: "cat nikita-heumann-your-latere-ai-cofounder.md > /dev/null && cat < notes/today.md", Dir: "/memory/user"})
	if res.ExitCode != 0 || string(res.Output) != "buy milk\n" {
		t.Errorf("got exit %d and %q", res.ExitCode, res.Output)
	}
}
