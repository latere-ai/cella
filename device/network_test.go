// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/hostsandbox"

	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/local"
)

// underSRT builds the local driver under srt whatever this machine runs, as
// a Linux computer, or a program that does not call hostsandbox.Main, does.
func underSRT(o local.Options) (runtime.Driver, error) {
	o.Isolation = hostsandbox.IsolationSRT
	return local.New(o)
}

// TestAnyIsRefusedUnderSRT: where the sandbox cannot enforce a network of
// any host, a connect, a change and a start that name it are refused with
// CodeNetworkUnenforceable, and the policy file is left as it was: the
// network is never run as something wider or narrower.
func TestAnyIsRefusedUnderSRT(t *testing.T) {
	anyHost := Network{Mode: NetworkAny, Blocked: []string{"pastebin.com"}}
	t.Run("a connect", func(t *testing.T) {
		paths := testPaths(t)
		project := projectIn(t, paths)
		e := testEngine(Config{Paths: paths, Keys: OpenKeys(paths.KeyFile()), Account: &fakeAccount{}, Enroll: &fakeEnroller{paths: paths}, NewDriver: underSRT})
		terms := Terms{Name: "Desk", SessionStart: SessionStartAllow, Folders: []Folder{{Path: project, Write: true}}, Network: anyHost}
		_, err := e.Connect(t.Context(), terms, "unchecked")
		if CodeOf(err) != CodeNetworkUnenforceable || !errors.Is(err, hostsandbox.ErrUnenforceableNetwork) {
			t.Errorf("Connect = %v, want %s", err, CodeNetworkUnenforceable)
		}
	})
	t.Run("a change", func(t *testing.T) {
		paths := testPaths(t)
		keys := OpenKeys(paths.KeyFile())
		connected(t, paths, keys)
		before, err := os.ReadFile(paths.PolicyFile())
		if err != nil {
			t.Fatal(err)
		}
		e := testEngine(Config{Paths: paths, Keys: keys, Account: &fakeAccount{exists: true}, Enroll: &fakeEnroller{paths: paths}, NewDriver: underSRT})
		if _, err := e.PutPolicy(t.Context(), PolicyChange{Network: &anyHost}); CodeOf(err) != CodeNetworkUnenforceable {
			t.Errorf("PutPolicy = %v, want %s", err, CodeNetworkUnenforceable)
		}
		if after, _ := os.ReadFile(paths.PolicyFile()); !bytes.Equal(before, after) {
			t.Errorf("a refused change rewrote the policy file:\n%s", after)
		}
	})
	t.Run("a start", func(t *testing.T) {
		paths := testPaths(t)
		keys := OpenKeys(paths.KeyFile())
		connected(t, paths, keys)
		p, _, err := LoadPolicy(paths.PolicyFile())
		if err != nil {
			t.Fatal(err)
		}
		p.Network = anyHost
		if err = SavePolicy(paths.PolicyFile(), p); err != nil {
			t.Fatal(err)
		}
		e := testEngine(Config{Paths: paths, Keys: keys, Account: &fakeAccount{exists: true}, NewDriver: underSRT})
		if err := e.Run(t.Context()); CodeOf(err) != CodeNetworkUnenforceable {
			t.Errorf("Run = %v, want %s", err, CodeNetworkUnenforceable)
		}
	})
}

// projectIn makes the project folder under the test's home and answers it.
func projectIn(t *testing.T, paths Paths) string {
	t.Helper()
	project := paths.Home + "/project"
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	return project
}

