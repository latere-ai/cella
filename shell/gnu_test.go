// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The GNU tests run each command line here and compare what it writes and
// its exit status with what /bin/sh, GNU coreutils, grep and findutils
// answered for the same line over the same files, recorded in
// testdata/gnu.golden. Setting SHELL_GOLDEN_IMAGE to an Ubuntu 24.04 image
// with the en_US.UTF-8 locale records the file again: the image runs
// privileged under podman, with the files on an ext4 file system so sizes
// and block counts are a disk's.

// fixtureEntry is a file, or a directory when dir is set, under /memory.
type fixtureEntry struct {
	name string
	data string
	dir  bool
	mod  string // UTC, as touch -d reads it
}

const (
	recent = "2026-10-01 12:34:56"
	older  = "2026-09-15 08:00:00"
	old    = "2024-01-02 03:04:05"
)

// fixture is the files every line starts with. A directory is listed after
// what is in it, so its time is set last.
var fixture = []fixtureEntry{
	{name: "shared/team.md", data: "team notes\nTODO: plan the offsite\n", mod: old},
	{name: "shared/plan/q4.md", data: "# Q4\nship the shell\n", mod: older},
	{name: "shared/plan", dir: true, mod: older},
	{name: "shared", dir: true, mod: old},
	{name: "user/notes.md", data: "# Notes\nbuy milk\nTODO: call Anna\ntodo later\n", mod: recent},
	{name: "user/a.txt", data: "apple\nbanana\napple\ncherry\nbanana\nbanana\n", mod: "2026-10-02 09:00:00"},
	{name: "user/B.txt", data: "Zebra\nalpha\nBeta\n10\n9\n100\n", mod: "2026-09-30 18:30:00"},
	{name: "user/data.csv", data: "name,age,city\nanna,31,Berlin\nbob,27,Munich\ncarl,45,Hamburg\n", mod: older},
	{name: "user/names.txt", data: "bc\nb-c\nB-c\nb_c\nBc\nábc\nabc\nAbc\n_under\n.dot\n10\n9\na b\nab\n", mod: older},
	{name: "user/sizes.txt", data: "1K\n2M\n512\n3G\n1.5K\n", mod: older},
	{name: "user/spaced.txt", data: "one\n\n\ntwo\n\tthree  \n", mod: older},
	{name: "user/.hidden", data: "secret\n", mod: old},
	{name: "user/nonl.txt", data: "first line\nlast line", mod: older},
	{name: "user/empty", data: "", mod: old},
	{name: "user/big.txt", data: strings.Repeat("x", 4999) + "\n", mod: old},
	{name: "user/my-notes.md", data: "dash\n", mod: older},
	{name: "user/my_notes.md", data: "underscore\n", mod: older},
	{name: "user/mynotes.md", data: "plain\n", mod: older},
	{name: "user/cofounder-notes.md", data: "# Cofounder\nmet on Monday\n", mod: recent},
	{name: "user/dir/one.md", data: "one\nTODO two\n", mod: recent},
	{name: "user/dir/sub/two.txt", data: "two\nthree\n", mod: older},
	{name: "user/dir/sub/.dot", data: "dot\n", mod: older},
	{name: "user/dir/sub", dir: true, mod: older},
	{name: "user/dir", dir: true, mod: recent},
	{name: "user/emptydir", dir: true, mod: old},
	{name: "user", dir: true, mod: recent},
	{name: "", dir: true, mod: old},
}

var gnuMounts = []Mount{{Dir: "/memory", ReadOnly: true}, {Dir: "/memory/user"}, {Dir: "/memory/shared", ReadOnly: true}}

