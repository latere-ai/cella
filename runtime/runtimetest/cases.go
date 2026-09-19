// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtimetest

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"latere.ai/x/cella/runtime"
)

func nameIsolationCapabilities(t tb, open func() runtime.Driver, _ Options) {
	d := open()
	name := d.Name()
	expect(t, name != "", "Name is empty")
	expect(t, d.Name() == name, "Name changed between calls: %q then %q", name, d.Name())
	iso := d.Isolation()
	classes := []string{"container", "vm", "process", "none"}
	expect(t, slices.Contains(classes, iso), "Isolation %q is not one of %v", iso, classes)
	expect(t, d.Isolation() == iso, "Isolation changed between calls: %q then %q", iso, d.Isolation())
	expect(t, reflect.DeepEqual(d.Capabilities(), d.Capabilities()), "Capabilities differ between calls")
	// An optional interface is implemented if and only if its capability is
	// declared: the controller and the API branch on the declaration alone.
	_, attacher := d.(runtime.Attacher)
	switch {
	case d.Capabilities().Attach && !attacher:
		t.Errorf("the driver declares Attach and does not implement runtime.Attacher")
	case !d.Capabilities().Attach && attacher:
		t.Errorf("the driver implements runtime.Attacher and does not declare Attach")
	}
}

func preflightAndReady(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	ctx := context.Background()
	must(t, d.Preflight(ctx), "Preflight")
	must(t, d.Ready(ctx), "Ready")
	if opts.Down == nil {
		t.Logf("Options.Down is nil: the Ready-down half is skipped")
		return
	}
	opts.Down()
	expect(t, d.Ready(ctx) != nil, "Ready is nil after Options.Down")
	opts.Up()
	must(t, d.Ready(ctx), "Ready after Options.Up")
}

func createInspectDelete(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	ctx := context.Background()
	const id = "sbx_cnf_create"
	labels := map[string]string{"team": "a", "purpose": "conformance"}
	spec := runtime.CreateSpec{ID: id, Name: "create", Owner: "alice@example.com", Labels: labels}
	state := create(t, d, opts, spec)
	expect(t, state.ID == id, "Inspect id %q, want %q", state.ID, id)
	expect(t, state.Name == spec.Name, "Inspect name %q, want %q", state.Name, spec.Name)
	expect(t, state.Owner == spec.Owner, "Inspect owner %q, want %q", state.Owner, spec.Owner)
	expect(t, maps.Equal(state.Labels, labels), "Inspect labels %v, want %v", state.Labels, labels)
	expect(t, state.Isolation == d.Isolation(), "State.Isolation %q, Isolation() %q", state.Isolation, d.Isolation())
	expect(t, !state.CreatedAt.IsZero(), "CreatedAt is zero")
	expect(t, !state.StartedAt.IsZero(), "StartedAt is zero while Running")
	expect(t, !state.LastActivityAt.IsZero(), "LastActivityAt is zero")
	expect(t, state.StoppedAt.IsZero(), "StoppedAt %v is set while Running", state.StoppedAt)
	expect(t, !state.StartedAt.Before(state.CreatedAt), "StartedAt %v before CreatedAt %v", state.StartedAt, state.CreatedAt)
	spec.Image = opts.Image
	_, err := d.Create(ctx, spec)
	wantErr(t, err, runtime.ErrAlreadyExists, "second Create of the same id")
	must(t, d.Delete(ctx, id), "Delete")
	_, err = d.Inspect(ctx, id)
	wantErr(t, err, runtime.ErrNotFound, "Inspect after Delete")
	err = d.Delete(ctx, id)
	expect(t, err == nil || errors.Is(err, runtime.ErrNotFound), "second Delete: %v, want nil or ErrNotFound", err)
	const absent = "sbx_cnf_absent"
	_, err = d.Inspect(ctx, absent)
	wantErr(t, err, runtime.ErrNotFound, "Inspect unknown id")
	wantErr(t, d.Start(ctx, absent), runtime.ErrNotFound, "Start unknown id")
	wantErr(t, d.Stop(ctx, absent), runtime.ErrNotFound, "Stop unknown id")
	wantErr(t, d.Update(ctx, absent, runtime.Change{Labels: &labels}), runtime.ErrNotFound, "Update unknown id")
	wantErr(t, d.Touch(ctx, absent), runtime.ErrNotFound, "Touch unknown id")
}

