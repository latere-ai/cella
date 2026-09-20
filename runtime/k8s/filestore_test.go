// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/internal/fileshell"
)

// The programs themselves are proven where a filesystem is: the podman
// package runs the conformance suite against a real engine and
// TestClusterConformance runs it against a cluster. What this package owns is
// what it sends and what it makes of the answer, which is what these tests
// hold.

// workspace is where the fixture's files live, which every program is
// measured against.
const workspace = driver.DefaultWorkdir

// record renders one stat record as a program prints it.
func record(mode, kind, size, mtime, name string) string {
	return strings.Join([]string{mode, kind, size, mtime, name}, "|") + "\x00"
}

// answer arranges one program's output and its exit.
func (h *harness) answer(out string, err error) {
	h.exec.handle = func(_ context.Context, _ execCall, _ io.Reader, stdout, _ io.Writer) error {
		if out != "" {
			if _, writeErr := io.WriteString(stdout, out); writeErr != nil {
				return writeErr
			}
		}
		return err
	}
}

// programs is every argv the recorder saw, with the program text dropped, so
// a test reads the operations and their arguments.
func (h *harness) programs() [][]string {
	h.exec.mu.Lock()
	defer h.exec.mu.Unlock()
	var out [][]string
	for _, c := range h.exec.calls {
		out = append(out, c.argv)
	}
	return out
}

func TestFileStoreSendsOneProgramPerOperation(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_files"
	h.created(t, spec(id))
	const p = workspace + "/dir/a.txt"
	for _, tc := range []struct {
		name string
		call func() error
		want []string
	}{
		{"Stat", func() error {
			h.answer(record("644", "regular file", "5", "1700000000", p), nil)
			_, err := h.Stat(t.Context(), id, p)
			return err
		}, fileshell.Stat(workspace, p)},
		{"ReadDir", func() error {
			h.answer("", nil)
			_, err := h.ReadDir(t.Context(), id, workspace)
			return err
		}, fileshell.ReadDir(workspace, workspace)},
		{"Mkdir", func() error {
			h.answer("", nil)
			return h.Mkdir(t.Context(), id, workspace+"/dir")
		}, fileshell.Mkdir(workspace, workspace+"/dir")},
		{"Remove", func() error {
			h.answer("", nil)
			return h.Remove(t.Context(), id, p)
		}, fileshell.Remove(workspace, p)},
		{"Move", func() error {
			h.answer("", nil)
			return h.Move(t.Context(), id, p, workspace+"/b.txt")
		}, fileshell.Move(workspace, p, workspace+"/b.txt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got := h.exec.last(); !slices.Equal(got.argv, tc.want) {
				t.Fatalf("%s sent %q, want %q", tc.name, got.argv, tc.want)
			}
			if got := h.exec.last().pod; got != objectName(id) {
				t.Fatalf("%s ran in %q, want the sandbox's own Pod", tc.name, got)
			}
		})
	}
}

func TestFileStoreReadsTheRecords(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_records"
	h.created(t, spec(id))

	h.answer(record("640", "regular file", "12", "1700000000", workspace+"/a.txt"), nil)
	info, err := h.Stat(t.Context(), id, workspace+"/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "a.txt" || info.Path != workspace+"/a.txt" || info.Size != 12 || info.Mode.Perm() != 0o640 || info.IsDir {
		t.Fatalf("the entry is %+v", info)
	}
	if info.ModTime.Unix() != 1700000000 {
		t.Fatalf("the entry's modification time is %v", info.ModTime)
	}

	// The programs print in the shell's order; the contract's is by name.
	h.answer(record("755", "directory", "4096", "1700000001", "sub")+
		record("600", "regular file", "1", "1700000002", "a b|c\nd")+
		record("644", "regular file", "2", "1700000003", ".hidden"), nil)
	entries, err := h.ReadDir(t.Context(), id, workspace)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{".hidden", "a b|c\nd", "sub"}) {
		t.Fatalf("the listing is %q", names)
	}
	if entries[2].Size != 0 || !entries[2].IsDir || entries[2].Path != workspace+"/sub" {
		t.Fatalf("the directory entry is %+v", entries[2])
	}

	h.answer("644|regular file|nonsense", nil)
	if _, err := h.ReadDir(t.Context(), id, workspace); err == nil {
		t.Fatal("a record that is not one was read as a listing")
	}
}

