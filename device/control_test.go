// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/worker"
)

// socketClient speaks HTTP to the control socket at path.
func socketClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
}

// controlOn serves e's local API on a socket under paths, and answers a
// client of it.
func controlOn(t *testing.T, e *Engine, paths Paths) *http.Client {
	t.Helper()
	ln, err := ListenControl(paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- e.ServeControl(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("the control socket ended with %v", err)
		}
	})
	return socketClient(paths.Socket())
}

// follow opens a stream of the local API and hands each line to the
// returned channel.
func follow(t *testing.T, c *http.Client, route string) <-chan map[string]any {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://device"+route, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("%s answered %s", route, res.Status)
	}
	lines := make(chan map[string]any, 64)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(res.Body)
		for scanner.Scan() {
			var v map[string]any
			if json.Unmarshal(scanner.Bytes(), &v) == nil {
				lines <- v
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = res.Body.Close()
	})
	return lines
}

// next waits for the next line of a stream whose kind is kind.
func next(t *testing.T, lines <-chan map[string]any, kind string) map[string]any {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case v, ok := <-lines:
			if !ok {
				t.Fatalf("the stream ended before a %s line", kind)
			}
			if kind == "" || v["kind"] == kind {
				return v
			}
		case <-deadline:
			t.Fatalf("no %s line arrived", kind)
		}
	}
}