// gnuLines are the lines compared, each run in /memory/user over a fresh
// copy of the fixture. find and grep -r visit a directory in the order the
// disk holds it, which no listing reproduces, so their output is sorted.
var gnuLines = []string{
	// ls
	"ls", "ls -a", "ls -A", "ls -l", "ls -la", "ls -lh", "ls -1", "ls -R", "ls -lR dir", "ls -t", "ls -S",
	"ls -r", "ls -ltr", "ls -d dir", "ls -ld dir emptydir", "ls -F", "ls -p dir", "ls dir notes.md emptydir",
	"ls missing", "ls missing notes.md", "ls -l notes.md big.txt", "ls /memory", "ls -la /memory/shared",
	"ls -lG", "ls -n", "ls dir/*.md", "ls *.txt", "ls --color=never", "ls -lhS", "ls ..", "ls -aR dir",
	"ls -lt dir", "ls -l emptydir", "ls -R emptydir", "ls -A dir/sub",
	// cat
	"cat notes.md", "cat -n notes.md a.txt", "cat -b spaced.txt", "cat -E notes.md", "cat -s spaced.txt",
	"cat -A spaced.txt", "cat -T spaced.txt", "cat missing", "cat dir", "cat missing notes.md", "cat < notes.md",
	"cat notes.md | cat -n", "cat empty", "cat -", "cat notes.md - a.txt",
	"X=1\ncat <<EOF\nhello $X\nEOF",
	// head and tail
	"head -n 2 notes.md", "head -2 a.txt", "head -c 5 notes.md", "head notes.md a.txt", "head -n -2 a.txt",
	"head -q notes.md a.txt", "head -v notes.md", "tail -n 2 a.txt", "tail -n +3 a.txt", "tail -3 a.txt",
	"tail -c 4 notes.md", "tail notes.md a.txt", "head missing", "head dir", "tail missing", "head -n 1 empty",
	"cat a.txt | head -n 3 | tail -n 1", "tail -n 0 a.txt", "head -c -3 notes.md",
	// wc
	"wc notes.md", "wc -l notes.md", "wc -l *.txt", "wc -w a.txt B.txt", "wc -c big.txt", "wc < notes.md",
	"cat notes.md | wc -l", "wc missing notes.md", "wc dir", "wc -lwc notes.md", "wc -m names.txt",
	"wc -L notes.md", "wc empty", "wc -l missing",
	// grep
	"grep -i todo notes.md", "grep -n TODO notes.md dir/one.md", "grep -rn TODO . | sort", "grep -r todo dir",
	"grep -ril todo . | sort", "grep -c banana a.txt", "grep -c a a.txt B.txt", "grep -v banana a.txt",
	"grep -E 'app|cher' a.txt", "grep -F 'a.' a.txt data.csv", "grep -w an a.txt", "grep -x apple a.txt",
	"grep -o an a.txt", "grep -A1 milk notes.md", "grep -B1 -n call notes.md", "grep -C1 cherry a.txt",
	"grep nothing notes.md", "grep x missing", "grep -q apple a.txt", "grep -s x missing",
	"grep -l apple *.txt", "grep -L apple *.txt", "grep -h apple a.txt notes.md", "grep -H apple a.txt",
	"grep 'ap\\(p\\)le' a.txt", "grep '^b' a.txt", "grep -m1 banana a.txt", "grep -e apple -e cherry a.txt",
	"grep apple dir", "grep -r --include='*.md' TODO . | sort", "grep -rn TODO | sort",
	"echo hi | grep h", "grep -i 'ABC' names.txt", "grep -n '' spaced.txt", "grep -c '' empty",
	"grep -rh TODO /memory/shared", "grep -E '^[0-9]+$' B.txt", "grep 'a\\|z' B.txt", "grep -in BETA B.txt",
	"grep -r TODO dir/sub", "grep -ow 'b[a-z]*' notes.md",
	// find
	"find . | sort", "find . -name '*.md' | sort", "find . -type d | sort", "find . -type f -name '*.txt' | sort",
	"find dir -maxdepth 1 | sort", "find . -maxdepth 1 -type f | sort", "find . -iname 'b*' | sort",
	"find . -empty | sort", "find missing", "find . -path './dir/*' | sort", "find . -not -name '*.md' -type f | sort",
	"find . -name '*.md' -o -name '*.csv' | sort", "find dir -mindepth 2 | sort",
	"find /memory -maxdepth 1 | sort", "find dir -type f | sort", "find . -name 'two*'", "find dir/sub -print | sort",
	"find . ! -type d -name '.*' | sort", "find . -maxdepth 0", "find /memory/shared -type f | sort",
	// sort, uniq and cut
	"sort a.txt", "sort -r a.txt", "sort -u a.txt", "sort B.txt", "sort -n B.txt", "sort -f B.txt",
	"sort -t, -k2 -n data.csv", "sort -t, -k3 data.csv", "sort -t, -k2,2 -nr data.csv", "sort a.txt B.txt",
	"sort missing", "sort -o out.txt a.txt && cat out.txt", "cat a.txt | sort | uniq -c", "sort -h sizes.txt",
	"sort names.txt", "sort -r names.txt", "sort -n sizes.txt", "sort -k1,1 names.txt", "sort -u -f B.txt",
	"sort -t, -k1,1 data.csv", "sort -s -t, -k2,2 data.csv", "LC_ALL=C sort names.txt", "sort dir",
	"uniq a.txt", "sort a.txt | uniq", "sort a.txt | uniq -c", "sort a.txt | uniq -d", "sort a.txt | uniq -u",
	"uniq -i B.txt", "uniq missing", "sort a.txt | uniq - out.txt; cat out.txt", "sort a.txt | uniq -cd",
	"cut -d, -f1 data.csv", "cut -d, -f2,3 data.csv", "cut -d, -f2- data.csv", "cut -c1-3 notes.md",
	"cut -b2 a.txt", "cut -d, -f1 --complement data.csv", "cut -d, -f1,3 --output-delimiter=' | ' data.csv",
	"cut -f1 notes.md", "cut -s -f1 notes.md", "cut -c1-2,4 a.txt", "cut -d, -f-2 data.csv", "cut -d, -f3 missing data.csv",
	"cut -c3- names.txt",
	// mkdir, rm, mv, cp and touch
	"mkdir new && ls -d new", "mkdir -p a/b/c && find a | sort", "mkdir dir", "mkdir -p dir && echo ok", "mkdir x/y",
	"mkdir -v n1 n2", "mkdir -pv p/q", "mkdir /memory/shared/x", "mkdir /memory/x", "mkdir notes.md",
	"mkdir -p notes.md/x", "mkdir -p dir/sub/deep && ls dir/sub",
	"rm a.txt && ls", "rm missing", "rm -f missing; echo $?", "rm dir", "rm -r dir && ls", "rm -rv dir | sort",
	"rm -d emptydir && ls", "rm -d dir", "rm -v notes.md a.txt", "rm /memory/shared/team.md",
	"rm -rf /memory/shared/plan", "rm -r .", "rm -r dir/sub/.. 2>&1; ls dir", "rm emptydir",
	"rm -rf dir emptydir missing && ls", "rm -f notes.md missing a.txt && ls",
	"mv a.txt c.txt && ls", "mv a.txt dir && ls dir", "mv a.txt B.txt dir && ls dir", "mv a.txt B.txt notes.md",
	"mv missing x", "mv a.txt a.txt", "mv -v a.txt c.txt", "mv dir dir2 && find dir2 | sort", "mv dir dir/sub",
	"mv dir notes.md", "mv notes.md emptydir && ls emptydir", "mv -n a.txt B.txt; cat B.txt",
	"mv a.txt /memory/shared/", "mv /memory/shared/team.md .", "mv a.txt nodir/x", "mv a.txt B.txt; cat B.txt",
	"mv notes.md dir/one.md && cat dir/one.md", "mv emptydir dir && ls dir",
	"cp a.txt c.txt && cat c.txt", "cp a.txt dir && ls dir", "cp dir d2", "cp -r dir d2 && find d2 | sort",
	"cp -rv dir d2 | sort", "cp a.txt a.txt", "cp missing x", "cp a.txt B.txt notes.md", "cp -n a.txt B.txt; cat B.txt",
	"cp a.txt /memory/shared/x", "cp /memory/shared/team.md . && cat team.md", "cp -r dir dir/sub",
	"cp -v a.txt c.txt", "cp a.txt nodir/x", "cp -r dir emptydir && find emptydir | sort", "cp -R /memory/shared/plan . && ls plan",
	"cp a.txt B.txt dir && ls dir", "cp emptydir e2", "cp -a dir d3 && ls d3",
	"mv -n a.txt B.txt; echo $?", "cp -n a.txt B.txt; echo $?", "cp -n a.txt new.txt; echo $?; cat new.txt",
	"mv -n a.txt new.txt; echo $?", "mv /memory/shared/plan .; ls plan", "mv dir /memory/", "mv a.txt /memory/user/dir/ && ls dir",
	"cp -r /memory/shared/plan /memory/shared/plan2", "mv -v a.txt dir/", "mv dir/sub . && ls sub",
	"touch new.txt && ls", "touch -c nope && ls", "touch notes.md; cat notes.md", "touch /memory/shared/team.md",
	"touch nodir/x", "touch /memory/shared/new", "touch dir && echo ok", "touch a b && ls a b",
	// the shell
	"echo hi > out.txt && cat out.txt", "echo a >> notes.md; tail -1 notes.md", "echo x > /memory/shared/x",
	"echo x > nodir/x", "cat < missing", "echo -n hi; echo", "echo 'a\\tb'", "echo -e 'x'", "echo 'stop\\cnow'; echo",
	"printf '%s-%d\\n' a 1 b 2", "cd dir && ls && pwd", "cd missing", "cd dir; cd missing; pwd",
	"cd /memory/user/dir/sub && cat two.txt", "test -f notes.md && echo yes", "[ -d dir ] && echo dir",
	"test -e missing || echo no", "for f in *.txt; do echo \"== $f\"; head -1 \"$f\"; done",
	"if grep -q milk notes.md; then echo found; fi", "x=dir; ls $x", "ls nothing*", "echo *.md", "echo dir/*",
	"set -e; false; echo no", "exit 3", "true && false || echo recovered", "cat notes.md | grep -i todo | wc -l",
	"while read -r line; do echo \"[$line]\"; done < a.txt", "cat notes.md > /dev/null && echo ok",
	"ls missing 2>/dev/null; echo $?", "ls missing 2>&1 | wc -l", "case x in x) echo yes;; esac",
	"cd .. && ls", "cd /memory && ls -l", "pwd", "cd dir/sub && pwd && cd ../.. && pwd", "echo one\necho two\ncat missing",
	"ls && cat cofounder-notes.md", "cd /memory/user && ls && cat cofounder-notes.md",
	"echo $((2+3))", "n=0; for f in dir/*; do n=$((n+1)); done; echo $n", "[ notes.md -nt empty ] && echo newer",
	"test -s empty || echo empty", "echo hi > /dev/null; echo $?", "{ echo a; echo b; } > out.txt; cat out.txt",
	"(cd dir; pwd); pwd", "cat notes.md | sort | head -n 2", "sort a.txt | uniq -c | sort -rn | head -n 1",
	// escapes, words and expansions
	"echo 'a\\bb'", "echo 'tab\\there'", "echo 'nl\\nx'", "echo '\\0101'", "echo 'back\\\\slash'", "echo 'f\\fv\\vr\\r'",
	"printf 'a\\tb\\n'", "cat notes\\.md", "echo a\\ b", "ls dir\\/one.md",
	"echo \"a\\\"b\" \"c\\$d\" \"e\\\\f\" \"g\\h\"", "cat <<EOF\na\\$b \\\\ \\x\nEOF", "cat <<'EOF'\n$x \\n\nEOF",
	"echo $((3*4)) $((10/3)) $((10%3)) $((2-5)) $((1<2)) $(( (1+2)*3 ))", "i=2; echo $((i+1))",
	"x=hello; echo ${x} ${#x}", "x=a; echo ${x:-b} \"$#\" $?", "x=\"dir\"; ls \"$x\"", "x='a b'; echo $x", "x=$((1+1)); echo $x",
	"x=a; y=b; echo \"$x$y\" $x$y", "x='*.md'; echo $x", "for f in dir/*; do cat \"$f\" 2>&1 | head -1; done",
	"cd -- dir && pwd", "cat 'a b'", "LC_COLLATE=C ls", "LANG=C ls",
	// more of each program
	"sort -g B.txt", "sort -g sizes.txt", "sort -t, -k1.2 data.csv", "sort -k1.2,1.3 names.txt", "sort -b -k2 spaced.txt",
	"sort -t, -k2b,2n data.csv", "sort -k2 notes.md", "sort -t, -k3,3r -k1 data.csv",
	"test -r notes.md && echo r", "test -w notes.md && echo w", "test -w /memory/shared/team.md || echo ro",
	"test -x notes.md || echo nox", "test -x dir && echo xdir",
	"grep -E 'p{2}' a.txt", "grep 'p\\{2\\}' a.txt", "grep 'ba\\(na\\)*' a.txt", "grep -E '(na)+$' a.txt",
	"grep '[[:upper:]]' B.txt", "grep '[^a-z]' B.txt", "grep -E 'a|^1' B.txt", "grep 'an\\?a' a.txt", "grep '\\.' names.txt",
	"grep -E '\\.' names.txt", "grep '*a' a.txt", "grep '[]a]' B.txt", "grep -e -x B.txt", "grep -c '' big.txt",
	"grep -r --exclude='*.md' TODO . | sort", "grep -r --exclude-dir=sub two . | sort", "grep -rl two dir | sort", "grep -r nothing dir; echo $?",
	"grep 'x\\+' big.txt | wc -c", "grep -E 'x+$' big.txt | wc -l", "grep -n -o 'a' a.txt | head -3", "grep -vc a a.txt",
	"find . \\( -name '*.md' -o -name '*.txt' \\) -type f | sort", "find . -not -type d -name 'my*' | sort",
	"find . -name '*.md' -a -type f | sort", "find dir -depth | sort", "find . -type f -empty", "find . -false",
	"find . -maxdepth 1 -true | sort", "find . -ipath '*SUB*' | sort", "find . -wholename './dir/one.md'", "find -L dir -name one.md",
	"cat -v notes.md", "cat -e spaced.txt", "cat -t spaced.txt", "cat -u notes.md", "cat nonl.txt", "cat nonl.txt notes.md",
	"tail -n 1 nonl.txt", "wc -l nonl.txt", "head -c 3 nonl.txt", "grep line nonl.txt", "sort nonl.txt a.txt",
	"cut -c1-4 nonl.txt", "uniq nonl.txt", "tail -c 5 nonl.txt", "head -n 1 nonl.txt", "tail -n +2 nonl.txt",
	"head -n +2 a.txt", "head -c 1k big.txt | wc -c", "tail -c +4990 big.txt | wc -c", "uniq -c -u a.txt", "uniq -d a.txt",
	"cut -d, -f2 --output-delimiter=: data.csv", "cut -b1,3 --output-delimiter=- a.txt", "cut -c2- --complement a.txt",
	"ls -lh big.txt", "ls -1a dir", "ls -l dir/sub", "ls -RA dir", "ls -l /memory/shared/plan", "ls -Fl dir",
	"wc -l < notes.md", "wc -c - < notes.md", "cat notes.md | wc", "wc -l a.txt - < B.txt",
	"for t in , ab; do sort -t \"$t\" -k2 data.csv; done",
}

