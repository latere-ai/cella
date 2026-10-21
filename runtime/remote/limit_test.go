// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/runtime/runtimetest"
)

// stream is one worker's stream to a hub over a pipe: the server side runs
// the worker's protocol, the hub side records how it was closed.
type stream struct {
	conn   *closingConn
	server *remote.Server
	served chan error
}

// openStream registers nothing: it opens the stream of a registration made
// already, says hello as that worker, and serves until the hub ends it.
func openStream(t *testing.T, hub *remote.Hub, environment, worker string, o remote.ServeOptions) *stream {
	t.Helper()
	control, workerSide := net.Pipe()
	s := &stream{conn: &closingConn{pipeConn: &pipeConn{conn: control}}, served: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = control.Close(); _ = workerSide.Close() })
	go func() { s.served <- hub.ServeWith(ctx, environment, s.conn, o) }()
	s.server = remote.NewServer(&pipeConn{conn: workerSide}, remote.ServerOptions{Worker: worker, Driver: runtimetest.Nop{}})
	go func() { _ = s.server.Run(ctx) }()
	return s
}

// closedWith waits for the hub to end the stream and reads the reason it
// closed it with.
func (s *stream) closedWith(t *testing.T) string {
	t.Helper()
	select {
	case <-s.served:
	case <-time.After(10 * time.Second):
		t.Fatal("the hub never ended the stream")
	}
	s.conn.mu.Lock()
	defer s.conn.mu.Unlock()
	return s.conn.reason
}

// connected waits until the hub reports n workers of the environment holding
// a stream.
func connected(t *testing.T, hub *remote.Hub, environment string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		count := 0
		for _, w := range hub.Workers(environment) {
			if w.Connected {
				count++
			}
		}
		if count == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the environment never held %d connected workers", n)
}

var hostA = &v1.HostInfo{OS: "linux", Arch: "amd64", Grants: []v1.HostGrant{{Path: "/home/a", Write: true}}}

// TestTheHubHoldsTheWorkerLimit is spec.workers.max at the hub: below the
// limit a registration is admitted; at it another key is refused; the live
// worker's key from the same host replaces it and closes its stream with
// worker_replaced; that key from another host is refused while the live
// worker holds its place; and the replacement says what met what.
func TestTheHubHoldsTheWorkerLimit(t *testing.T) {
	hub := remote.NewHub(remote.HubOptions{Offline: time.Minute})
	reg := remote.Registration{Driver: "native", Isolation: "none", Host: hostA}
	first, rep, err := hub.RegisterWith("env_one", reg, remote.Admission{JTI: "key-a", MaxWorkers: 1})
	if err != nil || rep != nil {
		t.Fatalf("the first registration answered %v, %v", rep, err)
	}
	live := openStream(t, hub, "env_one", first.Worker, remote.ServeOptions{})
	connected(t, hub, "env_one", 1)

	if _, rep, err = hub.RegisterWith("env_one", reg, remote.Admission{JTI: "key-b", MaxWorkers: 1}); !errors.Is(err, remote.ErrWorkerLimit) || rep != nil {
		t.Errorf("another key at the limit answered %v, %v", rep, err)
	}
	elsewhere := reg
	elsewhere.Host = &v1.HostInfo{OS: "darwin", Arch: "arm64"}
	_, rep, err = hub.RegisterWith("env_one", elsewhere, remote.Admission{JTI: "key-a", MaxWorkers: 1})
	if !errors.Is(err, remote.ErrWorkerLimit) || rep == nil || rep.Replaced || rep.Worker != first.Worker || rep.Next.Host.OS != "darwin" {
		t.Errorf("the key from another host answered %+v, %v", rep, err)
	}
	second, rep, err := hub.RegisterWith("env_one", reg, remote.Admission{JTI: "key-a", MaxWorkers: 1})
	if err != nil || rep == nil || !rep.Replaced || rep.Previous.Host.OS != "linux" {
		t.Fatalf("the key from the same host answered %+v, %v", rep, err)
	}
	if reason := live.closedWith(t); reason != remote.ReasonWorkerReplaced {
		t.Errorf("the replaced stream closed with %q", reason)
	}
	// With no limit, any number register, the replaced worker's key among
	// them.
	if _, rep, err = hub.RegisterWith("env_one", reg, remote.Admission{JTI: "key-b"}); err != nil || rep != nil {
		t.Errorf("a registration with no limit answered %v, %v", rep, err)
	}
	if second.Worker == first.Worker {
		t.Error("the replacement claims under the replaced worker's id")
	}
}

