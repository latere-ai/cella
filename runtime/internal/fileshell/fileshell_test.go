// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package fileshell

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	driver "latere.ai/x/cella/runtime"
)

func TestProgramsCarryEveryNameAsAParameter(t *testing.T) {
	const root = "/workspace"
	// A name that would be a flag, a glob and a command if the program
	// interpolated it rather than passing it.
	const nasty = "/workspace/-rf $(id) *|\n"
	for name, argv := range map[string][]string{
		"Stat":      Stat(root, nasty),
		"ReadDir":   ReadDir(root, nasty),
		"Cat":       Cat(root, nasty),
		"Mkdir":     Mkdir(root, nasty),
		"Remove":    Remove(root, nasty),
		"Move":      Move(root, nasty, nasty+"2"),
		"WriteBody": WriteBody(root, nasty, 10),
		"Commit":    Commit(root, nasty, nasty+"2", 0o640),
		"Discard":   Discard(root, nasty),
	} {
		if argv[0] != Shell || argv[1] != "-c" || argv[3] != "_" || argv[4] != root {
			t.Errorf("%s renders %q", name, argv[:5])
		}
		if strings.Contains(argv[2], nasty) {
			t.Errorf("%s put the name in the program text", name)
		}
		if !slices.Contains(argv[5:], nasty) {
			t.Errorf("%s does not carry the name as a parameter: %q", name, argv[5:])
		}
	}
	if got := WriteBody(root, "/workspace/f", 4); got[6] != "5" || got[7] != "4" {
		t.Errorf("a bounded write renders %q, want one byte past the bound and the bound", got[6:])
	}
	if got := WriteBody(root, "/workspace/f", 0); got[6] != "0" || got[7] != "0" {
		t.Errorf("an unbounded write renders %q", got[6:])
	}
	if got := Commit(root, "/workspace/s", "/workspace/f", fs.FileMode(0o2755)); got[7] != "755" {
		t.Errorf("a commit renders the mode %q", got[7])
	}
	if got := Staged("/workspace/dir/f.txt", "TOKEN"); got != "/workspace/dir/.cella-write-TOKEN" {
		t.Errorf("a body is staged at %q", got)
	}
}

func TestParseReadsWhatTheProgramsPrint(t *testing.T) {
	out := "755|directory|4096|1700000000|sub\x00" +
		"600|regular file|3|1700000001|a b|c\nd\x00"
	entries, err := Parse(out, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("the listing is %+v", entries)
	}
	if !entries[0].IsDir || entries[0].Size != 0 || entries[0].Mode != fs.ModeDir|0o755 || entries[0].Path != "/workspace/sub" {
		t.Errorf("the directory entry is %+v", entries[0])
	}
	if entries[1].Name != "a b|c\nd" || entries[1].Size != 3 || entries[1].Mode != 0o600 {
		t.Errorf("the entry with a pipe and a newline is %+v", entries[1])
	}
	if entries[1].ModTime.Unix() != 1700000001 || entries[1].ModTime.Location().String() != "UTC" {
		t.Errorf("the entry's modification time is %v", entries[1].ModTime)
	}
	if entries, err := Parse("", "/workspace"); entries != nil || err != nil {
		t.Errorf("an empty directory parsed as %+v, %v", entries, err)
	}
	for name, out := range map[string]string{
		"unterminated":  "644|regular file|1|2|a",
		"too few parts": "644|regular file|1\x00",
		"a bad mode":    "x|regular file|1|2|a\x00",
		"a bad size":    "644|regular file|x|2|a\x00",
		"a bad time":    "644|regular file|1|x|a\x00",
	} {
		if _, err := Parse(out, "/workspace"); err == nil {
			t.Errorf("%s parsed without an error", name)
		}
	}
}

func TestOneReadsASinglePath(t *testing.T) {
	info, err := One("644|regular file|5|1700000000|/workspace/dir/a.txt\x00", "/workspace/dir/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "a.txt" || info.Path != "/workspace/dir/a.txt" || info.Size != 5 {
		t.Fatalf("the entry is %+v", info)
	}
	if _, err := One("", "/workspace/a"); err == nil {
		t.Error("a stat that printed nothing was read as an entry")
	}
	if _, err := One("644|regular file|1|2|a\x00644|regular file|1|2|b\x00", "/workspace/a"); err == nil {
		t.Error("a stat that printed two records was read as one entry")
	}
	if _, err := One("nonsense", "/workspace/a"); err == nil {
		t.Error("a stat that printed nonsense was read as an entry")
	}
}