func TestGNU(t *testing.T) {
	if image := os.Getenv("SHELL_GOLDEN_IMAGE"); image != "" {
		record(t, image)
	}
	golden, now := readGolden(t)
	for i, line := range gnuLines {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			want, ok := golden[line]
			if !ok {
				t.Fatalf("no recorded answer for %q; record the file again", line)
			}
			if p := Check(line, "/memory/user", gnuMounts); !p.OK() {
				t.Fatalf("Check refuses %q: %+v", line, p)
			}
			fsys := fixtureFS()
			res, err := Run(context.Background(), memDirFS{fsys}, gnuMounts, Request{
				Command: line, Dir: "/memory/user", Name: "/bin/sh", Now: now,
			})
			if err != nil {
				t.Fatalf("Run(%q): %v", line, err)
			}
			got := gnuAnswer{output: string(res.Output), exit: res.ExitCode}
			if got != want {
				t.Errorf("%q\n got exit %d:\n%s\nwant exit %d:\n%s", line, got.exit, got.output, want.exit, want.output)
			}
		})
	}
}

type gnuAnswer struct {
	output string
	exit   int
}

func fixtureFS() *memFS {
	m := newMemFS(true)
	for _, e := range fixture {
		mod, err := time.Parse(time.DateTime, e.mod)
		if err != nil {
			panic(err)
		}
		name := path.Join("/memory", e.name)
		if e.dir {
			m.dir(name, mod)
		} else {
			m.put(name, e.data, mod)
		}
	}
	return m
}

