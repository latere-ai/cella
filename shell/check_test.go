// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import "testing"

// folders is a layout a program that serves people's files might mount:
// a read-only root of folders, one folder written to and one only read.
var folders = []Mount{{Dir: "/memory", ReadOnly: true}, {Dir: "/memory/user"}, {Dir: "/memory/shared", ReadOnly: true}}

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		line, dir string
		want      Plan
	}{
		// A line that starts on a machine's working directory runs here
		// once it has moved into a folder.
		{"cd /memory/user && ls && cat nikita-heumann-your-latere-ai-cofounder.md", "/workspace", Plan{}},
		{"cd /memory/user && ls", "/workspace", Plan{}},
		{"cd /memory/user && cat notes.md && grep -rn todo .", "/workspace", Plan{}},
		{"cd /memory/user/dir && cat ../notes.md", "/workspace", Plan{}},
		{"cd /memory/user && cd .. && ls", "/workspace", Plan{}},
		{"cat notes.md", "/memory/user", Plan{}},
		{"cat notes.md", "/workspace", Plan{Reason: ReasonPath}},
		{"cat notes.md", "", Plan{Reason: ReasonPath}},
		{"ls", "", Plan{Reason: ReasonPath}},
		{"pwd", "", Plan{Reason: ReasonPath}},
		{"echo hi", "", Plan{}},
		{"echo hi > /dev/null", "", Plan{}},
		{"cat /dev/null", "", Plan{Reason: ReasonPath}},
		// A cd that may fail leaves the line where it was; a folder's own
		// directory is always there.
		{"cd /memory/user/dir; cat notes.md", "/workspace", Plan{Reason: ReasonPath}},
		{"cd /memory/user; cat notes.md", "/workspace", Plan{}},
		{"cd /memory/user || exit 1; cat notes.md", "/workspace", Plan{}},
		{"cd / && ls", "/memory/user", Plan{Reason: ReasonPath}},
		{"cd ../.. && ls", "/memory/user", Plan{Reason: ReasonPath}},
		{"cd", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"cd -", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"cd -P /memory", "/memory/user", Plan{Reason: ReasonOption, Program: "cd"}},
		{"cd /memory/user/x /memory", "/memory/user", Plan{Reason: ReasonOption, Program: "cd"}},
		// What the line runs.
		{"cat /memory/user/a.json | jq .", "/workspace", Plan{Reason: ReasonProgram, Program: "jq"}},
		{"python3 /memory/user/x.py", "/workspace", Plan{Reason: ReasonProgram, Program: "python3"}},
		{"git status", "/memory/user", Plan{Reason: ReasonProgram, Program: "git"}},
		{"/bin/ls /memory", "/workspace", Plan{Reason: ReasonProgram, Program: "ls"}},
		{"eval ls", "/memory/user", Plan{Reason: ReasonProgram, Program: "eval"}},
		{"exec ls", "/memory/user", Plan{Reason: ReasonProgram, Program: "exec"}},
		{". ./x", "/memory/user", Plan{Reason: ReasonProgram, Program: "."}},
		{"ls | grep x | sort | uniq -c | head", "/memory/user", Plan{}},
		// Options a program does not take run where the program is.
		{"ls -Z", "/memory/user", Plan{Reason: ReasonOption, Program: "ls"}},
		{"ls --color", "/memory/user", Plan{Reason: ReasonOption, Program: "ls"}},
		{"grep", "/memory/user", Plan{Reason: ReasonOption, Program: "grep"}},
		{"find . -frob", "/memory/user", Plan{Reason: ReasonOption, Program: "find"}},
		{"sort -o/etc/x a", "/memory/user", Plan{Reason: ReasonOption, Program: "sort"}},
		{"set -o pipefail", "/memory/user", Plan{Reason: ReasonOption, Program: "set"}},
		{"rm -i a", "/memory/user", Plan{Reason: ReasonOption, Program: "rm"}},
		// Paths.
		{"cat /etc/passwd", "/memory/user", Plan{Reason: ReasonPath}},
		{"cat /memory/user/../../etc/passwd", "/memory/user", Plan{Reason: ReasonPath}},
		{"cat /memory/user/*/../../../etc/x", "/memory/user", Plan{Reason: ReasonPath}},
		{"ls /memory/user > /tmp/out", "/memory/user", Plan{Reason: ReasonPath}},
		{"sort -o /etc/x a", "/memory/user", Plan{Reason: ReasonPath}},
		{"test -f /etc/passwd", "/memory/user", Plan{Reason: ReasonPath}},
		{"[ -d /memory/user ] && ls /memory/user", "/workspace", Plan{}},
		{"x=/etc; cat $x/passwd", "/memory/user", Plan{Reason: ReasonPath}},
		{"x=/memory/user; cat $x/a", "/workspace", Plan{}},
		{"for f in /memory/user/*; do cat \"$f\"; done", "/workspace", Plan{}},
		{"for f in /etc/*; do echo $f; done", "/memory/user", Plan{Reason: ReasonPath}},
		{"ls *.md", "/workspace", Plan{Reason: ReasonPath}},
		{"find", "/workspace", Plan{Reason: ReasonPath}},
		{"grep -r x", "/workspace", Plan{Reason: ReasonPath}},
		// A write into a read-only folder runs here and is refused as a
		// read-only file system refuses it.
		{"echo x > /memory/shared/x", "/workspace", Plan{}},
		{"rm /memory/shared/team.md", "/workspace", Plan{}},
		// Expansions the line cannot know before something runs.
		{"cat $(ls /memory/user)", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"cat `ls`", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"$(echo ls)", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"cat <<EOF\n$(id)\nEOF", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"ls ~", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"echo $HOME", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"cat \"$1\"", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"head -n \"$n\" a", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"n=3; head -n $n a", "/memory/user", Plan{}},
		{"echo $((1+2))", "", Plan{}},
		{"echo $PWD", "/memory/user", Plan{}},
		{"echo $PWD", "/workspace", Plan{Reason: ReasonExpansion}},
		{"ls [a\"b]\"", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"ls [ab]\"c\"]", "/memory/user", Plan{}},
		// Constructs the line does not run.
		{"ls {a,b}", "/memory/user", Plan{Reason: ReasonConstruct}},
		{"ls &", "/memory/user", Plan{Reason: ReasonConstruct}},
		{"f() { ls; }; f", "/memory/user", Plan{Reason: ReasonConstruct}},
		{"while true; do cd /memory/user; done", "/memory/user", Plan{Reason: ReasonConstruct, Program: "cd"}},
		{"ls | while read l; do cd $l; done", "/memory/user", Plan{Reason: ReasonConstruct, Program: "cd"}},
		{"cat <(ls)", "/memory/user", Plan{Reason: ReasonSyntax}},
		{"ls (", "/memory/user", Plan{Reason: ReasonSyntax}},
		{"[[ -f a ]]", "/memory/user", Plan{Reason: ReasonProgram, Program: "[["}},
	} {
		got := Check(tc.line, tc.dir, folders)
		if got != tc.want {
			t.Errorf("Check(%q, %q) = %+v, want %+v", tc.line, tc.dir, got, tc.want)
		}
	}
}

