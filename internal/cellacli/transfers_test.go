// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/cella/internal/cellacli"
)

// TestCopyCarriesATreeOutOfASandbox: the archive route is streamed to disk
// and extracted under the destination, with the count the command reports.
func TestCopyCarriesATreeOutOfASandbox(t *testing.T) {
	archive := tarOf(map[string]string{
		"workspace/a.txt":     "one",
		"workspace/sub/b.txt": "two",
	})
	p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(archive)
	})
	destination := filepath.Join(t.TempDir(), "out")
	got := p.run(t, "", "cp", "dev:/workspace", destination)
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "2 file(s) written") {
		t.Errorf("stdout is %q", got.stdout)
	}
	for name, want := range map[string]string{
		filepath.Join(destination, "workspace", "a.txt"):        "one",
		filepath.Join(destination, "workspace", "sub", "b.txt"): "two",
	} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s was not written: %v", name, err)
		}
		if string(data) != want {
			t.Errorf("%s holds %q, want %q", name, data, want)
		}
	}
	if seen := p.last(); seen.Query.Get("path") != "/workspace" {
		t.Errorf("the export named %q", seen.Query.Get("path"))
	}
}

// TestAnArchiveThatWouldLeaveTheDestinationIsRefused: the names in an
// archive come from the other side, so the extraction holds them inside the
// directory the caller named.
func TestAnArchiveThatWouldLeaveTheDestinationIsRefused(t *testing.T) {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	body := "escaped"
	_ = w.WriteHeader(&tar.Header{Name: "../../escaped.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = w.Write([]byte(body))
	_ = w.Close()
	p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(buf.Bytes())
	})
	dir := t.TempDir()
	destination := filepath.Join(dir, "out")
	if got := p.run(t, "", "cp", "dev:/workspace", destination); got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	// The name is cleaned to the destination rather than followed out of it.
	if _, err := os.Stat(filepath.Join(dir, "escaped.txt")); err == nil {
		t.Fatal("the archive wrote outside the destination")
	}
	if _, err := os.Stat(filepath.Join(destination, "escaped.txt")); err != nil {
		t.Fatalf("the entry was not written inside the destination: %v", err)
	}
}