// TestAWorkerEndedBeforeItsStreamOpensIsToldOnIt: a registration replaced,
// revoked or deleted before its stream opened has that stream closed with the
// reason as soon as its hello arrives, and only once, rather than refused as
// a worker that never registered, which a worker answers by registering
// again.
func TestAWorkerEndedBeforeItsStreamOpensIsToldOnIt(t *testing.T) {
	reg := remote.Registration{Driver: "native", Isolation: "none", Host: hostA}
	for _, c := range []struct {
		name, reason string
		end          func(t *testing.T, hub *remote.Hub)
	}{
		{"replaced", remote.ReasonWorkerReplaced, func(t *testing.T, hub *remote.Hub) {
			if _, rep, err := hub.RegisterWith("env_early", reg, remote.Admission{JTI: "key-a", MaxWorkers: 1}); err != nil || rep == nil || !rep.Replaced {
				t.Fatalf("the same key from the same host answered %+v, %v", rep, err)
			}
		}},
		{"revoked", remote.ReasonKeyRevoked, func(t *testing.T, hub *remote.Hub) {
			if n := hub.Revoke("key-a"); n != 0 {
				t.Errorf("the revocation closed %d streams before any opened", n)
			}
		}},
		{"deleted", remote.ReasonServeRefused, func(_ *testing.T, hub *remote.Hub) { hub.Release("env_early") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			hub := remote.NewHub(remote.HubOptions{Offline: time.Minute})
			first, _, err := hub.RegisterWith("env_early", reg, remote.Admission{JTI: "key-a", MaxWorkers: 1})
			if err != nil {
				t.Fatal(err)
			}
			c.end(t, hub)
			if reason := openStream(t, hub, "env_early", first.Worker, remote.ServeOptions{}).closedWith(t); reason != c.reason {
				t.Errorf("the stream of the ended registration closed with %q, want %q", reason, c.reason)
			}
			if reason := openStream(t, hub, "env_early", first.Worker, remote.ServeOptions{}).closedWith(t); reason != "" {
				t.Errorf("a second stream of the ended registration closed with %q, want a plain close", reason)
			}
		})
	}
}

// TestAnEndedRegistrationIsToldWithinThePendingWindow: what ended a
// registration before its stream opened is kept for the pending window and
// no longer, and a registration that names the ended one's id starts afresh.
func TestAnEndedRegistrationIsToldWithinThePendingWindow(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	hub := remote.NewHub(remote.HubOptions{Offline: time.Minute, Now: func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}})
	reg := remote.Registration{Driver: "native", Isolation: "none"}
	stale, _, err := hub.RegisterWith("env_window", reg, remote.Admission{JTI: "key-a"})
	if err != nil {
		t.Fatal(err)
	}
	hub.Revoke("key-a")
	mu.Lock()
	now = now.Add(remote.HeartbeatTimeout + time.Second)
	mu.Unlock()
	// Ending another registration drops what is older than the window.
	if _, _, err = hub.RegisterWith("env_window", reg, remote.Admission{JTI: "key-b"}); err != nil {
		t.Fatal(err)
	}
	hub.Revoke("key-b")
	if reason := openStream(t, hub, "env_window", stale.Worker, remote.ServeOptions{}).closedWith(t); reason != "" {
		t.Errorf("a stream opened past the pending window closed with %q, want a plain close", reason)
	}

	named := reg
	named.Worker = "wrk_named"
	if _, _, err = hub.RegisterWith("env_window", named, remote.Admission{JTI: "key-c"}); err != nil {
		t.Fatal(err)
	}
	hub.Revoke("key-c")
	if _, _, err = hub.RegisterWith("env_window", named, remote.Admission{JTI: "key-d"}); err != nil {
		t.Fatal(err)
	}
	openStream(t, hub, "env_window", "wrk_named", remote.ServeOptions{})
	connected(t, hub, "env_window", 1)
}

// TestAStreamBeyondTheLimitIsRefused: two registrations that raced for one
// place are both admitted while neither holds a stream, and the second
// stream to open is closed with worker_limit.
func TestAStreamBeyondTheLimitIsRefused(t *testing.T) {
	hub := remote.NewHub(remote.HubOptions{Offline: time.Minute})
	reg := remote.Registration{Driver: "native", Isolation: "none"}
	a, _, err := hub.RegisterWith("env_race", reg, remote.Admission{JTI: "key-a", MaxWorkers: 2})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := hub.RegisterWith("env_race", reg, remote.Admission{JTI: "key-b", MaxWorkers: 2})
	if err != nil {
		t.Fatal(err)
	}
	// The limit drops to one after both registered, which is the race's
	// effect: two places taken for one.
	if _, _, err = hub.RegisterWith("env_race", reg, remote.Admission{JTI: "key-c", MaxWorkers: 1}); !errors.Is(err, remote.ErrWorkerLimit) {
		t.Fatalf("a third registration answered %v", err)
	}
	openStream(t, hub, "env_race", a.Worker, remote.ServeOptions{})
	connected(t, hub, "env_race", 1)
	late := openStream(t, hub, "env_race", b.Worker, remote.ServeOptions{})
	if reason := late.closedWith(t); reason != remote.ReasonWorkerLimit {
		t.Errorf("the stream beyond the limit closed with %q", reason)
	}
}