func stopStartKeepsTheWorkspace(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	ctx := context.Background()
	const id = "sbx_cnf_stopstart"
	s0 := create(t, d, opts, runtime.CreateSpec{ID: id, Name: "cycle", Owner: "alice"})
	importFiles(t, d, id, runtime.DefaultWorkdir, entry{name: "keep.txt", body: "kept"})
	must(t, d.Stop(ctx, id), "Stop")
	s1 := waitPhase(t, d, id, runtime.Stopped)
	expect(t, !s1.StoppedAt.IsZero(), "StoppedAt is zero after Stop")
	expect(t, !s1.StoppedAt.Before(s0.StartedAt), "StoppedAt %v before StartedAt %v", s1.StoppedAt, s0.StartedAt)
	must(t, d.Stop(ctx, id), "second Stop")
	s2, err := d.Inspect(ctx, id)
	must(t, err, "Inspect after second Stop")
	expect(t, s2.Phase == runtime.Stopped && s2.StoppedAt.Equal(s1.StoppedAt), "second Stop changed the state: %+v then %+v", s1, s2)
	must(t, d.Start(ctx, id), "Start")
	s3 := waitPhase(t, d, id, runtime.Running)
	expect(t, s3.StoppedAt.IsZero(), "StoppedAt %v still set after Start", s3.StoppedAt)
	expect(t, !s3.StartedAt.Before(s1.StoppedAt), "StartedAt %v not advanced past StoppedAt %v", s3.StartedAt, s1.StoppedAt)
	must(t, d.Start(ctx, id), "second Start")
	s4, err := d.Inspect(ctx, id)
	must(t, err, "Inspect after second Start")
	expect(t, s4.Phase == runtime.Running && s4.StartedAt.Equal(s3.StartedAt), "second Start changed the state: %+v then %+v", s3, s4)
	expect(t, s4.CreatedAt.Equal(s0.CreatedAt), "CreatedAt moved from %v to %v", s0.CreatedAt, s4.CreatedAt)
	got := script(t, d, opts, id, "cat keep.txt")
	expect(t, got == "kept", "workspace after the cycle: %q, want %q", got, "kept")
}

func updateEveryMutableField(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	ctx := context.Background()
	const id = "sbx_cnf_update"
	s0 := create(t, d, opts, runtime.CreateSpec{ID: id, Name: "update", Owner: "alice", Labels: map[string]string{"a": "1"}, Env: map[string]string{"GREETING": "hello"}, Lifecycle: runtime.Lifecycle{TTL: time.Hour, AutoStop: time.Minute}})
	expect(t, s0.AutoStop == time.Minute, "AutoStop %v, want %v", s0.AutoStop, time.Minute)
	expect(t, s0.ExpiresAt.Equal(s0.CreatedAt.Add(time.Hour)), "ExpiresAt %v, want CreatedAt+1h %v", s0.ExpiresAt, s0.CreatedAt.Add(time.Hour))
	labels := map[string]string{"b": "2"}
	must(t, d.Update(ctx, id, runtime.Change{Labels: &labels}), "Update labels")
	s1, err := d.Inspect(ctx, id)
	must(t, err, "Inspect")
	expect(t, maps.Equal(s1.Labels, labels), "labels after Update %v, want %v", s1.Labels, labels)
	env := map[string]string{"GREETING": "changed"}
	must(t, d.Update(ctx, id, runtime.Change{Env: &env}), "Update env")
	got := script(t, d, opts, id, `printf '%s' "$GREETING"`)
	expect(t, got == "changed", "env after Update: %q, want %q", got, "changed")
	must(t, d.Update(ctx, id, runtime.Change{Lifecycle: &runtime.Lifecycle{TTL: 2 * time.Hour, AutoStop: 2 * time.Minute, AutoDelete: 3 * time.Minute}}), "Update lifecycle")
	s2, err := d.Inspect(ctx, id)
	must(t, err, "Inspect")
	expect(t, s2.AutoStop == 2*time.Minute && s2.AutoDelete == 3*time.Minute, "lifecycle after Update: AutoStop %v AutoDelete %v", s2.AutoStop, s2.AutoDelete)
	expect(t, s2.ExpiresAt.Equal(s0.CreatedAt.Add(2*time.Hour)), "ExpiresAt %v, want CreatedAt+2h %v", s2.ExpiresAt, s0.CreatedAt.Add(2*time.Hour))
	must(t, d.Update(ctx, id, runtime.Change{Lifecycle: &runtime.Lifecycle{}}), "Update lifecycle to zero")
	s3, err := d.Inspect(ctx, id)
	must(t, err, "Inspect")
	expect(t, s3.ExpiresAt.IsZero() && s3.AutoStop == 0 && s3.AutoDelete == 0, "zero lifecycle not applied: %+v", s3)
	must(t, d.Update(ctx, id, runtime.Change{}), "empty Update")
	s4, err := d.Inspect(ctx, id)
	must(t, err, "Inspect")
	expect(t, maps.Equal(s4.Labels, labels), "empty Update changed labels to %v", s4.Labels)
}