func TestCheckRefusesBadMounts(t *testing.T) {
	for _, m := range [][]Mount{{{Dir: "/"}}, {{Dir: "rel"}}, {{Dir: ""}}} {
		if got := Check("echo hi", "", m); got.Reason != ReasonPath {
			t.Errorf("Check over %+v = %+v, want a path refusal", m, got)
		}
	}
}

// TestArgumentsAProgramDoesNotTake lists arguments each program refuses
// as GNU's refuses them; Check sends such a line where the program is, so
// its message is the program's own.
func TestArgumentsAProgramDoesNotTake(t *testing.T) {
	for _, line := range []string{
		"head -n x a", "tail -c y a", "head --lines", "head --bogus", "head -n", "head -n 1 -z a", "head -c 5Z a",
		"cut a", "cut -f1 -c1 a", "cut -d ab -f1 a", "cut -f 0 a", "cut -f 3-1 a", "cut -f x a", "cut -f1 -z a",
		"sort -k 0 a", "sort -k 1.0 a", "sort -k 1x a", "sort -k x a", "sort -k 1,0 a", "sort -k 1,x a", "sort -k 1.x a",
		"sort -k 1,1.x a", "sort -k 1,1z a", "sort -t ab a", "sort --key", "sort -c a", "sort --parallel=2 a",
		"uniq a b c", "uniq -f1 a", "mkdir", "touch", "touch -d now a", "cp a", "cp --preserve a b", "mv", "mv -T a b",
		"rm", "rm -rI a", "ls --color=always", "ls --color=bogus", "ls -lZ", "grep --color=always x a", "grep -E",
		"grep -P x a", "grep -f pats a", "find . -name", "find . -type z", "find . -maxdepth x", "find . '(' -name a",
		"find . -name a ')'", "find a -o", "find . !", "find . -exec ls '{}' ';'", "find . -delete", "find . -newer a",
		"wc --bogus", "cat -z", "cat --number=1 a", "wc --lines=2 a", "cut --fields", "grep --context",
	} {
		if got := Check(line, "/memory/user", folders); got.Reason != ReasonOption {
			t.Errorf("Check(%q) = %+v, want an option refusal", line, got)
		}
	}
}