func TestClassifyAnswersOnePerExit(t *testing.T) {
	for code, want := range map[int]error{
		0:            nil,
		ExitOutside:  driver.ErrInvalid,
		ExitAbsent:   driver.ErrNotFound,
		ExitKind:     driver.ErrInvalid,
		ExitTooLarge: driver.ErrTooLarge,
	} {
		err := Classify(code, "/workspace/a", "")
		if want == nil && err != nil {
			t.Errorf("exit %d answered %v", code, err)
		}
		if want != nil && !errors.Is(err, want) {
			t.Errorf("exit %d answered %v, want %v", code, err, want)
		}
	}
	err := Classify(2, "/workspace/a", " cat: no such file\n")
	if err == nil || !strings.Contains(err.Error(), "cat: no such file") || errors.Is(err, driver.ErrInvalid) {
		t.Errorf("an unexpected exit answered %v", err)
	}
	if err := Classify(2, "/workspace/a", ""); err == nil || strings.Contains(err.Error(), ":  ") {
		t.Errorf("an unexpected exit with nothing said answered %v", err)
	}
}

func TestSizeReadsTheStagedCount(t *testing.T) {
	if n, err := Size("42"); n != 42 || err != nil {
		t.Errorf("Size = %d, %v", n, err)
	}
	if _, err := Size("  "); err == nil {
		t.Error("a staged write that printed nothing reported a count")
	}
}

// shell runs one program against the host's own filesystem. The utilities the
// programs use are GNU's or BusyBox's; a host whose stat takes another flag
// cannot run them, and there the real-filesystem half of this suite is
// skipped. Every container image this contract runs carries one of the two.
func shell(t *testing.T, argv []string, stdin string) (string, int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, diagnostic strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &diagnostic
	err := cmd.Run()
	var coded *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &coded):
		return out.String(), coded.ExitCode()
	default:
		t.Fatalf("running %q: %v: %s", argv, err, diagnostic.String())
		return "", 0
	}
}

