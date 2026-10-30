// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

var day = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// filesFS is a file system that keeps files by path and no directories,
// with a note in /memory/user and one in the read-only folder.
func filesFS() *memFS {
	m := newMemFS(false)
	m.put("/memory/user/nikita-heumann-your-latere-ai-cofounder.md", "# Nikita\nmet on Monday\n", day)
	m.put("/memory/user/notes/today.md", "buy milk\n", day)
	m.put("/memory/shared/team.md", "team\n", day)
	return m
}

func run(t *testing.T, fsys FS, r Request) Result {
	t.Helper()
	res, err := Run(context.Background(), fsys, folders, r)
	if err != nil {
		t.Fatalf("Run(%q): %v", r.Command, err)
	}
	return res
}

func TestRunTheProductionLine(t *testing.T) {
	m := filesFS()
	res := run(t, m, Request{
		Command: "cd /memory/user && ls && cat nikita-heumann-your-latere-ai-cofounder.md",
		Dir:     "/workspace", Name: "/bin/sh", Line: 2,
	})
	want := "nikita-heumann-your-latere-ai-cofounder.md\nnotes\n# Nikita\nmet on Monday\n"
	if string(res.Output) != want || res.ExitCode != 0 || res.Dir != "/memory/user" {
		t.Errorf("got %q exit %d dir %q, want %q exit 0 dir /memory/user", res.Output, res.ExitCode, res.Dir, want)
	}
	if len(m.ops) != 0 {
		t.Errorf("a read changed the file system: %v", m.ops)
	}
}

func TestRunDeclinesWhatCheckRefuses(t *testing.T) {
	m := filesFS()
	_, err := Run(context.Background(), m, folders, Request{Command: "cd /memory/user && ls | jq .", Dir: "/workspace"})
	var d *Declined
	if !errors.As(err, &d) || d.Plan != (Plan{Reason: ReasonProgram, Program: "jq"}) {
		t.Fatalf("Run = %v, want a decline naming jq", err)
	}
	if !strings.Contains(d.Error(), "jq") {
		t.Errorf("the decline reads %q", d.Error())
	}
	if _, err := Run(context.Background(), m, []Mount{{Dir: "/"}}, Request{Command: "echo hi"}); err == nil {
		t.Error("Run over a mount at the root succeeded")
	}
	_, err = Run(context.Background(), m, folders, Request{Command: "cat $(ls)", Dir: "/memory/user"})
	if !errors.As(err, &d) || d.Error() != "shell: the command line does not run here: expansion" {
		t.Errorf("Run = %v", err)
	}
}

func TestRunKeepsTheDirectory(t *testing.T) {
	m := filesFS()
	for _, tc := range []struct {
		line, dir, want string
	}{
		{"echo hi", "", ""},
		{"echo hi", "/workspace", ""},
		{"cd /memory/user/notes", "/workspace", "/memory/user/notes"},
		{"cd /memory/user/missing", "/memory/user", "/memory/user"},
		{"cd ..", "/memory/user/notes", "/memory/user"},
	} {
		if res := run(t, m, Request{Command: tc.line, Dir: tc.dir}); res.Dir != tc.want {
			t.Errorf("%q from %q ended in %q, want %q", tc.line, tc.dir, res.Dir, tc.want)
		}
	}
}

func TestRunNumbersLinesFromTheRequest(t *testing.T) {
	res := run(t, filesFS(), Request{Command: "true\ncd /memory/user/missing", Dir: "/memory/user", Name: "/bin/sh", Line: 2})
	if want := "/bin/sh: 3: cd: can't cd to /memory/user/missing\n"; string(res.Output) != want || res.ExitCode != 2 {
		t.Errorf("got %q exit %d, want %q exit 2", res.Output, res.ExitCode, want)
	}
}

func TestReadOnlyFoldersRefuseEveryChange(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"echo x > /memory/shared/x", "sh: 1: cannot create /memory/shared/x: Read-only file system\n"},
		{"echo x >> /memory/shared/team.md", "sh: 1: cannot create /memory/shared/team.md: Read-only file system\n"},
		{"rm /memory/shared/team.md", "rm: cannot remove '/memory/shared/team.md': Read-only file system\n"},
		{"touch /memory/shared/team.md", "touch: cannot touch '/memory/shared/team.md': Read-only file system\n"},
		{"mkdir /memory/shared/x", "mkdir: cannot create directory ‘/memory/shared/x’: Read-only file system\n"},
		{"mkdir /memory/x", "mkdir: cannot create directory ‘/memory/x’: Read-only file system\n"},
		{"cp /memory/user/notes/today.md /memory/shared/", "cp: cannot create regular file '/memory/shared/today.md': Read-only file system\n"},
		{"sort -o /memory/shared/x /memory/shared/team.md", "sort: open failed: /memory/shared/x: Read-only file system\n"},
		{"rm -r /memory/user", "rm: cannot remove '/memory/user': Device or resource busy\n"},
		{"mv /memory/user /memory/u2", "mv: cannot move '/memory/user' to '/memory/u2': Device or resource busy\n"},
	} {
		m := filesFS()
		res := run(t, m, Request{Command: tc.line, Dir: "/workspace"})
		if string(res.Output) != tc.want || res.ExitCode == 0 {
			t.Errorf("%q: got %q exit %d, want %q", tc.line, res.Output, res.ExitCode, tc.want)
		}
		if len(m.ops) != 0 {
			t.Errorf("%q changed the file system: %v", tc.line, m.ops)
		}
	}
}

