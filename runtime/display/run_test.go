// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package display_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/runtime/display"
)

// recorder is a driver's way into a sandbox, faked: it records every command
// and answers each one from a table.
type recorder struct {
	mu   sync.Mutex
	ran  [][]string
	out  map[string]string // a substring of the script, and what it prints
	fail map[string]error  // a substring of the script, and how it fails
}

func (r *recorder) run(_ context.Context, argv []string, stdout io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ran = append(r.ran, argv)
	joined := strings.Join(argv, " ")
	for match, err := range r.fail {
		if strings.Contains(joined, match) {
			return err
		}
	}
	for match, out := range r.out {
		if strings.Contains(joined, match) {
			_, _ = io.WriteString(stdout, out)
		}
	}
	return nil
}

func (r *recorder) commands() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ran
}

func TestReadyAndInstall(t *testing.T) {
	t.Parallel()
	up := &recorder{}
	if err := display.Ready(t.Context(), up.run); err != nil {
		t.Fatalf("Ready on a desktop that answers: %v", err)
	}
	down := &recorder{fail: map[string]error{"xdpyinfo": errors.New("exit 1")}}
	err := display.Ready(t.Context(), down.run)
	if !errors.Is(err, display.ErrNotReady) {
		t.Fatalf("Ready on a desktop that does not answer: %v, want ErrNotReady", err)
	}
	if err = display.Install(t.Context(), up.run, screen); err != nil {
		t.Fatalf("Install: %v", err)
	}
	broken := &recorder{fail: map[string]error{"setsid": errors.New("exit 1")}}
	if err = display.Install(t.Context(), broken.run, screen); err == nil {
		t.Fatal("a supervisor that will not start reported success")
	}
}

func TestCapture(t *testing.T) {
	t.Parallel()
	shot := &recorder{out: map[string]string{"xwd": "\x89PNG"}}
	frame, err := display.Capture(t.Context(), shot.run, display.ScreenshotRequest{})
	if err != nil || string(frame) != "\x89PNG" {
		t.Fatalf("Capture = %q, %v", frame, err)
	}
	if _, err = display.Capture(t.Context(), shot.run, display.ScreenshotRequest{Format: "gif"}); err == nil {
		t.Fatal("an unknown format was captured")
	}
	empty := &recorder{}
	if _, err = display.Capture(t.Context(), empty.run, display.ScreenshotRequest{}); err == nil {
		t.Fatal("an empty capture read as a frame")
	}
	broken := &recorder{fail: map[string]error{"xwd": errors.New("exit 1")}}
	if _, err = display.Capture(t.Context(), broken.run, display.ScreenshotRequest{}); err == nil {
		t.Fatal("a failed capture read as a frame")
	}
}

// TestPerform holds the batch to its order and to where it stops: the events
// before the failure ran, the failure names its index, and the events after it
// did not run.
func TestPerform(t *testing.T) {
	t.Parallel()
	batch := []display.InputEvent{
		{Type: display.TypeMove, X: at(1), Y: at(2)},
		{Type: display.TypeWait, Ms: 1},
		{Type: display.TypeKey, Key: "Return"},
		{Type: display.TypeType, Text: "after"},
	}
	all := &recorder{}
	if err := display.Perform(t.Context(), all.run, batch); err != nil {
		t.Fatalf("Perform: %v", err)
	}
	if got := len(all.commands()); got != 3 {
		t.Fatalf("the batch ran %d commands, want 3: %v", got, all.commands())
	}

	stops := &recorder{fail: map[string]error{"key": errors.New("no such keysym")}}
	err := display.Perform(t.Context(), stops.run, batch)
	var failure *display.InputFailure
	if !errors.As(err, &failure) {
		t.Fatalf("Perform: %v, want an InputFailure", err)
	}
	if failure.Index != 2 || failure.Executed != 2 {
		t.Fatalf("the failure is %+v, want index 2 after two events", failure)
	}
	if got := len(stops.commands()); got != 2 {
		t.Fatalf("%d commands ran, want the move and the key: %v", got, stops.commands())
	}

	// A context that ends during a wait stops the batch at the wait.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	slow := &recorder{}
	err = display.Perform(ctx, slow.run, []display.InputEvent{{Type: display.TypeWait, Ms: display.MaxWaitMS}})
	if !errors.As(err, &failure) || failure.Index != 0 {
		t.Fatalf("a cancelled wait is %v", err)
	}
}

