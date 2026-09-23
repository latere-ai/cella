// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runtimetest

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/runtime"
)

// fakeDialer is a driver whose sandboxes each run an echo server on the ports
// they declare, reached over an in-memory pipe. Its switches are the ways a
// Dialer can break the contract, so the dial case is shown to fail each one.
type fakeDialer struct {
	Nop
	mu    sync.Mutex
	boxes map[string]*fakeBox
	// mute reads what a connection sends and writes nothing back.
	mute bool
	// single serves one connection at a time: a second dial ends the first.
	single bool
	// ignoresPhase dials a sandbox that is not running.
	ignoresPhase bool
	// ignoresID dials a sandbox that is not there.
	ignoresID bool
	// last is the connection single ends on the next dial.
	last net.Conn
}

// fakeBox is one sandbox the fake holds: its phase is all a dial reads.
type fakeBox struct{ phase string }

func newFakeDialer() *fakeDialer { return &fakeDialer{boxes: map[string]*fakeBox{}} }

func (d *fakeDialer) Capabilities() runtime.Capabilities { return runtime.Capabilities{Dial: true} }

func (d *fakeDialer) Create(_ context.Context, s runtime.CreateSpec) (runtime.Ref, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.boxes[s.ID] = &fakeBox{phase: runtime.Running}
	return runtime.Ref{ID: s.ID}, nil
}

func (d *fakeDialer) Inspect(_ context.Context, id string) (runtime.State, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	box, ok := d.boxes[id]
	if !ok {
		return runtime.State{}, runtime.ErrNotFound
	}
	return runtime.State{ID: id, Phase: box.phase}, nil
}

func (d *fakeDialer) Stop(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if box, ok := d.boxes[id]; ok {
		box.phase = runtime.Stopped
	}
	return nil
}

func (d *fakeDialer) Delete(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.boxes, id)
	return nil
}

func (d *fakeDialer) Dial(_ context.Context, id string, port int) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	box, ok := d.boxes[id]
	switch {
	case !ok && !d.ignoresID:
		return nil, runtime.ErrNotFound
	case ok && box.phase != runtime.Running && !d.ignoresPhase:
		return nil, runtime.ErrNotRunning
	}
	client, server := net.Pipe()
	go func() {
		defer func() { _ = server.Close() }()
		if d.mute {
			_, _ = io.Copy(io.Discard, server)
			return
		}
		_, _ = io.Copy(server, server)
	}()
	if d.single && d.last != nil {
		_ = d.last.Close()
	}
	d.last = client
	return client, nil
}

// echoOptions is what a suite over an image that serves a port is given.
func echoOptions() Options {
	return Options{Echo: func(port int) []string { return []string{"echo", strconv.Itoa(port)} }}
}

// TestDialCase holds the dial case to both halves of its job: it passes a
// Dialer that keeps the contract, it names the reason it cannot ask, and it
// fails each way a Dialer can break the contract.
func TestDialCase(t *testing.T) {
	poll, trip := pollTimeout, dialRoundTrip
	pollTimeout, dialRoundTrip = 300*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { pollTimeout, dialRoundTrip = poll, trip })

	t.Run("a conforming dialer", func(t *testing.T) {
		d := newFakeDialer()
		if r := runCase(t, "DialReachesAPort", d, echoOptions()); r.failed() || r.skipped() != "" {
			t.Fatalf("the case did not pass a conforming dialer:\n%s", r.report())
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if len(d.boxes) != 0 {
			t.Errorf("the case left %d sandboxes behind", len(d.boxes))
		}
	})

	for _, tc := range []struct {
		name, want string
		driver     runtime.Driver
		opts       Options
	}{
		{"undeclared", "declares no Dial", Nop{}, echoOptions()},
		{"no server", "Options.Echo is nil", newFakeDialer(), Options{}},
	} {
		t.Run("skips/"+tc.name, func(t *testing.T) {
			if r := runCase(t, "DialReachesAPort", tc.driver, tc.opts); !strings.Contains(r.skipped(), tc.want) {
				t.Fatalf("the case was not skipped for the right reason: %q", r.report())
			}
		})
	}

	for _, tc := range []struct {
		name  string
		build func() runtime.Driver
		want  string
	}{
		{"a connection that carries nothing back", func() runtime.Driver { d := newFakeDialer(); d.mute = true; return d }, "no line came back"},
		{"one connection at a time", func() runtime.Driver { d := newFakeDialer(); d.single = true; return d }, "stopped carrying bytes"},
		{"a stopped sandbox dialed", func() runtime.Driver { d := newFakeDialer(); d.ignoresPhase = true; return d }, "Dial on a stopped sandbox"},
		{"an absent sandbox dialed", func() runtime.Driver { d := newFakeDialer(); d.ignoresID = true; return d }, "Dial on an absent sandbox"},
		{"a declaration with no dialer behind it", func() runtime.Driver { return dialLiar{Nop{}} }, "does not implement runtime.Dialer"},
	} {
		t.Run("fails/"+tc.name, func(t *testing.T) {
			r := runCase(t, "DialReachesAPort", tc.build(), echoOptions())
			if !r.failed() {
				t.Fatalf("the case passed %s", tc.name)
			}
			if !strings.Contains(strings.Join(r.failures(), "\n"), tc.want) {
				t.Errorf("the case failed on %v, want a failure naming %q", r.failures(), tc.want)
			}
		})
	}
}
