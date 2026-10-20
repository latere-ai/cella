// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package device

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/worker"
)

// The answers a person gives a waiting create.
const (
	AnswerAllow    = "allow"
	AnswerDeny     = "deny"
	AnswerRemember = "remember"
	// answerRemembered, answerSessionStart and answerNoOne are what the
	// activity log records for a create nobody was asked about: a
	// remembered trigger, a session session_start allow starts, and one
	// refused because no one could answer.
	answerRemembered   = "remembered"
	answerSessionStart = "session_start_allow"
	answerNoOne        = "no_one_here"
)

// The ways a trigger fires, as the trigger-fires-on label names them.
const (
	FiresOnSchedule = "schedule"
	FiresOnEvents   = "events"
)

// The reasons a refused create carries back to the session, which the
// person who started it reads.
const (
	ReasonNoOneHere = "no one is here to approve on this computer"
	ReasonRefused   = "the owner of this computer refused this session"
)

// Approval is one create waiting for the person, with the facts the prompt
// shows. The facts come from the create's labels, which Topos sets, and a
// compromised platform could forge them: the approval protects the moment,
// since the person knows whether they just started something.
type Approval struct {
	ID string `json:"id"`
	// Sandbox is the sandbox the create would start.
	Sandbox string `json:"sandbox"`
	// Agent is the agent's display name, or its ref where the create
	// carries no name; AgentRef and AgentVersion name it exactly.
	Agent        string `json:"agent"`
	AgentRef     string `json:"agent_ref,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
	Session      string `json:"session,omitempty"`
	// Trigger is the trigger that started the session, empty for a session
	// a person started, and FiresOn how it fires.
	Trigger string `json:"trigger,omitempty"`
	FiresOn string `json:"fires_on,omitempty"`
	// Folder is the granted folder the session works in, and Write whether
	// the computer grants it read-write.
	Folder string `json:"folder,omitempty"`
	Write  bool   `json:"write"`
	// Rememberable is whether the person may answer remember: a trigger
	// that fires on a schedule, with the agent version to bind it to.
	Rememberable bool      `json:"rememberable"`
	Asked        time.Time `json:"asked"`
}

// StartedWords is who started the session, as a line that says a session
// started words it: "started by a person", or by which trigger.
func (a Approval) StartedWords() string {
	if a.Trigger == "" {
		return "started by a person"
	}
	switch a.FiresOn {
	case FiresOnSchedule:
		return "started by trigger " + a.Trigger + " on a schedule"
	case FiresOnEvents:
		return "started by trigger " + a.Trigger + " on delivered events"
	}
	return "started by trigger " + a.Trigger
}

// StartedBy is who started the session, as the prompt words it.
func (a Approval) StartedBy() string {
	if a.Trigger == "" {
		return "by a person"
	}
	switch a.FiresOn {
	case FiresOnSchedule:
		return "by trigger " + a.Trigger + " (on a schedule)"
	case FiresOnEvents:
		return "by trigger " + a.Trigger + " (on delivered events)"
	}
	return "by trigger " + a.Trigger
}

// approvalOf reads the facts of a create. host is what the driver grants,
// which decides whether the folder is read-write.
func approvalOf(c worker.Create, keys SessionLabels, host v1.HostInfo, now time.Time) Approval {
	labels := c.Labels
	if labels == nil {
		labels = c.Sandbox.Metadata.Labels
	}
	a := Approval{
		Sandbox:      c.Sandbox.Status.ID,
		AgentRef:     labelOf(labels, keys.Agent),
		AgentVersion: labelOf(labels, keys.AgentVersion),
		Session:      labelOf(labels, keys.Session),
		Trigger:      labelOf(labels, keys.Trigger),
		FiresOn:      labelOf(labels, keys.TriggerFiresOn),
		Folder:       c.Sandbox.Spec.Workspace.Host,
		Asked:        now.UTC(),
	}
	a.Agent = strings.TrimSpace(labelOf(c.Sandbox.Metadata.Annotations, keys.AgentName))
	if a.Agent == "" {
		a.Agent = a.AgentRef
	}
	if a.Folder != "" {
		if grant, ok := host.Grant(filepath.Clean(a.Folder)); ok {
			a.Write = grant.Write
		}
	}
	a.Rememberable = a.Trigger != "" && a.FiresOn == FiresOnSchedule && a.AgentVersion != ""
	return a
}

// The broker's refusals of an answer.
var (
	ErrNotPending      = errors.New("no create waits under this id")
	ErrNotRememberable = errors.New("only a trigger that fires on a schedule can be remembered")
	ErrBadAnswer       = errors.New("an answer is allow, deny or remember")
)

// broker holds the creates waiting for the person. Every attached client
// sees every create asked and every answer given, and the first answer
// wins.
type broker struct {
	emit func(Event)

	mu       sync.Mutex
	pending  map[string]*waiting
	order    []string
	attached int
}

type waiting struct {
	approval Approval
	answer   chan given
}

// given is an answer and the client that gave it.
type given struct{ answer, by string }

func newBroker(emit func(Event)) *broker {
	return &broker{emit: emit, pending: map[string]*waiting{}}
}

// attach counts a client that can answer, until the returned detach. When
// the last one leaves, every create still waiting is refused: nobody is left
// to answer it.
func (b *broker) attach() (detach func()) {
	b.mu.Lock()
	b.attached++
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.attached--
			last := b.attached == 0
			var orphans []*waiting
			if last {
				for _, id := range b.order {
					orphans = append(orphans, b.pending[id])
				}
				b.pending, b.order = map[string]*waiting{}, nil
			}
			b.mu.Unlock()
			for _, w := range orphans {
				b.emit(Event{Kind: EventAnswered, Approval: &w.approval, Answer: answerNoOne})
				w.answer <- given{answer: answerNoOne}
			}
		})
	}
}

// present reports whether any client can answer.
func (b *broker) present() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attached > 0
}

// ask waits for an answer to a, and the client that gave it, or for ctx to
// end, which withdraws it. With no client attached it answers at once that
// no one is here.
func (b *broker) ask(ctx context.Context, a Approval) (given, error) {
	a.ID = "apv_" + strings.ToLower(rand.Text()[:16])
	w := &waiting{approval: a, answer: make(chan given, 1)}
	b.mu.Lock()
	if b.attached == 0 {
		b.mu.Unlock()
		return given{answer: answerNoOne}, nil
	}
	b.pending[a.ID] = w
	b.order = append(b.order, a.ID)
	b.mu.Unlock()
	b.emit(Event{Kind: EventAsked, Approval: &a})
	select {
	case answer := <-w.answer:
		return answer, nil
	case <-ctx.Done():
		if b.drop(a.ID) {
			b.emit(Event{Kind: EventWithdrawn, Approval: &a})
		}
		return given{}, ctx.Err()
	}
}

// drop removes a waiting create, and reports whether it was still waiting.
func (b *broker) drop(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.pending[id]; !ok {
		return false
	}
	delete(b.pending, id)
	b.order = slices.DeleteFunc(b.order, func(o string) bool { return o == id })
	return true
}

// answer gives the person's answer to a waiting create. The first answer
// wins; a later one finds nothing waiting.
func (b *broker) answer(id, answer, by string) error {
	switch answer {
	case AnswerAllow, AnswerDeny, AnswerRemember:
	default:
		return ErrBadAnswer
	}
	b.mu.Lock()
	w, ok := b.pending[id]
	if !ok {
		b.mu.Unlock()
		return ErrNotPending
	}
	if answer == AnswerRemember && !w.approval.Rememberable {
		b.mu.Unlock()
		return ErrNotRememberable
	}
	delete(b.pending, id)
	b.order = slices.DeleteFunc(b.order, func(o string) bool { return o == id })
	b.mu.Unlock()
	// The answer is told before the waiting create hears it, so every client
	// reads it before the session it admits starts.
	b.emit(Event{Kind: EventAnswered, Approval: &w.approval, Answer: answer, By: by})
	w.answer <- given{answer: answer, by: by}
	return nil
}

// waitingNow lists the creates waiting, oldest first.
func (b *broker) waitingNow() []Approval {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Approval, 0, len(b.order))
	for _, id := range b.order {
		out = append(out, b.pending[id].approval)
	}
	return out
}
