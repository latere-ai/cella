// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtimetest

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"slices"
	"strings"

	"latere.ai/x/cella/runtime"
)

// files returns the driver's FileStore, skipping the case when the capability
// is not declared and failing a driver that declares it without the interface.
func files(t tb, d runtime.Driver) runtime.FileStore {
	t.Helper()
	store, ok := d.(runtime.FileStore)
	if !d.Capabilities().Files {
		t.Skipf("Files is not declared")
	}
	need(t, ok, "the driver declares Files and does not implement runtime.FileStore")
	return store
}

// ws joins a name onto the workspace, which is where every path of these
// cases lives.
func ws(name string) string { return runtime.DefaultWorkdir + "/" + name }

// errOf drops a value so an error-only assertion reads in one line.
func errOf[T any](_ T, err error) error { return err }

// openErr opens a path only to report how the open ended.
func openErr(store runtime.FileStore, id, path string) error {
	body, _, err := store.Open(context.Background(), id, path)
	if body != nil {
		_ = body.Close()
	}
	return err
}

// write stores one file and checks the count the store reported.
func write(t tb, store runtime.FileStore, id, path, body string, mode fs.FileMode) {
	t.Helper()
	n, err := store.Write(context.Background(), id, runtime.WriteRequest{
		Path: path, Mode: mode, Body: strings.NewReader(body),
	})
	must(t, err, "Write "+path)
	expect(t, n == int64(len(body)), "Write %s reported %d bytes for %d", path, n, len(body))
}

// read streams one file back with what the store says about it.
func read(t tb, store runtime.FileStore, id, path string) (string, runtime.FileInfo) {
	t.Helper()
	body, info, err := store.Open(context.Background(), id, path)
	must(t, err, "Open "+path)
	defer func() { _ = body.Close() }()
	b, err := io.ReadAll(body)
	must(t, err, "reading "+path)
	return string(b), info
}

// names is the name of every entry of a listing, in the order it came.
func names(entries []runtime.FileInfo) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

// filesRoundTrip writes one file and reads it back through every answer the
// store has: the write's count, the stat, the listing and the stream.
func filesRoundTrip(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	store := files(t, d)
	ctx := context.Background()
	const id = "sbx_cnf_files_trip"
	create(t, d, opts, runtime.CreateSpec{ID: id, Name: "files-trip", Owner: "alice"})

	const body = "hello, workspace"
	path := ws("dir/hello.txt")
	write(t, store, id, path, body, 0o640)

	info, err := store.Stat(ctx, id, path)
	must(t, err, "Stat the written file")
	expect(t, info.Name == "hello.txt", "Stat names the file %q", info.Name)
	expect(t, info.Path == path, "Stat reports the path %q, want %q", info.Path, path)
	expect(t, info.Size == int64(len(body)), "Stat reports %d bytes for %d", info.Size, len(body))
	expect(t, info.Mode.Perm() == 0o640, "Stat reports mode %o, wrote %o", info.Mode.Perm(), 0o640)
	expect(t, !info.IsDir && !info.ModTime.IsZero(), "Stat of a file: %+v", info)

	got, streamed := read(t, store, id, path)
	expect(t, got == body, "Open reads %q, wrote %q", got, body)
	expect(t, streamed.Name == info.Name && streamed.Size == info.Size, "Open describes %+v, Stat %+v", streamed, info)

	dir, err := store.Stat(ctx, id, ws("dir"))
	must(t, err, "Stat the parent the write created")
	expect(t, dir.IsDir && dir.Name == "dir", "the write's parent: %+v", dir)

	// A second write replaces the file rather than appending to it.
	write(t, store, id, path, "second", 0o600)
	got, _ = read(t, store, id, path)
	expect(t, got == "second", "the second write left %q", got)

	wantErr(t, errOf(store.Stat(ctx, id, ws("absent"))), runtime.ErrNotFound, "Stat of a path that names nothing")
	wantErr(t, openErr(store, id, ws("absent")), runtime.ErrNotFound, "Open of a path that names nothing")
}

