// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/local"
)

// The control socket's routes for a desktop client (spec 018): connecting
// through the socket, the client's credentials, the host policy and the
// removal.

// fakeEnroller is the command's side of a connect, a rename and a removal,
// in memory: a connect writes the host policy with testEnv as its device.
type fakeEnroller struct {
	paths Paths

	mu        sync.Mutex
	connects  []Policy
	renamed   map[string]string
	removed   []string
	renameErr error
	removeErr error
	// nameErr is the platform refusing the name at a connect.
	nameErr error
	// renameStarted, when set, is told of each rename, which then waits for
	// renameGo, so a test can act while a rename is under way.
	renameStarted chan struct{}
	renameGo      chan struct{}
}

func (f *fakeEnroller) Propose(previous Policy, q ConnectQuery) (Policy, error) {
	p := previous
	p.Name = CleanName(q.Name)
	if q.SessionStart != "" {
		p.SessionStart = q.SessionStart
	}
	if len(q.Folders) > 0 {
		p.Folders = nil
		for _, v := range q.Folders {
			folder, err := ParseFolder(v, f.paths.Home, f.paths.Home)
			if err != nil {
				return Policy{}, err
			}
			folder.Path = Shorten(f.paths.Home, folder.Path)
			p.Folders = append(p.Folders, folder)
		}
	}
	p.Network = Network{Mode: NetworkAllowlist, Hosts: []string{"github.com"}}
	if q.Network == NetworkNone {
		p.Network = Network{Mode: NetworkNone}
	}
	return p, nil
}

func (f *fakeEnroller) Connect(_ context.Context, p Policy) (Connected, error) {
	p.Device = testEnv
	if err := SavePolicy(f.paths.PolicyFile(), p); err != nil {
		return Connected{}, err
	}
	f.mu.Lock()
	f.connects = append(f.connects, p)
	nameErr := f.nameErr
	f.mu.Unlock()
	return Connected{Policy: p, NameError: nameErr}, nil
}

func (f *fakeEnroller) Rename(_ context.Context, env, name string) error {
	f.mu.Lock()
	started, proceed := f.renameStarted, f.renameGo
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
		<-proceed
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.renameErr != nil {
		return f.renameErr
	}
	if f.renamed == nil {
		f.renamed = map[string]string{}
	}
	f.renamed[env] = name
	return nil
}

func (f *fakeEnroller) Remove(_ context.Context, env string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, env)
	return nil
}

func (f *fakeEnroller) removedNow() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

// send is a request with a JSON body to the local API, answering the status
// code and the body.
func send(t *testing.T, c *http.Client, method, route, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, "http://device"+route, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

// statusOf reads the engine's status through the socket.
func statusOf(t *testing.T, c *http.Client) Status {
	t.Helper()
	code, body := get(t, c, "/v1/status")
	var s Status
	if code != http.StatusOK || json.Unmarshal([]byte(body), &s) != nil {
		t.Fatalf("status answered %d %s", code, body)
	}
	return s
}

// connectThroughSocket asks a proposal for folder and connects with its
// consent, as a client that showed it does.
func connectThroughSocket(t *testing.T, c *http.Client, folder string) Proposal {
	t.Helper()
	code, body := get(t, c, "/v1/connect?name=Test+computer&folder="+url.QueryEscape(folder))
	var proposal Proposal
	if code != http.StatusOK || json.Unmarshal([]byte(body), &proposal) != nil {
		t.Fatalf("the proposal answered %d %s", code, body)
	}
	if len(proposal.Problems) != 0 {
		t.Fatalf("the proposal has problems: %v", proposal.Problems)
	}
	request, _ := json.Marshal(map[string]any{"policy": proposal.Policy, "consent": proposal.Consent})
	if code, body := post(t, c, "/v1/connect", string(request)); code != http.StatusOK {
		t.Fatalf("the connect answered %d %s", code, body)
	}
	return proposal
}

// errorCode is the code of a refusal of the local API.
func errorCode(body string) string {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &envelope)
	return envelope.Error.Code
}

