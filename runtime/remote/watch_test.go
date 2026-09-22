// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/runtime/runtimetest"
)

// watchingDriver is a worker's driver that watches: every Watch hands the
// test the channel its events go down, and List answers what the test set
// and counts its calls.
type watchingDriver struct {
	runtimetest.Nop
	opened chan chan remote.Event

	mu      sync.Mutex
	states  []driver.State
	lists   int
	watches int
	// failing is how many of the next Watch calls fail, and listErr what
	// List fails with.
	failing int
	listErr error
}

func newWatchingDriver() *watchingDriver {
	return &watchingDriver{opened: make(chan chan remote.Event, 4)}
}

func (d *watchingDriver) Watch(context.Context) (<-chan remote.Event, error) {
	feed := make(chan remote.Event)
	d.mu.Lock()
	d.watches++
	if d.failing > 0 {
		d.failing--
		d.mu.Unlock()
		return nil, errors.New("the substrate stopped answering")
	}
	d.mu.Unlock()
	d.opened <- feed
	return feed, nil
}

func (d *watchingDriver) List(context.Context, driver.Filter) ([]driver.State, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lists++
	if d.listErr != nil {
		return nil, d.listErr
	}
	return slices.Clone(d.states), nil
}

func (d *watchingDriver) fail(watches int, listErr error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failing, d.listErr = watches, listErr
}

func (d *watchingDriver) set(states ...driver.State) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.states = states
}

func (d *watchingDriver) counts() (lists, watches int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lists, d.watches
}

// feed is the channel of the next Watch the worker opens.
func (d *watchingDriver) feed(t *testing.T) chan remote.Event {
	t.Helper()
	select {
	case feed := <-d.opened:
		return feed
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never watched its driver")
		return nil
	}
}

// next is the next event the control plane's consumer receives.
func next(t *testing.T, events <-chan remote.Event) remote.Event {
	t.Helper()
	select {
	case e, open := <-events:
		if !open {
			t.Fatal("the control plane's watch closed")
		}
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("no event crossed the seam inside the deadline")
		return remote.Event{}
	}
}

// ids is the sandboxes the remote driver lists.
func ids(t *testing.T, d *remote.Driver) []string {
	t.Helper()
	states, err := d.List(t.Context(), driver.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(states))
	for _, s := range states {
		out = append(out, s.ID)
	}
	slices.Sort(out)
	return out
}

// openWatchSeam is a seam whose worker reports its whole list once, at the
// start, so every later change the control plane holds arrived as an event
// or as the List a relist caused.
func openWatchSeam(t *testing.T, host driver.Driver) *seam {
	t.Helper()
	return openSeamOver(t, host, remote.HubOptions{Offline: time.Minute},
		remote.ServerOptions{ReportInterval: time.Hour}, seamWrap{})
}

// TestRemoteWatch is design 021's row: the events of a worker's driver cross
// the seam to the control plane's Watch in the order they happened, and what
// they change is what Inspect and List answer. A relist makes the control
// plane issue a List on the worker's stream, and the relist reaches the
// consumer only once that List's answer is in place, so a consumer that lists
// on a relist reads the environment as the worker's driver listed it.
func TestRemoteWatch(t *testing.T) {
	host := newWatchingDriver()
	s := openWatchSeam(t, host)
	ctx := t.Context()
	events, err := s.driver.Watch(ctx)
	if err != nil {
		t.Fatalf("the watch failed: %v", err)
	}
	feed := host.feed(t)

	a := driver.State{ID: "sbx_a", Name: "a", Owner: "ops", Phase: driver.Running}
	b := driver.State{ID: "sbx_b", Name: "b", Owner: "ops", Phase: driver.Pending}
	stopped := a
	stopped.Phase = driver.Stopped
	sent := []remote.Event{
		{Type: remote.EventAdded, State: a},
		{Type: remote.EventAdded, State: b},
		{Type: remote.EventModified, State: stopped},
		{Type: remote.EventLost, State: b},
		{Type: remote.EventDeleted, State: stopped},
	}
	for i, e := range sent {
		feed <- e
		got := next(t, events)
		if got.Type != e.Type || got.State.ID != e.State.ID || got.State.Phase != e.State.Phase {
			t.Fatalf("event %d crossed as %s %s %s, want %s %s %s",
				i, got.Type, got.State.ID, got.State.Phase, e.Type, e.State.ID, e.State.Phase)
		}
		if i == 2 {
			// The modification is what Inspect answers, without an
			// operation on the worker: the fake's Inspect would answer an
			// empty state.
			state, inspectErr := s.driver.Inspect(ctx, a.ID)
			if inspectErr != nil || state.Phase != driver.Stopped || state.Owner != "ops" {
				t.Errorf("Inspect after the modification reads %+v (%v)", state, inspectErr)
			}
		}
	}
	if got := ids(t, s.driver); !slices.Equal(got, []string{"sbx_b"}) {
		t.Errorf("after the events the environment lists %v, want the one not deleted", got)
	}

	// A relist: the worker's driver now holds two sandboxes no event named.
	c := driver.State{ID: "sbx_c", Name: "c", Owner: "ops", Phase: driver.Running}
	d := driver.State{ID: "sbx_d", Name: "d", Owner: "ops", Phase: driver.Running}
	host.set(c, d)
	before, _ := host.counts()
	feed <- remote.Event{Type: remote.EventRelist}
	if got := next(t, events); got.Type != remote.EventRelist {
		t.Fatalf("the relist crossed as %s", got.Type)
	}
	// Listed at once, with nothing waited on: the List's answer was in place
	// before the relist was delivered.
	if got := ids(t, s.driver); !slices.Equal(got, []string{"sbx_c", "sbx_d"}) {
		t.Errorf("on the relist the environment lists %v, want what the worker's driver listed", got)
	}
	if after, _ := host.counts(); after != before+1 {
		t.Errorf("the relist caused %d Lists on the worker, want one", after-before)
	}

	// A relist whose List fails leaves what the control plane holds as it
	// was, and the consumer is not told to read a state that was not
	// rebuilt; the next event still crosses.
	host.fail(0, errors.New("the substrate stopped answering"))
	feed <- remote.Event{Type: remote.EventRelist}
	feed <- remote.Event{Type: remote.EventAdded, State: a}
	if got := next(t, events); got.Type != remote.EventAdded {
		t.Errorf("after a relist whose List failed the consumer read %s, want the next event", got.Type)
	}
	if got := ids(t, s.driver); !slices.Equal(got, []string{"sbx_a", "sbx_c", "sbx_d"}) {
		t.Errorf("after a relist whose List failed the environment lists %v", got)
	}

	// An environment that is deleted ends its consumers' watch.
	s.hub.Release("env_test")
	select {
	case _, open := <-events:
		if open {
			t.Errorf("a deleted environment's watch delivered an event rather than closing")
		}
	case <-time.After(10 * time.Second):
		t.Errorf("a deleted environment's watch stayed open")
	}
}