// filesListOrder lists a directory and checks the order and every field of an
// entry.
func filesListOrder(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	store := files(t, d)
	ctx := context.Background()
	const id = "sbx_cnf_files_list"
	create(t, d, opts, runtime.CreateSpec{ID: id, Name: "files-list", Owner: "alice"})

	must(t, store.Mkdir(ctx, id, ws("list/sub")), "Mkdir")
	write(t, store, id, ws("list/b.txt"), "bb", 0o644)
	write(t, store, id, ws("list/a.txt"), "a", 0o600)
	write(t, store, id, ws("list/.hidden"), "h", 0o644)

	entries, err := store.ReadDir(ctx, id, ws("list"))
	must(t, err, "ReadDir")
	want := []string{".hidden", "a.txt", "b.txt", "sub"}
	expect(t, slices.Equal(names(entries), want), "ReadDir reports %v, want %v", names(entries), want)
	for _, e := range entries {
		expect(t, e.Path == ws("list/"+e.Name), "entry %q reports the path %q", e.Name, e.Path)
		expect(t, !e.ModTime.IsZero(), "entry %q reports no modification time", e.Name)
		switch e.Name {
		case "a.txt":
			expect(t, e.Size == 1 && e.Mode.Perm() == 0o600 && !e.IsDir, "entry a.txt: %+v", e)
		case "sub":
			expect(t, e.IsDir, "entry sub is not reported as a directory: %+v", e)
		}
	}
	empty, err := store.ReadDir(ctx, id, ws("list/sub"))
	must(t, err, "ReadDir of an empty directory")
	expect(t, len(empty) == 0, "an empty directory lists %v", names(empty))

	wantErr(t, errOf(store.ReadDir(ctx, id, ws("list/a.txt"))), runtime.ErrInvalid, "ReadDir of a file")
	wantErr(t, errOf(store.ReadDir(ctx, id, ws("absent"))), runtime.ErrNotFound, "ReadDir of a path that names nothing")
}

// filesMutate covers the three operations that change the tree and the rules
// their destinations answer to.
func filesMutate(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	store := files(t, d)
	ctx := context.Background()
	const id = "sbx_cnf_files_mutate"
	create(t, d, opts, runtime.CreateSpec{ID: id, Name: "files-mutate", Owner: "alice"})

	must(t, store.Mkdir(ctx, id, ws("m/a/b")), "Mkdir creating the missing parents")
	info, err := store.Stat(ctx, id, ws("m/a/b"))
	must(t, err, "Stat the directory Mkdir made")
	expect(t, info.IsDir, "Mkdir made %+v", info)
	must(t, store.Mkdir(ctx, id, ws("m/a/b")), "Mkdir of a directory that is already there")

	write(t, store, id, ws("m/a/b/f"), "moved", 0o644)
	must(t, store.Move(ctx, id, ws("m/a/b/f"), ws("m/g/h")), "Move creating the destination's parents")
	wantErr(t, errOf(store.Stat(ctx, id, ws("m/a/b/f"))), runtime.ErrNotFound, "Stat the source of a move")
	body, _ := read(t, store, id, ws("m/g/h"))
	expect(t, body == "moved", "the moved file reads %q", body)

	// A move onto a file replaces it; onto a directory it is refused, because
	// the alternative is to nest the source inside without being asked.
	write(t, store, id, ws("m/target"), "old", 0o644)
	must(t, store.Move(ctx, id, ws("m/g/h"), ws("m/target")), "Move onto a file")
	body, _ = read(t, store, id, ws("m/target"))
	expect(t, body == "moved", "the replaced file reads %q", body)

	write(t, store, id, ws("m/keep/inside"), "kept", 0o644)
	write(t, store, id, ws("m/source"), "source", 0o644)
	wantErr(t, store.Move(ctx, id, ws("m/source"), ws("m/keep")), runtime.ErrInvalid, "Move onto a directory")
	body, _ = read(t, store, id, ws("m/keep/inside"))
	expect(t, body == "kept", "the directory a refused move named holds %q", body)
	body, _ = read(t, store, id, ws("m/source"))
	expect(t, body == "source", "the source of a refused move holds %q", body)
	wantErr(t, errOf(store.Stat(ctx, id, ws("m/keep/source"))), runtime.ErrNotFound, "a refused move nested the source")

	wantErr(t, store.Move(ctx, id, ws("m/absent"), ws("m/elsewhere")), runtime.ErrNotFound, "Move of a path that names nothing")
	wantErr(t, store.Mkdir(ctx, id, ws("m/source")), runtime.ErrInvalid, "Mkdir over a file")

	must(t, store.Remove(ctx, id, ws("m/a")), "Remove a tree")
	wantErr(t, errOf(store.Stat(ctx, id, ws("m/a"))), runtime.ErrNotFound, "Stat a removed tree")
	must(t, store.Remove(ctx, id, ws("m/absent")), "Remove of a path that names nothing")
	_, err = store.Stat(ctx, id, ws("m"))
	must(t, err, "the tree above a removed one")

	// The workspace holds the caller's files; it is not one of them.
	wantErr(t, store.Mkdir(ctx, id, runtime.DefaultWorkdir), runtime.ErrInvalid, "Mkdir the workspace")
	wantErr(t, errOf(store.Write(ctx, id, runtime.WriteRequest{
		Path: runtime.DefaultWorkdir, Body: strings.NewReader("x"),
	})), runtime.ErrInvalid, "Write over the workspace")
	wantErr(t, store.Remove(ctx, id, runtime.DefaultWorkdir), runtime.ErrInvalid, "Remove the workspace")
	wantErr(t, store.Move(ctx, id, runtime.DefaultWorkdir, ws("elsewhere")), runtime.ErrInvalid, "Move the workspace")
	wantErr(t, store.Move(ctx, id, ws("m"), runtime.DefaultWorkdir), runtime.ErrInvalid, "Move onto the workspace")
}