// posix reports whether this host's utilities are the ones the programs use.
func posix(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := exec.CommandContext(t.Context(), "stat", "-c", "%a", root).Run(); err != nil {
		t.Skipf("this host's stat is not the one a container image carries: %v", err)
	}
	// The temporary directory may itself be behind a link, and the programs
	// measure against the resolved root.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// TestProgramsAgainstAFilesystem runs every program where a filesystem
// answers it. It is the same contract the conformance suite drives through a
// driver, at the one layer where the shell itself is the subject.
func TestProgramsAgainstAFilesystem(t *testing.T) {
	root := posix(t)
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("StatAndList", func(t *testing.T) {
		name := "a b|c\nd.txt"
		p := write("dir/"+name, "hello")
		out, code := shell(t, Stat(root, p), "")
		if code != 0 {
			t.Fatalf("stat exited %d", code)
		}
		info, err := One(out, p)
		if err != nil || info.Name != name || info.Size != 5 || info.Mode.Perm() != 0o644 {
			t.Fatalf("the entry is %+v: %v", info, err)
		}
		out, code = shell(t, ReadDir(root, filepath.Join(root, "dir")), "")
		if code != 0 {
			t.Fatalf("the listing exited %d", code)
		}
		entries, err := Parse(out, filepath.Join(root, "dir"))
		if err != nil || len(entries) != 1 || entries[0].Name != name {
			t.Fatalf("the listing is %+v: %v", entries, err)
		}
		if _, code := shell(t, ReadDir(root, p), ""); code != ExitKind {
			t.Errorf("listing a file exited %d, want %d", code, ExitKind)
		}
		if _, code := shell(t, Stat(root, filepath.Join(root, "absent")), ""); code != ExitAbsent {
			t.Errorf("stat of a path that names nothing exited %d, want %d", code, ExitAbsent)
		}
	})

	t.Run("Containment", func(t *testing.T) {
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
			t.Fatal(err)
		}
		for name, argv := range map[string][]string{
			"stat through a link":   Stat(root, filepath.Join(root, "escape", "secret")),
			"cat through a link":    Cat(root, filepath.Join(root, "escape", "secret")),
			"write through a link":  WriteBody(root, filepath.Join(root, "escape", "planted"), 0),
			"mkdir through a link":  Mkdir(root, filepath.Join(root, "escape", "planted")),
			"remove through a link": Remove(root, filepath.Join(root, "escape", "secret")),
			"an absolute path":      Stat(root, "/etc/hosts"),
			// The driver refuses this one before it sends anything; the
			// program refuses it too, because the rule holds on both sides.
			"a traversal": Stat(root, root+"/../elsewhere"),
		} {
			if _, code := shell(t, argv, ""); code != ExitOutside {
				t.Errorf("%s exited %d, want %d", name, code, ExitOutside)
			}
		}
		if _, err := os.Stat(filepath.Join(outside, "planted")); !errors.Is(err, os.ErrNotExist) {
			t.Error("a refused program still wrote outside the workspace")
		}
	})

	t.Run("WriteStagesAndCommits", func(t *testing.T) {
		p := write("bounded.txt", "before")
		staged := Staged(p, "TOKEN")
		out, code := shell(t, WriteBody(root, staged, 4), "12345")
		if code != ExitTooLarge {
			t.Fatalf("a body past the bound exited %d, want %d", code, ExitTooLarge)
		}
		if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
			t.Error("a refused body stayed staged")
		}
		if body, _ := os.ReadFile(p); string(body) != "before" {
			t.Fatalf("a refused write left %q", body)
		}
		out, code = shell(t, WriteBody(root, staged, 4), "1234")
		if code != 0 {
			t.Fatalf("a body at the bound exited %d", code)
		}
		if n, err := Size(out); n != 4 || err != nil {
			t.Fatalf("the staged write reported %d: %v", n, err)
		}
		// Nothing is committed until the second program runs.
		if body, _ := os.ReadFile(p); string(body) != "before" {
			t.Fatalf("the staged write already replaced the file: %q", body)
		}
		if _, code := shell(t, Commit(root, staged, p, 0o640), ""); code != 0 {
			t.Fatalf("the commit exited %d", code)
		}
		body, err := os.ReadFile(p)
		if err != nil || string(body) != "1234" {
			t.Fatalf("the committed file is %q: %v", body, err)
		}
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0o640 {
			t.Fatalf("the committed file's mode is %v: %v", info.Mode(), err)
		}
		if _, code := shell(t, Discard(root, staged), ""); code != 0 {
			t.Errorf("discarding what is no longer there exited %d", code)
		}
	})

	t.Run("MoveTakesTheExactDestination", func(t *testing.T) {
		source := write("move/source.txt", "moved")
		if err := os.MkdirAll(filepath.Join(root, "move", "into"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, code := shell(t, Move(root, source, filepath.Join(root, "move", "into")), ""); code != ExitKind {
			t.Errorf("a move onto a directory exited %d, want %d", code, ExitKind)
		}
		if _, err := os.Stat(filepath.Join(root, "move", "into", "source.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Error("a refused move nested the source inside the directory")
		}
		destination := filepath.Join(root, "move", "deeper", "target.txt")
		if _, code := shell(t, Move(root, source, destination), ""); code != 0 {
			t.Fatal("a move to a new path was refused")
		}
		if body, err := os.ReadFile(destination); err != nil || string(body) != "moved" {
			t.Fatalf("the moved file is %q: %v", body, err)
		}
		if _, code := shell(t, Move(root, source, destination), ""); code != ExitAbsent {
			t.Errorf("a move of a path that names nothing exited %d, want %d", code, ExitAbsent)
		}
	})

	t.Run("MkdirAndRemove", func(t *testing.T) {
		tree := filepath.Join(root, "tree", "a", "b")
		if _, code := shell(t, Mkdir(root, tree), ""); code != 0 {
			t.Fatal("mkdir of a missing tree was refused")
		}
		if info, err := os.Stat(tree); err != nil || !info.IsDir() {
			t.Fatalf("mkdir made %v: %v", info, err)
		}
		if _, code := shell(t, Mkdir(root, tree), ""); code != 0 {
			t.Error("mkdir of a directory that is already there was refused")
		}
		file := write("tree/file.txt", "x")
		if _, code := shell(t, Mkdir(root, file), ""); code != ExitKind {
			t.Errorf("mkdir over a file exited %d, want %d", code, ExitKind)
		}
		if _, code := shell(t, Remove(root, filepath.Join(root, "tree")), ""); code != 0 {
			t.Fatal("removing a tree was refused")
		}
		if _, err := os.Stat(filepath.Join(root, "tree")); !errors.Is(err, os.ErrNotExist) {
			t.Error("the tree survived its removal")
		}
		if _, code := shell(t, Remove(root, filepath.Join(root, "tree")), ""); code != 0 {
			t.Error("removing what is no longer there was refused")
		}
	})
}