// TestAWatchThatFallsBehindRelists holds that the control plane never waits
// for a consumer of Watch: one that has not read for more events than its
// queue holds loses them to one relist, and what they changed is already in
// what List answers.
func TestAWatchThatFallsBehindRelists(t *testing.T) {
	host := newWatchingDriver()
	s := openWatchSeam(t, host)
	events, err := s.driver.Watch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	feed := host.feed(t)
	const sent = 300
	for i := range sent {
		feed <- remote.Event{Type: remote.EventAdded, State: driver.State{ID: "sbx_" + strconv.Itoa(i), Owner: "ops"}}
	}
	waitFor(t, "every event applied", func() bool { return len(ids(t, s.driver)) == sent })

	var got []remote.Event
	for {
		select {
		case e := <-events:
			got = append(got, e)
			continue
		case <-time.After(200 * time.Millisecond):
		}
		break
	}
	if len(got) >= sent {
		t.Errorf("a consumer that fell behind was handed all %d events", len(got))
	}
	relisted := slices.IndexFunc(got, func(e remote.Event) bool { return e.Type == remote.EventRelist })
	if relisted < 0 {
		t.Fatalf("a consumer that fell behind was never told to relist: %d events", len(got))
	}
	if last := got[len(got)-1]; last.State.ID != "sbx_"+strconv.Itoa(sent-1) {
		t.Errorf("the last event is %s %s, want the last one sent", last.Type, last.State.ID)
	}
}

// TestAClosedWatchRelists holds design 004's rule across the seam: a watch
// whose channel closed is followed by a relist, on the worker when its
// driver's channel closes, and on the control plane when the stream that
// carried the watch ends.
func TestAClosedWatchRelists(t *testing.T) {
	host := newWatchingDriver()
	s := openWatchSeam(t, host)
	events, err := s.driver.Watch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first := host.feed(t)
	host.set(driver.State{ID: "sbx_after", Owner: "ops"})
	close(first)
	second := host.feed(t)
	if got := next(t, events); got.Type != remote.EventRelist {
		t.Fatalf("a closed driver watch crossed as %s, want a relist", got.Type)
	}
	if got := ids(t, s.driver); !slices.Equal(got, []string{"sbx_after"}) {
		t.Errorf("after the relist the environment lists %v", got)
	}
	if _, watches := host.counts(); watches != 2 {
		t.Errorf("the worker watched %d times, want again after the close", watches)
	}
	// The second watch carries events as the first did.
	second <- remote.Event{Type: remote.EventAdded, State: driver.State{ID: "sbx_second", Owner: "ops"}}
	if got := next(t, events); got.State.ID != "sbx_second" {
		t.Errorf("the second watch carried %s %s", got.Type, got.State.ID)
	}

	// The driver fails to watch again: the worker's Watch operation ends
	// with the failure, the consumer is told to relist, and the control
	// plane opens a Watch again on the same stream.
	host.fail(1, nil)
	close(second)
	if got := next(t, events); got.Type != remote.EventRelist {
		t.Fatalf("a watch that failed crossed as %s, want a relist", got.Type)
	}
	third := host.feed(t)
	third <- remote.Event{Type: remote.EventAdded, State: driver.State{ID: "sbx_third", Owner: "ops"}}
	for {
		got := next(t, events)
		if got.Type == remote.EventRelist {
			continue // the reopened watch may be announced as a relist first
		}
		if got.State.ID != "sbx_third" {
			t.Errorf("the reopened watch carried %s %s", got.Type, got.State.ID)
		}
		break
	}

	// The stream ends: the consumer is told to relist, because events may
	// be missed until a worker watches again.
	_ = s.workerSide.Close()
	_ = s.control.Close()
	if got := next(t, events); got.Type != remote.EventRelist {
		t.Errorf("a watch whose stream ended crossed as %s, want a relist", got.Type)
	}
}

