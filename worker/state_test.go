// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package worker_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/worker"
)

// stateWorker builds a worker against the plane whose every change of
// connection lands on the returned channel, and whose reconnect waits until
// the test sends on step, so the test sets the plane up before each attempt.
func stateWorker(t *testing.T, p *plane) (*worker.Worker, <-chan worker.State, chan<- time.Time) {
	t.Helper()
	states := make(chan worker.State, 16)
	w, err := worker.New(worker.Options{
		URL: p.url, Key: key("an-environment-key"), Driver: nativeDriver(t), Version: "test",
		State: func(s worker.State) { states <- s },
	})
	if err != nil {
		t.Fatal(err)
	}
	step := make(chan time.Time)
	w.SetAfter(func(time.Duration) <-chan time.Time { return step })
	return w, states, step
}

// nextState is the next change the worker told, or a failure after ten
// seconds.
func nextState(t *testing.T, states <-chan worker.State, what string) worker.State {
	t.Helper()
	select {
	case s := <-states:
		return s
	case <-time.After(10 * time.Second):
		t.Fatalf("the worker told no state %s", what)
		return worker.State{}
	}
}

// TestStateTellsTheConnection: an embedder is told each change of the
// worker's connection in order. A stream that opens is Connected with the
// registration's answer; one that ends is not, with what ended it and the wait
// before the next attempt; a registration the control plane answered with an
// error is a *RegistrationError carrying its status and code; and a refusal
// the worker stops on is told last, with Reconnect false and the Refusal.
func TestStateTellsTheConnection(t *testing.T) {
	p := newPlane(t)
	p.drop.Store(true)
	w, states, step := stateWorker(t, p)
	ended := make(chan error, 1)
	go func() { ended <- w.Run(t.Context()) }()

	connected := nextState(t, states, "after the first stream opened")
	if !connected.Connected || connected.Err != nil || connected.Reconnect || connected.Refusal != nil {
		t.Fatalf("an open stream was told as %+v", connected)
	}
	if registered := connected.Registered; !strings.HasPrefix(registered.Worker, "wrk_") ||
		registered.Environment != "env_test" || registered.HeartbeatInterval == "" || registered.Lease == "" {
		t.Errorf("the connected state carries the registration %+v", registered)
	}
	dropped := nextState(t, states, "after the stream was dropped")
	if dropped.Connected || dropped.Err == nil || !dropped.Reconnect || dropped.RetryIn != worker.MinBackoff ||
		dropped.Refusal != nil {
		t.Errorf("a dropped stream was told as %+v", dropped)
	}

	p.drop.Store(false)
	p.mu.Lock()
	p.refuse = true
	p.mu.Unlock()
	step <- time.Now()
	refused := nextState(t, states, "after a refused registration")
	var registration *worker.RegistrationError
	if !errors.As(refused.Err, &registration) || registration.StatusCode != http.StatusUnauthorized ||
		registration.Code != "unauthenticated" || !strings.Contains(registration.Error(), "401") {
		t.Errorf("a registration refused with 401 was told with %v", refused.Err)
	}
	if refused.Connected || !refused.Reconnect || refused.RetryIn != 2*worker.MinBackoff {
		t.Errorf("a refused registration was told as %+v", refused)
	}

	p.mu.Lock()
	p.refuse = false
	p.mu.Unlock()
	reason := remote.ReasonKeyRevoked
	p.closeWith.Store(&reason)
	step <- time.Now()
	if s := nextState(t, states, "before the stream was closed"); !s.Connected {
		t.Errorf("the stream the control plane then closed was told as %+v", s)
	}
	last := nextState(t, states, "after the key was revoked")
	if last.Connected || last.Reconnect || last.Refusal == nil || last.Refusal.Code != remote.ReasonKeyRevoked ||
		!errors.Is(last.Err, last.Refusal) {
		t.Errorf("a refusal the worker stops on was told as %+v", last)
	}
	select {
	case err := <-ended:
		var refusal *worker.Refusal
		if !errors.As(err, &refusal) || refusal.Code != remote.ReasonKeyRevoked {
			t.Errorf("Run returned %v, want the key_revoked refusal", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the refusal")
	}
	select {
	case s := <-states:
		t.Errorf("a state was told after the last one: %+v", s)
	default:
	}
}

// TestStateTellsTheStop: a worker whose context ends is told with Reconnect
// false and the context's cause, whether the stream was open or the worker was
// waiting to register again.
func TestStateTellsTheStop(t *testing.T) {
	stopped := errors.New("the embedder paused the worker")
	for _, tc := range []struct {
		name   string
		refuse bool
	}{
		{"while connected", false},
		{"while waiting to register again", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlane(t)
			p.mu.Lock()
			p.refuse = tc.refuse
			p.mu.Unlock()
			w, states, _ := stateWorker(t, p)
			ctx, cancel := context.WithCancelCause(t.Context())
			ended := make(chan error, 1)
			go func() { ended <- w.Run(ctx) }()
			if first := nextState(t, states, "before the stop"); first.Connected == tc.refuse {
				t.Fatalf("the first state was %+v", first)
			}
			cancel(stopped)
			last := nextState(t, states, "after the context ended")
			if last.Connected || last.Reconnect || last.Refusal != nil || !errors.Is(last.Err, stopped) {
				t.Errorf("a worker whose context ended was told as %+v", last)
			}
			select {
			case err := <-ended:
				if err != nil {
					t.Errorf("Run returned %v on its context ending", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return after its context ended")
			}
		})
	}
}