// TestCopyCarriesATreeIntoASandbox: a local tree is streamed as one archive
// under dest, with the names relative to the source's own parent.
func TestCopyCarriesATreeIntoASandbox(t *testing.T) {
	source := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(source, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"a.txt": "one", "sub/b.txt": "two"} {
		if err := os.WriteFile(filepath.Join(source, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	got := p.run(t, "", "cp", source, "dev:/workspace")
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	seen := p.last()
	if seen.Method != "PUT" || seen.Query.Get("dest") != "/workspace" {
		t.Fatalf("the import called %s with dest %q", seen.Method, seen.Query.Get("dest"))
	}
	names := map[string]string{}
	tr := tar.NewReader(strings.NewReader(seen.Body))
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		names[header.Name] = string(content)
	}
	for name, want := range map[string]string{"tree/a.txt": "one", "tree/sub/b.txt": "two"} {
		if names[name] != want {
			t.Errorf("the archive holds %q for %s, want %q (it holds %v)", names[name], name, want, names)
		}
	}
	// One file on its own travels the same way.
	single := p.run(t, "", "cp", filepath.Join(source, "a.txt"), "dev:/workspace")
	if single.code != 0 {
		t.Fatalf("exit %d, stderr %q", single.code, single.stderr)
	}
	if !strings.Contains(p.last().Body, "a.txt") {
		t.Error("the single file did not travel in the archive")
	}
	// A source that is not there is the caller's own mistake.
	if missing := p.run(t, "", "cp", filepath.Join(source, "absent"), "dev:/workspace"); missing.code != 2 {
		t.Errorf("a source that is not there exited %d, want 2", missing.code)
	}
}

// TestFilesGetAndPutReachDisk: one file in either direction, with the
// destination the caller named.
func TestFilesGetAndPutReachDisk(t *testing.T) {
	p := newPlane(t, routes)
	dir := t.TempDir()
	destination := filepath.Join(dir, "a.txt")
	got := p.run(t, "", "files", "get", "dev:/workspace/a.txt", destination, "--json")
	if got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "abc" {
		t.Fatalf("the file holds %q, %v", data, err)
	}
	if !strings.Contains(got.stdout, `"bytes":3`) {
		t.Errorf("the answer is %q", got.stdout)
	}

	source := filepath.Join(dir, "b.txt")
	if err = os.WriteFile(source, []byte("written from a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if put := p.run(t, "", "files", "put", source, "dev:/workspace/b.txt"); put.code != 0 {
		t.Fatalf("exit %d, stderr %q", put.code, put.stderr)
	}
	if p.last().Body != "written from a file" {
		t.Fatalf("what arrived is %q", p.last().Body)
	}
	// A source that is not there is a usage error and reaches no request.
	if missing := p.run(t, "", "files", "put", filepath.Join(dir, "absent"), "dev:/workspace/c.txt"); missing.code != 2 {
		t.Errorf("a source that is not there exited %d, want 2", missing.code)
	}
	// A destination that cannot be created is one too.
	if bad := p.run(t, "", "files", "get", "dev:/workspace/a.txt", filepath.Join(source, "under-a-file")); bad.code != 2 {
		t.Errorf("a destination under a file exited %d, want 2", bad.code)
	}
}

// TestApplyWaitsForTheSandboxToRun is -w: the object is read until it is
// Running, and a sandbox that failed ends the wait.
func TestApplyWaitsForTheSandboxToRun(t *testing.T) {
	document := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"dev"},"spec":{"image":"example/image:1"}}`
	t.Run("a sandbox that becomes Running", func(t *testing.T) {
		var reads atomic.Int32
		p := newPlane(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, sandboxJSON("dev", "sbx_1", "Pending"))
				return
			}
			phase := "Pending"
			if reads.Add(1) > 1 {
				phase = "Running"
			}
			_, _ = io.WriteString(w, sandboxJSON("dev", "sbx_1", phase))
		})
		got := p.run(t, document, "apply", "-f", "-", "-w")
		if got.code != 0 {
			t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
		}
		if reads.Load() < 2 {
			t.Errorf("the wait read the object %d time(s)", reads.Load())
		}
	})

	t.Run("a sandbox that failed", func(t *testing.T) {
		p := newPlane(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusCreated)
			}
			_, _ = io.WriteString(w, sandboxJSON("dev", "sbx_1", "Failed"))
		})
		got := p.run(t, document, "apply", "-f", "-", "-w")
		if got.code != 1 {
			t.Fatalf("exit %d, want 1", got.code)
		}
		if !strings.Contains(got.stderr, "Failed") {
			t.Errorf("stderr is %q", got.stderr)
		}
	})

	t.Run("a sandbox that never runs", func(t *testing.T) {
		p := newPlane(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusCreated)
			}
			_, _ = io.WriteString(w, sandboxJSON("dev", "sbx_1", "Pending"))
		})
		got := runWith(t, cellacli.Env{
			Args:   []string{"apply", "-f", "-", "-w", "--timeout", "10ms"},
			Stdin:  strings.NewReader(document),
			Getenv: environment(map[string]string{"CELLA_URL": p.server.URL, "CELLA_TOKEN": "t"}),
		})
		if got.code != 1 || !strings.Contains(got.stderr, "Pending") {
			t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
		}
	})
}

// TestApplyWaits is --wait and its short form -w: the create asks the server
// to hold its answer with the command's timeout, and a server that answers
// the sandbox Running ends the command with no read after it. Without the
// flag the create asks for no hold.
func TestApplyWaits(t *testing.T) {
	document := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"dev"},"spec":{"image":"example/image:1"}}`
	p := newPlane(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, sandboxJSON("dev", "sbx_1", "Running"))
	})
	for _, flags := range [][]string{{"--wait"}, {"-w", "--timeout", "30s"}} {
		before := len(p.seen())
		got := p.run(t, document, append([]string{"apply", "-f", "-"}, flags...)...)
		if got.code != 0 {
			t.Fatalf("%v exited %d, stderr %q", flags, got.code, got.stderr)
		}
		calls := p.seen()[before:]
		if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Query.Get("wait") != "1" {
			t.Fatalf("%v sent %+v, want one held create", flags, calls)
		}
		if want := map[bool]string{true: "2m0s", false: "30s"}[flags[0] == "--wait"]; calls[0].Query.Get("timeout") != want {
			t.Fatalf("%v sent the bound %q, want %q", flags, calls[0].Query.Get("timeout"), want)
		}
	}
	if got := p.run(t, document, "apply", "-f", "-"); got.code != 0 || p.last().Query.Has("wait") {
		t.Fatalf("a create without --wait exited %d and sent %v", got.code, p.last().Query)
	}
}