func TestFileStoreMapsTheExits(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_exits"
	h.created(t, spec(id))
	for _, tc := range []struct {
		code int
		want error
	}{
		{fileshell.ExitOutside, driver.ErrInvalid},
		{fileshell.ExitAbsent, driver.ErrNotFound},
		{fileshell.ExitKind, driver.ErrInvalid},
		{fileshell.ExitTooLarge, driver.ErrTooLarge},
	} {
		h.answer("", exited(tc.code))
		if _, err := h.Stat(t.Context(), id, workspace+"/a"); !errors.Is(err, tc.want) {
			t.Errorf("exit %d answered %v, want %v", tc.code, err, tc.want)
		}
	}
	// A code the programs do not use is the operation's own failure, with
	// what it said about it.
	h.exec.handle = func(_ context.Context, _ execCall, _ io.Reader, _, stderr io.Writer) error {
		_, _ = io.WriteString(stderr, "stat: Permission denied")
		return exited(2)
	}
	err := h.Mkdir(t.Context(), id, workspace+"/a")
	if err == nil || errors.Is(err, driver.ErrInvalid) || !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("an unexpected exit answered %v", err)
	}
}

func TestFileStoreCommitsAWriteOnlyOnceTheBodyIsWhole(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_write"
	h.created(t, spec(id))
	const p = workspace + "/a.txt"

	var body strings.Builder
	h.exec.handle = func(_ context.Context, c execCall, stdin io.Reader, stdout, _ io.Writer) error {
		if stdin != nil {
			n, err := io.Copy(&body, stdin)
			if err != nil {
				return err
			}
			_, _ = io.WriteString(stdout, strconv.FormatInt(n, 10))
		}
		_ = c
		return nil
	}
	n, err := h.Write(t.Context(), id, driver.WriteRequest{Path: p, Mode: 0o600, Body: strings.NewReader("hello")})
	if err != nil || n != 5 {
		t.Fatalf("the write reported %d bytes: %v", n, err)
	}
	if body.String() != "hello" {
		t.Fatalf("the body reached the container as %q", body.String())
	}
	sent := h.programs()
	if len(sent) != 2 {
		t.Fatalf("a write sent %d programs, want the staged body and the commit", len(sent))
	}
	staged := sent[0][5]
	if !strings.HasPrefix(staged, workspace+"/.cella-write-") {
		t.Fatalf("the body was staged at %q", staged)
	}
	if !slices.Equal(sent[1], fileshell.Commit(workspace, staged, p, 0o600)) {
		t.Fatalf("the commit sent %q", sent[1])
	}
	if h.exec.calls[0].pod != h.exec.calls[1].pod {
		t.Fatalf("the write ran in two containers: %q and %q", h.exec.calls[0].pod, h.exec.calls[1].pod)
	}

	// A body that ends early commits nothing and leaves nothing staged.
	h.exec.calls = nil
	h.exec.handle = func(_ context.Context, _ execCall, stdin io.Reader, _, _ io.Writer) error {
		if stdin != nil {
			_, err := io.Copy(io.Discard, stdin)
			return err
		}
		return nil
	}
	_, err = h.Write(t.Context(), id, driver.WriteRequest{
		Path: p, Body: io.MultiReader(strings.NewReader("half"), failingReader{}),
	})
	if err == nil {
		t.Fatal("a body that failed part way was written")
	}
	sent = h.programs()
	if len(sent) != 2 || !slices.Equal(sent[1], fileshell.Discard(workspace, sent[0][5])) {
		t.Fatalf("a failed body sent %q, want the staged write and a discard", sent)
	}
}