// three creates the sandboxes the list cases share: two of alice, one stopped, one of bob.
func three(t tb, d runtime.Driver, opts Options) (a, b, c string) {
	t.Helper()
	a, b, c = "sbx_cnf_list_a", "sbx_cnf_list_b", "sbx_cnf_list_c"
	create(t, d, opts, runtime.CreateSpec{ID: a, Name: "list-a", Owner: "alice", Labels: map[string]string{"k": "1"}})
	create(t, d, opts, runtime.CreateSpec{ID: b, Name: "list-b", Owner: "alice", Labels: map[string]string{"k": "2"}})
	create(t, d, opts, runtime.CreateSpec{ID: c, Name: "list-c", Owner: "bob"})
	must(t, d.Stop(context.Background(), b), "Stop "+b)
	waitPhase(t, d, b, runtime.Stopped)
	return a, b, c
}

func listReadsIdentityBack(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	a, b, c := three(t, d, opts)
	states, err := d.List(context.Background(), runtime.Filter{})
	must(t, err, "List")
	byID := map[string]runtime.State{}
	for _, s := range states {
		byID[s.ID] = s
	}
	for _, want := range []struct {
		id, name, owner string
		labels          map[string]string
	}{{a, "list-a", "alice", map[string]string{"k": "1"}}, {b, "list-b", "alice", map[string]string{"k": "2"}}, {c, "list-c", "bob", nil}} {
		got, ok := byID[want.id]
		expect(t, ok, "List omits %s", want.id)
		expect(t, got.Name == want.name && got.Owner == want.owner && maps.Equal(got.Labels, want.labels), "List reads %s back as %+v", want.id, got)
	}
}

func filterSelectsOnLabels(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	a, b, c := three(t, d, opts)
	for _, tc := range []struct {
		name    string
		filter  runtime.Filter
		matches func(runtime.State) bool
		in, out []string
	}{
		{"Owner", runtime.Filter{Owner: "alice"}, func(s runtime.State) bool { return s.Owner == "alice" }, []string{a, b}, []string{c}},
		{"Phase", runtime.Filter{Phase: runtime.Stopped}, func(s runtime.State) bool { return s.Phase == runtime.Stopped }, []string{b}, []string{a, c}},
		{"IDs", runtime.Filter{IDs: []string{a, c}}, func(s runtime.State) bool { return s.ID == a || s.ID == c }, []string{a, c}, []string{b}},
		{"All", runtime.Filter{Owner: "alice", Phase: runtime.Running, IDs: []string{a, b, c}}, func(s runtime.State) bool { return s.ID == a }, []string{a}, []string{b, c}},
	} {
		states, err := d.List(context.Background(), tc.filter)
		must(t, err, "List "+tc.name)
		var ids []string
		for _, s := range states {
			ids = append(ids, s.ID)
			expect(t, tc.matches(s), "Filter %s returned %+v, which does not match", tc.name, s)
		}
		for _, id := range tc.in {
			expect(t, slices.Contains(ids, id), "Filter %s omits %s: %v", tc.name, id, ids)
		}
		for _, id := range tc.out {
			expect(t, !slices.Contains(ids, id), "Filter %s includes %s: %v", tc.name, id, ids)
		}
	}
}

