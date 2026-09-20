// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/internal/fileshell"
)

// The programs themselves run against a real engine in TestPodmanConformance,
// which is where a filesystem answers them. What this package owns is what it
// sends and what it makes of the answer, which is what these tests hold.

// workspace is where a sandbox of these fixtures keeps its files.
const workspace = driver.DefaultWorkdir

// sessions is every exec the engine ran, oldest first. The engine numbers
// them, so the order is the count in the id and not its text.
func (f *fake) sessions() []*fakeExec {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.execs))
	for id := range f.execs {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int { return order(a) - order(b) })
	out := make([]*fakeExec, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.execs[id])
	}
	return out
}

func order(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "exec"))
	return n
}

// statRecord renders one stat record as a program prints it.
func statRecord(mode, kind, size, mtime, name string) string {
	return strings.Join([]string{mode, kind, size, mtime, name}, "|") + "\x00"
}

// filesFake is a fake engine with one running sandbox and the workspace every
// program is measured against.
func filesFake(t *testing.T) (*fake, *Driver) {
	t.Helper()
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	return f, d
}

func TestFileStoreSendsOneProgramPerOperation(t *testing.T) {
	const p = workspace + "/dir/a.txt"
	for _, tc := range []struct {
		name string
		out  string
		call func(*Driver) error
		want []string
	}{
		{"Stat", statRecord("644", "regular file", "5", "1700000000", p), func(d *Driver) error {
			_, err := d.Stat(t.Context(), "sbx_a", p)
			return err
		}, fileshell.Stat(workspace, p)},
		{"ReadDir", "", func(d *Driver) error {
			_, err := d.ReadDir(t.Context(), "sbx_a", workspace)
			return err
		}, fileshell.ReadDir(workspace, workspace)},
		{"Mkdir", "", func(d *Driver) error {
			return d.Mkdir(t.Context(), "sbx_a", workspace+"/dir")
		}, fileshell.Mkdir(workspace, workspace+"/dir")},
		{"Remove", "", func(d *Driver) error {
			return d.Remove(t.Context(), "sbx_a", p)
		}, fileshell.Remove(workspace, p)},
		{"Move", "", func(d *Driver) error {
			return d.Move(t.Context(), "sbx_a", p, workspace+"/b.txt")
		}, fileshell.Move(workspace, p, workspace+"/b.txt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, d := filesFake(t)
			f.run = func([]string, []string, string) fakeExecResult {
				return fakeExecResult{stdout: tc.out}
			}
			if err := tc.call(d); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			sessions := f.sessions()
			if len(sessions) != 1 {
				t.Fatalf("%s ran %d sessions, want one", tc.name, len(sessions))
			}
			if !slices.Equal(sessions[0].cmd, tc.want) {
				t.Fatalf("%s sent %q, want %q", tc.name, sessions[0].cmd, tc.want)
			}
		})
	}
}

func TestFileStoreReadsTheRecords(t *testing.T) {
	f, d := filesFake(t)
	f.run = func([]string, []string, string) fakeExecResult {
		return fakeExecResult{stdout: statRecord("640", "regular file", "12", "1700000000", workspace+"/a.txt")}
	}
	info, err := d.Stat(t.Context(), "sbx_a", workspace+"/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "a.txt" || info.Size != 12 || info.Mode.Perm() != 0o640 || info.IsDir {
		t.Fatalf("the entry is %+v", info)
	}

	f.run = func([]string, []string, string) fakeExecResult {
		return fakeExecResult{stdout: statRecord("755", "directory", "4096", "1700000001", "sub") +
			statRecord("600", "regular file", "1", "1700000002", "a b|c\nd")}
	}
	entries, err := d.ReadDir(t.Context(), "sbx_a", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "a b|c\nd" || !entries[1].IsDir {
		t.Fatalf("the listing is %+v", entries)
	}
	if entries[1].Path != workspace+"/sub" {
		t.Fatalf("the directory entry reports the path %q", entries[1].Path)
	}
}

func TestFileStoreMapsTheExits(t *testing.T) {
	f, d := filesFake(t)
	for _, tc := range []struct {
		code int
		want error
	}{
		{fileshell.ExitOutside, driver.ErrInvalid},
		{fileshell.ExitAbsent, driver.ErrNotFound},
		{fileshell.ExitKind, driver.ErrInvalid},
		{fileshell.ExitTooLarge, driver.ErrTooLarge},
	} {
		f.run = func([]string, []string, string) fakeExecResult {
			return fakeExecResult{code: tc.code}
		}
		if err := d.Mkdir(t.Context(), "sbx_a", workspace+"/a"); !errors.Is(err, tc.want) {
			t.Errorf("exit %d answered %v, want %v", tc.code, err, tc.want)
		}
	}
	f.run = func([]string, []string, string) fakeExecResult {
		return fakeExecResult{stderr: "mkdir: Permission denied", code: 2}
	}
	err := d.Mkdir(t.Context(), "sbx_a", workspace+"/a")
	if err == nil || errors.Is(err, driver.ErrInvalid) || !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("an unexpected exit answered %v", err)
	}
}