// TestAnyIsNeverProposed: the defaults are no network of any host; a query
// asking for one is refused, at the function and through the engine; and a
// file that had one proposes the allow list of DefaultHosts in its place.
func TestAnyIsNeverProposed(t *testing.T) {
	named := func() string { return "Desk" }
	if p, err := ProposePolicy(Policy{}, ConnectQuery{}, "/h", "/h/site", named); err != nil || p.Network.Mode != NetworkAllowlist {
		t.Errorf("the default network is %+v, %v", p.Network, err)
	}
	if _, err := ProposePolicy(Policy{}, ConnectQuery{Network: NetworkAny}, "/h", "/h/site", named); err == nil || !strings.Contains(err.Error(), "never proposed") {
		t.Errorf("a query for any was proposed: %v", err)
	}
	previous := Policy{Name: "Desk", Folders: []Folder{{Path: "~/site", Write: true}},
		Network: Network{Mode: NetworkAny, Hosts: []string{"github.com:22"}, Blocked: []string{"pastebin.com"}}}
	p, err := ProposePolicy(previous, ConnectQuery{}, "/h", "/h/site", named)
	if err != nil || p.Network.Mode != NetworkAllowlist || !slices.Equal(p.Network.Hosts, DefaultHosts) || p.Network.Blocked != nil {
		t.Errorf("a file that had any proposes %+v, %v", p.Network, err)
	}
	if p, err = ProposePolicy(previous, ConnectQuery{Network: NetworkNone}, "/h", "/h/site", named); err != nil || p.Network.Mode != NetworkNone || p.Network.Blocked != nil {
		t.Errorf("a file that had any, asked for none, proposes %+v, %v", p.Network, err)
	}

	paths := testPaths(t)
	e := testEngine(Config{Paths: paths, Keys: OpenKeys(paths.KeyFile()), Account: &fakeAccount{},
		Enroll: NewEnrollment(EnrollmentOptions{Paths: paths, Name: named})})
	if _, err := e.ProposeConnect(ConnectQuery{Network: NetworkAny, Folders: []string{"~/site"}}); CodeOf(err) != CodePolicyInvalid {
		t.Errorf("ProposeConnect for any = %v, want %s", err, CodePolicyInvalid)
	}
}

// TestAnyIsCheckedAsTheAllowListIs: a blocked list on another network, a
// host under any that names no port, and a blocked entry the sandbox would
// not read are each refused, and a policy of any with neither is not.
func TestAnyIsCheckedAsTheAllowListIs(t *testing.T) {
	base := Policy{Folders: []Folder{{Path: "~/site"}}}
	for _, c := range []struct {
		network Network
		want    string
	}{
		{Network{Mode: NetworkAllowlist, Hosts: []string{"github.com"}, Blocked: []string{"pastebin.com"}}, "network.blocked is the deny list of network.mode any"},
		{Network{Mode: NetworkAny, Hosts: []string{"github.com"}}, `"github.com" names none`},
		{Network{Mode: "open"}, "it is allowlist, none or any"},
	} {
		p := base
		p.Network = c.network
		if problems := p.Problems("/h"); len(problems) != 1 || !strings.Contains(problems[0], c.want) {
			t.Errorf("%+v has the problems %v, want one saying %q", c.network, problems, c.want)
		}
	}
	p := base
	p.Network = Network{Mode: NetworkAny, Hosts: []string{"github.com:22"}, Blocked: []string{"pastebin.com", "*.example.net"}}
	if problems := p.Problems("/h"); len(problems) != 0 {
		t.Errorf("a policy of any has the problems %v", problems)
	}

	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	connected(t, paths, keys)
	e := testEngine(Config{Paths: paths, Keys: keys, Account: &fakeAccount{exists: true}, Enroll: &fakeEnroller{paths: paths}})
	bad := Network{Mode: NetworkAny, Blocked: []string{"2130706433"}}
	if _, err := e.PutPolicy(t.Context(), PolicyChange{Network: &bad}); CodeOf(err) != CodePolicyInvalid || !strings.Contains(err.Error(), "2130706433") {
		t.Errorf("a blocked entry the sandbox would not read was taken: %v", err)
	}
}

