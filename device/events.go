// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"sync"
	"time"
)

// The kinds of event a client of the engine receives.
const (
	EventStatus    = "status"
	EventActivity  = "activity"
	EventAsked     = "asked"
	EventAnswered  = "answered"
	EventWithdrawn = "withdrawn"
	// EventStarted is a session admitted to the computer, asked or not,
	// with the facts its prompt would show, so a client can tell the person.
	EventStarted = "started"
	EventNotice  = "notice"
	// EventNetworkRefused is a connection a session's command was refused
	// by this computer's network or the session's own, with the host and
	// port it asked for, so a client can offer to allow the host for the
	// session (AllowSessionHost) or for every session (SaveHost).
	EventNetworkRefused = "network_refused"
)

// The states of the engine.
const (
	StateStarting = "starting"
	StateOnline   = "online"
	StateOffline  = "offline"
	StatePaused   = "paused"
	// StateNotConnected is `serve` on a computer no device is connected as
	// yet, or whose device was removed: it waits for a client to connect it
	// through the control socket.
	StateNotConnected = "not_connected"
)

// Event is one thing a client of the engine is told: a change of status, a
// line of activity, a create asked, answered or withdrawn, a session
// started, a connection refused, or a notice the person should read.
type Event struct {
	Kind     string          `json:"kind"`
	Status   *Status         `json:"status,omitempty"`
	Activity *Entry          `json:"activity,omitempty"`
	Approval *Approval       `json:"approval,omitempty"`
	Refusal  *NetworkRefusal `json:"refusal,omitempty"`
	// Answer and By are an answered create's: the answer and the client
	// that gave it.
	Answer string `json:"answer,omitempty"`
	By     string `json:"by,omitempty"`
	// Notice is a sentence for the person, such as a warning that the key
	// is kept in a file.
	Notice string `json:"notice,omitempty"`
}

// NetworkRefusal is one connection a command was refused: the host it asked
// for and the port, and the session, agent and sandbox it came from. It
// carries nothing of the request: no path, header or body. The host is the
// one the command asked for, chosen by whatever runs there, so a client
// shows it exactly. A command repeating a refusal is told once a minute.
// Session is empty for a sandbox its plane names no session on; a client
// then allows the host by the sandbox's id.
type NetworkRefusal struct {
	Time    time.Time `json:"time"`
	Session string    `json:"session,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	Sandbox string    `json:"sandbox"`
	Host    string    `json:"host"`
	Port    int       `json:"port"`
}

// Status is what /v1/status answers and the terminal's status line shows.
type Status struct {
	Device string `json:"device"`
	Name   string `json:"name"`
	// State is starting, online, offline, paused or not_connected, and
	// Detail why it is offline.
	State  string `json:"state"`
	Online bool   `json:"online"`
	Detail string `json:"detail,omitempty"`
	// Sessions is how many sessions have a sandbox running on the computer.
	Sessions int `json:"sessions"`
	// Waiting is how many creates wait for the person.
	Waiting int    `json:"waiting"`
	Version string `json:"version"`
	// Upgrade is set when a newer release was found while serving.
	Upgrade string `json:"upgrade,omitempty"`
	// Credentials is set on an engine that takes its tokens from a client
	// (`serve --credentials client`): for each audience, valid or needed.
	Credentials *CredentialsState `json:"credentials,omitempty"`
}

// subscriberBuffer is how many events a client may fall behind by before it
// is dropped; a dropped client sees its stream end and connects again.
const subscriberBuffer = 256

// hub fans events out to every subscriber.
type hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func newHub() *hub { return &hub{subs: map[chan Event]struct{}{}} }

// subscribe answers a channel of every event from now on, and the function
// that ends it.
func (h *hub) subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			if _, ok := h.subs[ch]; ok {
				delete(h.subs, ch)
				close(ch)
			}
			h.mu.Unlock()
		})
	}
}

// publish hands e to every subscriber, dropping one that fell too far
// behind.
func (h *hub) publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}