func execStreamsAndExits(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	ctx := context.Background()
	const id = "sbx_cnf_exec"
	create(t, d, opts, runtime.CreateSpec{ID: id, Name: "exec", Owner: "alice", Env: map[string]string{"SANDBOX_VAR": "from-sandbox"}})
	out, errOut, code, err := run(t, d, id, runtime.ExecRequest{Command: opts.shell("printf out; printf err >&2")})
	must(t, err, "Exec")
	expect(t, out == "out" && errOut == "err" && code == 0, "streams and exit: stdout %q stderr %q code %d", out, errOut, code)
	_, _, code, err = run(t, d, id, runtime.ExecRequest{Command: opts.shell("exit 3")})
	must(t, err, "Exec exit 3")
	expect(t, code == 3, "exit code %d, want 3", code)
	got := script(t, d, opts, id, `printf '%s' "$SANDBOX_VAR"`)
	expect(t, got == "from-sandbox", "sandbox env: %q", got)
	out, _, _, err = run(t, d, id, runtime.ExecRequest{Command: opts.shell(`printf '%s' "$SANDBOX_VAR"`), Env: map[string]string{"SANDBOX_VAR": "from-request"}})
	must(t, err, "Exec with env")
	expect(t, out == "from-request", "request env does not override: %q", out)
	importFiles(t, d, id, runtime.DefaultWorkdir, entry{name: "sub/", typ: tar.TypeDir}, entry{name: "sub/marker", body: "here"})
	out, _, code, err = run(t, d, id, runtime.ExecRequest{Command: opts.shell("cat marker"), Workdir: runtime.DefaultWorkdir + "/sub"})
	must(t, err, "Exec with workdir")
	expect(t, out == "here" && code == 0, "workdir: stdout %q code %d", out, code)

	// Streaming: the first byte is readable while the command is still running.
	e, err := d.Exec(ctx, id, runtime.ExecRequest{Command: opts.shell("printf first; sleep 0.5; printf second")})
	must(t, err, "Exec streaming")
	var stderr string
	var wg sync.WaitGroup
	wg.Go(func() { b, _ := io.ReadAll(e.Stderr()); stderr = string(b) })
	first := make([]byte, 1)
	_, err = io.ReadFull(e.Stdout(), first)
	must(t, err, "read first byte")
	early, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = e.Wait(early)
	cancel()
	expect(t, err != nil, "Wait returned before the command finished writing")
	rest, err := io.ReadAll(e.Stdout())
	must(t, err, "read the rest")
	wg.Wait()
	bounded, cancel := context.WithTimeout(ctx, pollTimeout)
	code, err = e.Wait(bounded)
	cancel()
	must(t, err, "Wait streaming")
	expect(t, string(first)+string(rest) == "firstsecond" && code == 0 && stderr == "", "streamed %q%q code %d stderr %q", first, rest, code, stderr)
	must(t, e.Close(), "Close after Wait")

	_, _, _, err = run(t, d, id, runtime.ExecRequest{Command: opts.shell("sleep 60"), Timeout: 50 * time.Millisecond})
	wantErr(t, err, context.DeadlineExceeded, "Timeout")
	reqCtx, cancelReq := context.WithCancel(ctx)
	e, err = d.Exec(reqCtx, id, runtime.ExecRequest{Command: opts.shell("sleep 60")})
	must(t, err, "Exec to cancel")
	cancelReq()
	drain(e)
	bounded, cancel = context.WithTimeout(ctx, pollTimeout)
	_, err = e.Wait(bounded)
	cancel()
	wantErr(t, err, context.Canceled, "Wait after the request context is cancelled")
	must(t, e.Close(), "Close cancelled exec")
	e, err = d.Exec(ctx, id, runtime.ExecRequest{Command: opts.shell("sleep 60")})
	must(t, err, "Exec to close")
	must(t, e.Close(), "Close running exec")
	bounded, cancel = context.WithTimeout(ctx, pollTimeout)
	_, err = e.Wait(bounded)
	cancel()
	expect(t, err != nil, "Wait is nil after Close ended the command")

	_, err = d.Exec(ctx, id, runtime.ExecRequest{})
	wantErr(t, err, runtime.ErrInvalid, "empty Command")
	_, err = d.Exec(ctx, "sbx_cnf_absent", runtime.ExecRequest{Command: opts.shell("true")})
	wantErr(t, err, runtime.ErrNotFound, "Exec unknown id")
	if d.Capabilities().Attach {
		out, _, code, err = run(t, d, id, runtime.ExecRequest{Command: []string{"cat"}, Stdin: strings.NewReader("ping")})
		must(t, err, "Exec with Stdin under Attach")
		expect(t, out == "ping" && code == 0, "Stdin under Attach: %q code %d", out, code)
	} else {
		_, err = d.Exec(ctx, id, runtime.ExecRequest{Command: []string{"cat"}, Stdin: strings.NewReader("ping")})
		wantErr(t, err, runtime.ErrUnsupported, "Stdin without Attach")
		_, err = d.Exec(ctx, id, runtime.ExecRequest{Command: opts.shell("true"), TTY: true})
		wantErr(t, err, runtime.ErrUnsupported, "TTY without Attach")
	}
	must(t, d.Stop(ctx, id), "Stop")
	waitPhase(t, d, id, runtime.Stopped)
	_, err = d.Exec(ctx, id, runtime.ExecRequest{Command: opts.shell("true")})
	wantErr(t, err, runtime.ErrNotRunning, "Exec while Stopped")
}