// TestAnyIsStatedPlainly: the connect screen and the facts a client shows
// say what any reaches and that everything readable in a granted folder
// can be sent to any public host, with the hosts it blocks and the ports it
// adds; the engine says the same at each start.
func TestAnyIsStatedPlainly(t *testing.T) {
	p := Policy{Name: "Desk", Folders: []Folder{{Path: "~/site", Write: true}},
		Network: Network{Mode: NetworkAny, Hosts: []string{"github.com:22"}, Blocked: []string{"pastebin.com", "*.example.net"}}}
	screen := ConnectScreen(p)
	for _, want := range []string{
		"Network: any.", "any public host", "not this computer, the networks\nit is on, or these hosts:", "pastebin.com, *.example.net",
		"Everything readable in a granted folder can be sent to any public host.", "github.com:22",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("the connect screen lacks %q:\n%s", want, screen)
		}
	}
	flat := strings.Join(strings.Fields(screen), " ")
	kinds := []string{}
	for _, f := range ConnectFacts(p) {
		kinds = append(kinds, f.Kind)
		for _, want := range append([]string{f.Text, f.Note}, f.Items...) {
			if want != "" && !strings.Contains(flat, want) {
				t.Errorf("the screen lacks the %s fact's %q", f.Kind, want)
			}
		}
	}
	if !slices.Contains(kinds, FactNetworkPorts) {
		t.Errorf("the facts %v name no ports of any", kinds)
	}
	unblocked := ConnectScreen(Policy{Name: "Desk", Network: Network{Mode: NetworkAny}})
	if !strings.Contains(unblocked, "not this computer or the\nnetworks it is on.") || !strings.Contains(unblocked, "can be sent to any public host") {
		t.Errorf("any with nothing blocked reads:\n%s", unblocked)
	}
	if notice := NetworkNotice(p); !strings.Contains(notice, "any public host") || !strings.Contains(notice, "can be sent to any public host") {
		t.Errorf("the notice of any reads %q", notice)
	}
	if notice := NetworkNotice(Policy{Network: Network{Mode: NetworkAllowlist}}); notice != "" {
		t.Errorf("an allow list has the notice %q", notice)
	}
	// A consent covers the deny list: a client that showed one list cannot
	// connect with another.
	other := p
	other.Network.Blocked = []string{"pastebin.com"}
	if Consent(TermsOf(p), ConnectFacts(p)) == Consent(TermsOf(other), ConnectFacts(other)) {
		t.Error("two deny lists have one consent")
	}
}

// TestARefusalIsAnEvent: a connection a command was refused reaches every
// subscriber as a network_refused event and the activity log as a line,
// each with the host, the port and the session, agent and sandbox it came
// from, and nothing of the request.
func TestARefusalIsAnEvent(t *testing.T) {
	paths := testPaths(t)
	e := testEngine(Config{Paths: paths, Keys: OpenKeys(paths.KeyFile())})
	activity, err := OpenActivity(paths.ActivityDir(testEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = activity.Close() })
	e.activity = activity
	events, stop := e.Subscribe()
	defer stop()
	labels := sessionLabels("ses_1")
	labels[testLabels.Trigger] = "trg_1"
	e.refusedHost(local.Refusal{Sandbox: "sbx_1", Labels: labels, Host: "pastebin.com", Port: 443})

	var line, refused *Event
	for line == nil || refused == nil {
		select {
		case ev := <-events:
			switch ev.Kind {
			case EventActivity:
				line = &ev
			case EventNetworkRefused:
				refused = &ev
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the refusal was not told")
		}
	}
	r := refused.Refusal
	if r == nil || r.Host != "pastebin.com" || r.Port != 443 || r.Session != "ses_1" || r.Agent != "agt_1" || r.Sandbox != "sbx_1" || r.Time.IsZero() {
		t.Fatalf("the event carries %+v", r)
	}
	raw, err := json.Marshal(refused)
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		Refusal map[string]any `json:"refusal"`
	}
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for k := range fields.Refusal {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"agent", "host", "port", "sandbox", "session", "time"}; !slices.Equal(keys, want) {
		t.Errorf("the event carries %v, want %v alone", keys, want)
	}
	entry := line.Activity
	if entry.Type != TypeNetworkRefused || entry.Host != "pastebin.com" || entry.Port != 443 || entry.Session != "ses_1" ||
		entry.Agent != "agt_1" || entry.Sandbox != "sbx_1" || entry.Command != nil || entry.Path != "" || entry.Trigger != "" {
		t.Errorf("the activity line is %+v", entry)
	}
	if got := entry.Line(paths.Home); !strings.HasSuffix(got, "ses_1  network_refused  pastebin.com:443") {
		t.Errorf("the line reads %q", got)
	}
	written, err := ReadActivity(paths.ActivityDir(testEnv), time.Time{})
	if err != nil || len(written) != 1 || written[0].Type != TypeNetworkRefused || written[0].Host != "pastebin.com" {
		t.Errorf("the activity log holds %+v, %v", written, err)
	}
}