// failingReader is a body that ends before it said it would.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the body failed part way") }

func TestFileStoreStreamsAFileAndEndsIt(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_stream"
	h.created(t, spec(id))
	const p = workspace + "/a.txt"

	stopped := make(chan struct{}, 1)
	h.exec.handle = func(ctx context.Context, c execCall, _ io.Reader, stdout, _ io.Writer) error {
		if slices.Equal(c.argv, fileshell.Stat(workspace, p)) {
			_, err := io.WriteString(stdout, record("644", "regular file", "5", "1700000000", p))
			return err
		}
		if _, err := io.WriteString(stdout, "hel"); err != nil {
			return err
		}
		<-ctx.Done()
		stopped <- struct{}{}
		return ctx.Err()
	}
	body, info, err := h.Open(t.Context(), id, p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 5 {
		t.Fatalf("the entry is %+v", info)
	}
	head := make([]byte, 3)
	if _, err := io.ReadFull(body, head); err != nil || string(head) != "hel" {
		t.Fatalf("the stream carried %q: %v", head, err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	<-stopped

	// A program that fails part way reports why instead of ending the body.
	h.exec.handle = func(_ context.Context, c execCall, _ io.Reader, stdout, _ io.Writer) error {
		if slices.Equal(c.argv, fileshell.Stat(workspace, p)) {
			_, err := io.WriteString(stdout, record("644", "regular file", "5", "1700000000", p))
			return err
		}
		_, _ = io.WriteString(stdout, "hel")
		return exited(fileshell.ExitAbsent)
	}
	body, _, err = h.Open(t.Context(), id, p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	if _, err := io.ReadAll(body); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("a stream that failed answered %v", err)
	}

	// A directory is not a stream.
	h.answer(record("755", "directory", "4096", "1700000000", workspace), nil)
	if _, _, err := h.Open(t.Context(), id, workspace); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("opening a directory answered %v", err)
	}
}

func TestFileStoreUsesAHelperWhileStopped(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_stopped_files"
	h.created(t, spec(id))
	if err := h.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	h.answer("", nil)
	if err := h.Mkdir(t.Context(), id, workspace+"/late"); err != nil {
		t.Fatal(err)
	}
	pod := h.exec.last().pod
	if pod != objectName(id)+"-files" {
		t.Fatalf("a stopped sandbox ran the program in %q", pod)
	}
	pods, err := h.cs.CoreV1().Pods(namespace).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pods.Items {
		if p.Name == pod {
			t.Fatalf("the helper Pod %s outlived the operation", pod)
		}
	}
}

func TestFileStoreRefusesBeforeItSends(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_refuse"
	h.created(t, spec(id))
	for _, p := range []string{"/etc/hosts", workspace + "/../etc", "relative", "", workspace + "/nul\x00byte"} {
		if _, err := h.Stat(t.Context(), id, p); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("Stat %q answered %v", p, err)
		}
	}
	if err := h.Move(t.Context(), id, workspace+"/a", "/etc/hosts"); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a move out of the workspace answered %v", err)
	}
	if err := h.Remove(t.Context(), id, workspace); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("removing the workspace answered %v", err)
	}
	if err := h.Move(t.Context(), id, workspace, workspace+"/a"); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("moving the workspace answered %v", err)
	}
	if h.exec.count() != 0 {
		t.Errorf("a refused path still sent %d program(s)", h.exec.count())
	}
	if _, err := h.Stat(t.Context(), "sbx_absent", workspace); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a sandbox that is not there answered %v", err)
	}
}

// TestFileStoreNeedsACluster: a driver built without a connection answers the
// capability it cannot provide rather than dialling nothing.
func TestFileStoreNeedsACluster(t *testing.T) {
	h := newHarness(t)
	const id = "sbx_nostream"
	h.created(t, spec(id))
	h.stream = nil
	if _, err := h.Stat(t.Context(), id, workspace); !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("a driver without a cluster connection answered %v", err)
	}
}