func TestFileStoreCommitsAWriteOnlyOnceTheBodyIsWhole(t *testing.T) {
	f, d := filesFake(t)
	const p = workspace + "/a.txt"
	f.run = func(cmd []string, _ []string, _ string) fakeExecResult {
		if isProgram(cmd, fileshell.WriteBody(workspace, "s", 0)) {
			return fakeExecResult{stdout: "5"}
		}
		return fakeExecResult{}
	}
	n, err := d.Write(t.Context(), "sbx_a", driver.WriteRequest{Path: p, Mode: 0o600, Body: strings.NewReader("hello")})
	if err != nil || n != 5 {
		t.Fatalf("the write reported %d bytes: %v", n, err)
	}
	sessions := f.sessions()
	if len(sessions) != 2 {
		t.Fatalf("a write ran %d sessions, want the staged body and the commit", len(sessions))
	}
	staged := sessions[0].cmd[5]
	if !strings.HasPrefix(staged, workspace+"/.cella-write-") {
		t.Fatalf("the body was staged at %q", staged)
	}
	if body := f.typedInto(sessions[0]); body != "hello" {
		t.Fatalf("the body reached the engine as %q", body)
	}
	if !slices.Equal(sessions[1].cmd, fileshell.Commit(workspace, staged, p, 0o600)) {
		t.Fatalf("the commit sent %q", sessions[1].cmd)
	}

	// A body past the bound is the program's refusal, and nothing is
	// committed after it.
	f2, d2 := filesFake(t)
	f2.run = func(cmd []string, _ []string, _ string) fakeExecResult {
		if isProgram(cmd, fileshell.WriteBody(workspace, "s", 0)) {
			return fakeExecResult{code: fileshell.ExitTooLarge}
		}
		return fakeExecResult{}
	}
	if _, err := d2.Write(t.Context(), "sbx_a", driver.WriteRequest{
		Path: p, MaxBytes: 4, Body: strings.NewReader("12345"),
	}); !errors.Is(err, driver.ErrTooLarge) {
		t.Fatalf("a body past the bound answered %v", err)
	}
	for _, session := range f2.sessions() {
		if isProgram(session.cmd, fileshell.Commit(workspace, "s", "d", 0o644)) {
			t.Fatal("a refused write was committed")
		}
	}

	// A body that ended early leaves nothing staged and commits nothing.
	f3, d3 := filesFake(t)
	f3.run = func([]string, []string, string) fakeExecResult { return fakeExecResult{stdout: "4"} }
	if _, err := d3.Write(t.Context(), "sbx_a", driver.WriteRequest{
		Path: p, Body: io.MultiReader(strings.NewReader("half"), failingReader{}),
	}); err == nil {
		t.Fatal("a body that failed part way was written")
	}
	sent := f3.sessions()
	if len(sent) != 2 || !isProgram(sent[1].cmd, fileshell.Discard(workspace, "s")) {
		t.Fatalf("a failed body ran %d sessions, want the staged write and a discard", len(sent))
	}
}

// isProgram reports whether an argv runs the same program as a sample of it,
// whatever arguments each carries.
func isProgram(argv, sample []string) bool {
	return len(argv) > 2 && len(sample) > 2 && argv[2] == sample[2]
}

// failingReader is a body that ends before it said it would.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the body failed part way") }