func logsFollow(t tb, open func() runtime.Driver, opts Options) {
	if opts.NoMainCommand {
		t.Skipf("Options.NoMainCommand: the driver runs no main command")
	}
	d := open()
	ctx := context.Background()
	const id = "sbx_cnf_logs"
	create(t, d, opts, runtime.CreateSpec{ID: id, Name: "logs", Owner: "alice", Command: opts.shell("printf 'one\\ntwo\\n'; while [ ! -f go ]; do sleep 0.01; done; printf 'three\\n'; exit 4")})
	snapshot := func(req runtime.LogsRequest) string {
		t.Helper()
		r, err := d.Logs(ctx, id, req)
		must(t, err, "Logs")
		b, err := io.ReadAll(r)
		must(t, err, "read Logs")
		must(t, r.Close(), "close Logs")
		return string(b)
	}
	deadline := time.Now().Add(pollTimeout)
	for snapshot(runtime.LogsRequest{}) != "one\ntwo\n" {
		need(t, time.Now().Before(deadline), "Logs snapshot never showed the first lines: %q", snapshot(runtime.LogsRequest{}))
		time.Sleep(10 * time.Millisecond)
	}
	follow, err := d.Logs(ctx, id, runtime.LogsRequest{Follow: true})
	must(t, err, "Logs follow")
	head := make([]byte, len("one\ntwo\n"))
	_, err = io.ReadFull(follow, head)
	must(t, err, "read the lines written before Logs opened")
	importFiles(t, d, id, runtime.DefaultWorkdir, entry{name: "go"})
	tail, err := io.ReadAll(follow)
	must(t, err, "read until the process ends")
	must(t, follow.Close(), "close follow")
	expect(t, string(head)+string(tail) == "one\ntwo\nthree\n", "followed %q%q", head, tail)
	state := waitPhase(t, d, id, "Failed")
	expect(t, state.ExitCode != nil && *state.ExitCode == 4, "exit code in State: %v", state.ExitCode)
	expect(t, !state.StoppedAt.IsZero(), "StoppedAt is zero after the main process ended")
	after := snapshot(runtime.LogsRequest{})
	expect(t, after == "one\ntwo\nthree\n", "Logs after the process: %q", after)
	last := snapshot(runtime.LogsRequest{TailLines: 1})
	expect(t, last == "three\n", "TailLines 1: %q", last)
	none := snapshot(runtime.LogsRequest{Since: time.Now().Add(time.Hour)})
	expect(t, none == "", "Since in the future: %q", none)
}