// TestAWorkerThatDoesNotWatchIsNotAsked holds that the control plane opens a
// Watch only on a worker whose driver said it watches, and that a Watch
// reaching a driver that does not is its refusal, not a hang.
func TestAWorkerThatDoesNotWatchIsNotAsked(t *testing.T) {
	worker := &tap{}
	s := openSeamOver(t, runtimetest.Nop{}, remote.HubOptions{Offline: time.Minute},
		remote.ServerOptions{ReportInterval: time.Hour},
		seamWrap{worker: func(c remote.FrameConn) remote.FrameConn { worker.FrameConn = c; return worker }})
	if err := s.driver.Touch(t.Context(), "sbx_1"); err != nil {
		t.Fatal(err)
	}
	received, _ := worker.seen()
	if slices.Contains(received, remote.MessageOperation+":"+remote.OpWatch) {
		t.Errorf("the control plane opened a Watch on a worker whose driver does not watch")
	}
	if !slices.Contains(received, remote.MessageOperation+":"+remote.OpTouch) {
		t.Fatalf("the tap saw no operation cross, so it proves nothing: %v", received)
	}

	sink := &recordingSink{}
	_, err := remote.Execute(t.Context(), runtimetest.Nop{}, remote.OpWatch, remote.Request{}, sink)
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("a Watch on a driver that does not watch is %v, want ErrUnsupported", err)
	}
	if !errors.Is(sink.accepted, driver.ErrUnsupported) {
		t.Errorf("the refusal was not the operation's acceptance: %v", sink.accepted)
	}

	// A driver that watches and fails to is refused the same way, at the
	// acceptance.
	failing := newWatchingDriver()
	failing.fail(1, nil)
	sink = &recordingSink{}
	if _, err = remote.Execute(t.Context(), failing, remote.OpWatch, remote.Request{}, sink); err == nil || sink.accepted == nil {
		t.Errorf("a Watch the driver failed answered %v and accepted %v", err, sink.accepted)
	}
	// A Watch whose operation is cancelled ends with the cancellation,
	// whether it was relaying or waiting to watch again.
	for _, closeFirst := range []bool{false, true} {
		watching := newWatchingDriver()
		ctx, cancel := context.WithCancel(t.Context())
		ended := make(chan error, 1)
		go func() {
			_, watchErr := remote.Execute(ctx, watching, remote.OpWatch, remote.Request{}, &recordingSink{})
			ended <- watchErr
		}()
		feed := watching.feed(t)
		if closeFirst {
			close(feed)
		}
		cancel()
		select {
		case err = <-ended:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("a cancelled Watch ended with %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a cancelled Watch did not end")
		}
	}
	// A relay that cannot send ends the Watch with that failure.
	watching := newWatchingDriver()
	ended := make(chan error, 1)
	go func() {
		_, watchErr := remote.Execute(t.Context(), watching, remote.OpWatch, remote.Request{},
			&recordingSink{eventErr: remote.ErrLinkClosed})
		ended <- watchErr
	}()
	watching.feed(t) <- remote.Event{Type: remote.EventAdded, State: driver.State{ID: "sbx_1"}}
	if err = <-ended; !errors.Is(err, remote.ErrLinkClosed) {
		t.Errorf("a Watch whose relay failed ended with %v", err)
	}
}

// recordingSink is the worker's side of one operation without a connection:
// what the executor accepted, answered and reported.
type recordingSink struct {
	accepted error
	events   []remote.Event
	eventErr error
}

func (s *recordingSink) Reader(byte) io.Reader               { return strings.NewReader("") }
func (s *recordingSink) Writer(byte) io.WriteCloser          { return nopWriter{} }
func (s *recordingSink) Resizes() <-chan [2]int              { return nil }
func (s *recordingSink) Accept(err error) error              { s.accepted = err; return nil }
func (s *recordingSink) Answer(remote.Response, error) error { return nil }
func (s *recordingSink) Event(e remote.Event) error {
	if s.eventErr != nil {
		return s.eventErr
	}
	s.events = append(s.events, e)
	return nil
}
