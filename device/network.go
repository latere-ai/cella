// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"context"
	"errors"
	"net"
	"slices"
	"time"

	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/local"
)

// What a session's commands are refused and allowed on the network. A
// command keeps the network it started with: the operating system's sandbox
// fixes it when the command starts, so a host allowed while a command runs
// reaches the session's next command, never the one that was refused.

// ErrNoSession is a host allowed for a session no sandbox of which runs on
// this computer: it ended, was removed, or never ran here.
var ErrNoSession = errors.New("no sandbox of this session runs on this computer")

// sessionOf is the session a sandbox serves, by its plane's session label,
// or the sandbox itself where the plane names no session.
func (e *Engine) sessionOf(sandbox string, labels map[string]string) string {
	if session := labelOf(labels, e.cfg.Labels.Session); session != "" {
		return session
	}
	return sandbox
}

// allowedFor is the local driver's Allowed: what a sandbox's next command
// reaches beyond the host policy the driver was built from, the hosts
// allowed for its session and those saved to the policy since.
func (e *Engine) allowedFor(sandbox string, labels map[string]string) []string {
	e.allowMu.Lock()
	defer e.allowMu.Unlock()
	out := slices.Clone(e.allowed[e.sessionOf(sandbox, labels)])
	for _, entry := range e.saved {
		if !slices.Contains(out, entry) {
			out = append(out, entry)
		}
	}
	return out
}

// refusedHost is the local driver's OnRefused: a connection a command was
// refused becomes a line in the activity log and an event every client
// hears, with the host and port and the session, agent and sandbox it came
// from, and nothing of the request.
func (e *Engine) refusedHost(r local.Refusal) {
	refusal := NetworkRefusal{
		Time: e.cfg.Now().UTC(), Session: labelOf(r.Labels, e.cfg.Labels.Session), Agent: labelOf(r.Labels, e.cfg.Labels.Agent),
		Sandbox: r.Sandbox, Host: r.Host, Port: r.Port,
	}
	e.log(Entry{
		Time: refusal.Time, Type: TypeNetworkRefused, Sandbox: refusal.Sandbox, Session: refusal.Session, Agent: refusal.Agent,
		Host: refusal.Host, Port: refusal.Port,
	})
	e.hub.publish(Event{Kind: EventNetworkRefused, Refusal: &refusal})
}

// AllowSessionHost lets one session's later commands on this computer reach
// host on port, as a NetworkRefusal names them, without changing the host
// policy: session is the refusal's Session, or its Sandbox where it names
// none. Under an allowlist the host joins the list for the session; under
// any it lifts this computer's own block of the host by name, and a port
// beyond 443 and 80 is admitted. The allowance holds while the engine
// serves and a sandbox of the session runs here, and is forgotten when the
// session ends or is removed here, or the engine stops, pauses or reads its
// policy again. A command that runs keeps the network it started with: the
// host reaches the session's next command. It is refused with ErrNotRunning
// on an engine that is not serving, ErrNoSession for a session no sandbox
// of which runs here, and CodeHostNotAllowed for what is not a host or a
// port, a network of none, and a host a wildcard entry blocks. A session's
// own network, which its plane asked for, still applies: the allowance
// widens what this computer allows that session.
func (e *Engine) AllowSessionHost(ctx context.Context, session, host string, port int) error {
	entry, err := local.HostEntry(host, port)
	if err != nil {
		return Fail(CodeHostNotAllowed, err.Error(), err)
	}
	e.mu.Lock()
	d, policy := e.driver, e.policy
	e.mu.Unlock()
	if d == nil {
		return ErrNotRunning
	}
	if _, err = asLocal(policy.Network).Allow(entry); err != nil {
		return Fail(CodeHostNotAllowed, err.Error(), err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	states, err := d.List(ctx, runtime.Filter{})
	if err != nil {
		return Fail(CodeServeFailed, "list the sandboxes", err)
	}
	var sandbox, agent string
	for _, s := range states {
		if s.Phase == runtime.Running && e.sessionOf(s.ID, s.Labels) == session {
			sandbox, agent = s.ID, labelOf(s.Labels, e.cfg.Labels.Agent)
			break
		}
	}
	if sandbox == "" {
		return ErrNoSession
	}
	e.allowMu.Lock()
	if e.allowed == nil {
		e.allowed = map[string][]string{}
	}
	if !slices.Contains(e.allowed[session], entry) {
		e.allowed[session] = append(e.allowed[session], entry)
	}
	e.allowMu.Unlock()
	named := ""
	if session != sandbox {
		named = session
	}
	e.log(Entry{Time: e.cfg.Now().UTC(), Type: TypeHostAllowed, Sandbox: sandbox, Session: named, Agent: agent, Host: hostOf(entry), Port: port})
	return nil
}

// SaveHost writes host on port into the host policy, so every session
// reaches it: under an allowlist it joins network.hosts; under any it lifts
// the policy's own block of the host by name, and a port beyond 443 and 80
// joins network.hosts. The running sessions' next commands reach it at
// once, and the engine reads it from the file at its next start; nothing
// restarts, so no running session stops. A command that runs keeps the
// network it started with. It is refused with CodeNotConnected on a
// computer not connected, and CodeHostNotAllowed for what is not a host or
// a port, a network of none, and a host a wildcard entry blocks.
func (e *Engine) SaveHost(host string, port int) error {
	entry, err := local.HostEntry(host, port)
	if err != nil {
		return Fail(CodeHostNotAllowed, err.Error(), err)
	}
	// allowMu is taken first, as serveOnce takes it, so a driver built
	// meanwhile reads either the policy before the change and the saved
	// entry after it, or the policy with the change.
	e.allowMu.Lock()
	defer e.allowMu.Unlock()
	e.policyMu.Lock()
	p, err := e.connectedPolicy()
	if err == nil {
		var n local.Network
		if n, err = asLocal(p.Network).Allow(entry); err != nil {
			err = Fail(CodeHostNotAllowed, err.Error(), err)
		} else {
			p.Network.Hosts, p.Network.Blocked = n.Hosts, n.Blocked
			err = e.refusePolicy(p)
		}
	}
	if err == nil {
		if serr := SavePolicy(e.cfg.Paths.PolicyFile(), p); serr != nil {
			err = Fail(CodePolicyInvalid, "write the host policy file", serr)
		}
	}
	e.policyMu.Unlock()
	if err != nil {
		return err
	}
	e.mu.Lock()
	serving := e.running && e.policy.Device == p.Device
	if serving {
		e.policy.Network = p.Network
	}
	e.mu.Unlock()
	if serving && !slices.Contains(e.saved, entry) {
		e.saved = append(e.saved, entry)
	}
	e.log(Entry{Time: e.cfg.Now().UTC(), Type: TypeHostSaved, Host: hostOf(entry), Port: port})
	return nil
}

// hostOf is the host an entry names, without its port.
func hostOf(entry string) string {
	if host, _, err := net.SplitHostPort(entry); err == nil {
		return host
	}
	return entry
}

// asLocal is a host policy's network as the local driver reads it.
func asLocal(n Network) local.Network {
	return local.Network{Mode: n.Mode, Hosts: slices.Clone(n.Hosts), Blocked: slices.Clone(n.Blocked)}
}
