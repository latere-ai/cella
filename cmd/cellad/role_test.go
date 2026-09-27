// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/cella/internal/api"
	"latere.ai/x/cella/internal/store"
)

// answering is a handler that names itself in its answer.
func answering(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, name)
	})
}

func fetchBody(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(body)
}

// slot serves a role switch the way cellad composes the API slot: the
// forwarder asks the switch first, and forwards to the writer the test's
// lookup names, which is a stub that names itself.
func slot(t *testing.T, sw *roleSwitch, hold time.Duration, writer *atomic.Value) *httptest.Server {
	t.Helper()
	successor := httptest.NewServer(answering("successor"))
	t.Cleanup(successor.Close)
	forwarder := api.NewForwarder(api.ForwarderOptions{
		Local: sw.serveLocally, Changed: sw.watch, Hold: hold, Log: slog.New(slog.DiscardHandler),
		Writer: func(context.Context) (string, error) {
			if named, _ := writer.Load().(bool); named {
				return successor.URL, nil
			}
			return "", nil
		},
	})
	server := httptest.NewServer(forwarder)
	t.Cleanup(server.Close)
	return server
}

// TestTheRoleSwitchServesForwardsOrHolds is the API slot of spec 076: a
// request that reaches a standby goes to the writer the lease names; one that
// finds no writer is held and, when this process promotes meanwhile, answered
// here rather than forwarded to itself or refused; and one that finds neither
// for the whole hold is refused with control_plane_unavailable.
func TestTheRoleSwitchServesForwardsOrHolds(t *testing.T) {
	t.Run("the lease names a writer", func(t *testing.T) {
		var writer atomic.Value
		writer.Store(true)
		server := slot(t, newRoleSwitch(), time.Second, &writer)
		if status, body := fetchBody(t, server.URL); status != http.StatusOK || body != "successor" {
			t.Errorf("the request was answered %d %q, want the writer's", status, body)
		}
	})
	t.Run("this process promotes while the request is held", func(t *testing.T) {
		var writer atomic.Value
		sw := newRoleSwitch()
		server := slot(t, sw, 5*time.Second, &writer)
		time.AfterFunc(200*time.Millisecond, func() { sw.promoted(answering("local")) })
		if status, body := fetchBody(t, server.URL); status != http.StatusOK || body != "local" {
			t.Errorf("the held request was answered %d %q, want this process's answer once it promoted", status, body)
		}
	})
	t.Run("no writer for the whole hold", func(t *testing.T) {
		var writer atomic.Value
		server := slot(t, newRoleSwitch(), 200*time.Millisecond, &writer)
		status, body := fetchBody(t, server.URL)
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatalf("the refusal is %q: %v", body, err)
		}
		if status != http.StatusServiceUnavailable || envelope.Error.Code != "control_plane_unavailable" {
			t.Errorf("the request was answered %d %q, want 503 control_plane_unavailable", status, body)
		}
	})
}