func TestPortsListening(t *testing.T) {
	t.Parallel()
	table := &recorder{out: map[string]string{"/proc/net/tcp": "" +
		"  sl  local_address rem_address   st\n" +
		"   0: 00000000:1F90 00000000:0000 0A 0 0 0\n"}}
	ports, err := display.PortsListening(t.Context(), table.run)
	if err != nil || !ports[8080] {
		t.Fatalf("PortsListening = %v, %v", ports, err)
	}
	broken := &recorder{fail: map[string]error{"/proc/net/tcp": errors.New("exit 1")}}
	if _, err = display.PortsListening(t.Context(), broken.run); err == nil {
		t.Fatal("a failed probe reported ports")
	}
}

// TestFramesDropsForASlowReader is the pacing rule: a consumer that has not
// read the frame it has loses the ones that arrive while it is behind, and
// never queues them.
func TestFramesDropsForASlowReader(t *testing.T) {
	t.Parallel()
	var stream strings.Builder
	for _, body := range []string{"one", "two", "three", "four"} {
		stream.WriteString("0000000" + string(rune('0'+len(body))) + body)
	}
	ended := make(chan struct{})
	frames := display.Frames(t.Context(), io.NopCloser(strings.NewReader(stream.String())),
		display.FormatPNG, func() { close(ended) })
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end")
	}
	var got []string
	for frame := range frames {
		if frame.Format != display.FormatPNG || frame.At.IsZero() {
			t.Fatalf("the frame is %+v", frame)
		}
		got = append(got, string(frame.Data))
	}
	// The channel holds one frame, so a reader that arrives after the stream
	// has ended sees the first frame and no backlog.
	if len(got) != 1 || got[0] != "one" {
		t.Fatalf("the frames are %v, want the one the channel had room for", got)
	}
}

// TestFramesDeliversToAReaderThatKeepsUp is the other half: nothing is dropped
// from a consumer that reads each frame before the next arrives.
func TestFramesDeliversToAReaderThatKeepsUp(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	frames := display.Frames(t.Context(), pr, display.FormatPNG, nil)
	go func() {
		for _, body := range []string{"one", "two", "three"} {
			_, _ = io.WriteString(pw, "00000003"+body[:3])
			time.Sleep(time.Millisecond)
		}
		_ = pw.Close()
	}()
	var got []string
	for frame := range frames {
		got = append(got, string(frame.Data))
	}
	if len(got) == 0 {
		t.Fatal("a reader that keeps up saw no frame")
	}
	if got[0] != "one" {
		t.Fatalf("the frames are %v", got)
	}
}

// TestFramesEndsWithTheContext holds the session to its caller: cancelling the
// context closes the channel and runs the driver's clean-up.
func TestFramesEndsWithTheContext(t *testing.T) {
	t.Parallel()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	ended := make(chan struct{})
	frames := display.Frames(ctx, pr, display.FormatPNG, func() { close(ended) })
	go func() {
		_, _ = io.WriteString(pw, "00000003one")
	}()
	if frame := <-frames; string(frame.Data) != "one" {
		t.Fatalf("the first frame is %q", frame.Data)
	}
	cancel()
	// The reader is still open, so the loop leaves on the cancelled context
	// after it has finished the frame it was reading.
	go func() {
		_, _ = io.WriteString(pw, "00000003two")
		_ = pw.Close()
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled session did not end")
	}
	for range frames { //nolint:revive // draining what the channel still holds
	}
}