// TestRevokeClosesTheKeysStreams: revoking a key closes every stream it
// registered with key_revoked and leaves another key's alone.
func TestRevokeClosesTheKeysStreams(t *testing.T) {
	hub := remote.NewHub(remote.HubOptions{Offline: time.Minute})
	reg := remote.Registration{Driver: "native", Isolation: "none"}
	a, _, _ := hub.RegisterWith("env_keys", reg, remote.Admission{JTI: "key-a"})
	b, _, _ := hub.RegisterWith("env_keys", reg, remote.Admission{JTI: "key-b"})
	revoked := openStream(t, hub, "env_keys", a.Worker, remote.ServeOptions{})
	openStream(t, hub, "env_keys", b.Worker, remote.ServeOptions{})
	connected(t, hub, "env_keys", 2)
	if n := hub.Revoke("key-a"); n != 1 {
		t.Errorf("the revocation closed %d streams, want one", n)
	}
	if reason := revoked.closedWith(t); reason != remote.ReasonKeyRevoked {
		t.Errorf("the revoked key's stream closed with %q", reason)
	}
	connected(t, hub, "env_keys", 1)
	if hub.Revoke("") != 0 || hub.Revoke("key-nobody") != 0 {
		t.Error("revoking no key closed a stream")
	}
}

// TestReleaseRefusesToServe: a deleted environment's streams close with
// serve_refused, which a worker reads as the end of what it ran.
func TestReleaseRefusesToServe(t *testing.T) {
	hub := remote.NewHub(remote.HubOptions{Offline: time.Minute})
	a, _, _ := hub.RegisterWith("env_gone", remote.Registration{Driver: "native", Isolation: "none"}, remote.Admission{})
	s := openStream(t, hub, "env_gone", a.Worker, remote.ServeOptions{})
	connected(t, hub, "env_gone", 1)
	hub.Release("env_gone")
	if reason := s.closedWith(t); reason != remote.ReasonServeRefused {
		t.Errorf("the deleted environment's stream closed with %q", reason)
	}
}

// TestALiveStreamIsRecheckedOnItsTTL: environment.serve is asked again on
// the decision's TTL with the registration the stream named; a refusal
// closes the stream with serve_refused, and two answers in a row with no
// decision close it with try-again.
func TestALiveStreamIsRecheckedOnItsTTL(t *testing.T) {
	t.Run("a refusal", func(t *testing.T) {
		hub := remote.NewHub(remote.HubOptions{Offline: time.Minute})
		reg := remote.Registration{Driver: "native", Isolation: "none", Host: hostA}
		a, _, _ := hub.RegisterWith("env_re", reg, remote.Admission{JTI: "key-a", TTL: 20 * time.Millisecond})
		var mu sync.Mutex
		var seen []remote.Registration
		var asked atomic.Int32
		s := openStream(t, hub, "env_re", a.Worker, remote.ServeOptions{
			Recheck: func(_ context.Context, r remote.Registration) (time.Duration, error) {
				mu.Lock()
				seen = append(seen, r)
				mu.Unlock()
				if asked.Add(1) < 3 {
					return 20 * time.Millisecond, nil
				}
				return 0, remote.ErrServeRefused
			},
		})
		if reason := s.closedWith(t); reason != remote.ReasonServeRefused {
			t.Errorf("the refused stream closed with %q", reason)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(seen) != 3 || seen[0].Host == nil || seen[0].Host.OS != "linux" {
			t.Errorf("the rechecks saw %+v", seen)
		}
	})
	t.Run("no decision twice", func(t *testing.T) {
		hub := remote.NewHub(remote.HubOptions{Offline: time.Minute})
		a, _, _ := hub.RegisterWith("env_re", remote.Registration{Driver: "native", Isolation: "none"},
			remote.Admission{TTL: 20 * time.Millisecond})
		var asked atomic.Int32
		s := openStream(t, hub, "env_re", a.Worker, remote.ServeOptions{
			Recheck: func(context.Context, remote.Registration) (time.Duration, error) {
				switch asked.Add(1) {
				case 1:
					return 0, errors.New("the authorizer is not answering")
				case 2:
					return 20 * time.Millisecond, nil
				}
				return 0, errors.New("the authorizer is not answering")
			},
		})
		if reason := s.closedWith(t); reason != remote.ReasonAuthorizerUnavailable || asked.Load() != 4 {
			t.Errorf("the stream closed with %q after %d rechecks", reason, asked.Load())
		}
		if code := s.conn.code; code != remote.CloseTryAgain {
			t.Errorf("the stream closed with the code %d", code)
		}
	})
}

// TestRecheckWaitsAtMostFiveMinutes: a decision naming no TTL, or one longer
// than MaxRecheck, is asked again after MaxRecheck.
func TestRecheckWaitsAtMostFiveMinutes(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second, time.Hour} {
		if got := remote.RecheckAfter(ttl); got != remote.MaxRecheck {
			t.Errorf("a TTL of %v rechecks after %v", ttl, got)
		}
	}
	if got := remote.RecheckAfter(time.Minute); got != time.Minute {
		t.Errorf("a TTL of a minute rechecks after %v", got)
	}
}