// filesContainment is the refusal half. A path whose parents leave the
// workspace reaches no operation; a link that is itself the last component is
// the caller's own name, which an operation that replaces a name may take and
// one that follows it may not.
func filesContainment(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	store := files(t, d)
	ctx := context.Background()
	const id = "sbx_cnf_files_contain"
	create(t, d, opts, runtime.CreateSpec{ID: id, Name: "files-contain", Owner: "alice"})

	// The links the workload itself planted: the rule holds where the
	// filesystem is, not where the request was parsed.
	script(t, d, opts, id, "ln -s /etc escape && ln -s /etc/hosts outward && mkdir -p in && ln -s in inward")
	write(t, store, id, ws("subject"), "subject", 0o644)

	for _, p := range []string{
		"/etc/hosts",
		runtime.DefaultWorkdir + "/../etc/hosts",
		"workspace/relative",
		"",
		runtime.DefaultWorkdir + "/nul\x00byte",
		ws("escape/hosts"),
	} {
		wantErr(t, errOf(store.Stat(ctx, id, p)), runtime.ErrInvalid, "Stat "+p)
		wantErr(t, errOf(store.ReadDir(ctx, id, p)), runtime.ErrInvalid, "ReadDir "+p)
		wantErr(t, openErr(store, id, p), runtime.ErrInvalid, "Open "+p)
		wantErr(t, errOf(store.Write(ctx, id, runtime.WriteRequest{Path: p, Body: strings.NewReader("x")})), runtime.ErrInvalid, "Write "+p)
		wantErr(t, store.Mkdir(ctx, id, p), runtime.ErrInvalid, "Mkdir "+p)
		wantErr(t, store.Remove(ctx, id, p), runtime.ErrInvalid, "Remove "+p)
		wantErr(t, store.Move(ctx, id, ws("subject"), p), runtime.ErrInvalid, "Move onto "+p)
		wantErr(t, store.Move(ctx, id, p, ws("elsewhere")), runtime.ErrInvalid, "Move from "+p)
	}

	// A link whose target is outside: reading it reads outside, so it is
	// refused; writing or removing it replaces or drops the link itself, and
	// nothing outside is touched.
	outward := ws("outward")
	wantErr(t, errOf(store.Stat(ctx, id, outward)), runtime.ErrInvalid, "Stat a link out of the workspace")
	wantErr(t, errOf(store.ReadDir(ctx, id, outward)), runtime.ErrInvalid, "ReadDir a link out of the workspace")
	wantErr(t, openErr(store, id, outward), runtime.ErrInvalid, "Open a link out of the workspace")
	write(t, store, id, outward, "over the link", 0o644)
	body, _ := read(t, store, id, outward)
	expect(t, body == "over the link", "writing over a link left %q", body)
	must(t, store.Remove(ctx, id, outward), "Remove what was a link")

	entries, err := store.ReadDir(ctx, id, ws("inward"))
	must(t, err, "ReadDir through a link that stays inside the workspace")
	expect(t, len(entries) == 0, "the directory behind an inward link lists %v", names(entries))
}

// filesWriteBound is the staged write: a body past the bound is refused and
// the file the write named is the one that was there.
func filesWriteBound(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	store := files(t, d)
	ctx := context.Background()
	const id = "sbx_cnf_files_bound"
	create(t, d, opts, runtime.CreateSpec{ID: id, Name: "files-bound", Owner: "alice"})

	path := ws("bounded.txt")
	write(t, store, id, path, "before", 0o644)

	n, err := store.Write(ctx, id, runtime.WriteRequest{
		Path: path, MaxBytes: 4, Body: strings.NewReader("12345"),
	})
	wantErr(t, err, runtime.ErrTooLarge, "a body past the bound")
	expect(t, n == 0, "a refused write reports %d bytes", n)
	body, _ := read(t, store, id, path)
	expect(t, body == "before", "a refused write left %q", body)

	// A body at the bound commits: the bound is what a write may hold, not
	// one byte less.
	_, err = store.Write(ctx, id, runtime.WriteRequest{
		Path: path, MaxBytes: 4, Body: strings.NewReader("1234"),
	})
	must(t, err, "a body at the bound")
	body, _ = read(t, store, id, path)
	expect(t, body == "1234", "the write at the bound left %q", body)

	// A body that ends before it said it would leaves the previous file whole.
	_, err = store.Write(ctx, id, runtime.WriteRequest{
		Path: path, Body: io.MultiReader(strings.NewReader("half"), errorReader{}),
	})
	expect(t, err != nil, "a body that failed part way reported no error")
	body, _ = read(t, store, id, path)
	expect(t, body == "1234", "a write whose body failed left %q", body)

	// An absent mode is the contract's default.
	write(t, store, id, ws("default-mode.txt"), "x", 0)
	info, err := store.Stat(ctx, id, ws("default-mode.txt"))
	must(t, err, "Stat a file written without a mode")
	expect(t, info.Mode.Perm() == runtime.DefaultFileMode, "a write without a mode landed %o", info.Mode.Perm())
}