// TestAHandoffHoldsNewRequestsAndCutsTheSlowOnes: a writer that stops serving
// holds the requests that arrive after, and forwards them to the successor
// once the lease names one; a request it was answering and that outlasts the
// handoff's wait has its context ended, and the handoff reports it cut.
func TestAHandoffHoldsNewRequestsAndCutsTheSlowOnes(t *testing.T) {
	var writer atomic.Value
	sw := newRoleSwitch()
	entered := make(chan struct{})
	ended := make(chan error, 1)
	sw.promoted(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(entered)
			<-r.Context().Done()
			ended <- r.Context().Err()
			return
		}
		_, _ = io.WriteString(w, "local")
	}))
	server := slot(t, sw, 5*time.Second, &writer)
	go func() {
		res, err := http.Get(server.URL + "/slow")
		if err == nil {
			_ = res.Body.Close()
		}
	}()
	<-entered

	sw.stopServing()
	held := make(chan string, 1)
	go func() {
		_, body := fetchBody(t, server.URL+"/next")
		held <- body
	}()
	if cut := sw.drain(200 * time.Millisecond); cut != 1 {
		t.Errorf("the handoff cut %d requests, want the slow one", cut)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the slow request ended with %v, want its context cancelled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the slow request was still running after the handoff cut it")
	}
	writer.Store(true)
	select {
	case body := <-held:
		if body != "successor" {
			t.Errorf("a request that arrived during the handoff was answered %q, want the successor's", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request held during the handoff never reached the successor")
	}
}

// scriptedLease is the writer lease as a test scripts it: the row a read
// returns and what each take or renewal answers.
type scriptedLease struct {
	mu       sync.Mutex
	row      store.Lease
	acquire  []func() (bool, error)
	acquired int
}

func (l *scriptedLease) Holder() string { return "this-process" }

func (l *scriptedLease) Lease(context.Context, string) (store.Lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.row, nil
}

func (l *scriptedLease) Acquire(context.Context, string, time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acquired++
	if len(l.acquire) == 0 {
		return true, nil
	}
	next := l.acquire[0]
	if len(l.acquire) > 1 {
		l.acquire = l.acquire[1:]
	}
	return next()
}

func (l *scriptedLease) takes() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acquired
}

// TestAStandbyTakesTheWriterLeaseOnlyOnceItIsFree: a standby reads the row
// and does not ask to take a lease another live holder has, and takes it once
// the holder's term lapsed.
func TestAStandbyTakesTheWriterLeaseOnlyOnceItIsFree(t *testing.T) {
	lease := &scriptedLease{row: store.Lease{Name: store.WriterLease, Holder: "the-writer", Live: true}}
	took := make(chan bool, 1)
	go func() { took <- awaitWriterLease(t.Context(), lease, nil, slog.New(slog.DiscardHandler)) }()
	time.Sleep(2500 * time.Millisecond)
	if n := lease.takes(); n != 0 {
		t.Fatalf("the standby asked to take a lease a live writer holds %d times", n)
	}
	lease.mu.Lock()
	lease.row.Live = false
	lease.mu.Unlock()
	select {
	case held := <-took:
		if !held {
			t.Fatal("the standby did not take the lapsed lease")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the standby did not take the lapsed lease within its poll")
	}
}

// TestLosingTheWriterLeaseExits: a writer that renews into another holder, or
// that renews nothing for the whole term, is demoted; one failed renewal
// followed by a good one is not a loss.
func TestLosingTheWriterLeaseExits(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	outage := errors.New("the store did not answer")
	t.Run("another holder", func(t *testing.T) {
		lease := &scriptedLease{acquire: []func() (bool, error){func() (bool, error) { return false, nil }}}
		if err := watchWriterLease(t.Context(), lease, 15*time.Second, nil, log); !errors.Is(err, errDemoted) {
			t.Errorf("a renewal into another holder answered %v, want errDemoted", err)
		}
	})
	t.Run("no renewal for the term", func(t *testing.T) {
		lease := &scriptedLease{acquire: []func() (bool, error){func() (bool, error) { return false, outage }}}
		started := time.Now()
		err := watchWriterLease(t.Context(), lease, 2*time.Second, nil, log)
		if !errors.Is(err, errDemoted) || !errors.Is(err, outage) {
			t.Errorf("a store that stopped answering answered %v, want errDemoted with the outage", err)
		}
		if waited := time.Since(started); waited < 2*time.Second {
			t.Errorf("the writer was demoted after %v, before its term", waited)
		}
	})
	t.Run("one failed renewal", func(t *testing.T) {
		lease := &scriptedLease{acquire: []func() (bool, error){
			func() (bool, error) { return false, outage },
			func() (bool, error) { return true, nil },
		}}
		stop := make(chan struct{})
		time.AfterFunc(3500*time.Millisecond, func() { close(stop) })
		if err := watchWriterLease(t.Context(), lease, 2*time.Second, stop, log); err != nil {
			t.Errorf("one failed renewal demoted the writer: %v", err)
		}
	})
}