// failingWriter is a stream that cannot be written, which is what a closed
// pipe on the other side of the command is.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the stream is gone") }

// TestAnOutputThatCannotBeWrittenIsReported: a command whose output stream
// is gone fails rather than reporting success it could not deliver.
func TestAnOutputThatCannotBeWrittenIsReported(t *testing.T) {
	p := newPlane(t, routes)
	for _, args := range [][]string{
		{"get", "sandboxes"},
		{"get", "sandboxes", "--json"},
		{"get", "sandboxes", "-o", "name"},
		{"get", "sandbox", "dev"},
		{"get", "sandbox", "dev", "--json"},
		{"get", "sandbox", "dev", "-o", "name"},
		{"get", "secrets"},
		{"get", "secrets", "-o", "name"},
		{"get", "secret", "api"},
		{"get", "secret", "api", "--json"},
		{"get", "secret", "api", "-o", "name"},
		{"delete", "sandbox", "dev"},
		{"start", "dev"},
		{"egress", "dev"},
		{"egress", "dev", "--json"},
		{"files", "ls", "dev:/workspace"},
		{"files", "stat", "dev:/workspace/a.txt"},
		{"files", "stat", "dev:/workspace/a.txt", "--json"},
		{"files", "mkdir", "dev:/workspace/sub"},
		{"files", "mkdir", "dev:/workspace/sub", "--json"},
		{"files", "rm", "dev:/workspace/a.txt"},
		{"files", "mv", "dev:/workspace/a.txt", "dev:/workspace/b.txt"},
		{"files", "get", "dev:/workspace/a.txt"},
		{"files", "put", "-", "dev:/workspace/b.txt"},
		{"logs", "dev"},
		{"version"},
		{"version", "--json"},
		{"cp", "dev:/workspace", filepath.Join(t.TempDir(), "out")},
	} {
		var errOut bytes.Buffer
		code := cellacli.Run(context.Background(), cellacli.Env{
			Args:   args,
			Stdin:  strings.NewReader(""),
			Stdout: failingWriter{},
			Stderr: &errOut,
			Now:    func() time.Time { return atTheSameHour },
			Getenv: environment(map[string]string{"CELLA_URL": p.server.URL, "CELLA_TOKEN": "t"}),
		})
		if code != 1 {
			t.Errorf("`cella %s` exited %d with no output stream, want 1", strings.Join(args, " "), code)
		}
	}
}