func post(t *testing.T, c *http.Client, route, body string) (int, string) {
	t.Helper()
	res, err := c.Post("http://device"+route, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

func get(t *testing.T, c *http.Client, route string) (int, string) {
	t.Helper()
	res, err := c.Get("http://device" + route)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

// aCreate is a create for a session a person started, on folder.
func aCreate(folder string) worker.Create {
	labels := sessionLabels("ses_c")
	return worker.Create{Labels: labels, Sandbox: v1.Sandbox{
		Metadata: v1.Metadata{Labels: labels},
		Spec:     v1.SandboxSpec{Workspace: v1.Workspace{Host: folder}},
		Status:   v1.SandboxStatus{ID: "sbx_c"},
	}}
}

func TestControlSocket(t *testing.T) {
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	project := connected(t, paths, keys)
	e := testEngine(Config{Paths: paths, Keys: keys, Account: &fakeAccount{}, Version: "v-test"})
	e.policy = Policy{Device: testEnv, Name: "Test computer"}
	e.host = v1.HostInfo{Grants: []v1.HostGrant{{Path: project, Write: true}}}
	c := controlOn(t, e, paths)

	t.Run("the socket is 0600 in a 0700 directory", func(t *testing.T) {
		info, err := os.Stat(paths.Socket())
		if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
			t.Errorf("the socket is %v, %v; want a socket of mode 0600", info.Mode(), err)
		}
		dir, err := os.Stat(filepath.Dir(paths.Socket()))
		if err != nil || dir.Mode().Perm() != 0o700 {
			t.Errorf("its directory is %v, %v; want 0700", dir.Mode(), err)
		}
	})

	t.Run("a connection from another user is refused", func(t *testing.T) {
		path := filepath.Join(filepath.Dir(paths.Socket()), "other.sock")
		ln, err := listenControl(path, os.Getuid()+1, peerUID)
		if err != nil {
			t.Fatal(err)
		}
		ctx := t.Context()
		go func() { _ = e.ServeControl(ctx, ln) }()
		if _, err := socketClient(path).Get("http://device/v1/status"); err == nil {
			t.Error("a process of another user was answered")
		}
		// The peer's credentials are what decide, as the operating system
		// reports them.
		raw, err := net.Dial("unix", paths.Socket())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		if uid, err := peerUID(raw.(*net.UnixConn)); err != nil || uid != os.Getuid() {
			t.Errorf("the peer reads as uid %d, %v; want %d", uid, err, os.Getuid())
		}
	})

	t.Run("status", func(t *testing.T) {
		status, body := get(t, c, "/v1/status")
		var s Status
		if status != http.StatusOK || json.Unmarshal([]byte(body), &s) != nil || s.Device != testEnv || s.Version != "v-test" {
			t.Errorf("status answered %d %s", status, body)
		}
	})

	t.Run("with no client attached a create is refused at once", func(t *testing.T) {
		started := time.Now()
		v := e.Admit(t.Context(), aCreate(project))
		if v.Allow || v.Reason != ReasonNoOneHere || time.Since(started) > time.Second {
			t.Errorf("an unattended create read %+v after %s", v, time.Since(started))
		}
	})

	t.Run("an answer over the socket admits, and every client sees it", func(t *testing.T) {
		first := follow(t, c, "/v1/approvals?follow=1")
		second := follow(t, c, "/v1/approvals?follow=1")
		waitFor(t, "both clients to attach", func() bool { return e.broker.present() })
		verdict := make(chan worker.Verdict, 1)
		go func() { verdict <- e.Admit(context.Background(), aCreate(project)) }()
		asked := next(t, first, EventAsked)
		next(t, second, EventAsked)
		approval := asked["approval"].(map[string]any)
		if approval["session"] != "ses_c" || approval["folder"] != project || approval["write"] != true {
			t.Errorf("the create's facts are %v", approval)
		}
		if status, body := get(t, c, "/v1/approvals"); status != http.StatusOK || !strings.Contains(body, approval["id"].(string)) {
			t.Errorf("the waiting list answered %d %s", status, body)
		}
		id := approval["id"].(string)
		if status, body := post(t, c, "/v1/approvals/"+id, `{"answer":"remember"}`); status != http.StatusBadRequest || !strings.Contains(body, "not_rememberable") {
			t.Errorf("remember for a person's session answered %d %s", status, body)
		}
		if status, body := post(t, c, "/v1/approvals/"+id, `{"answer":"allow","client":"Latere for Mac"}`); status != http.StatusNoContent {
			t.Fatalf("the answer answered %d %s", status, body)
		}
		if v := <-verdict; !v.Allow {
			t.Errorf("the create was not admitted: %+v", v)
		}
		for _, stream := range []<-chan map[string]any{first, second} {
			answered := next(t, stream, EventAnswered)
			if answered["answer"] != AnswerAllow || answered["by"] != "Latere for Mac" {
				t.Errorf("a client saw %v", answered)
			}
			// The session that starts is told on the same stream, which is
			// what a desktop client tells the person of.
			started := next(t, stream, EventStarted)
			if facts, _ := started["approval"].(map[string]any); facts["session"] != "ses_c" || facts["folder"] != project {
				t.Errorf("a client was told of the start as %v", started)
			}
		}
		if status, body := post(t, c, "/v1/approvals/"+id, `{"answer":"deny"}`); status != http.StatusNotFound || !strings.Contains(body, "not_pending") {
			t.Errorf("a second answer answered %d %s; the first answer wins", status, body)
		}
	})

	t.Run("the answers refused", func(t *testing.T) {
		if status, body := post(t, c, "/v1/approvals/apv_x", `{"answer":"maybe"}`); status != http.StatusBadRequest || !strings.Contains(body, "invalid_answer") {
			t.Errorf("an unknown answer answered %d %s", status, body)
		}
		if status, _ := post(t, c, "/v1/approvals/apv_x", `{"answer":`); status != http.StatusBadRequest {
			t.Errorf("a broken body answered %d", status)
		}
		for _, route := range []string{"/v1/pause", "/v1/resume", "/v1/reload"} {
			if status, body := post(t, c, route, ""); status != http.StatusConflict || !strings.Contains(body, "not_serving") {
				t.Errorf("%s of an engine not serving answered %d %s", route, status, body)
			}
		}
	})

	t.Run("activity", func(t *testing.T) {
		activity, err := OpenActivity(paths.ActivityDir(testEnv))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = activity.Close() }()
		e.mu.Lock()
		e.activity = activity
		e.mu.Unlock()
		e.log(Entry{Time: time.Now().UTC(), Type: "Exec", Command: []string{"make"}})
		if status, body := get(t, c, "/v1/activity"); status != http.StatusOK || !strings.Contains(body, `"make"`) {
			t.Errorf("the activity answered %d %s", status, body)
		}
		if status, body := get(t, c, "/v1/activity?since=soon"); status != http.StatusBadRequest || !strings.Contains(body, "invalid_since") {
			t.Errorf("a bad since answered %d %s", status, body)
		}
		lines := follow(t, c, "/v1/activity?follow=1&since=1h")
		if first := next(t, lines, ""); first["type"] != "Exec" {
			t.Errorf("the follow began with %v", first)
		}
		e.log(Entry{Time: time.Now().UTC(), Type: "Write", Path: "/x"})
		if line := next(t, lines, ""); line["type"] != "Write" {
			t.Errorf("the follow carried %v", line)
		}
	})
}

// latere devices pause and resume are clients of the control socket: they
// pause and resume a serving engine, and say so when none serves.
func TestControlPost(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	if _, err := ControlPost(t.Context(), paths.Socket(), "/v1/pause"); CodeOf(err) != CodeNotServing {
		t.Errorf("pause with no socket returned %v", err)
	}
	keys := OpenKeys(paths.KeyFile())
	connected(t, paths, keys)
	p := newStubPlane(t)
	e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
	controlOn(t, e, paths)
	if _, err := ControlPost(t.Context(), paths.Socket(), "/v1/pause"); CodeOf(err) != CodeNotServing {
		t.Errorf("pause of an engine not running returned %v", err)
	}
	running(t, e)
	p.waitConnected(t)
	status, err := ControlPost(t.Context(), paths.Socket(), "/v1/pause")
	if err != nil || status.State != StatePaused {
		t.Fatalf("pause answered %+v, %v", status, err)
	}
	if status, err = ControlPost(t.Context(), paths.Socket(), "/v1/resume"); err != nil {
		t.Fatalf("resume answered %+v, %v", status, err)
	}
	waitFor(t, "the engine to serve again", func() bool { return e.Status().State == StateOnline })
	if _, err = ControlPost(t.Context(), paths.Socket(), "/v1/nothing"); CodeOf(err) != CodeControlUnavailable {
		t.Errorf("an unknown route returned %v", err)
	}
}

func TestControlSocketPathTooLong(t *testing.T) {
	long := "/tmp/" + strings.Repeat("x", 120) + "/control.sock"
	if _, err := ListenControl(long); CodeOf(err) != CodeControlUnavailable || !strings.Contains(err.Error(), "XDG_STATE_HOME") {
		t.Errorf("a socket path past the limit reads %v", err)
	}
}

func TestControlSocketReplacesAStaleSocket(t *testing.T) {
	paths := testPaths(t)
	first, err := ListenControl(paths.Socket())
	if err != nil {
		t.Fatal(err)
	}
	// An engine that crashed leaves its socket file behind.
	first.(*controlListener).SetUnlinkOnClose(false)
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := ListenControl(paths.Socket())
	if err != nil {
		t.Fatalf("a stale socket was not replaced: %v", err)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(paths.Socket()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a closed socket left its file: %v", err)
	}
}

func TestStageCannotReachTheControlSocket(t *testing.T) {
	requireSandbox(t)
	nc, err := exec.LookPath("nc")
	if err != nil {
		t.Skip("nc is not installed here, so no stage can try the socket")
	}
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	project := connected(t, paths, keys)
	p := newStubPlane(t)
	e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
	controlOn(t, e, paths)
	answering(t, e, AnswerAllow)
	running(t, e)
	p.waitConnected(t)
	request := `printf 'GET /v1/status HTTP/1.0\r\n\r\n' | "$1" -w 2 -U "$2"`
	// From the engine's own user outside any sandbox, the socket answers.
	out, err := exec.Command("/bin/sh", "-c", request, "sh", nc, paths.Socket()).Output()
	if err != nil || !strings.Contains(string(out), "200 OK") {
		t.Fatalf("the socket did not answer outside a sandbox: %v %q", err, out)
	}
	if _, err := p.driver.Create(t.Context(), deviceCreate("sbx_sock", project, sessionLabels("ses_sock"))); err != nil {
		t.Fatal(err)
	}
	if code, out := run(t, p.driver, "sbx_sock", "/bin/sh", "-c", request, "sh", nc, paths.Socket()); code == 0 && strings.Contains(out, "200 OK") {
		t.Errorf("a stage's command reached the control socket: %q", out)
	}
}

// Every client reads an answer before the session it admits starts: the
// broker tells the answer before the waiting create hears it, so the
// started event the create then sends comes after it on every stream.
func TestAnswerIsToldBeforeTheSessionStarts(t *testing.T) {
	events := make(chan Event, 8)
	admitted := make(chan struct{})
	b := newBroker(func(e Event) {
		if e.Kind == EventAnswered {
			// Give a create that already heard its answer the time to
			// start before the answer is told; one that has not heard it
			// cannot start meanwhile.
			select {
			case <-admitted:
			case <-time.After(200 * time.Millisecond):
			}
		}
		events <- e
	})
	defer b.attach()()
	go func() {
		defer close(admitted)
		if got, err := b.ask(context.Background(), Approval{Session: "ses_o"}); err != nil || got.answer != AnswerAllow {
			t.Errorf("ask answered %+v, %v", got, err)
		}
		// What Admit sends once the answer admits the create.
		events <- Event{Kind: EventStarted}
	}()
	asked := <-events
	if err := b.answer(asked.Approval.ID, AnswerAllow, "a client"); err != nil {
		t.Fatal(err)
	}
	<-admitted
	if first, second := <-events, <-events; first.Kind != EventAnswered || second.Kind != EventStarted {
		t.Fatalf("the clients read %s, then %s", first.Kind, second.Kind)
	}
}
