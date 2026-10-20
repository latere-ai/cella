// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"strings"
	"testing"
)

func TestConnectScreenWording(t *testing.T) {
	policy := Policy{
		Name: "MacBook Pro", Folders: []Folder{{Path: "~/code/site", Write: true}, {Path: "~/notes"}},
		Network: Network{Mode: NetworkAllowlist, Hosts: []string{"github.com", "pypi.org"}},
	}
	common := []string{
		`"MacBook Pro"`, "~/code/site (read-write)", "~/notes (read-only)", ".env", "github.com, pypi.org",
		"can carry data out", "session's log and to\nthe model", "organization can\nread it too", "Type yes",
		"still approved in the chat,\nunless you approved a matching one before",
	}
	// A computer connected with no session_start, the default, starts the
	// person's own sessions without asking, and says who else that reaches.
	allow := ConnectScreen(policy)
	for _, want := range append(common,
		"Sessions you start, and your triggers that run on a schedule, start on\nthis computer without asking here.",
		"mail or a\nwebhook asks here first", "Anyone who can start sessions as you can reach these folders",
		"never your credentials", "latere devices policy --session-start ask",
	) {
		if !strings.Contains(allow, want) {
			t.Errorf("the connect screen lacks %q:\n%s", want, allow)
		}
	}
	policy.SessionStart = SessionStartAsk
	ask := ConnectScreen(policy)
	for _, want := range append(common[:len(common)-1],
		"Each session asks for your approval here before it starts.", "also approved in the chat",
		"latere devices policy --session-start allow",
	) {
		if !strings.Contains(ask, want) {
			t.Errorf("the connect screen under ask lacks %q:\n%s", want, ask)
		}
	}
	if strings.Contains(ask, "without asking here.") {
		t.Errorf("the connect screen under ask says sessions start without asking:\n%s", ask)
	}
	if none := ConnectScreen(Policy{Network: Network{Mode: NetworkNone}}); !strings.Contains(none, "Network: none") {
		t.Errorf("a computer with no network reads:\n%s", none)
	}
}

func TestSessionStartNotice(t *testing.T) {
	if allow := SessionStartNotice(Policy{}); !strings.Contains(allow, "start on this computer without asking here") ||
		!strings.Contains(allow, "--session-start ask") || strings.Contains(allow, "\n") {
		t.Errorf("the notice for a file with no session_start reads %q", allow)
	}
	if ask := SessionStartNotice(Policy{SessionStart: SessionStartAsk}); !strings.Contains(ask, "Each session asks") ||
		!strings.Contains(ask, "--session-start allow") {
		t.Errorf("the notice under ask reads %q", ask)
	}
}