const goldenFile = "testdata/gnu.golden"

func readGolden(t *testing.T) (map[string]gnuAnswer, time.Time) {
	t.Helper()
	data, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatalf("read the recorded answers: %v", err)
	}
	head, rest, ok := strings.Cut(string(data), "\n")
	unix, err := strconv.ParseInt(strings.TrimPrefix(head, "### NOW "), 10, 64)
	if !ok || err != nil {
		t.Fatalf("%s starts with %q, not the time it was recorded", goldenFile, head)
	}
	out := map[string]gnuAnswer{}
	for _, block := range strings.Split(rest, "### CMD ")[1:] {
		quoted, body, _ := strings.Cut(block, "\n")
		line, err := strconv.Unquote(quoted)
		if err != nil {
			t.Fatalf("%s: line %s: %v", goldenFile, quoted, err)
		}
		output, status, ok := strings.CutLast(body, "\n### EXIT ")
		if !ok {
			t.Fatalf("%s: %q has no exit status", goldenFile, line)
		}
		code, err := strconv.Atoi(strings.TrimSpace(status))
		if err != nil {
			t.Fatalf("%s: %q: %v", goldenFile, line, err)
		}
		out[line] = gnuAnswer{output: output, exit: code}
	}
	return out, time.Unix(unix, 0).UTC()
}

