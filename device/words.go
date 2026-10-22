// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"fmt"
	"strings"
)

// The sentences that tell the person how sessions start here, and how to
// change it, at every start and on the connect screen.
const (
	sessionStartAllowWords = "Sessions you start, and your triggers that run on a schedule, start on\n" +
		"this computer without asking here. A trigger that runs on mail or a\n" +
		"webhook asks here first, since anyone with its address can send it input.\n" +
		"Each command that changes something is still approved in the chat,\n" +
		"unless you approved a matching one before."
	sessionStartAskWords = "Each session asks for your approval here before it starts. Each command\n" +
		"that changes something is also approved in the chat, unless you approved\n" +
		"a matching one before."
	switchToAsk   = "To be asked here before every session starts, run\n`latere devices policy --session-start ask`."
	switchToAllow = "To let your own sessions and scheduled triggers start without asking\nhere, run `latere devices policy --session-start allow`."
	// anyWords say what the network any means: everything a session can
	// read here can leave for any public host.
	anyWords = "Everything readable in a granted folder can be sent to any public host."
)

// NetworkNotice is what the engine says at every start on a computer whose
// network is any, and "" for any other: what it reaches, and that
// everything readable in a granted folder can be sent to any public host.
func NetworkNotice(p Policy) string {
	if p.Network.Mode != NetworkAny {
		return ""
	}
	return "This computer's network is any: commands reach any public host but this computer, its networks and the hosts " +
		"network.blocked names. " + anyWords + " To narrow it, set network.mode to allowlist in the host policy file."
}

// SessionStartNotice is what the engine says at every start about how
// sessions start, so a computer connected before session_start existed
// learns the default it now has.
func SessionStartNotice(p Policy) string {
	if p.SessionStart == SessionStartAsk {
		return strings.ReplaceAll(sessionStartAskWords+" "+switchToAllow, "\n", " ")
	}
	return strings.ReplaceAll(sessionStartAllowWords+" "+switchToAsk, "\n", " ")
}

// ConnectScreen is what the person is shown before a computer is
// connected: what sessions may use, what the agent reads, and who may ask.
// GET /v1/connect answers the same facts.
func ConnectScreen(p Policy) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Connect this computer to Latere as %q?\n\n", p.Name)
	for _, f := range connectFacts(p) {
		b.WriteString(f.screen())
		// The folders run into what is readable in them; every other fact
		// ends with a blank line.
		if f.Kind != FactFolders {
			b.WriteString("\n")
		}
	}
	b.WriteString("Type yes to connect this computer: ")
	return b.String()
}