func TestFilesWithoutDirectories(t *testing.T) {
	m := filesFS()
	res := run(t, m, Request{Command: "mkdir -p a/b && ls -d a/b && echo x > a/b/c && rm a/b/c && ls a", Dir: "/memory/user"})
	if string(res.Output) != "a/b\nb\n" || res.ExitCode != 0 {
		t.Errorf("got %q exit %d", res.Output, res.ExitCode)
	}
	// A directory mkdir made lasts for the line, and one whose last file
	// went is gone with it.
	res = run(t, m, Request{Command: "ls; rm -r notes && ls", Dir: "/memory/user"})
	if want := "nikita-heumann-your-latere-ai-cofounder.md\nnotes\nnikita-heumann-your-latere-ai-cofounder.md\n"; string(res.Output) != want {
		t.Errorf("got %q, want %q", res.Output, want)
	}
	want := []string{"write /memory/user/a/b/c", "remove /memory/user/a/b/c", "remove /memory/user/notes/today.md"}
	if !slices.Equal(m.ops, want) {
		t.Errorf("ops %v, want %v", m.ops, want)
	}
}

func TestMoveWritesThenRemoves(t *testing.T) {
	m := filesFS()
	res := run(t, m, Request{Command: "mv notes n2 && mv nikita-heumann-your-latere-ai-cofounder.md n2/ && find . | sort", Dir: "/memory/user"})
	if want := ".\n./n2\n./n2/nikita-heumann-your-latere-ai-cofounder.md\n./n2/today.md\n"; string(res.Output) != want {
		t.Errorf("got %q, want %q", res.Output, want)
	}
	want := []string{
		"write /memory/user/n2/today.md", "remove /memory/user/notes/today.md",
		"write /memory/user/n2/nikita-heumann-your-latere-ai-cofounder.md", "remove /memory/user/nikita-heumann-your-latere-ai-cofounder.md",
	}
	if !slices.Equal(m.ops, want) {
		t.Errorf("ops %v, want %v", m.ops, want)
	}

	r := filesFS()
	res = run(t, memRenameFS{r}, Request{Command: "mv notes n2 && mv n2/today.md . && ls", Dir: "/memory/user"})
	if want := "nikita-heumann-your-latere-ai-cofounder.md\ntoday.md\n"; string(res.Output) != want {
		t.Errorf("got %q, want %q", res.Output, want)
	}
	if want := []string{"rename /memory/user/notes /memory/user/n2", "rename /memory/user/n2/today.md /memory/user/today.md"}; !slices.Equal(r.ops, want) {
		t.Errorf("ops %v, want %v", r.ops, want)
	}
}

func TestFileSystemErrorsAreSaidAsTheSystemSaysThem(t *testing.T) {
	changed := &Explained{Words: "changed since it was last read; read it again before writing", Err: fs.ErrExist}
	for _, tc := range []struct {
		op, path string
		err      error
		line     string
		want     string
		exit     int
	}{
		{"write", "/memory/user/notes/today.md", changed, "cp nikita-heumann-your-latere-ai-cofounder.md notes/today.md",
			"cp: cannot create regular file 'notes/today.md': changed since it was last read; read it again before writing\n", 1},
		{"write", "/memory/user/x", changed, "echo hi > x",
			"sh: 1: cannot create x: changed since it was last read; read it again before writing\n", 1},
		{"write", "/memory/user/x", changed, "echo hi > x; echo $?",
			"sh: 1: cannot create x: changed since it was last read; read it again before writing\n0\n", 0},
		{"read", "/memory/user/notes/today.md", syscall.EIO, "cat notes/today.md", "cat: notes/today.md: Input/output error\n", 1},
		{"read", "/memory/user/notes/today.md", errors.New("unreachable"), "head notes/today.md",
			"head: cannot open 'notes/today.md' for reading: Input/output error\n", 1},
		{"remove", "/memory/user/notes/today.md", fs.ErrPermission, "rm notes/today.md", "rm: cannot remove 'notes/today.md': Permission denied\n", 1},
		{"readdir", "/memory/user/notes", syscall.EIO, "ls notes", "ls: cannot open directory 'notes': Input/output error\n", 2},
		{"readdir", "/memory/user/notes", syscall.EIO, "ls -R", "ls: cannot open directory './notes': Input/output error\n.:\nnikita-heumann-your-latere-ai-cofounder.md\nnotes\n", 1},
		{"stat", "/memory/user/x", syscall.EACCES, "ls x", "ls: cannot access 'x': Permission denied\n", 2},
	} {
		m := filesFS()
		m.fail = map[string]error{tc.op + " " + tc.path: tc.err}
		res := run(t, m, Request{Command: tc.line, Dir: "/memory/user"})
		if string(res.Output) != tc.want || res.ExitCode != tc.exit {
			t.Errorf("%q: got %q exit %d, want %q exit %d", tc.line, res.Output, res.ExitCode, tc.want, tc.exit)
		}
	}
}