func tarOutAndIn(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	ctx := context.Background()
	const a, b = "sbx_cnf_tar_a", "sbx_cnf_tar_b"
	create(t, d, opts, runtime.CreateSpec{ID: a, Name: "tar-a", Owner: "alice"})
	create(t, d, opts, runtime.CreateSpec{ID: b, Name: "tar-b", Owner: "alice"})
	tree := []entry{{name: "tree/", typ: tar.TypeDir, mode: 0o755}, {name: "tree/hello.txt", body: "hello", mode: 0o640}, {name: "tree/bin/", typ: tar.TypeDir, mode: 0o755}, {name: "tree/bin/run.sh", body: "#!/bin/sh\n", mode: 0o755}}
	importFiles(t, d, a, runtime.DefaultWorkdir, tree...)
	var out bytes.Buffer
	must(t, d.ExportTar(ctx, a, []string{runtime.DefaultWorkdir + "/tree"}, &out), "ExportTar")
	exported := out.Bytes()
	got := readArchive(t, bytes.NewReader(exported))
	for _, want := range tree {
		name := strings.TrimSuffix(want.name, "/")
		e, ok := got[name]
		expect(t, ok, "export omits %q: %v", name, slices.Sorted(maps.Keys(got)))
		expect(t, e.body == want.body && e.mode&0o777 == want.mode, "export %q: body %q mode %o, want %q %o", name, e.body, e.mode, want.body, want.mode)
	}
	must(t, d.ImportTar(ctx, b, runtime.DefaultWorkdir, bytes.NewReader(exported)), "ImportTar the export")
	read := script(t, d, opts, b, "cat tree/hello.txt")
	expect(t, read == "hello", "round trip through a second sandbox: %q", read)
	out.Reset()
	must(t, d.ExportTar(ctx, a, nil, &out), "ExportTar of the whole workspace")
	_, ok := readArchive(t, &out)["tree/hello.txt"]
	expect(t, ok, "whole-workspace export omits tree/hello.txt")

	for _, bad := range []entry{{name: "../escape", body: "x"}, {name: "/abs", body: "x"}, {name: "link", typ: tar.TypeSymlink, link: "../../escape"}, {name: "hard", typ: tar.TypeLink, link: "tree/hello.txt"}} {
		err := d.ImportTar(ctx, a, runtime.DefaultWorkdir, bytes.NewReader(archive(t, bad)))
		wantErr(t, err, runtime.ErrInvalid, "ImportTar entry "+bad.name)
	}
	wantErr(t, d.ImportTar(ctx, a, "/tmp", bytes.NewReader(archive(t, entry{name: "x", body: "x"}))), runtime.ErrInvalid, "ImportTar outside the workspace")
	wantErr(t, d.ExportTar(ctx, a, []string{"/etc"}, io.Discard), runtime.ErrInvalid, "ExportTar outside the workspace")
	wantErr(t, d.ExportTar(ctx, a, []string{runtime.DefaultWorkdir + "/../etc"}, io.Discard), runtime.ErrInvalid, "ExportTar traversal")
	wantErr(t, d.ImportTar(ctx, "sbx_cnf_absent", runtime.DefaultWorkdir, bytes.NewReader(archive(t))), runtime.ErrNotFound, "ImportTar unknown id")

	must(t, d.Stop(ctx, a), "Stop")
	waitPhase(t, d, a, runtime.Stopped)
	if !d.Capabilities().Files {
		t.Logf("Files is not declared: the Stopped half is skipped")
		return
	}
	must(t, d.ImportTar(ctx, a, runtime.DefaultWorkdir, bytes.NewReader(archive(t, entry{name: "stopped.txt", body: "while stopped"}))), "ImportTar while Stopped under Files")
	out.Reset()
	must(t, d.ExportTar(ctx, a, []string{runtime.DefaultWorkdir + "/stopped.txt"}, &out), "ExportTar while Stopped under Files")
	expect(t, readArchive(t, &out)["stopped.txt"].body == "while stopped", "export while Stopped misses the import")
	must(t, d.Start(ctx, a), "Start")
	waitPhase(t, d, a, runtime.Running)
	read = script(t, d, opts, a, "cat stopped.txt")
	expect(t, read == "while stopped", "file imported while Stopped: %q", read)
}