func TestServeWaitsNotConnected(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	p := newStubPlane(t)
	enroll := &fakeEnroller{paths: paths}
	e := New(Config{Paths: paths, URL: p.url, Keys: keys, Account: &fakeAccount{exists: true}, Version: "test",
		Enroll: enroll, WaitToConnect: true})
	c := controlOn(t, e, paths)
	_, done := running(t, e)
	waitFor(t, "the state not_connected", func() bool { return e.Status().State == StateNotConnected })
	if s := statusOf(t, c); s.Device != "" || s.Online {
		t.Errorf("a computer not connected reads %+v", s)
	}
	if _, err := os.Stat(paths.PolicyFile()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("waiting wrote a host policy file: %v", err)
	}
	select {
	case <-done:
		t.Fatal("serve ended on a computer not connected")
	default:
	}
}

func TestConnectThroughSocket(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	project := filepath.Join(paths.Home, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	p := newStubPlane(t)
	enroll := &fakeEnroller{paths: paths}
	e := New(Config{Paths: paths, URL: p.url, Keys: keys, Account: &fakeAccount{exists: true}, Version: "test",
		Enroll: enroll, WaitToConnect: true})
	c := controlOn(t, e, paths)
	running(t, e)
	waitFor(t, "the state not_connected", func() bool { return e.Status().State == StateNotConnected })

	t.Run("a folder neither absolute nor under ~ is refused", func(t *testing.T) {
		code, body := get(t, c, "/v1/connect?folder=project")
		if code != http.StatusUnprocessableEntity || errorCode(body) != CodePolicyInvalid {
			t.Errorf("a relative folder answered %d %s", code, body)
		}
	})

	var proposal Proposal
	t.Run("the proposal names the terms and the facts the screen states", func(t *testing.T) {
		code, body := get(t, c, "/v1/connect?name=Test+computer&folder=~/project")
		if code != http.StatusOK || json.Unmarshal([]byte(body), &proposal) != nil {
			t.Fatalf("the proposal answered %d %s", code, body)
		}
		if proposal.Policy.Name != "Test computer" || len(proposal.Policy.Folders) != 1 || proposal.Policy.Folders[0].Path != "~/project" ||
			!proposal.Policy.Folders[0].Write || proposal.Policy.SessionStart != SessionStartAllow || len(proposal.Problems) != 0 {
			t.Errorf("the proposal reads %+v", proposal)
		}
		var kinds []string
		for _, f := range proposal.Facts {
			kinds = append(kinds, f.Kind)
		}
		if strings.Join(kinds, ",") != "folders,secrets,network,who,log" || !strings.HasPrefix(proposal.Consent, "sha256:") {
			t.Errorf("the facts are %v and the consent %q", kinds, proposal.Consent)
		}
	})

	t.Run("a consent for other terms is refused and connects nothing", func(t *testing.T) {
		changed := proposal.Policy
		changed.Network = Network{Mode: NetworkNone, Hosts: []string{}}
		request, _ := json.Marshal(map[string]any{"policy": changed, "consent": proposal.Consent})
		code, body := post(t, c, "/v1/connect", string(request))
		if code != http.StatusUnprocessableEntity || errorCode(body) != CodeNotConfirmed {
			t.Errorf("a stale consent answered %d %s", code, body)
		}
		if len(enroll.connects) != 0 {
			t.Errorf("a refused consent connected: %v", enroll.connects)
		}
	})

	t.Run("the consent connects, and the engine serves the device", func(t *testing.T) {
		request, _ := json.Marshal(map[string]any{"policy": proposal.Policy, "consent": proposal.Consent})
		if code, body := post(t, c, "/v1/connect", string(request)); code != http.StatusOK {
			t.Fatalf("the connect answered %d %s", code, body)
		}
		p.waitConnected(t)
		waitFor(t, "the device to serve", func() bool { s := e.Status(); return s.Online && s.Device == testEnv })
		code, body := get(t, c, "/v1/connect?folder=~/project")
		if code != http.StatusUnprocessableEntity || errorCode(body) != CodeAlreadyConnected {
			t.Errorf("a proposal on a connected computer answered %d %s", code, body)
		}
	})
}

func TestRemoveThroughSocket(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	if err := os.MkdirAll(filepath.Join(paths.Home, "project"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := newStubPlane(t)
	enroll := &fakeEnroller{paths: paths}
	e := New(Config{Paths: paths, URL: p.url, Keys: keys, Account: &fakeAccount{exists: true}, Version: "test",
		Enroll: enroll, WaitToConnect: true})
	c := controlOn(t, e, paths)
	_, done := running(t, e)
	waitFor(t, "the state not_connected", func() bool { return e.Status().State == StateNotConnected })
	connectThroughSocket(t, c, "~/project")
	waitFor(t, "the device to serve", func() bool { return e.Status().Online })

	code, body := post(t, c, "/v1/remove", "")
	var s Status
	if code != http.StatusOK || json.Unmarshal([]byte(body), &s) != nil || s.State != StateNotConnected || s.Device != "" {
		t.Fatalf("the removal answered %d %s", code, body)
	}
	if got := enroll.removedNow(); !slices.Equal(got, []string{testEnv}) {
		t.Errorf("the platform was asked to remove %v", got)
	}
	if _, ok, err := keys.Get(testEnv); ok || err != nil {
		t.Errorf("the key is still kept: %v, %v", ok, err)
	}
	if _, err := os.Stat(paths.PolicyFile()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the host policy file is still there: %v", err)
	}
	if _, err := os.Stat(paths.ActivityDir(testEnv)); err != nil {
		t.Errorf("the activity log went with the device: %v", err)
	}
	select {
	case <-done:
		t.Fatal("serve ended with the device's removal")
	default:
	}

	// Connected again, the local form forgets the device here and asks the
	// platform nothing.
	connectThroughSocket(t, c, "~/project")
	waitFor(t, "the device to serve again", func() bool { return e.Status().Online })
	if code, body := post(t, c, "/v1/remove", `{"local":true}`); code != http.StatusOK {
		t.Fatalf("the local removal answered %d %s", code, body)
	}
	if got := enroll.removedNow(); len(got) != 1 {
		t.Errorf("the local removal reached the platform: %v", got)
	}
	if code, body := post(t, c, "/v1/remove", ""); code != http.StatusUnprocessableEntity || errorCode(body) != CodeNotConnected {
		t.Errorf("a removal with no device answered %d %s", code, body)
	}
}

func TestPolicyThroughSocket(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	connected(t, paths, keys)
	other := filepath.Join(paths.Home, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	p := newStubPlane(t)
	enroll := &fakeEnroller{paths: paths}
	var mu sync.Mutex
	var built [][]local.Grant
	e := New(Config{Paths: paths, URL: p.url, Keys: keys, Account: &fakeAccount{exists: true}, Version: "test", Enroll: enroll,
		NewDriver: func(o local.Options) (runtime.Driver, error) {
			if !strings.Contains(o.DataDir, "check-") {
				mu.Lock()
				built = append(built, o.Grants)
				mu.Unlock()
			}
			return local.New(o)
		}})
	c := controlOn(t, e, paths)
	running(t, e)
	p.waitConnected(t)

	t.Run("the policy reads as the file", func(t *testing.T) {
		code, body := get(t, c, "/v1/policy")
		var got Policy
		if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || got.Device != testEnv || got.Folders[0].Path != "~/project" {
			t.Errorf("the policy answered %d %s", code, body)
		}
	})

	t.Run("a folder the driver refuses is refused with its problem", func(t *testing.T) {
		code, body := send(t, c, http.MethodPut, "/v1/policy", `{"folders":[{"path":"relative/x","write":true}]}`)
		if code != http.StatusUnprocessableEntity || errorCode(body) != CodePolicyInvalid || !strings.Contains(body, "relative/x") {
			t.Errorf("a relative folder answered %d %s", code, body)
		}
		if p, _, _ := LoadPolicy(paths.PolicyFile()); p.Folders[0].Path != "~/project" {
			t.Errorf("a refused change was written: %+v", p)
		}
	})

	t.Run("a rename the platform refuses changes nothing", func(t *testing.T) {
		enroll.mu.Lock()
		enroll.renameErr = errors.New("the platform is down")
		enroll.mu.Unlock()
		t.Cleanup(func() { enroll.mu.Lock(); enroll.renameErr = nil; enroll.mu.Unlock() })
		code, body := send(t, c, http.MethodPut, "/v1/policy", `{"name":"Elsewhere"}`)
		if code != http.StatusUnprocessableEntity || errorCode(body) != CodeRenameFailed {
			t.Errorf("a refused rename answered %d %s", code, body)
		}
		if p, _, _ := LoadPolicy(paths.PolicyFile()); p.Name != "Test computer" {
			t.Errorf("a refused rename was written: %+v", p)
		}
	})

	t.Run("a change is written, named at the platform, and served", func(t *testing.T) {
		code, body := send(t, c, http.MethodPut, "/v1/policy",
			`{"name":"Work laptop","folders":[{"path":"`+other+`","write":false}],"network":{"mode":"none","hosts":["github.com"]}}`)
		var s Status
		if code != http.StatusOK || json.Unmarshal([]byte(body), &s) != nil || s.Name != "Work laptop" {
			t.Fatalf("the change answered %d %s", code, body)
		}
		file, _, err := LoadPolicy(paths.PolicyFile())
		if err != nil || file.Name != "Work laptop" || len(file.Folders) != 1 || file.Folders[0].Path != "~/other" || file.Folders[0].Write ||
			file.Network.Mode != NetworkNone || len(file.Network.Hosts) != 0 || file.Device != testEnv {
			t.Errorf("the file reads %+v, %v", file, err)
		}
		enroll.mu.Lock()
		renamed := enroll.renamed[testEnv]
		enroll.mu.Unlock()
		if renamed != "Work laptop" {
			t.Errorf("the platform was told the name %q", renamed)
		}
		waitFor(t, "the driver to serve the new folder", func() bool {
			mu.Lock()
			defer mu.Unlock()
			last := built[len(built)-1]
			return len(last) == 1 && last[0].Path == other && !last[0].Write
		})
	})

	t.Run("a remembered trigger is forgotten", func(t *testing.T) {
		if code, body := send(t, c, http.MethodDelete, "/v1/policy/remembered/trg_none", ""); code != http.StatusNotFound || errorCode(body) != "not_remembered" {
			t.Errorf("forgetting what is not remembered answered %d %s", code, body)
		}
		file, _, _ := LoadPolicy(paths.PolicyFile())
		file.Remember("trg_daily", "4", time.Now())
		if err := SavePolicy(paths.PolicyFile(), file); err != nil {
			t.Fatal(err)
		}
		if code, body := send(t, c, http.MethodDelete, "/v1/policy/remembered/trg_daily", ""); code != http.StatusNoContent {
			t.Errorf("forgetting a remembered trigger answered %d %s", code, body)
		}
		if file, _, _ := LoadPolicy(paths.PolicyFile()); len(file.RememberedTriggers) != 0 {
			t.Errorf("the trigger is still remembered: %+v", file.RememberedTriggers)
		}
	})
}

// testToken is an unsigned JWT for the audience, expiring at exp.
func testToken(audience string, exp time.Time) string {
	part := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return part(map[string]any{"alg": "none"}) + "." + part(map[string]any{"sub": "p1", "aud": audience, "exp": exp.Unix()}) + ".sig"
}

func TestClientCredentials(t *testing.T) {
	t.Run("a token is checked, kept, waited for, and needed again before it expires", func(t *testing.T) {
		creds := NewCredentials(nil, 200*time.Millisecond)
		if s := creds.State(); s.Cella != CredentialNeeded || s.Platform != CredentialNeeded {
			t.Errorf("an empty store reads %+v", s)
		}
		if _, err := creds.Token(t.Context(), AudienceCella); !errors.Is(err, ErrCredentialsNeeded) {
			t.Errorf("a token none handed answered %v", err)
		}
		var ce *CredentialsError
		for name, held := range map[string]HeldToken{
			"another audience": {Token: testToken("topos", time.Now().Add(time.Hour))},
			"expiring":         {Token: testToken(AudienceCella, time.Now().Add(30*time.Second))},
			"not a JWT":        {Token: "opaque", ExpiresAt: time.Now().Add(time.Hour)},
		} {
			if err := creds.Put(map[string]HeldToken{AudienceCella: held}); !errors.As(err, &ce) {
				t.Errorf("a token %s was kept: %v", name, err)
			}
		}
		needed := make(chan struct{}, 4)
		creds.notifyNeed(func() { needed <- struct{}{} })
		waited := make(chan string, 1)
		go func() {
			token, _ := creds.Token(context.Background(), AudiencePlatform)
			waited <- token
		}()
		platform := testToken(AudiencePlatform, time.Now().Add(credentialsMargin+time.Second))
		if err := creds.Put(map[string]HeldToken{AudienceCella: {Token: testToken(AudienceCella, time.Now().Add(time.Hour))}, AudiencePlatform: {Token: platform}}); err != nil {
			t.Fatal(err)
		}
		if got := <-waited; got != platform {
			t.Errorf("a waiting call was handed %q", got)
		}
		if s := creds.State(); s.Cella != CredentialValid || s.Platform != CredentialValid {
			t.Errorf("held tokens read %+v", s)
		}
		select {
		case <-needed:
		case <-time.After(5 * time.Second):
			t.Fatal("no client was told the platform token is needed again")
		}
		if s := creds.State(); s.Platform != CredentialNeeded || s.Cella != CredentialValid {
			t.Errorf("a token within a minute of its expiry reads %+v", s)
		}
	})

	t.Run("the socket takes tokens only on an engine that serves with a client's", func(t *testing.T) {
		paths := testPaths(t)
		saved := New(Config{Paths: paths, Keys: OpenKeys(paths.KeyFile()), Account: &fakeAccount{}})
		c := controlOn(t, saved, paths)
		token := testToken(AudienceCella, time.Now().Add(time.Hour))
		if code, body := send(t, c, http.MethodPut, "/v1/credentials", `{"cella":{"token":"`+token+`"}}`); code != http.StatusConflict || errorCode(body) != "credentials_not_taken" {
			t.Errorf("an engine on the saved login answered %d %s", code, body)
		}

		paths2 := testPaths(t)
		creds := NewCredentials(nil, 0)
		client := New(Config{Paths: paths2, Keys: OpenKeys(paths2.KeyFile()), Account: &fakeAccount{}, Credentials: creds})
		c2 := controlOn(t, client, paths2)
		if s := statusOf(t, c2); s.Credentials == nil || s.Credentials.Cella != CredentialNeeded {
			t.Errorf("a client's engine with no token reads %+v", s.Credentials)
		}
		if code, body := send(t, c2, http.MethodPut, "/v1/credentials", `{"cella":{"token":"`+testToken("topos", time.Now().Add(time.Hour))+`"}}`); code != http.StatusBadRequest || errorCode(body) != "invalid_credentials" {
			t.Errorf("a token for another audience answered %d %s", code, body)
		}
		if code, body := send(t, c2, http.MethodPut, "/v1/credentials", `{"cella":{"token":"`+token+`"}}`); code != http.StatusOK || !strings.Contains(body, `"cella":"valid"`) {
			t.Errorf("a cella token answered %d %s", code, body)
		}
		if strings.Contains(statusOf(t, c2).Detail, token) {
			t.Error("the token reached the status")
		}
	})
}

func TestConnectScreenMatchesFacts(t *testing.T) {
	for _, p := range []Policy{
		{Name: "Mac", Folders: []Folder{{Path: "~/a", Write: true}, {Path: "~/b"}}, Network: Network{Mode: NetworkAllowlist, Hosts: []string{"github.com", "pypi.org"}}},
		{Name: "Mac", SessionStart: SessionStartAsk, Folders: []Folder{{Path: "~/a"}}, Network: Network{Mode: NetworkNone}},
	} {
		screen := strings.Join(strings.Fields(ConnectScreen(p)), " ")
		for _, f := range ConnectFacts(p) {
			for _, want := range append([]string{f.Text, f.Note}, f.Items...) {
				if want != "" && !strings.Contains(screen, want) {
					t.Errorf("the screen lacks the %s fact's %q:\n%s", f.Kind, want, screen)
				}
			}
		}
	}
}