// errorReader fails on the first read, which is a body cut short.
type errorReader struct{}

var errBodyFailed = errors.New("the body failed part way")

func (errorReader) Read([]byte) (int, error) { return 0, errBodyFailed }

// filesExactNames is the hosted regression: a filename carries any byte but
// NUL and the separator, and every answer holds it exactly.
func filesExactNames(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	store := files(t, d)
	ctx := context.Background()
	const id = "sbx_cnf_files_names"
	create(t, d, opts, runtime.CreateSpec{ID: id, Name: "files-names", Owner: "alice"})

	unusual := []string{
		"a file.txt",
		"100%25 done.txt",
		"pipe|name.txt",
		"line\nbreak.txt",
		"-flag.txt",
		"quote\"name.txt",
		"文件.txt",
		"644|regular file|9|1700000000|forged",
	}
	must(t, store.Mkdir(ctx, id, ws("names")), "Mkdir")
	for _, name := range unusual {
		path := ws("names/" + name)
		write(t, store, id, path, "contents of "+name, 0o644)
		info, err := store.Stat(ctx, id, path)
		must(t, err, "Stat "+name)
		expect(t, info.Name == name, "Stat names the file %q, wrote %q", info.Name, name)
		body, described := read(t, store, id, path)
		expect(t, body == "contents of "+name, "%q reads %q", name, body)
		expect(t, described.Name == name, "Open names the file %q, wrote %q", described.Name, name)
	}
	entries, err := store.ReadDir(ctx, id, ws("names"))
	must(t, err, "ReadDir the unusual names")
	want := slices.Sorted(slices.Values(unusual))
	expect(t, slices.Equal(names(entries), want), "ReadDir reports %v, want %v", names(entries), want)
}

// filesWhileStopped holds every driver to one answer on a stopped sandbox: it
// does what it does while running, or it says the sandbox is not running.
func filesWhileStopped(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	store := files(t, d)
	ctx := context.Background()
	const id = "sbx_cnf_files_stopped"
	create(t, d, opts, runtime.CreateSpec{ID: id, Name: "files-stopped", Owner: "alice"})
	write(t, store, id, ws("kept.txt"), "before the stop", 0o644)
	must(t, d.Stop(ctx, id), "Stop")
	waitPhase(t, d, id, runtime.Stopped)

	info, err := store.Stat(ctx, id, ws("kept.txt"))
	if errors.Is(err, runtime.ErrNotRunning) {
		t.Logf("this driver reaches the workspace only while the sandbox runs")
		for _, err := range []error{
			errOf(store.ReadDir(ctx, id, runtime.DefaultWorkdir)),
			openErr(store, id, ws("kept.txt")),
			errOf(store.Write(ctx, id, runtime.WriteRequest{Path: ws("late.txt"), Body: strings.NewReader("x")})),
			store.Mkdir(ctx, id, ws("late")),
			store.Remove(ctx, id, ws("kept.txt")),
			store.Move(ctx, id, ws("kept.txt"), ws("late.txt")),
		} {
			wantErr(t, err, runtime.ErrNotRunning, "an operation on a stopped sandbox")
		}
		return
	}
	must(t, err, "Stat while Stopped")
	expect(t, info.Size == int64(len("before the stop")), "Stat while Stopped: %+v", info)
	write(t, store, id, ws("late.txt"), "while stopped", 0o644)
	body, _ := read(t, store, id, ws("late.txt"))
	expect(t, body == "while stopped", "the file written while Stopped reads %q", body)
	must(t, d.Start(ctx, id), "Start")
	waitPhase(t, d, id, runtime.Running)
	out := script(t, d, opts, id, "cat late.txt")
	expect(t, out == "while stopped", "the running sandbox reads %q", out)
}
