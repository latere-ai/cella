// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/device"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/worker"
)

// An application embeds the engine in its own process and drives it
// through the Go API alone: it proposes and connects the computer with the
// person's consent, answers a session's approval as the person, renames the
// computer, and removes it. This test is that embedder, with a fake account
// and a fake enroller standing in for the plane, and it reaches nothing but
// the package's exported surface.

// labels are the keys the embedder's plane names sessions under.
var labels = device.SessionLabels{Session: "example.com/session", Agent: "example.com/agent", AgentName: "example.com/agent-name"}

// account is the engine's Account; nothing here serves, so no key is
// minted.
type account struct{}

func (account) MintKey(context.Context, string, time.Duration) (device.Key, error) {
	return device.Key{}, nil
}
func (account) RevokeKey(context.Context, string, string) error { return nil }
func (account) Exists(context.Context, string) (bool, error)    { return true, nil }

// enroller is the embedder's Enroller: it keeps the host policy file and
// records what it was asked of the plane.
type enroller struct {
	paths device.Paths
	mu    sync.Mutex
	asked []string
}

func (e *enroller) note(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.asked = append(e.asked, s)
}

func (e *enroller) Propose(previous device.Policy, q device.ConnectQuery) (device.Policy, error) {
	return device.ProposePolicy(previous, q, e.paths.Home, e.paths.Home, func() string { return "Embedded Mac" })
}

func (e *enroller) Connect(_ context.Context, p device.Policy) (device.Connected, error) {
	p.Device = "dev-embedded000000"
	if err := device.SavePolicy(e.paths.PolicyFile(), p); err != nil {
		return device.Connected{}, err
	}
	e.note("connect " + p.Name)
	return device.Connected{Policy: p}, nil
}

func (e *enroller) Rename(_ context.Context, env, name string) error {
	e.note("rename " + env + " " + name)
	return nil
}

func (e *enroller) Remove(_ context.Context, env string) error {
	e.note("remove " + env)
	return nil
}

func TestAnApplicationEmbedsTheEngine(t *testing.T) {
	// The temporary directory with its links resolved, as the operating
	// system's sandbox names it.
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(home, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("/tmp", "dev")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	paths := device.Paths{Home: home, Config: filepath.Join(home, ".config", "latere"), State: state, Cache: filepath.Join(home, ".cache")}
	enroll := &enroller{paths: paths}
	e := device.New(device.Config{
		Paths: paths, Keys: device.OpenKeys(paths.KeyFile()), Account: account{},
		Enroll: enroll, Labels: labels, Version: "embedded-test",
	})
	ctx := t.Context()

	// Connect: propose, show the facts, and connect with their consent.
	proposal, err := e.ProposeConnect(device.ConnectQuery{Name: "Studio", Folders: []string{project}, SessionStart: device.SessionStartAsk, Network: device.NetworkNone})
	if err != nil {
		t.Fatalf("ProposeConnect: %v", err)
	}
	if len(proposal.Facts) == 0 || proposal.Consent == "" || len(proposal.Problems) != 0 {
		t.Fatalf("the proposal is %+v", proposal)
	}
	if _, err = e.Connect(ctx, proposal.Policy, proposal.Consent); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	policy, err := e.Policy()
	if err != nil || policy.Device != "dev-embedded000000" || policy.Name != "Studio" || policy.SessionStart != device.SessionStartAsk {
		t.Fatalf("after Connect the policy is %+v, %v", policy, err)
	}

	// A session asks to start; the embedder hears it and answers it.
	events, unsubscribe := e.Subscribe()
	defer unsubscribe()
	detach := e.Attach()
	verdict := make(chan worker.Verdict, 1)
	go func() {
		verdict <- e.Admit(ctx, worker.Create{
			Labels: map[string]string{labels.Session: "ses_1", labels.Agent: "agt_1"},
			Sandbox: v1.Sandbox{
				Metadata: v1.Metadata{Annotations: map[string]string{labels.AgentName: "Site helper"}},
				Spec:     v1.SandboxSpec{Workspace: v1.Workspace{Host: project}},
				Status:   v1.SandboxStatus{ID: "sbx_1"},
			},
		})
	}()
	asked := waitFor(t, events, func(ev device.Event) bool { return ev.Kind == device.EventAsked })
	if asked.Approval.Agent != "Site helper" || asked.Approval.Session != "ses_1" {
		t.Errorf("the approval names %+v", asked.Approval)
	}
	if waiting := e.Waiting(); len(waiting) != 1 || waiting[0].ID != asked.Approval.ID {
		t.Errorf("Waiting = %+v", waiting)
	}
	if err = e.Answer(asked.Approval.ID, device.AnswerAllow, "the application"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if v := <-verdict; !v.Allow {
		t.Fatalf("the session was refused: %+v", v)
	}
	detach()

	// Rename: a name alone changes the plane's record and the file.
	name := "Studio Mac"
	if _, err = e.PutPolicy(ctx, device.PolicyChange{Name: &name}); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}
	if policy, _ = e.Policy(); policy.Name != name {
		t.Errorf("after PutPolicy the name is %q", policy.Name)
	}

	// Remove: the plane's record and every file of the device.
	status, err := e.Remove(ctx, false)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if status.Device != "" {
		t.Errorf("after Remove the status names the device %q", status.Device)
	}
	if _, ok, _ := device.LoadPolicy(paths.PolicyFile()); ok {
		t.Error("the host policy file outlived the removal")
	}
	want := []string{"connect Studio", "rename dev-embedded000000 Studio Mac", "remove dev-embedded000000"}
	enroll.mu.Lock()
	got := slices.Clone(enroll.asked)
	enroll.mu.Unlock()
	if !slices.Equal(got, want) {
		t.Errorf("the plane was asked %v, want %v", got, want)
	}
}

// waitFor answers the first event match accepts, or fails after five
// seconds.
func waitFor(t *testing.T, events <-chan device.Event, match func(device.Event) bool) device.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("the event never came")
		}
	}
}