// TestApplyReportsAnOutputItCouldNotWrite is the same rule on the two
// commands whose answer is the object they applied.
func TestApplyReportsAnOutputItCouldNotWrite(t *testing.T) {
	p := newPlane(t, routes)
	document := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"dev"},"spec":{"image":"i"}}`
	for _, args := range [][]string{{"apply", "-f", "-"}, {"apply", "-f", "-", "--json"}} {
		var errOut bytes.Buffer
		code := cellacli.Run(context.Background(), cellacli.Env{
			Args:   args,
			Stdin:  strings.NewReader(document),
			Stdout: failingWriter{},
			Stderr: &errOut,
			Getenv: environment(map[string]string{"CELLA_URL": p.server.URL, "CELLA_TOKEN": "t"}),
		})
		if code != 1 {
			t.Errorf("`cella %s` exited %d with no output stream, want 1", strings.Join(args, " "), code)
		}
	}
}

// TestASecretValueThatIsNotThereIsAUsageError: the two sources are
// exclusive, and a source that holds nothing is the caller's own mistake.
func TestASecretValueThatIsNotThereIsAUsageError(t *testing.T) {
	p := newPlane(t, routes)
	document := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Secret","metadata":{"name":"api"},"spec":{"kind":"static"}}`
	for _, args := range [][]string{
		{"apply", "-f", "-", "--value-from-env", "NOTHING_HOLDS_THIS"},
		{"apply", "-f", "-", "--value-file", filepath.Join(t.TempDir(), "absent")},
		{"apply", "-f", "-", "--value-from-env", "A", "--value-file", "b"},
		{"apply", "-f", "-", "--value-from-env", "A", "-w"},
	} {
		got := p.run(t, document, args...)
		if got.code != 2 {
			t.Errorf("`cella %s` exited %d, want 2: %q", strings.Join(args, " "), got.code, got.stderr)
		}
	}
	// A value belongs to a Secret and not to a Sandbox.
	sandbox := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"dev"},"spec":{}}`
	if got := p.run(t, sandbox, "apply", "-f", "-", "--value-file", "anything"); got.code != 2 {
		t.Errorf("a Sandbox with a value exited %d, want 2", got.code)
	}
}

// TestEveryFileSubcommandReportsARefusal: each route answers the envelope of
// design 008, and each subcommand exits by it.
func TestEveryFileSubcommandReportsARefusal(t *testing.T) {
	p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
		refuse(w, 404, "not_found", "There is no such object.")
	})
	for _, args := range [][]string{
		{"files", "ls", "dev:/workspace"},
		{"files", "stat", "dev:/workspace/a.txt"},
		{"files", "get", "dev:/workspace/a.txt"},
		{"files", "put", "-", "dev:/workspace/a.txt"},
		{"files", "mkdir", "dev:/workspace/sub"},
		{"files", "rm", "dev:/workspace/a.txt"},
		{"files", "mv", "dev:/workspace/a.txt", "dev:/workspace/b.txt"},
		{"cp", "dev:/workspace", t.TempDir()},
		{"cp", "testdata", "dev:/workspace"},
	} {
		got := p.run(t, "", args...)
		if got.code != 4 {
			t.Errorf("`cella %s` exited %d, want 4", strings.Join(args, " "), got.code)
		}
	}
}

// TestAnArchiveCarriesItsDirectoriesAndItsFailures: a directory entry is
// made on the way in, and an archive that ends early is a failure the
// command reports.
func TestAnArchiveCarriesItsDirectoriesAndItsFailures(t *testing.T) {
	var whole bytes.Buffer
	w := tar.NewWriter(&whole)
	_ = w.WriteHeader(&tar.Header{Name: "workspace/sub/", Mode: 0o755, Typeflag: tar.TypeDir})
	_ = w.WriteHeader(&tar.Header{Name: "workspace/sub/a.txt", Mode: 0o600, Size: 3, Typeflag: tar.TypeReg})
	_, _ = w.Write([]byte("one"))
	_ = w.Close()

	p := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(whole.Bytes())
	})
	destination := filepath.Join(t.TempDir(), "out")
	if got := p.run(t, "", "cp", "dev:/workspace", destination); got.code != 0 {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	info, err := os.Stat(filepath.Join(destination, "workspace", "sub"))
	if err != nil || !info.IsDir() {
		t.Fatalf("the directory entry was not made: %v", err)
	}

	cut := newPlane(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(whole.Bytes()[:300])
	})
	if got := cut.run(t, "", "cp", "dev:/workspace", filepath.Join(t.TempDir(), "out")); got.code != 1 {
		t.Fatalf("an archive that ended early exited %d, want 1", got.code)
	}
}