// TestAHostIsAllowedForOneSession: a host allowed for one session reaches
// that session's next command and no other session's; the host policy file
// is not touched; the allowance is forgotten once the session's sandbox
// stops; and a session that runs nothing here is refused.
func TestAHostIsAllowedForOneSession(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	project := connected(t, paths, keys)
	before, err := os.ReadFile(paths.PolicyFile())
	if err != nil {
		t.Fatal(err)
	}
	p := newStubPlane(t)
	e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
	answering(t, e, AnswerAllow)
	running(t, e)
	p.waitConnected(t)
	if err := e.AllowSessionHost(t.Context(), "ses_a", "example.com", 443); !errors.Is(err, ErrNoSession) {
		t.Errorf("a session that runs nothing here was allowed a host: %v", err)
	}
	labelsA, labelsB := sessionLabels("ses_a"), sessionLabels("ses_b")
	for id, labels := range map[string]map[string]string{"sbx_a": labelsA, "sbx_b": labelsB} {
		if _, err := p.driver.Create(t.Context(), deviceCreate(id, project, labels)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.AllowSessionHost(t.Context(), "ses_a", "Example.com", 443); err != nil {
		t.Fatal(err)
	}
	if err := e.AllowSessionHost(t.Context(), "ses_a", "example.com", 8443); err != nil {
		t.Fatal(err)
	}
	if got := e.allowedFor("sbx_a", labelsA); !slices.Equal(got, []string{"example.com", "example.com:8443"}) {
		t.Errorf("the allowed session's next command reaches %v beyond the policy", got)
	}
	if got := e.allowedFor("sbx_b", labelsB); len(got) != 0 {
		t.Errorf("another session's next command reaches %v beyond the policy", got)
	}
	if after, _ := os.ReadFile(paths.PolicyFile()); !bytes.Equal(before, after) {
		t.Errorf("allowing a host for one session rewrote the policy file:\n%s", after)
	}
	if err := e.AllowSessionHost(t.Context(), "ses_a", "exa mple.com", 443); CodeOf(err) != CodeHostNotAllowed {
		t.Errorf("a host that is not one was allowed: %v", err)
	}
	if err := p.driver.Stop(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the ended session's allowance to be forgotten", func() bool { return len(e.allowedFor("sbx_a", labelsA)) == 0 })
	if err := e.AllowSessionHost(t.Context(), "ses_a", "example.com", 443); !errors.Is(err, ErrNoSession) {
		t.Errorf("a session whose sandbox stopped was allowed a host: %v", err)
	}
	if entries, err := ReadActivity(paths.ActivityDir(testEnv), time.Time{}); err == nil {
		allowed := slices.IndexFunc(entries, func(e Entry) bool {
			return e.Type == TypeHostAllowed && e.Session == "ses_a" && e.Sandbox == "sbx_a" && e.Host == "example.com" && e.Port == 443
		})
		if allowed < 0 {
			t.Errorf("the activity log does not record the allowance: %+v", entries)
		}
	} else {
		t.Error(err)
	}
}

// TestAHostIsSavedForEverySession: a host saved to the policy is written to
// the file, reaches every session's next command at once, stops no running
// session, and is read from the file by the next driver; on a network of
// none it is refused and the file is left as it was.
func TestAHostIsSavedForEverySession(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	project := connected(t, paths, keys)
	p := newStubPlane(t)
	e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
	answering(t, e, AnswerAllow)
	running(t, e)
	p.waitConnected(t)
	labels := sessionLabels("ses_s")
	if _, err := p.driver.Create(t.Context(), deviceCreate("sbx_s", project, labels)); err != nil {
		t.Fatal(err)
	}
	if err := e.SaveHost("pypi.org", 443); err != nil {
		t.Fatal(err)
	}
	saved, _, err := LoadPolicy(paths.PolicyFile())
	if err != nil || !slices.Equal(saved.Network.Hosts, []string{"github.com", "pypi.org"}) {
		t.Fatalf("the policy file holds %+v, %v", saved.Network, err)
	}
	if got := e.allowedFor("sbx_s", labels); !slices.Equal(got, []string{"pypi.org"}) {
		t.Errorf("a running session's next command reaches %v beyond the driver's policy", got)
	}
	if got := e.allowedFor("sbx_other", sessionLabels("ses_other")); !slices.Equal(got, []string{"pypi.org"}) {
		t.Errorf("another session's next command reaches %v beyond the driver's policy", got)
	}
	if state, err := p.driver.Inspect(t.Context(), "sbx_s"); err != nil || state.Phase != runtime.Running {
		t.Errorf("saving a host left the session %+v, %v", state, err)
	}
	if err := e.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(); err != nil {
		t.Fatal(err)
	}
	p.waitConnected(t)
	waitFor(t, "the next driver to read the saved host", func() bool {
		host, ok := p.live()
		return ok && host != nil && slices.Contains(host.Network.Hosts, "pypi.org")
	})
	if got := e.allowedFor("sbx_s", labels); len(got) != 0 {
		t.Errorf("the next driver still widens by %v what its policy holds", got)
	}

	none := Network{Mode: NetworkNone}
	if _, err := e.PutPolicy(t.Context(), PolicyChange{Network: &none}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paths.PolicyFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SaveHost("pastebin.com", 443); CodeOf(err) != CodeHostNotAllowed {
		t.Errorf("a host was saved to a network of none: %v", err)
	}
	if after, _ := os.ReadFile(paths.PolicyFile()); !bytes.Equal(before, after) {
		t.Errorf("a refused save rewrote the policy file:\n%s", after)
	}
}

// TestAllowancesThroughTheSocket: a client allows a host for a session and
// saves one to the policy through the control socket, hears each refusal
// on the approvals stream, and is told a session that runs nothing here.
func TestAllowancesThroughTheSocket(t *testing.T) {
	sandboxRuntime(t)
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	project := connected(t, paths, keys)
	p := newStubPlane(t)
	e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
	c := controlOn(t, e, paths)
	answering(t, e, AnswerAllow)
	running(t, e)
	p.waitConnected(t)
	labels := sessionLabels("ses_k")
	if _, err := p.driver.Create(t.Context(), deviceCreate("sbx_k", project, labels)); err != nil {
		t.Fatal(err)
	}
	approvals := follow(t, c, "/v1/approvals?follow=1")
	e.refusedHost(local.Refusal{Sandbox: "sbx_k", Labels: labels, Host: "pastebin.com", Port: 443})
	if refusal, _ := next(t, approvals, EventNetworkRefused)["refusal"].(map[string]any); refusal["host"] != "pastebin.com" || refusal["session"] != "ses_k" {
		t.Errorf("the stream told the refusal %v", refusal)
	}
	if code, body := post(t, c, "/v1/sessions/ses_k/hosts", `{"host": "pastebin.com", "port": 443}`); code != http.StatusNoContent {
		t.Errorf("allowing a host for the session answered %d %s", code, body)
	}
	if got := e.allowedFor("sbx_k", labels); !slices.Equal(got, []string{"pastebin.com"}) {
		t.Errorf("the session's next command reaches %v", got)
	}
	if code, body := post(t, c, "/v1/sessions/ses_gone/hosts", `{"host": "pastebin.com", "port": 443}`); code != http.StatusNotFound || !strings.Contains(body, "no_session") {
		t.Errorf("a session that runs nothing here answered %d %s", code, body)
	}
	if code, body := post(t, c, "/v1/policy/hosts", `{"host": "pypi.org", "port": 443}`); code != http.StatusNoContent {
		t.Errorf("saving a host answered %d %s", code, body)
	}
	if saved, _, err := LoadPolicy(paths.PolicyFile()); err != nil || !slices.Contains(saved.Network.Hosts, "pypi.org") {
		t.Errorf("the policy file holds %+v, %v", saved.Network, err)
	}
	if code, body := post(t, c, "/v1/policy/hosts", `{"host": "pypi.org", "port": 0}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, CodeHostNotAllowed) {
		t.Errorf("saving a port that is not one answered %d %s", code, body)
	}
}
