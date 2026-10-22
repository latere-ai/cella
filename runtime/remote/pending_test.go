// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime/remote"
)

// pendingOf reads whether the hub reports one worker as pending.
func pendingOf(t *testing.T, hub *remote.Hub, environment, worker string) bool {
	t.Helper()
	for _, w := range hub.Workers(environment) {
		if w.Worker == worker {
			return w.Pending
		}
	}
	t.Fatalf("the hub reports no worker %s", worker)
	return false
}

// TestARegistrationIsPendingUntilItsStreamOpens: a worker is pending from its
// registration until its stream opens, never again once it has opened, and
// not once the pending window has passed with no stream.
func TestARegistrationIsPendingUntilItsStreamOpens(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	hub := remote.NewHub(remote.HubOptions{Offline: time.Minute, Now: func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}})
	registration := remote.Registration{Driver: "podman", Isolation: v1.IsolationContainer}
	streamed, err := hub.Register("env_a", registration)
	if err != nil {
		t.Fatal(err)
	}
	if !pendingOf(t, hub, "env_a", streamed.Worker) {
		t.Error("a registration whose stream has not opened is not pending")
	}
	s := openStream(t, hub, "env_a", streamed.Worker, remote.ServeOptions{})
	connected(t, hub, "env_a", 1)
	if pendingOf(t, hub, "env_a", streamed.Worker) {
		t.Error("a worker holding its stream is still pending")
	}
	_ = s.conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for hub.Workers("env_a")[0].Connected {
		if time.Now().After(deadline) {
			t.Fatal("the hub never saw the stream end")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pendingOf(t, hub, "env_a", streamed.Worker) {
		t.Error("a worker whose stream ended is pending again")
	}

	waiting, err := hub.Register("env_a", registration)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(remote.HeartbeatTimeout + time.Second)
	mu.Unlock()
	if pendingOf(t, hub, "env_a", waiting.Worker) {
		t.Error("a registration past the pending window with no stream is still pending")
	}
}

// TestTheHubLogsTheStreamItOpens: the hub logs a worker's stream once the
// hello binds it to its registration, naming the worker, and logs nothing of
// the kind for a registration whose stream has not opened. The registration
// already counts in the environment's status, so the line is the one sign
// that operations reach the worker.
func TestTheHubLogsTheStreamItOpens(t *testing.T) {
	var mu sync.Mutex
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logged.Write(p)
	}), nil))
	read := func() string {
		mu.Lock()
		defer mu.Unlock()
		return logged.String()
	}
	hub := remote.NewHub(remote.HubOptions{Offline: time.Minute, Log: log})
	registration := remote.Registration{Driver: "podman", Isolation: v1.IsolationContainer}
	opened, err := hub.Register("env_a", registration)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := hub.Register("env_a", registration)
	if err != nil {
		t.Fatal(err)
	}
	openStream(t, hub, "env_a", opened.Worker, remote.ServeOptions{})
	waitFor(t, "the opened stream logged", func() bool {
		return strings.Contains(read(), `msg="a worker's stream opened" environment=env_a worker=`+opened.Worker)
	})
	if n := strings.Count(read(), "a worker's stream opened"); n != 1 {
		t.Errorf("one stream opened and the hub logged %d:\n%s", n, read())
	}
	if strings.Contains(read(), waiting.Worker) {
		t.Errorf("a registration whose stream has not opened was logged:\n%s", read())
	}
}
