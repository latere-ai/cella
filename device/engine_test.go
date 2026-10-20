// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/local"
	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/worker"
)

// engineOn builds an engine for paths against the plane, with the account
// and the store given.
func engineOn(paths Paths, p *stubPlane, account Account, keys KeyStore) *Engine {
	return testEngine(Config{Paths: paths, URL: p.url, Keys: keys, Account: account, Version: "test"})
}

// sessionLabels are the labels Topos puts on a session's sandbox.
func sessionLabels(session string) map[string]string {
	return map[string]string{testLabels.Session: session, testLabels.Agent: "agt_1", testLabels.AgentVersion: "4"}
}

func TestActivityLogHasNoContent(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	project := connected(t, paths, keys)
	p := newStubPlane(t)
	e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
	seen := answering(t, e, AnswerAllow)
	stop, _ := running(t, e)
	p.waitConnected(t)

	if _, err := p.driver.Create(t.Context(), deviceCreate("sbx_1", project, sessionLabels("ses_1"))); err != nil {
		t.Fatal(err)
	}
	// The agent's name reaches the approval from the create's annotation,
	// through the worker.
	if got := seen.all(); len(got) != 1 || got[0].Agent != "Site helper" || got[0].Session != "ses_1" || !got[0].Write {
		t.Errorf("the person was asked about %+v", got)
	}
	if code, out := run(t, p.driver, "sbx_1", "sh", "-c", "echo OUTPUT-WORDS"); code != 0 || !strings.Contains(out, "OUTPUT-WORDS") {
		t.Fatalf("the exec answered %d %q", code, out)
	}
	files := p.driver.(runtime.FileStore)
	written := filepath.Join(project, "notes.txt")
	if _, err := files.Write(t.Context(), "sbx_1", runtime.WriteRequest{Path: written, Mode: 0o600, Body: strings.NewReader("FILE-CONTENT")}); err != nil {
		t.Fatal(err)
	}
	r, _, err := files.Open(t.Context(), "sbx_1", written)
	if err != nil {
		t.Fatal(err)
	}
	var read bytes.Buffer
	_, _ = read.ReadFrom(r)
	_ = r.Close()
	if read.String() != "FILE-CONTENT" {
		t.Fatalf("the file read back as %q", read.String())
	}
	if err = stop(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(paths.ActivityDir(testEnv), "activity.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	for _, secret := range []string{"OUTPUT-WORDS\n", "FILE-CONTENT"} {
		if strings.Contains(log, secret) && !strings.Contains(log, `"echo OUTPUT-WORDS"`) {
			t.Errorf("the activity log carries %q:\n%s", secret, log)
		}
	}
	if strings.Contains(log, "FILE-CONTENT") {
		t.Errorf("the activity log carries a file's content:\n%s", log)
	}
	entries, err := ReadActivity(paths.ActivityDir(testEnv), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, entry := range entries {
		types = append(types, entry.Type)
		if entry.Type == remote.OpExec && (entry.Session != "ses_1" || !slices.Equal(entry.Command, []string{"sh", "-c", "echo OUTPUT-WORDS"})) {
			t.Errorf("the exec's line is %+v", entry)
		}
		if entry.Type == remote.OpWrite && entry.Path != written {
			t.Errorf("the write's line is %+v", entry)
		}
	}
	for _, want := range []string{TypeApproval, remote.OpCreate, remote.OpExec, remote.OpWrite, remote.OpOpen} {
		if !slices.Contains(types, want) {
			t.Errorf("the activity log has no %s line: %v", want, types)
		}
	}
	if info, err := os.Stat(filepath.Join(paths.ActivityDir(testEnv), "activity.ndjson")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the activity log is %v, %v; want mode 0600", info, err)
	}
}

func TestStopEndsBackgroundCommands(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	project := connected(t, paths, keys)
	p := newStubPlane(t)
	e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
	answering(t, e, AnswerAllow)
	stop, _ := running(t, e)
	p.waitConnected(t)

	// Each command is found on this computer by its own command line. A pid
	// the command reports for itself would not do: on Linux, bubblewrap
	// starts every sandbox in a PID namespace of its own, where the shell is
	// pid 2 whatever this computer calls it.
	base := 300000 + time.Now().UnixNano()%100000
	mainLine, backgroundLine := fmt.Sprintf("sleep %d", base), fmt.Sprintf("sleep %d", base+1)
	spec := deviceCreate("sbx_bg", project, sessionLabels("ses_bg"))
	spec.Command = []string{"sh", "-c", "exec " + mainLine}
	if _, err := p.driver.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	// The exec's context lasts as long as the test, as a session's does:
	// the driver ends a command whose caller went away, and run's context
	// ends with run.
	x, err := p.driver.Exec(t.Context(), "sbx_bg", runtime.ExecRequest{
		Command: []string{"sh", "-c", backgroundLine + " >/dev/null 2>&1 &"},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if code, err := x.Wait(waitCtx); err != nil || code != 0 {
		t.Fatalf("the background command did not start: %d %v", code, err)
	}
	lookup := func(line string) []int {
		found, err := processesRunning(line)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	pids := map[string]int{}
	waitFor(t, "the sandbox's command to run", func() bool {
		found := lookup(mainLine)
		if len(found) == 1 {
			pids["the sandbox's command"] = found[0]
		}
		return len(found) == 1
	})
	// What an exec leaves in the background can end with the exec, as the
	// driver and, on Linux, the exec's own PID namespace end it; one still
	// running is held to the same stop as the sandbox's command.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if found := lookup(backgroundLine); len(found) == 1 {
			pids["the background command"] = found[0]
			break
		}
		if time.Now().After(deadline) {
			t.Log("the background command ended with its exec")
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run returned %v", err)
	}
	for what, pid := range pids {
		waitFor(t, what+" to end", func() bool { return !alive(pid) })
	}
}

// alive reports whether a process runs, a zombie not counted.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	// A child of this test process that ended is a zombie until it is
	// reaped; a stage is the child of a shell the driver started, so its
	// state is read from ps where it is.
	out, err := psState(pid)
	return err != nil || !strings.HasPrefix(out, "Z")
}

func TestOwnPathsDenied(t *testing.T) {
	requireSandbox(t)
	paths := testPaths(t)
	// The granted folder is the configuration's parent, ~/.config, which
	// holds ~/.config/latere: the login token and the host policy file.
	if err := os.MkdirAll(paths.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Token, []byte(`{"access_token":"LOGIN-TOKEN"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SavePolicy(paths.PolicyFile(), Policy{
		Device: testEnv, Name: "Test", Folders: []Folder{{Path: "~/.config", Write: true}},
		Network: Network{Mode: NetworkNone},
	}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(paths.Home, ".config", "tool.conf")
	if err := os.WriteFile(other, []byte("readable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys := OpenKeys(paths.KeyFile())
	now := time.Now().UTC()
	if err := keys.Put(Key{Environment: testEnv, Token: "key-1", JTI: "jti-1", MintedAt: now, ExpiresAt: now.Add(KeyTTL)}); err != nil {
		t.Fatal(err)
	}
	p := newStubPlane(t)
	e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
	answering(t, e, AnswerAllow)
	running(t, e)
	p.waitConnected(t)
	granted := filepath.Join(paths.Home, ".config")
	if _, err := p.driver.Create(t.Context(), deviceCreate("sbx_own", granted, sessionLabels("ses_own"))); err != nil {
		t.Fatal(err)
	}
	// A program that stats its own output, such as cat, runs with the
	// sandboxes under the home directory, as they are on a person's
	// computer.
	if code, out := run(t, p.driver, "sbx_own", "cat", other); code != 0 || out != "readable\n" {
		t.Fatalf("cat of a file in the granted folder answered %d %q", code, out)
	}
	readFile := `IFS= read -r line < "$1" && printf '%s' "$line"`
	for _, denied := range []string{paths.Token, paths.PolicyFile()} {
		if code, out := run(t, p.driver, "sbx_own", "sh", "-c", readFile, "sh", denied); code == 0 || strings.Contains(out, "LOGIN-TOKEN") {
			t.Errorf("a stage read %s: %d %q", denied, code, out)
		}
	}
	if err := os.MkdirAll(paths.ActivityDir(testEnv), 0o700); err != nil {
		t.Fatal(err)
	}
	if code, out := run(t, p.driver, "sbx_own", "ls", paths.Devices()); code == 0 || strings.Contains(out, testEnv) {
		t.Errorf("a stage lists the engine's state directory %s: %q", paths.Devices(), out)
	}
	if !strings.HasPrefix(paths.DataDir(testEnv), paths.Cache) {
		t.Fatalf("the sandboxes run outside the cache directory: %s", paths.DataDir(testEnv))
	}
	if code, out := run(t, p.driver, "sbx_own", "sh", "-c", `echo ran in "$HOME"`); code != 0 || !strings.Contains(out, paths.DataDir(testEnv)) {
		t.Errorf("a stage did not run in its scratch home under the cache directory: %d %q", code, out)
	}
}

func TestKeyRotation(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	connected(t, paths, keys)
	old := Key{Environment: testEnv, Token: "key-1", JTI: "jti-1", MintedAt: time.Now().Add(-8 * 24 * time.Hour).UTC(), ExpiresAt: time.Now().Add(22 * 24 * time.Hour).UTC()}
	if err := keys.Put(old); err != nil {
		t.Fatal(err)
	}
	order := &recorder{}
	p := newStubPlane(t)
	p.order = order
	account := &fakeAccount{exists: true, order: order}
	e := engineOn(paths, p, account, keys)
	running(t, e)
	p.waitConnected(t)
	waitFor(t, "the old key to be revoked", func() bool { return len(account.revokedNow()) == 1 })
	if got := account.revokedNow(); got[0] != "jti-1" {
		t.Errorf("revoked %v, want the old key's jti", got)
	}
	got := order.String()
	if !strings.HasPrefix(got, "mint key-2, register key-2") || !strings.Contains(got, "revoke jti-1") ||
		strings.Index(got, "revoke jti-1") < strings.Index(got, "register key-2") {
		t.Errorf("the rotation ran %q; want the new key minted, registered, then the old one revoked", got)
	}
	if strings.Contains(got, "register key-1") {
		t.Errorf("the old key registered: %q", got)
	}
	kept, ok, err := keys.Get(testEnv)
	if err != nil || !ok || kept.Token != "key-2" {
		t.Errorf("the store holds %+v, %v, %v; want the new key", kept, ok, err)
	}

	// A young key is kept as it is.
	paths2 := testPaths(t)
	keys2 := OpenKeys(paths2.KeyFile())
	connected(t, paths2, keys2)
	account2 := &fakeAccount{exists: true}
	p2 := newStubPlane(t)
	e2 := engineOn(paths2, p2, account2, keys2)
	running(t, e2)
	p2.waitConnected(t)
	if account2.minted != 0 || p2.bearersNow()[0] != "key-1" {
		t.Errorf("a young key was replaced: minted %d, registered with %v", account2.minted, p2.bearersNow())
	}
}

func TestRemovedDeviceStops(t *testing.T) {
	for _, tc := range []struct {
		name     string
		act      func(p *stubPlane)
		code     string
		keepsKey bool
	}{
		{"serve_refused at registration", func(p *stubPlane) { p.refuse(http.StatusForbidden, remote.ReasonServeRefused) }, CodeRemoved, false},
		{"key_revoked at registration", func(p *stubPlane) { p.refuse(http.StatusForbidden, remote.ReasonKeyRevoked) }, CodeRemoved, false},
		{"serve_refused on the stream", func(p *stubPlane) { p.endStream(remote.ReasonServeRefused) }, CodeRemoved, false},
		{"key_revoked on the stream", func(p *stubPlane) { p.endStream(remote.ReasonKeyRevoked) }, CodeRemoved, false},
		{"worker_replaced on the stream", func(p *stubPlane) { p.endStream(remote.ReasonWorkerReplaced) }, CodeWorkerReplaced, true},
		{"worker_replaced", func(p *stubPlane) { p.refuse(http.StatusConflict, remote.ReasonWorkerReplaced) }, CodeWorkerReplaced, true},
		{"worker_limit", func(p *stubPlane) { p.refuse(http.StatusConflict, remote.ReasonWorkerLimit) }, CodeWorkerLimit, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandboxRuntime(t)
			paths := testPaths(t)
			keys := OpenKeys(paths.KeyFile())
			project := connected(t, paths, keys)
			p := newStubPlane(t)
			e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
			answering(t, e, AnswerAllow)
			onStream := strings.Contains(tc.name, "on the stream")
			if !onStream {
				tc.act(p)
			}
			stop, done := running(t, e)
			if onStream {
				p.waitConnected(t)
				if _, err := p.driver.Create(t.Context(), deviceCreate("sbx_r", project, sessionLabels("ses_r"))); err != nil {
					t.Fatal(err)
				}
				tc.act(p)
			}
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("the engine did not stop")
			}
			if err := stop(); CodeOf(err) != tc.code {
				t.Fatalf("Run returned %v, want %s", err, tc.code)
			}
			_, kept, kerr := keys.Get(testEnv)
			if kerr != nil || kept != tc.keepsKey {
				t.Errorf("the key is kept: %v (%v), want %v", kept, kerr, tc.keepsKey)
			}
			policy, ok, perr := LoadPolicy(paths.PolicyFile())
			if perr != nil || !ok || len(policy.Folders) != 1 {
				t.Errorf("the host policy was not kept: %+v %v %v", policy, ok, perr)
			}
			if tc.code == CodeRemoved && policy.Device != "" {
				t.Errorf("a removed device is still named in the host policy: %s", policy.Device)
			}
			if tc.code != CodeRemoved && policy.Device != testEnv {
				t.Errorf("a device that was not removed is no longer named: %q", policy.Device)
			}
			if _, err := os.Stat(paths.ActivityDir(testEnv)); err != nil {
				t.Errorf("the activity log was not kept: %v", err)
			}
		})
	}
}

func TestRemovedWhileAway(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	connected(t, paths, keys)
	p := newStubPlane(t)
	p.refuse(http.StatusUnauthorized, "unauthenticated")
	e := engineOn(paths, p, &fakeAccount{exists: false}, keys)
	stop, done := running(t, e)
	select {
	case <-done:
		if err := stop(); CodeOf(err) != CodeRemoved {
			t.Fatalf("Run returned %v, want %s", err, CodeRemoved)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a refused key whose environment is gone did not stop the engine")
	}
	if _, ok, _ := keys.Get(testEnv); ok {
		t.Error("the key of a removed device is kept")
	}
}

func TestRefusedKeyOfALiveDeviceIsReplaced(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	connected(t, paths, keys)
	p := newStubPlane(t)
	p.mu.Lock()
	p.register = func(w http.ResponseWriter, bearer string) bool {
		if bearer != "key-1" {
			return false
		}
		w.WriteHeader(http.StatusUnauthorized)
		return true
	}
	p.mu.Unlock()
	account := &fakeAccount{exists: true}
	e := engineOn(paths, p, account, keys)
	running(t, e)
	p.waitConnected(t)
	if got := p.bearersNow(); got[len(got)-1] != "key-2" {
		t.Errorf("the worker registered with %v; want a new key after the refused one", got)
	}
	if e.Status().State != StateOnline {
		t.Errorf("the engine reads %+v", e.Status())
	}
}

func TestKeyAtStart(t *testing.T) {
	sandboxRuntime(t)
	t.Run("none kept is minted", func(t *testing.T) {
		paths := testPaths(t)
		keys := OpenKeys(paths.KeyFile())
		connected(t, paths, keys)
		if err := keys.Delete(testEnv); err != nil {
			t.Fatal(err)
		}
		p := newStubPlane(t)
		running(t, engineOn(paths, p, &fakeAccount{exists: true}, keys))
		p.waitConnected(t)
		if k, ok, _ := keys.Get(testEnv); !ok || k.Token != "key-2" {
			t.Errorf("the store holds %+v", k)
		}
	})
	t.Run("an expired key with no sign-in stops", func(t *testing.T) {
		paths := testPaths(t)
		keys := OpenKeys(paths.KeyFile())
		connected(t, paths, keys)
		past := time.Now().Add(-time.Hour).UTC()
		if err := keys.Put(Key{Environment: testEnv, Token: "key-1", JTI: "jti-1", MintedAt: past.Add(-KeyTTL), ExpiresAt: past}); err != nil {
			t.Fatal(err)
		}
		p := newStubPlane(t)
		e := engineOn(paths, p, &fakeAccount{mintErr: errors.New("the saved login expired")}, keys)
		if err := e.Run(t.Context()); CodeOf(err) != CodeSignInExpired || !strings.Contains(err.Error(), "latere login") {
			t.Errorf("Run returned %v, want %s", err, CodeSignInExpired)
		}
	})
	t.Run("a rotation that cannot mint serves with the current key", func(t *testing.T) {
		paths := testPaths(t)
		keys := OpenKeys(paths.KeyFile())
		connected(t, paths, keys)
		if err := keys.Put(Key{Environment: testEnv, Token: "key-1", JTI: "jti-1", MintedAt: time.Now().Add(-10 * 24 * time.Hour), ExpiresAt: time.Now().Add(20 * 24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		p := newStubPlane(t)
		e := engineOn(paths, p, &fakeAccount{mintErr: errors.New("the saved login expired")}, keys)
		events, stopEvents := e.Subscribe()
		defer stopEvents()
		running(t, e)
		p.waitConnected(t)
		if got := p.bearersNow(); got[0] != "key-1" {
			t.Errorf("the worker registered with %v", got)
		}
		if !sawNotice(events, "could not be replaced") {
			t.Error("the person was not told the key could not be replaced")
		}
	})
	t.Run("a removed device's mint stops as removed", func(t *testing.T) {
		paths := testPaths(t)
		keys := OpenKeys(paths.KeyFile())
		connected(t, paths, keys)
		if err := keys.Delete(testEnv); err != nil {
			t.Fatal(err)
		}
		p := newStubPlane(t)
		e := engineOn(paths, p, &fakeAccount{mintErr: ErrRemoved}, keys)
		if err := e.Run(t.Context()); CodeOf(err) != CodeRemoved {
			t.Errorf("Run returned %v, want %s", err, CodeRemoved)
		}
	})
	t.Run("a key in the file is said at the start", func(t *testing.T) {
		paths := testPaths(t)
		keys := &Keys{Keychain: failingKeychain{}, File: KeyFile{Path: paths.KeyFile()}}
		connected(t, paths, keys)
		p := newStubPlane(t)
		e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
		events, stopEvents := e.Subscribe()
		defer stopEvents()
		running(t, e)
		if !sawNotice(events, paths.KeyFile()) {
			t.Error("the engine did not say the key is kept in a file")
		}
	})
}

// failingKeychain is a host with no keychain.
type failingKeychain struct{}

func (failingKeychain) Get(string) (Key, bool, error) { return Key{}, false, errors.New("no keychain") }
func (failingKeychain) Put(Key) error                 { return errors.New("no keychain") }
func (failingKeychain) Delete(string) error           { return errors.New("no keychain") }

// sawNotice waits for a notice containing text.
func sawNotice(events <-chan Event, text string) bool {
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return false
			}
			if ev.Kind == EventNotice && strings.Contains(ev.Notice, text) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// The engine follows the worker's State: online once the registration is
// taken and the stream is open, offline with why when the stream ends or a
// registration is answered with an error, and online again once the worker
// reconnects.
func TestEngineFollowsTheWorkerState(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	connected(t, paths, keys)
	p := newStubPlane(t)
	e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
	events, stopEvents := e.Subscribe()
	defer stopEvents()
	running(t, e)
	p.waitConnected(t)
	waitFor(t, "online", func() bool { return e.Status().State == StateOnline })
	// The next registration is answered as a control plane that is not
	// ready answers it, once; the one after it is taken.
	var once sync.Once
	p.mu.Lock()
	p.register = func(w http.ResponseWriter, _ string) bool {
		refused := false
		once.Do(func() {
			refused = true
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"not_ready","message":"not ready"}}`)
		})
		return refused
	}
	p.mu.Unlock()
	p.endStream("")
	var details []string
	deadline := time.After(30 * time.Second)
	for len(details) < 3 || details[len(details)-1] != StateOnline {
		select {
		case ev := <-events:
			if ev.Kind != EventStatus {
				continue
			}
			// The online of the first connection came before the stream
			// ended, and is not part of what is read here.
			if ev.Status.State == StateOnline && len(details) > 0 {
				details = append(details, StateOnline)
			} else if ev.Status.State == StateOffline {
				details = append(details, ev.Status.Detail)
			}
		case <-deadline:
			t.Fatalf("the engine went through %q; want offline when the stream ended, offline at the refused registration, then online", details)
		}
	}
	if len(details) != 3 || !strings.HasPrefix(details[0], "the connection to the control plane ended: ") ||
		details[1] != "the control plane answered the registration with 503 Service Unavailable" {
		t.Errorf("the engine went through %q", details)
	}
}

// Each State the worker reports reads as the engine's state, and the last
// one, as Run returns, leaves the state to whatever ended Run.
func TestWorkerStateReads(t *testing.T) {
	paths := testPaths(t)
	e := testEngine(Config{Paths: paths, Keys: OpenKeys(paths.KeyFile()), Account: &fakeAccount{}})
	e.workerState(worker.State{Err: errors.New("dial tcp: connection refused"), Reconnect: true, RetryIn: time.Second})
	if s := e.Status(); s.State != StateOffline || s.Detail != "the control plane could not be reached: dial tcp: connection refused" {
		t.Errorf("a control plane never reached reads %+v", s)
	}
	e.workerState(worker.State{Connected: true})
	if s := e.Status(); s.State != StateOnline || !s.Online {
		t.Errorf("a connected worker reads %+v", s)
	}
	e.workerState(worker.State{Reconnect: true})
	if s := e.Status(); s.State != StateOffline || s.Detail != "the connection to the control plane ended: no reason was given; reconnecting" {
		t.Errorf("a stream that ended reads %+v", s)
	}
	e.workerState(worker.State{Err: &worker.RegistrationError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized"}, Reconnect: true})
	if s := e.Status(); s.State != StateOffline || s.Detail != "the control plane refused this computer's key" {
		t.Errorf("a refused key reads %+v", s)
	}
	e.workerState(worker.State{Err: context.Canceled})
	if s := e.Status(); s.Detail != "the control plane refused this computer's key" {
		t.Errorf("the last State changed the state to %+v", s)
	}
}

func TestEngineAdmits(t *testing.T) {
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	connected(t, paths, keys)
	e := testEngine(Config{Paths: paths, Keys: keys, Account: &fakeAccount{}})
	e.policy.Device = testEnv
	e.host = v1.HostInfo{Grants: []v1.HostGrant{{Path: filepath.Join(paths.Home, "project"), Write: true}}}
	create := func(labels map[string]string) worker.Create {
		return worker.Create{Labels: labels, Sandbox: v1.Sandbox{
			Metadata: v1.Metadata{Labels: labels, Annotations: map[string]string{testLabels.AgentName: "Site helper"}},
			Spec:     v1.SandboxSpec{Workspace: v1.Workspace{Host: filepath.Join(paths.Home, "project")}},
			Status:   v1.SandboxStatus{ID: "sbx_a"},
		}}
	}
	scheduled := map[string]string{testLabels.Session: "ses_t", testLabels.Agent: "agt_1", testLabels.AgentVersion: "4", testLabels.Trigger: "trg_1", testLabels.TriggerFiresOn: FiresOnSchedule}

	if v := e.Admit(t.Context(), create(sessionLabels("ses_1"))); v.Allow || v.Reason != ReasonNoOneHere {
		t.Errorf("with no client attached a create reads %+v", v)
	}
	seen := answering(t, e, AnswerRemember)
	if v := e.Admit(t.Context(), create(scheduled)); !v.Allow {
		t.Fatalf("a remembered answer did not admit: %+v", v)
	}
	a := seen.all()[0]
	if a.Agent != "Site helper" || a.AgentRef != "agt_1" || a.AgentVersion != "4" || !a.Write || !a.Rememberable ||
		a.StartedBy() != "by trigger trg_1 (on a schedule)" {
		t.Errorf("the approval's facts are %+v", a)
	}
	policy, _, _ := LoadPolicy(paths.PolicyFile())
	if !policy.Remembers("trg_1", "4", time.Now()) {
		t.Fatalf("the trigger was not written to the host policy: %+v", policy.RememberedTriggers)
	}
	if v := e.Admit(t.Context(), create(scheduled)); !v.Allow || len(seen.all()) != 1 {
		t.Errorf("a remembered trigger was asked again: %+v, asked %d times", v, len(seen.all()))
	}
	changed := map[string]string{}
	maps.Copy(changed, scheduled)
	changed[testLabels.AgentVersion] = "5"
	e.Admit(t.Context(), create(changed))
	if len(seen.all()) != 2 {
		t.Error("a remembered trigger whose agent changed was not asked")
	}
	policy, _, _ = LoadPolicy(paths.PolicyFile())
	if !policy.Remembers("trg_1", "5", time.Now()) || policy.Remembers("trg_1", "4", time.Now()) {
		t.Errorf("remembering the changed agent did not replace the entry: %+v", policy.RememberedTriggers)
	}
	// A trigger forgotten while the engine runs asks again at once.
	if !policy.Forget("trg_1") {
		t.Fatal("forget found nothing")
	}
	if err := SavePolicy(paths.PolicyFile(), policy); err != nil {
		t.Fatal(err)
	}
	e.Admit(t.Context(), create(changed))
	if len(seen.all()) != 3 {
		t.Errorf("a forgotten trigger was admitted without asking; asked %d times", len(seen.all()))
	}
}

func TestBrokerAnswers(t *testing.T) {
	var events []Event
	var mu sync.Mutex
	b := newBroker(func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() })
	detach := b.attach()
	ask := func(a Approval) chan given {
		out := make(chan given, 1)
		go func() {
			g, err := b.ask(context.Background(), a)
			if err != nil {
				t.Error(err)
			}
			out <- g
		}()
		waitFor(t, "the create to wait", func() bool { return len(b.waitingNow()) > 0 })
		return out
	}
	person := ask(Approval{Session: "ses_p"})
	id := b.waitingNow()[0].ID
	if err := b.answer(id, AnswerRemember, "x"); !errors.Is(err, ErrNotRememberable) {
		t.Errorf("remember for a session a person started reads %v", err)
	}
	if err := b.answer(id, "maybe", "x"); !errors.Is(err, ErrBadAnswer) {
		t.Errorf("an unknown answer reads %v", err)
	}
	if err := b.answer(id, AnswerDeny, "the terminal"); err != nil {
		t.Fatal(err)
	}
	if g := <-person; g.answer != AnswerDeny || g.by != "the terminal" {
		t.Errorf("the answer reached the create as %+v", g)
	}
	if err := b.answer(id, AnswerAllow, "another client"); !errors.Is(err, ErrNotPending) {
		t.Errorf("a second answer reads %v; the first answer wins", err)
	}
	events_ := ask(Approval{Trigger: "trg_e", FiresOn: FiresOnEvents, AgentVersion: "1"})
	if err := b.answer(b.waitingNow()[0].ID, AnswerRemember, "x"); !errors.Is(err, ErrNotRememberable) {
		t.Errorf("remember for a trigger on delivered events reads %v", err)
	}
	// The last client leaving refuses what still waits: nobody is left to
	// answer it.
	detach()
	if g := <-events_; g.answer != answerNoOne {
		t.Errorf("a create left waiting by the last client reads %+v", g)
	}
	detach()
	if b.present() {
		t.Error("a client is counted after it detached")
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.attach()
	withdrawn := make(chan error, 1)
	go func() { _, err := b.ask(ctx, Approval{}); withdrawn <- err }()
	waitFor(t, "the create to wait", func() bool { return len(b.waitingNow()) == 1 })
	cancel()
	if err := <-withdrawn; !errors.Is(err, context.Canceled) {
		t.Errorf("a withdrawn create reads %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	for _, want := range []string{EventAsked, EventAnswered, EventWithdrawn} {
		if !slices.Contains(kinds, want) {
			t.Errorf("no %s event among %v", want, kinds)
		}
	}
}

func TestApprovalFacts(t *testing.T) {
	host := v1.HostInfo{Grants: []v1.HostGrant{{Path: "/h/site", Write: true}, {Path: "/h/site/ro"}}}
	c := worker.Create{Sandbox: v1.Sandbox{
		Metadata: v1.Metadata{Labels: map[string]string{testLabels.Agent: "agt_9", testLabels.Trigger: "trg_9", testLabels.TriggerFiresOn: FiresOnEvents}},
		Spec:     v1.SandboxSpec{Workspace: v1.Workspace{Host: "/h/site/ro/x"}},
	}}
	a := approvalOf(c, testLabels, host, time.Now())
	if a.Agent != "agt_9" || a.Write || a.Rememberable || a.StartedBy() != "by trigger trg_9 (on delivered events)" {
		t.Errorf("the facts read %+v, started %q", a, a.StartedBy())
	}
	if (Approval{}).StartedBy() != "by a person" || (Approval{Trigger: "t"}).StartedBy() != "by trigger t" {
		t.Error("who started a session is misworded")
	}
	scheduled := approvalOf(worker.Create{Labels: map[string]string{testLabels.Trigger: "trg", testLabels.TriggerFiresOn: FiresOnSchedule}}, testLabels, host, time.Now())
	if scheduled.Rememberable {
		t.Error("a scheduled trigger with no agent version can be remembered")
	}
}

func TestEnginePauseResumeReload(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	project := connected(t, paths, keys)
	p := newStubPlane(t)
	e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
	if err := e.Pause(); !errors.Is(err, ErrNotRunning) {
		t.Errorf("a pause of an engine not running reads %v", err)
	}
	answering(t, e, AnswerAllow)
	running(t, e)
	p.waitConnected(t)
	if _, err := p.driver.Create(t.Context(), deviceCreate("sbx_p", project, sessionLabels("ses_p"))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the session to be counted", func() bool { return e.Status().Sessions == 1 })
	if err := e.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := e.Pause(); err != nil {
		t.Errorf("a second pause failed: %v", err)
	}
	waitFor(t, "paused", func() bool { return e.Status().State == StatePaused })
	waitFor(t, "the worker to disconnect", func() bool {
		_, ok := p.live()
		return !ok
	})
	check, err := local.New(local.Options{DataDir: paths.DataDir(testEnv), Home: paths.Home})
	if err != nil {
		t.Fatal(err)
	}
	// The worker stops its sandboxes before its run returns, which can come
	// a moment after the stream closes, so the stop is waited for, not read
	// once.
	waitFor(t, "a pause to stop the sandbox", func() bool {
		state, err := check.Inspect(t.Context(), "sbx_p")
		return err == nil && state.Phase == runtime.Stopped
	})
	if err = e.Resume(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "online after the resume", func() bool { return e.Status().State == StateOnline })

	// A reload with a file naming another device is refused, and one with a
	// new folder registers anew with it.
	policy, _, _ := LoadPolicy(paths.PolicyFile())
	policy.Device = "dev-zzzzzzzzzzzzzzzz"
	if err = SavePolicy(paths.PolicyFile(), policy); err != nil {
		t.Fatal(err)
	}
	if err = e.Reload(); CodeOf(err) != CodePolicyInvalid {
		t.Errorf("a reload naming another device reads %v", err)
	}
	docs := filepath.Join(paths.Home, "docs")
	if err = os.MkdirAll(docs, 0o700); err != nil {
		t.Fatal(err)
	}
	policy.Device = testEnv
	policy.Folders = append(policy.Folders, Folder{Path: "~/docs"})
	if err = SavePolicy(paths.PolicyFile(), policy); err != nil {
		t.Fatal(err)
	}
	if err = e.Reload(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the new folder to be registered", func() bool {
		host, ok := p.live()
		return ok && host != nil && len(host.Grants) == 2
	})
	policy.Folders = append(policy.Folders, Folder{Path: "/"})
	if err = SavePolicy(paths.PolicyFile(), policy); err != nil {
		t.Fatal(err)
	}
	if err = e.Reload(); CodeOf(err) != CodePolicyInvalid {
		t.Errorf("a reload granting the root reads %v", err)
	}
}

func TestEngineRefusals(t *testing.T) {
	t.Run("not connected", func(t *testing.T) {
		paths := testPaths(t)
		e := testEngine(Config{Paths: paths, Keys: OpenKeys(paths.KeyFile())})
		if err := e.Run(t.Context()); CodeOf(err) != CodeNotConnected {
			t.Errorf("Run returned %v", err)
		}
	})
	t.Run("an unusable policy", func(t *testing.T) {
		paths := testPaths(t)
		if err := SavePolicy(paths.PolicyFile(), Policy{Device: testEnv, Network: Network{Mode: "open"}}); err != nil {
			t.Fatal(err)
		}
		e := testEngine(Config{Paths: paths, Keys: OpenKeys(paths.KeyFile())})
		if err := e.Run(t.Context()); CodeOf(err) != CodePolicyInvalid {
			t.Errorf("Run returned %v", err)
		}
	})
	t.Run("a sandbox runtime missing", func(t *testing.T) {
		if goruntime.GOOS == "darwin" {
			// This test binary calls hostsandbox.Main, so on macOS the
			// engine confines with Seatbelt, which is part of the system:
			// no sandbox runtime can be missing there.
			t.Skip("Seatbelt is part of macOS")
		}
		t.Setenv("PATH", "/nonexistent")
		paths := testPaths(t)
		keys := OpenKeys(paths.KeyFile())
		connected(t, paths, keys)
		p := newStubPlane(t)
		e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
		err := e.Run(t.Context())
		if CodeOf(err) != CodeSandboxUnavailable || !strings.Contains(err.Error(), "srt") {
			t.Errorf("Run returned %v", err)
		}
	})
	t.Run("root", func(t *testing.T) {
		paths := testPaths(t)
		keys := OpenKeys(paths.KeyFile())
		connected(t, paths, keys)
		e := testEngine(Config{Paths: paths, Keys: keys, Account: &fakeAccount{}, NewDriver: func(local.Options) (runtime.Driver, error) {
			return nil, local.ErrRunsAsRoot
		}})
		if err := e.Run(t.Context()); CodeOf(err) != CodeRunsAsRoot {
			t.Errorf("Run returned %v", err)
		}
	})
}

func TestEngineAnnouncesAnUpgrade(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	connected(t, paths, keys)
	p := newStubPlane(t)
	e := testEngine(Config{Paths: paths, URL: p.url, Keys: keys, Account: &fakeAccount{exists: true}, Version: "v0.22.0",
		Upgrade: func(context.Context) (string, bool) { return "v0.23.0", true }, UpgradeEvery: 50 * time.Millisecond})
	events, stopEvents := e.Subscribe()
	defer stopEvents()
	running(t, e)
	if !sawNotice(events, "restart to apply") {
		t.Fatal("no upgrade was announced")
	}
	if got := e.Status().Upgrade; !strings.Contains(got, "v0.23.0") {
		t.Errorf("the status names the upgrade %q", got)
	}
}

func TestLockRefusesASecondEngine(t *testing.T) {
	path := filepath.Join(testPaths(t).Devices(), "lock")
	first, err := TakeLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = TakeLock(path); CodeOf(err) != CodeAlreadyServed || !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Errorf("a second lock reads %v", err)
	}
	if err = first.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := TakeLock(path)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	if err = again.Release(); err != nil {
		t.Fatal(err)
	}
	if err = (*Lock)(nil).Release(); err != nil {
		t.Error(err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the state directory is %v, %v; want 0700", info, err)
	}
}

func TestKeyExpiresWhileServing(t *testing.T) {
	sandboxRuntime(t)
	expiring := func(t *testing.T, account *fakeAccount) (*stubPlane, *Engine, KeyStore) {
		t.Helper()
		paths := testPaths(t)
		keys := OpenKeys(paths.KeyFile())
		connected(t, paths, keys)
		now := time.Now().UTC()
		if err := keys.Put(Key{Environment: testEnv, Token: "key-1", JTI: "jti-1", MintedAt: now, ExpiresAt: now.Add(1500 * time.Millisecond)}); err != nil {
			t.Fatal(err)
		}
		p := newStubPlane(t)
		return p, engineOn(paths, p, account, keys), keys
	}
	t.Run("a sign-in that still works renews it", func(t *testing.T) {
		p, e, keys := expiring(t, &fakeAccount{exists: true})
		running(t, e)
		waitFor(t, "a registration with the renewed key", func() bool {
			got := p.bearersNow()
			return len(got) > 0 && got[len(got)-1] == "key-2"
		})
		if k, _, _ := keys.Get(testEnv); k.Token != "key-2" {
			t.Errorf("the store holds %+v", k)
		}
	})
	t.Run("one that cannot stops with the sign-in refusal", func(t *testing.T) {
		account := &fakeAccount{exists: true}
		p, e, _ := expiring(t, account)
		stop, done := running(t, e)
		p.waitConnected(t)
		account.mu.Lock()
		account.mintErr = errors.New("the saved login expired")
		account.mu.Unlock()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("the engine kept serving with an expired key")
		}
		if err := stop(); CodeOf(err) != CodeSignInExpired {
			t.Errorf("Run returned %v, want %s", err, CodeSignInExpired)
		}
	})
}

func TestErrorRendering(t *testing.T) {
	cause := errors.New("underneath")
	err := Fail(CodeRemoved, "", cause)
	if !errors.Is(err, cause) || err.Detail != "underneath" || CodeOf(fmt.Errorf("wrapped: %w", err)) != CodeRemoved || CodeOf(cause) != "" {
		t.Errorf("the refusal reads %+v", err)
	}
	want := "device_removed: This computer was removed from your devices.\nRun `latere devices connect` to connect it again.\ndetail: underneath"
	if err.Error() != want {
		t.Errorf("the refusal renders %q", err.Error())
	}
	for code, r := range refusals {
		if r.message == "" || strings.ContainsAny(r.message+r.hint, "\u2013\u2014") {
			t.Errorf("%s has an empty sentence or a dash: %+v", code, r)
		}
	}
}