// record runs every line in the image and writes what it answered.
func record(t *testing.T, image string) {
	t.Helper()
	podman, err := exec.LookPath("podman")
	if err != nil {
		t.Fatalf("recording needs podman: %v", err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "record.sh")
	if err := os.WriteFile(script, []byte(recordScript()), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(podman, "run", "--rm", "--privileged", "--user", "root", "--env", "TZ=UTC",
		"-v", script+":/record.sh:ro", image, "/bin/sh", "/record.sh")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("recording failed: %v\n%s", err, stderr.String())
	}
	// The output is split by hand: a scanner's lines lose a carriage
	// return before the newline, which echo '\r' writes.
	var b strings.Builder
	for i, l := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		switch {
		case i == 0:
			b.WriteString(l + "\n")
		case strings.HasPrefix(l, "### CASE "):
			n, err := strconv.Atoi(strings.TrimPrefix(l, "### CASE "))
			if err != nil || n >= len(gnuLines) {
				t.Fatalf("recording answered %q", l)
			}
			fmt.Fprintf(&b, "### CMD %s\n", strconv.Quote(gnuLines[n]))
		default:
			b.WriteString(selinuxMark.ReplaceAllString(l, "$1 ") + "\n")
		}
	}
	if err := os.WriteFile(goldenFile, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// selinuxMark is the dot ls -l prints after the mode of a file with a
// security context. A host whose kernel runs SELinux labels every file,
// which a sandbox's file system does not, so the recording leaves it out.
var selinuxMark = regexp.MustCompile(`^([-dl][-rwxsStT]{9})\. `)

// recordScript makes /memory an ext4 file system with /memory/user
// writable and /memory itself and /memory/shared read-only, then runs each
// line over a fresh copy of the user's files.
func recordScript() string {
	var b strings.Builder
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	b.WriteString(`set -e
dd if=/dev/zero of=/tmp/memory.img bs=1M count=32 2>/dev/null
mkfs.ext4 -q /tmp/memory.img
dev=$(losetup -f)
[ -e "$dev" ] || mknod "$dev" b 7 "${dev#/dev/loop}"
losetup "$dev" /tmp/memory.img
trap 'umount -R /memory; losetup -d "$dev"' EXIT
mkdir -p /memory
mount "$dev" /memory
rmdir /memory/lost+found
`)
	populate := func(prefix string) {
		for _, e := range fixture {
			if !strings.HasPrefix(e.name, prefix) {
				continue
			}
			p := path.Join("/memory", e.name)
			if e.dir {
				fmt.Fprintf(&b, "mkdir -p %s\n", q(p))
			} else {
				fmt.Fprintf(&b, "mkdir -p %s\nprintf '%%s' %s > %s\n", q(path.Dir(p)), q(e.data), q(p))
			}
		}
		for _, e := range fixture {
			if strings.HasPrefix(e.name, prefix) {
				fmt.Fprintf(&b, "touch -d %s %s\n", q(e.mod+" UTC"), q(path.Join("/memory", e.name)))
			}
		}
	}
	populate("shared")
	b.WriteString("mkdir -p /memory/user\n")
	for _, e := range fixture {
		if e.name == "" {
			fmt.Fprintf(&b, "touch -d %s /memory\n", q(e.mod+" UTC"))
		}
	}
	b.WriteString(`mount --bind /memory/user /memory/user
mount --bind /memory/shared /memory/shared
mount -o remount,bind,ro /memory/shared
mount -o remount,bind,ro /memory
reset() {
	find /memory/user -mindepth 1 -delete
`)
	populate("user/")
	for _, e := range fixture {
		if e.name == "user" {
			fmt.Fprintf(&b, "\ttouch -d %s /memory/user\n", q(e.mod+" UTC"))
		}
	}
	b.WriteString("}\nset +e\ndate +'### NOW %s'\n")
	for i, line := range gnuLines {
		fmt.Fprintf(&b, "reset\necho '### CASE %d'\n(cd /memory/user && exec /bin/sh -c %s) </dev/null >/tmp/out 2>&1\nrc=$?\ncat /tmp/out\nprintf '\\n### EXIT %%d\\n' \"$rc\"\n", i, q(line))
	}
	return b.String()
}