func touchStampsActivity(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	ctx := context.Background()
	const id = "sbx_cnf_touch"
	s0 := create(t, d, opts, runtime.CreateSpec{ID: id, Name: "touch", Owner: "alice"})
	deadline := time.Now().Add(pollTimeout)
	for {
		must(t, d.Touch(ctx, id), "Touch")
		s, err := d.Inspect(ctx, id)
		must(t, err, "Inspect")
		if s.LastActivityAt.After(s0.LastActivityAt) {
			return
		}
		need(t, time.Now().Before(deadline), "LastActivityAt did not advance past %v", s0.LastActivityAt)
		time.Sleep(10 * time.Millisecond)
	}
}

func detachRecovers(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	if !d.Capabilities().Detach {
		t.Skipf("Detach is not declared")
	}
	const id = "sbx_cnf_detach"
	s0 := create(t, d, opts, runtime.CreateSpec{ID: id, Name: "detach", Owner: "alice"})
	second := open()
	s1, err := second.Inspect(context.Background(), id)
	must(t, err, "Inspect through a second driver")
	expect(t, s1.Name == s0.Name && s1.Owner == s0.Owner && s1.CreatedAt.Equal(s0.CreatedAt), "second driver reads %+v, first wrote %+v", s1, s0)
}

// attacherOf returns the driver's Attacher, or nil with the case skipped when
// the driver declares no Attach. The declaration and the interface are checked
// against each other in NameIsolationCapabilities.
func attacherOf(t tb, d runtime.Driver) runtime.Attacher {
	t.Helper()
	if !d.Capabilities().Attach {
		t.Skipf("the driver declares no Attach")
		return nil
	}
	a, ok := d.(runtime.Attacher)
	if !ok {
		t.Fatalf("the driver declares Attach and does not implement runtime.Attacher")
		return nil
	}
	return a
}

// pump reads a session in the background so a case waits for what the terminal
// wrote with a bound, instead of blocking in Read with no deadline.
type pump struct {
	mu   sync.Mutex
	buf  []byte
	err  error
	done chan struct{}
}

func newPump(r io.Reader) *pump {
	p := &pump{done: make(chan struct{})}
	go func() {
		defer close(p.done)
		b := make([]byte, 4096)
		for {
			n, err := r.Read(b)
			p.mu.Lock()
			p.buf = append(p.buf, b[:n]...)
			p.err = err
			p.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return p
}
func (p *pump) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.buf)
}
func (p *pump) readErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// await waits for marker in what the terminal has written so far.
func (p *pump) await(t tb, marker string) {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for !strings.Contains(p.text(), marker) {
		need(t, time.Now().Before(deadline), "the session never wrote %q; it wrote %q", marker, p.text())
		time.Sleep(10 * time.Millisecond)
	}
}

// typed writes one line into the session as a person typing would.
func typed(t tb, s runtime.Session, line string) {
	t.Helper()
	_, err := s.Write([]byte(line + "\n"))
	must(t, err, "Write "+line)
}

// attachSandbox creates the sandbox the attach cases run in.
func attachSandbox(t tb, d runtime.Driver, opts Options, id string) {
	t.Helper()
	create(t, d, opts, runtime.CreateSpec{ID: id, Name: "attach", Owner: "alice"})
}

