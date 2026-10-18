// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package local

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// TestCreateRefusesWhatItCannotRun: an id that names no directory, a negative
// deadline, arguments with no command, a pool entry, an image and a volume
// are refused, and a second create of one id finds it there.
func TestCreateRefusesWhatItCannotRun(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Lookup: os.LookupEnv})
	for name, c := range map[string]struct {
		spec driver.CreateSpec
		want error
	}{
		"a bad id":            {driver.CreateSpec{ID: "../escape"}, driver.ErrInvalid},
		"a negative ttl":      {driver.CreateSpec{ID: "sbx_ttl", Lifecycle: driver.Lifecycle{TTL: -time.Second}}, driver.ErrInvalid},
		"args and no command": {driver.CreateSpec{ID: "sbx_args", Args: []string{"x"}}, driver.ErrInvalid},
		"a pool entry":        {driver.CreateSpec{ID: "sbx_pool", Prewarm: true}, driver.ErrUnsupported},
		"an image":            {driver.CreateSpec{ID: "sbx_image", Image: "registry.example/base:1"}, driver.ErrUnsupported},
		"a volume":            {driver.CreateSpec{ID: "sbx_volume", Volumes: []driver.Mount{{Path: "/data"}}}, driver.ErrUnsupported},
		"a workdir outside":   {driver.CreateSpec{ID: "sbx_workdir", Workdir: "/elsewhere"}, driver.ErrInvalid},
	} {
		if _, err := d.Create(context.Background(), c.spec); !errors.Is(err, c.want) {
			t.Errorf("%s answered %v, want %v", name, err, c.want)
		}
	}
	sandbox(t, d, driver.CreateSpec{ID: "sbx_twice", Lifecycle: driver.Lifecycle{TTL: time.Hour}})
	if _, err := d.Create(context.Background(), driver.CreateSpec{ID: "sbx_twice"}); !errors.Is(err, driver.ErrAlreadyExists) {
		t.Errorf("a second create answered %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Create(ctx, driver.CreateSpec{ID: "sbx_canceled"}); !errors.Is(err, context.Canceled) {
		t.Errorf("a create whose caller left answered %v", err)
	}
	for _, call := range []func() error{
		func() error { _, err := d.Inspect(ctx, "sbx_twice"); return err },
		func() error { _, err := d.List(ctx, driver.Filter{}); return err },
		func() error { return d.Start(ctx, "sbx_twice") },
		func() error { return d.Stop(ctx, "sbx_twice") },
		func() error { return d.Delete(ctx, "sbx_twice") },
		func() error { return d.Touch(ctx, "sbx_twice") },
		func() error {
			_, err := d.Exec(ctx, "sbx_twice", driver.ExecRequest{Command: []string{"true"}})
			return err
		},
		func() error { _, err := d.Logs(ctx, "sbx_twice", driver.LogsRequest{}); return err },
		func() error { return d.Ready(ctx) },
	} {
		if err := call(); !errors.Is(err, context.Canceled) {
			t.Errorf("a call whose caller left answered %v", err)
		}
	}
	if _, err := d.Inspect(context.Background(), "../escape"); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("an inspect of a bad id answered %v", err)
	}
	if err := os.WriteFile(d.record("sbx_twice"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Inspect(context.Background(), "sbx_twice"); err == nil {
		t.Error("a record that does not read was read")
	}
	if _, err := d.List(context.Background(), driver.Filter{}); err == nil {
		t.Error("a list over a record that does not read passed")
	}
}

// TestUpdateChangesWhatItMay: labels, the environment, the token and the
// deadlines change; an adoption, a change of size and a negative deadline are
// refused; and a touch stamps activity.
func TestUpdateChangesWhatItMay(t *testing.T) {
	shim(t)
	h := newHost(t)
	readme := filepath.Join(h.home, ".gitconfig")
	if err := os.WriteFile(readme, []byte("[user]"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := h.open(t, Options{ReadPaths: []string{readme}, Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_update", Token: []byte("first")})
	if link, err := os.Readlink(filepath.Join(d.paths("sbx_update").home, ".gitconfig")); err != nil || link != readme {
		t.Errorf("the home links the read path as %q, %v", link, err)
	}
	labels, env := map[string]string{"a": "1"}, map[string]string{"GREETING": "hi"}
	if err := d.Update(context.Background(), "sbx_update", driver.Change{
		Labels: &labels, Env: &env, Token: []byte("second"), Lifecycle: &driver.Lifecycle{TTL: time.Hour, AutoStop: time.Minute},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	r := mustLoad(t, d, "sbx_update")
	token, _ := os.ReadFile(d.paths("sbx_update").token)
	if r.State.Labels["a"] != "1" || r.Env["GREETING"] != "hi" || r.Env[driver.TokenFileEnv] == "" || string(token) != "second" ||
		r.State.AutoStop != time.Minute || r.State.ExpiresAt.IsZero() {
		t.Errorf("the update left %+v with the token %q", r, token)
	}
	if err := d.Update(context.Background(), "sbx_update", driver.Change{Lifecycle: &driver.Lifecycle{TTL: -time.Second}}); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a negative deadline answered %v", err)
	}
	if err := d.Update(context.Background(), "sbx_update", driver.Change{Adopt: &driver.Adoption{Owner: "alice"}}); err == nil {
		t.Error("an adoption was taken")
	}
	if err := d.Update(context.Background(), "sbx_update", driver.Change{Resources: &driver.Resources{CPU: "1"}}); err == nil {
		t.Error("a change of size was taken")
	}
	before := mustLoad(t, d, "sbx_update").State.LastActivityAt
	time.Sleep(10 * time.Millisecond)
	if err := d.Touch(context.Background(), "sbx_update"); err != nil {
		t.Fatal(err)
	}
	if after := mustLoad(t, d, "sbx_update").State.LastActivityAt; !after.After(before) {
		t.Error("a touch did not stamp activity")
	}
	if err := d.Touch(context.Background(), "sbx_absent"); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a touch of nothing answered %v", err)
	}
}

// TestExecRefusesWhatItCannotRun: a terminal, no command, a workdir outside
// the workspace and a sandbox that is not running are refused; a closed
// command answers Wait with the close.
func TestExecRefusesWhatItCannotRun(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_exec"})
	for name, req := range map[string]driver.ExecRequest{
		"a terminal":           {Command: []string{"true"}, TTY: true},
		"no command":           {},
		"a workdir outside":    {Command: []string{"true"}, Workdir: "/etc"},
		"a workdir up and out": {Command: []string{"true"}, Workdir: "/workspace/../etc"},
	} {
		if _, err := d.Exec(context.Background(), "sbx_exec", req); err == nil {
			t.Errorf("%s was run", name)
		}
	}
	e, err := d.Exec(context.Background(), "sbx_exec", driver.ExecRequest{Command: []string{"sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Wait(context.Background()); !errors.Is(err, driver.ErrNotRunning) {
		t.Errorf("a closed command answered %v", err)
	}
	if _, err = io.ReadAll(e.Stdout()); err != nil {
		t.Errorf("a closed command's output does not end: %v", err)
	}
	waiting, err := d.Exec(context.Background(), "sbx_exec", driver.ExecRequest{Command: []string{"sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err = waiting.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a wait past its context answered %v", err)
	}
	_ = waiting.Close()
	if err = d.Stop(context.Background(), "sbx_exec"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(context.Background(), "sbx_exec", driver.ExecRequest{Command: []string{"true"}}); !errors.Is(err, driver.ErrNotRunning) {
		t.Errorf("an exec in a stopped sandbox answered %v", err)
	}
	if err = d.Start(context.Background(), "sbx_exec"); err != nil {
		t.Fatal(err)
	}
	if err = d.Start(context.Background(), "sbx_exec"); err != nil {
		t.Errorf("a second start answered %v", err)
	}
}

// TestLogsReadTheMainStage: the last lines, a log not written yet, a since
// after the last write, and a followed log a reader closes.
func TestLogsReadTheMainStage(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_quiet"})
	if r, err := d.Logs(context.Background(), "sbx_quiet", driver.LogsRequest{}); err != nil {
		t.Fatal(err)
	} else if body, _ := io.ReadAll(r); len(body) != 0 {
		t.Errorf("a sandbox with no main command logged %q", body)
	}
	if _, err := d.Logs(context.Background(), "sbx_quiet", driver.LogsRequest{TailLines: -1}); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("negative tail lines answered %v", err)
	}
	sandbox(t, d, driver.CreateSpec{ID: "sbx_talks", Command: []string{"/bin/sh", "-c", "printf 'one\\ntwo\\n'; sleep 30"}})
	// The poll ends as soon as the line is there; the deadline only bounds a
	// stage that never writes, and a loaded suite under the race detector can
	// take seconds to start one.
	deadline := time.Now().Add(30 * time.Second)
	for {
		r, err := d.Logs(context.Background(), "sbx_talks", driver.LogsRequest{TailLines: 1})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(r)
		if string(body) == "two\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the last line reads %q", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	followed, err := d.Logs(context.Background(), "sbx_talks", driver.LogsRequest{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 4)
	if _, err = io.ReadFull(followed, head); err != nil || string(head) != "one\n" {
		t.Fatalf("the followed log opened with %q, %v", head, err)
	}
	if err = followed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(followed); err != nil {
		t.Errorf("a closed follow does not end: %v", err)
	}
	if got := lastLines([]byte("a\nb\nc\n"), 5); string(got) != "a\nb\nc\n" {
		t.Errorf("more lines than the log holds read %q", got)
	}
}

// TestArchivesHoldTheGrants: an archive goes into the sandbox's own
// workspace and never into a host folder; an export skips the deny table and
// what is not a file; and an archive of a bad path or an entry of another type
// is refused.
func TestArchivesHoldTheGrants(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Deny: []string{"~/project/secret.txt"}, Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_tar_host", Workspace: driver.Workspace{Path: h.project, Host: h.project}})
	if err := d.ImportTar(context.Background(), "sbx_tar_host", h.project, bytes.NewReader(archive(t, "x.txt", "x"))); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("an archive into a host folder answered %v", err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(h.project, "a-link")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := d.ExportTar(context.Background(), "sbx_tar_host", nil, &out); err != nil {
		t.Fatalf("ExportTar: %v", err)
	}
	names := map[string]bool{}
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
	}
	if names["secret.txt"] || names["a-link"] || !names["notes.txt"] {
		t.Errorf("the export named %v", names)
	}
	if err := d.ExportTar(context.Background(), "sbx_tar_host", []string{"/etc"}, io.Discard); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("an export outside the workspace answered %v", err)
	}
	if err := d.ExportTar(context.Background(), "sbx_absent", nil, io.Discard); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("an export of nothing answered %v", err)
	}
	sandbox(t, d, driver.CreateSpec{ID: "sbx_tar"})
	for name, body := range map[string][]byte{
		"a bad path":     archive(t, "../escape.txt", "x"),
		"a link entry":   linkArchive(t),
		"not an archive": []byte("not a tar archive at all, but long enough to be read as one header block"),
	} {
		if err := d.ImportTar(context.Background(), "sbx_tar", driver.DefaultWorkdir, bytes.NewReader(body)); err == nil {
			t.Errorf("%s was extracted", name)
		}
	}
	if err := d.ImportTar(context.Background(), "sbx_tar", driver.DefaultWorkdir+"/in", bytes.NewReader(archive(t, "dir/y.txt", "y"))); err != nil {
		t.Fatalf("ImportTar: %v", err)
	}
	if body, _, err := d.Open(context.Background(), "sbx_tar", driver.DefaultWorkdir+"/in/dir/y.txt"); err != nil {
		t.Errorf("the extracted file does not read: %v", err)
	} else {
		_ = body.Close()
	}
}

func archive(t *testing.T, name, body string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func linkArchive(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "link", Linkname: "/etc/hosts", Typeflag: tar.TypeSymlink}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestFilesInAReadOnlyGrantAndTheScratch: a read-only grant is read and never
// written, a move never crosses from the workspace to the scratch, and the
// scratch directories status.paths reports are reached by their host paths.
func TestFilesInAReadOnlyGrantAndTheScratch(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_docs", Workspace: driver.Workspace{Path: h.docs, Host: h.docs}})
	ctx := context.Background()
	readme := filepath.Join(h.docs, "readme.txt")
	if _, err := d.Stat(ctx, "sbx_docs", readme); err != nil {
		t.Errorf("a read-only file does not stat: %v", err)
	}
	for name, call := range map[string]func() error{
		"Write": func() error {
			_, err := d.Write(ctx, "sbx_docs", driver.WriteRequest{Path: readme, Body: strings.NewReader("x")})
			return err
		},
		"Mkdir":  func() error { return d.Mkdir(ctx, "sbx_docs", filepath.Join(h.docs, "new")) },
		"Remove": func() error { return d.Remove(ctx, "sbx_docs", readme) },
	} {
		if err := call(); !errors.Is(err, driver.ErrWorkspaceNotGranted) {
			t.Errorf("%s in a read-only grant answered %v", name, err)
		}
	}
	state, err := d.Inspect(ctx, "sbx_docs")
	if err != nil || state.Paths == nil {
		t.Fatalf("the sandbox reports no scratch paths: %+v, %v", state, err)
	}
	helper := filepath.Join(state.Paths.Home, "bin", "helper")
	if _, err = d.Write(ctx, "sbx_docs", driver.WriteRequest{Path: helper, Mode: 0o755, Body: strings.NewReader("#!/bin/sh\n")}); err != nil {
		t.Fatalf("a helper does not write into the scratch home: %v", err)
	}
	if info, err := d.Stat(ctx, "sbx_docs", helper); err != nil || info.Mode.Perm() != 0o755 {
		t.Errorf("the helper reads back as %+v, %v", info, err)
	}
	if err := d.Move(ctx, "sbx_docs", helper, filepath.Join(h.docs, "helper")); err == nil {
		t.Error("a move crossed from the scratch into the workspace")
	}
	if err := d.Remove(ctx, "sbx_docs", h.docs); err == nil {
		t.Error("the workspace itself was removed")
	}
	if _, err := os.Stat(h.docs); err != nil {
		t.Errorf("the host folder is gone: %v", err)
	}
	if err := d.Mkdir(ctx, "sbx_docs", filepath.Join(state.Paths.Tmp, "a", "b")); err != nil {
		t.Errorf("a directory does not make in the scratch: %v", err)
	}
	if _, err := d.Write(ctx, "sbx_docs", driver.WriteRequest{Path: filepath.Join(state.Paths.Tmp, "file"), Body: strings.NewReader("x")}); err != nil {
		t.Fatal(err)
	}
	if err := d.Mkdir(ctx, "sbx_docs", filepath.Join(state.Paths.Tmp, "file", "below")); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a directory below a file answered %v", err)
	}
	if _, err := d.Write(ctx, "sbx_docs", driver.WriteRequest{Path: filepath.Join(state.Paths.Tmp, "file", "below"), Body: strings.NewReader("x")}); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a file below a file answered %v", err)
	}
}

// TestTheHostReportsWhatItAllows: the host names its platform, grants and
// network, and an allow list's hosts are lowercased once each.
func TestTheHostReportsWhatItAllows(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Network: Network{Mode: NetworkAllowlist, Hosts: []string{"GitHub.com", "github.com", " ", "pypi.org"}}, Lookup: os.LookupEnv})
	host := d.Host()
	if host.OS == "" || host.Arch == "" || host.Network.Mode != NetworkAllowlist ||
		strings.Join(host.Network.Hosts, ",") != "github.com,pypi.org" {
		t.Errorf("the host reports %+v", host)
	}
	if d.Name() != Name || d.Isolation() != v1.IsolationProcess || !d.Capabilities().ExecStdin {
		t.Errorf("the driver declares %s %s %+v", d.Name(), d.Isolation(), d.Capabilities())
	}
	if got := expand(h.home, "~"); got != h.home {
		t.Errorf("~ expands to %s", got)
	}
	if _, err := New(Options{DataDir: tempDir(t), Home: h.home, ReadPaths: []string{filepath.Join(h.home, "absent")}}); err != nil {
		t.Errorf("a read path that does not exist yet was refused: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(d.data, "sandboxes")); err != nil {
		t.Fatal(err)
	}
	if err := d.Ready(context.Background()); err == nil {
		t.Error("a driver whose data directory is gone is ready")
	}
	if _, err := d.List(context.Background(), driver.Filter{}); err == nil {
		t.Error("a list over a data directory that is gone passed")
	}
}

// cancelOnRead cancels its context on its first read and hands back what it
// holds, so the copy reading through it meets a context that ended mid-way.
type cancelOnRead struct {
	cancel context.CancelFunc
	body   io.Reader
}

func (c *cancelOnRead) Read(p []byte) (int, error) {
	c.cancel()
	return c.body.Read(p)
}

// TestWritesEndWithTheirCaller: a write whose caller leaves mid-way, or that
// is longer than it allows, leaves nothing behind; a write over a directory
// and a directory over a file are refused; and a record that cannot be
// written fails the change that wrote it.
func TestWritesEndWithTheirCaller(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_writes"})
	target := driver.DefaultWorkdir + "/out.txt"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &cancelOnRead{cancel: cancel, body: strings.NewReader(strings.Repeat("x", 64<<10))}
	if _, err := d.Write(ctx, "sbx_writes", driver.WriteRequest{Path: target, Body: body}); !errors.Is(err, context.Canceled) {
		t.Errorf("a write whose caller left answered %v", err)
	}
	if _, err := d.Write(context.Background(), "sbx_writes", driver.WriteRequest{Path: target, MaxBytes: 2, Body: strings.NewReader("abc")}); !errors.Is(err, driver.ErrTooLarge) {
		t.Errorf("a write over its limit answered %v", err)
	}
	if entries, err := d.ReadDir(context.Background(), "sbx_writes", driver.DefaultWorkdir); err != nil || len(entries) != 0 {
		t.Errorf("the failed writes left %+v, %v", entries, err)
	}
	if err := d.Mkdir(context.Background(), "sbx_writes", driver.DefaultWorkdir+"/dir"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(context.Background(), "sbx_writes", driver.WriteRequest{Path: driver.DefaultWorkdir, Body: strings.NewReader("x")}); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a write over the workspace itself answered %v", err)
	}
	if got := expand(h.home, "/a/../b"); got != "/b" {
		t.Errorf("a path with no ~ expands to %s", got)
	}
	if err := os.Mkdir(d.record("sbx_writes")+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := d.Touch(context.Background(), "sbx_writes"); err == nil {
		t.Error("a change whose record could not be written passed")
	}
}