func TestCheckFollowsTheLine(t *testing.T) {
	for _, tc := range []struct {
		line, dir string
		want      Plan
	}{
		{"if cd /memory/user/dir; then ls; else ls /memory/user; fi", "/workspace", Plan{}},
		{"if cd /memory/user/dir; then ls; fi; ls", "/workspace", Plan{Reason: ReasonPath}},
		{"if test -d /memory/user/a; then echo a; elif test -d /memory/user/b; then echo b; else echo c; fi", "", Plan{}},
		{"if true; then cd /memory/user; fi && ls", "/workspace", Plan{Reason: ReasonPath}},
		// Redirections.
		{"ls >| /memory/user/out 2>&1 1>&2 3>&- <&0", "/memory/user", Plan{}},
		{"cat 3< a", "/memory/user", Plan{}},
		{"cat <> a", "/memory/user", Plan{Reason: ReasonConstruct}},
		{"cat 2>&x", "/memory/user", Plan{Reason: ReasonConstruct}},
		{"x=a; cat > $x", "/memory/user", Plan{}},
		{"cat > $y", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"cat > *.md", "/memory/user", Plan{}},
		{"ls > /etc/x", "/memory/user", Plan{Reason: ReasonPath}},
		{"cat <<-EOF\n\tx\n\tEOF", "", Plan{}},
		{"for x in a b; do cat > $x; done", "/memory/user", Plan{Reason: ReasonExpansion}},
		// The shell's options and builtins.
		{"set -eux; set +e; set -o errexit -o nounset; set +o xtrace", "", Plan{}},
		{"set -o", "", Plan{Reason: ReasonOption, Program: "set"}},
		{"set $x", "", Plan{Reason: ReasonExpansion}},
		{"read -r a b < /memory/user/x; echo $a", "/workspace", Plan{}},
		{"read -p x a", "/memory/user", Plan{Reason: ReasonOption, Program: "read"}},
		{"x=-r; read $x a", "/memory/user", Plan{}},
		{"read $y", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"pwd -P", "/memory/user", Plan{}},
		{"pwd -x", "/memory/user", Plan{Reason: ReasonOption, Program: "pwd"}},
		{"exit $y", "", Plan{Reason: ReasonExpansion}},
		{"test /memory/user/a -nt /etc/passwd", "", Plan{Reason: ReasonPath}},
		{"test -f $y", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"test $x = a", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"echo *.md", "/workspace", Plan{Reason: ReasonPath}},
		{"printf '%s\\n' /memory/user/*", "/workspace", Plan{}},
		{"true /etc/*", "/workspace", Plan{}},
		// Arithmetic and assignments.
		{"x=1; echo $((x+2)) $(($x*2)) $((1+$#)) $(($?))", "", Plan{}},
		{"echo $((y+1))", "", Plan{Reason: ReasonExpansion}},
		{"echo $(($y+1))", "", Plan{Reason: ReasonExpansion}},
		{"x=1; echo $((x=5))", "", Plan{Reason: ReasonExpansion}},
		{"x=1; echo $((x++))", "", Plan{Reason: ReasonExpansion}},
		{"echo $(( $(ls) ))", "", Plan{Reason: ReasonExpansion}},
		{"x=$(ls)", "", Plan{Reason: ReasonExpansion}},
		{"x=/memory; y=user; cat \"$x/$y/a\" $x/$y/b", "/workspace", Plan{}},
		{"a[1]=x", "", Plan{Reason: ReasonExpansion}},
		{"x+=y", "", Plan{Reason: ReasonProgram, Program: "x+=y"}},
		{"x=a ls", "/memory/user", Plan{}},
		{"x=$y ls", "/memory/user", Plan{Reason: ReasonExpansion}},
		// Patterns.
		{"ls [!a]*", "/memory/user", Plan{}},
		{"ls []a]*", "/memory/user", Plan{}},
		{"ls [", "/memory/user", Plan{}},
		{"ls ]", "/memory/user", Plan{}},
		{"for f in /memory/user/*; do cat \"$f/x\"; done", "/workspace", Plan{}},
		{"for f in /memory/user/*; do cat \"$f/../../../etc/passwd\"; done", "/workspace", Plan{Reason: ReasonExpansion}},
		{"for f in /memory/user/*; do echo $f; done", "/workspace", Plan{}},
		{"x='*'; cat $x", "/memory/user", Plan{}},
		{"x='/etc/*'; cat $x", "/memory/user", Plan{Reason: ReasonPath}},
		{"case $y in a) ls;; esac", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"x=b; case $x in a) ls;; *) ls /memory;; esac", "/memory/user", Plan{}},
		{"case a in $y) ls;; esac", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"./run.sh", "/memory/user", Plan{Reason: ReasonProgram, Program: "run.sh"}},
		{"$y", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"cat $'a'", "/memory/user", Plan{}},
		{"cat \"$@\"", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"cat -- \"$1\"", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"echo ${y:-/etc}", "/memory/user", Plan{Reason: ReasonExpansion}},
		{"x=a; echo ${x%a}", "/memory/user", Plan{}},
		{"x=a; cat ${x%a}", "/memory/user", Plan{Reason: ReasonExpansion}},
	} {
		if got := Check(tc.line, tc.dir, folders); got != tc.want {
			t.Errorf("Check(%q, %q) = %+v, want %+v", tc.line, tc.dir, got, tc.want)
		}
	}
}