func attachRoundTrip(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	a := attacherOf(t, d)
	if a == nil {
		return
	}
	const id = "sbx_cnf_attach"
	attachSandbox(t, d, opts, id)
	s, err := a.Attach(context.Background(), id, runtime.AttachRequest{Cols: 80, Rows: 24})
	must(t, err, "Attach")
	t.Cleanup(func() { _ = s.Close() })
	p := newPump(s)
	// The marker is printed, not echoed: a terminal echoes what is typed, so
	// the assertion has to name something only the process inside can write.
	typed(t, s, `printf 'ROUND%s\n' TRIP`)
	p.await(t, "ROUNDTRIP")
	typed(t, s, "exit 7")
	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()
	code, err := s.Wait(ctx)
	must(t, err, "Wait")
	expect(t, code == 7, "the session exited %d, want 7", code)
	select {
	case <-p.done:
	case <-time.After(pollTimeout):
		t.Errorf("the session's output stream stayed open after the shell exited")
	}
}

func attachResize(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	a := attacherOf(t, d)
	if a == nil {
		return
	}
	const id = "sbx_cnf_resize"
	attachSandbox(t, d, opts, id)
	s, err := a.Attach(context.Background(), id, runtime.AttachRequest{Cols: 80, Rows: 24})
	must(t, err, "Attach")
	t.Cleanup(func() { _ = s.Close() })
	p := newPump(s)
	typed(t, s, "stty size")
	p.await(t, "24 80")
	must(t, s.Resize(120, 40), "Resize")
	typed(t, s, "stty size")
	p.await(t, "40 120")
}

// attachCloseEndsTheStream asserts what Close means on every driver: the
// session is over for its caller. Whether the process inside also ends is the
// driver's: a driver over an engine with no exec kill leaves it running until
// the sandbox stops, and its package documentation says so.
func attachCloseEndsTheStream(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	a := attacherOf(t, d)
	if a == nil {
		return
	}
	const id = "sbx_cnf_attachclose"
	attachSandbox(t, d, opts, id)
	s, err := a.Attach(context.Background(), id, runtime.AttachRequest{Cols: 80, Rows: 24})
	must(t, err, "Attach")
	p := newPump(s)
	typed(t, s, `printf 'ALI%s\n' VE`)
	p.await(t, "ALIVE")
	must(t, s.Close(), "Close")
	select {
	case <-p.done:
	case <-time.After(pollTimeout):
		t.Fatalf("the session's output stream stayed open after Close")
	}
	expect(t, p.readErr() != nil, "Read after Close reported no error")
	_, err = s.Write([]byte("x"))
	expect(t, err != nil, "Write after Close reported no error")
	must(t, s.Close(), "second Close")
}

func execStdin(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	if !d.Capabilities().Attach {
		t.Skipf("the driver declares no Attach, so Exec takes no Stdin")
		return
	}
	const id = "sbx_cnf_execstdin"
	attachSandbox(t, d, opts, id)
	out, errOut, code, err := run(t, d, id, runtime.ExecRequest{
		Command: opts.shell(`read line; printf 'got:%s' "$line"; printf 'onstderr' >&2`),
		Stdin:   strings.NewReader("hello\n"),
	})
	must(t, err, "Exec with Stdin")
	expect(t, code == 0, "Exec with Stdin exited %d", code)
	expect(t, strings.Contains(out, "got:hello"), "stdout %q does not carry what was written to stdin", out)
	expect(t, strings.Contains(errOut, "onstderr"), "stderr %q is not the command's, so the streams did not stay separate", errOut)
}

func execTTY(t tb, open func() runtime.Driver, opts Options) {
	d := open()
	if !d.Capabilities().Attach {
		t.Skipf("the driver declares no Attach, so Exec takes no TTY")
		return
	}
	const id = "sbx_cnf_exectty"
	attachSandbox(t, d, opts, id)
	out, errOut, code, err := run(t, d, id, runtime.ExecRequest{
		Command: opts.shell(`if [ -t 1 ]; then printf 'ISATTY\n'; else printf 'ISPIPE\n'; fi; printf 'ONERR\n' >&2`),
		TTY:     true,
	})
	must(t, err, "Exec with TTY")
	expect(t, code == 0, "Exec with TTY exited %d", code)
	expect(t, strings.Contains(out, "ISATTY"), "stdout %q says the command had no terminal", out)
	expect(t, strings.Contains(out, "ONERR"), "stdout %q does not carry stderr, which a terminal merges into it", out)
	expect(t, errOut == "", "Stderr under a TTY carried %q, want nothing", errOut)
}