func TestFileStoreStreamsAFile(t *testing.T) {
	f, d := filesFake(t)
	const p = workspace + "/a.txt"
	f.run = func(cmd []string, _ []string, _ string) fakeExecResult {
		if isProgram(cmd, fileshell.Stat(workspace, "p")) {
			return fakeExecResult{stdout: statRecord("644", "regular file", "5", "1700000000", p)}
		}
		return fakeExecResult{stdout: "hello"}
	}
	body, info, err := d.Open(t.Context(), "sbx_a", p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 5 || info.Name != "a.txt" {
		t.Fatalf("the entry is %+v", info)
	}
	read, err := io.ReadAll(body)
	if err != nil || string(read) != "hello" {
		t.Fatalf("the stream carried %q: %v", read, err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}

	// A program that fails part way reports why rather than ending the body.
	f.run = func(cmd []string, _ []string, _ string) fakeExecResult {
		if isProgram(cmd, fileshell.Stat(workspace, "p")) {
			return fakeExecResult{stdout: statRecord("644", "regular file", "5", "1700000000", p)}
		}
		return fakeExecResult{stdout: "hel", stderr: "cat: gone", code: fileshell.ExitAbsent}
	}
	body, _, err = d.Open(t.Context(), "sbx_a", p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	if _, err := io.ReadAll(body); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("a stream that failed answered %v", err)
	}

	// A directory is not a stream.
	f.run = func([]string, []string, string) fakeExecResult {
		return fakeExecResult{stdout: statRecord("755", "directory", "4096", "1700000000", workspace)}
	}
	if _, _, err := d.Open(t.Context(), "sbx_a", workspace); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("opening a directory answered %v", err)
	}
}

// TestFileStoreNeedsARunningSandbox: this driver reaches the workspace over an
// exec session, which a stopped container has none of, and the contract's
// answer for that is ErrNotRunning.
func TestFileStoreNeedsARunningSandbox(t *testing.T) {
	f, d := filesFake(t)
	f.run = func([]string, []string, string) fakeExecResult { return fakeExecResult{} }
	if err := d.Stop(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"Stat":    errOf(d.Stat(t.Context(), "sbx_a", workspace+"/a")),
		"ReadDir": errOf(d.ReadDir(t.Context(), "sbx_a", workspace)),
		"Open":    openErr(d, "sbx_a", workspace+"/a"),
		"Write":   errOf(d.Write(t.Context(), "sbx_a", driver.WriteRequest{Path: workspace + "/a", Body: strings.NewReader("x")})),
		"Mkdir":   d.Mkdir(t.Context(), "sbx_a", workspace+"/a"),
		"Remove":  d.Remove(t.Context(), "sbx_a", workspace+"/a"),
		"Move":    d.Move(t.Context(), "sbx_a", workspace+"/a", workspace+"/b"),
	} {
		if !errors.Is(err, driver.ErrNotRunning) {
			t.Errorf("%s on a stopped sandbox answered %v", name, err)
		}
	}
}

func TestFileStoreRefusesBeforeItSends(t *testing.T) {
	f, d := filesFake(t)
	for _, p := range []string{"/etc/hosts", workspace + "/../etc", "relative", "", workspace + "/nul\x00byte"} {
		if _, err := d.Stat(t.Context(), "sbx_a", p); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("Stat %q answered %v", p, err)
		}
	}
	if err := d.Move(t.Context(), "sbx_a", workspace+"/a", "/etc/hosts"); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a move out of the workspace answered %v", err)
	}
	if err := d.Remove(t.Context(), "sbx_a", workspace); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("removing the workspace answered %v", err)
	}
	if err := d.Move(t.Context(), "sbx_a", workspace, workspace+"/a"); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("moving the workspace answered %v", err)
	}
	if got := len(f.sessions()); got != 0 {
		t.Errorf("a refused path still ran %d session(s)", got)
	}
	if _, err := d.Stat(t.Context(), "sbx_absent", workspace); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a sandbox that is not there answered %v", err)
	}
}

func errOf[T any](_ T, err error) error { return err }

func openErr(d *Driver, id, path string) error {
	body, _, err := d.Open(context.Background(), id, path)
	if body != nil {
		_ = body.Close()
	}
	return err
}
