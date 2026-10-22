// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package device

import (
	"slices"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// curl is a command that asks its network for host over HTTPS and never
// reaches the internet: the network refuses the host, or the name does not
// resolve.
func curl(host string) []string {
	return []string{"/usr/bin/curl", "-sS", "-o", "/dev/null", "--max-time", "10", "https://" + host + "/"}
}

// refusalsOf collects the refusals every client hears, by host.
func refusalsOf(t *testing.T, e *Engine) func(host string) []NetworkRefusal {
	t.Helper()
	events, stop := e.Subscribe()
	t.Cleanup(stop)
	got := make(chan NetworkRefusal, 64)
	go func() {
		for ev := range events {
			if ev.Kind == EventNetworkRefused {
				got <- *ev.Refusal
			}
		}
	}()
	var seen []NetworkRefusal
	return func(host string) []NetworkRefusal {
		for {
			select {
			case r := <-got:
				seen = append(seen, r)
			default:
				var out []NetworkRefusal
				for _, r := range seen {
					if r.Host == host {
						out = append(out, r)
					}
				}
				return out
			}
		}
	}
}

// TestARefusedHostIsAllowedForOneSession is the whole path under Seatbelt:
// a session's command refused a host is told to every client with its
// session, agent and sandbox; once the host is allowed for that session,
// its next command is no longer refused it, while another session's still
// is. The hosts are under .invalid, so nothing reaches the internet.
func TestARefusedHostIsAllowedForOneSession(t *testing.T) {
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	project := connected(t, paths, keys)
	p := newStubPlane(t)
	e := engineOn(paths, p, &fakeAccount{exists: true}, keys)
	answering(t, e, AnswerAllow)
	refusals := refusalsOf(t, e)
	running(t, e)
	p.waitConnected(t)
	for id, session := range map[string]string{"sbx_a": "ses_a", "sbx_b": "ses_b"} {
		spec := deviceCreate(id, project, sessionLabels(session))
		spec.Egress = runtime.Egress{Mode: string(v1.EgressOpen)}
		if _, err := p.driver.Create(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
	}
	const host = "allowed.invalid"
	if code, _ := run(t, p.driver, "sbx_a", curl(host)...); code == 0 {
		t.Fatal("a host outside the allow list was reached")
	}
	waitFor(t, "the refusal to be told", func() bool { return len(refusals(host)) > 0 })
	if r := refusals(host)[0]; r.Session != "ses_a" || r.Agent != "agt_1" || r.Sandbox != "sbx_a" || r.Port != 443 {
		t.Errorf("the refusal reads %+v", r)
	}
	if err := e.AllowSessionHost(t.Context(), "ses_a", host, 443); err != nil {
		t.Fatal(err)
	}
	if code, _ := run(t, p.driver, "sbx_a", curl(host)...); code == 0 {
		t.Fatal("a name under .invalid resolved")
	}
	if code, _ := run(t, p.driver, "sbx_b", curl(host)...); code == 0 {
		t.Fatal("a name under .invalid resolved")
	}
	waitFor(t, "the other session's refusal to be told", func() bool {
		return slices.ContainsFunc(refusals(host), func(r NetworkRefusal) bool { return r.Session == "ses_b" })
	})
	// A refusal comes before the command's own answer, so one is in by now.
	time.Sleep(500 * time.Millisecond)
	if a := slices.DeleteFunc(refusals(host), func(r NetworkRefusal) bool { return r.Session != "ses_a" }); len(a) != 1 {
		t.Errorf("the allowed session was refused the host again: %+v", a)
	}
}

// TestAnyServesUnderSeatbelt: on macOS a computer whose network is any is
// served, registers the deny list as an open network, and refuses to allow
// a host a wildcard entry blocks, which only narrowing that entry lifts.
func TestAnyServesUnderSeatbelt(t *testing.T) {
	paths := testPaths(t)
	keys := OpenKeys(paths.KeyFile())
	project := connected(t, paths, keys)
	p := newStubPlane(t)
	e := testEngine(Config{Paths: paths, URL: p.url, Keys: keys, Account: &fakeAccount{exists: true}, Version: "test", Enroll: &fakeEnroller{paths: paths}})
	answering(t, e, AnswerAllow)
	running(t, e)
	p.waitConnected(t)
	anyHost := Network{Mode: NetworkAny, Blocked: []string{"pastebin.com", "*.example.net"}}
	if _, err := e.PutPolicy(t.Context(), PolicyChange{Network: &anyHost}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the deny list to be registered", func() bool {
		host, ok := p.live()
		return ok && host != nil && host.Network.Mode == string(v1.EgressOpen) && slices.Equal(host.Network.Blocked, anyHost.Blocked)
	})
	if _, err := p.driver.Create(t.Context(), deviceCreate("sbx_any", project, sessionLabels("ses_any"))); err != nil {
		t.Fatal(err)
	}
	if err := e.AllowSessionHost(t.Context(), "ses_any", "a.example.net", 443); CodeOf(err) != CodeHostNotAllowed {
		t.Errorf("a host a wildcard blocks was allowed: %v", err)
	}
	if err := e.AllowSessionHost(t.Context(), "ses_any", "pastebin.com", 443); err != nil {
		t.Errorf("a host the deny list names was refused: %v", err)
	}
}