func TestOutputIsBounded(t *testing.T) {
	res := run(t, filesFS(), Request{Command: "cat nikita-heumann-your-latere-ai-cofounder.md", Dir: "/memory/user", MaxOutput: 4})
	if string(res.Output) != "# Ni" || !res.Truncated {
		t.Errorf("got %q truncated %v", res.Output, res.Truncated)
	}
}

func TestACanceledLineStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, filesFS(), folders, Request{Command: "while true; do ls; done", Dir: "/memory/user"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want the context's error", err)
	}
}

func TestNothingOutsideTheMountsIsReached(t *testing.T) {
	m := filesFS()
	m.put("/workspace/secret", "s\n", day)
	res := run(t, m, Request{Command: "cd /memory && ls && ls -a user; pwd", Dir: "/memory/user"})
	if want := "shared\nuser\n.\n..\nnikita-heumann-your-latere-ai-cofounder.md\nnotes\n/memory\n"; string(res.Output) != want {
		t.Errorf("got %q, want %q", res.Output, want)
	}
}

func TestTheCLocaleOrdersByBytes(t *testing.T) {
	m := newMemFS(false)
	for _, n := range []string{"b", "B", "a", "_c"} {
		m.put("/memory/user/"+n, "", day)
	}
	res := run(t, m, Request{Command: "ls; LC_ALL=C ls", Dir: "/memory/user"})
	if want := "a\nb\nB\n_c\nB\n_c\na\nb\n"; string(res.Output) != want {
		t.Errorf("got %q, want %q", res.Output, want)
	}
}

func TestMovesAcrossMounts(t *testing.T) {
	mounts := []Mount{{Dir: "/a"}, {Dir: "/b"}, {Dir: "/c", ReadOnly: true}}
	m := newMemFS(false)
	m.now = day
	m.put("/a/d/x", "x\n", day)
	m.put("/a/d/e/y", "y\n", day)
	m.put("/a/f", "f\n", day)
	m.put("/c/r", "r\n", day)
	res, err := Run(context.Background(), m, mounts, Request{
		Command: "mv /a/d /b/ && mv -v /a/f /b/g && find /a /b | sort && ls -ld /c && mv /c/r /b/ ; ls /b",
		Now:     day,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "renamed '/a/f' -> '/b/g'\n/a\n/b\n/b/d\n/b/d/e\n/b/d/e/y\n/b/d/x\n/b/g\n" +
		"drwxr-xr-x 2 root root 4096 Oct  1 12:00 /c\n" +
		"mv: cannot remove '/c/r': Read-only file system\nd\ng\nr\n"
	if string(res.Output) != want {
		t.Errorf("got %q, want %q", res.Output, want)
	}
	// A directory moves within a mount in one step when the file system
	// can, and file by file when it cannot.
	r := newMemFS(false)
	r.put("/a/d/x", "x\n", day)
	res, err = Run(context.Background(), memRenameFS{r}, mounts, Request{Command: "mkdir /a/d/n && mv /a/d /a/e && ls -R /a"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "/a:\ne\n\n/a/e:\nn\nx\n\n/a/e/n:\n"; string(res.Output) != want {
		t.Errorf("got %q, want %q", res.Output, want)
	}
}

func TestExplainedErrorsUnwrap(t *testing.T) {
	err := &Explained{Words: "refused by policy", Err: fs.ErrPermission}
	if !errors.Is(err, fs.ErrPermission) || err.Error() != "refused by policy" {
		t.Errorf("Explained reads %q and unwraps to %v", err.Error(), errors.Unwrap(err))
	}
	if got := errorText(errors.New("anything")); got != "Input/output error" {
		t.Errorf("an unknown error reads %q", got)
	}
	if got := errorText(fs.ErrExist); got != "File exists" {
		t.Errorf("fs.ErrExist reads %q", got)
	}
}

func TestDirectoriesARemoveLeaves(t *testing.T) {
	m := newMemFS(true)
	m.put("/memory/user/d/x", "x\n", day)
	m.dir("/memory/user/e", day)
	res := run(t, memDirFS{m}, Request{Command: "rm -d d; rm -dv e; rmdir_ok=$?; echo $rmdir_ok; rm -rf d && ls", Dir: "/memory/user"})
	if want := "rm: cannot remove 'd': Directory not empty\nremoved directory 'e'\n0\n"; string(res.Output) != want {
		t.Errorf("got %q, want %q", res.Output, want)
	}
}
