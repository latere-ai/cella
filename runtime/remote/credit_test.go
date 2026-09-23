// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/runtime/runtimetest"
)

// waitFor polls a condition the stream reaches on its own, failing the test
// when it is not reached inside the deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not reached inside the deadline", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// tap sits between one side and its end of the connection and notes every
// control message crossing it each way, operations by their type, which is
// how a test says what one side never sent. With stripHello it removes the
// window and the watch from the hello its side writes, which is the hello of
// a worker of a release before credit.
type tap struct {
	remote.FrameConn
	stripHello bool

	mu            sync.Mutex
	read, written []string
}

func (c *tap) ReadFrame() ([]byte, error) {
	raw, err := c.FrameConn.ReadFrame()
	if err == nil {
		c.note(&c.read, raw)
	}
	return raw, err
}

func (c *tap) WriteFrame(raw []byte) error {
	if c.stripHello {
		raw = withoutCredit(raw)
	}
	c.note(&c.written, raw)
	return c.FrameConn.WriteFrame(raw)
}

func (c *tap) note(list *[]string, raw []byte) {
	_, stream, payload, err := remote.DecodeFrame(raw)
	if err != nil || stream != remote.StreamControl {
		return
	}
	m, err := remote.DecodeMessage(payload)
	if err != nil {
		return
	}
	kind := m.Type
	if m.Type == remote.MessageOperation {
		kind += ":" + remote.OperationType(m)
	}
	c.mu.Lock()
	*list = append(*list, kind)
	c.mu.Unlock()
}

// seen reports what crossed the tap: read is what this side received and
// written what it sent.
func (c *tap) seen() (read, written []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.read...), append([]string(nil), c.written...)
}

// withoutCredit rewrites a hello as a release before credit writes it.
func withoutCredit(raw []byte) []byte {
	operation, stream, payload, err := remote.DecodeFrame(raw)
	if err != nil || stream != remote.StreamControl {
		return raw
	}
	m, err := remote.DecodeMessage(payload)
	if err != nil || m.Type != remote.MessageHello {
		return raw
	}
	m.Window, m.Watch = 0, false
	rewritten, err := remote.EncodeMessage(operation, m)
	if err != nil {
		return raw
	}
	return rewritten
}

// rawLink is one link run against a peer the test speaks for by hand. What
// the link writes back is drained and counted, so the link never waits on a
// peer that is not reading.
type rawLink struct {
	link  *remote.Link
	peer  *pipeConn
	ended chan error

	mu       sync.Mutex
	messages []remote.Message
	bytes    map[string]int
}

func openRawLink(t *testing.T, o remote.LinkOptions) *rawLink {
	t.Helper()
	near, far := net.Pipe()
	l := &rawLink{
		link: remote.NewLink(&pipeConn{conn: near}, o), peer: &pipeConn{conn: far},
		ended: make(chan error, 1), bytes: map[string]int{},
	}
	go func() { l.ended <- l.link.Run(context.Background()) }()
	go func() {
		for {
			raw, err := l.peer.ReadFrame()
			if err != nil {
				return
			}
			operation, stream, payload, err := remote.DecodeFrame(raw)
			if err != nil {
				continue
			}
			l.mu.Lock()
			if stream == remote.StreamControl {
				if m, decodeErr := remote.DecodeMessage(payload); decodeErr == nil {
					l.messages = append(l.messages, m)
				}
			} else {
				l.bytes[operation+string(rune('0'+stream))] += len(payload)
			}
			l.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		l.link.Shutdown(nil)
		_ = far.Close()
	})
	return l
}

// send writes one control message from the peer.
func (l *rawLink) send(t *testing.T, operation string, m remote.Message) {
	t.Helper()
	raw, err := remote.EncodeMessage(operation, m)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.peer.WriteFrame(raw); err != nil {
		t.Fatalf("the peer could not write %s: %v", m.Type, err)
	}
}

// frame writes one sub-stream frame from the peer.
func (l *rawLink) frame(t *testing.T, operation string, stream byte, payload []byte) {
	t.Helper()
	raw, err := remote.EncodeFrame(operation, stream, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.peer.WriteFrame(raw); err != nil {
		t.Fatalf("the peer could not write a frame: %v", err)
	}
}

// received is how many bytes of one sub-stream the link sent the peer.
func (l *rawLink) received(operation string, stream byte) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bytes[operation+string(rune('0'+stream))]
}

// credits is every credit the link sent the peer.
func (l *rawLink) credits() []remote.Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []remote.Message
	for _, m := range l.messages {
		if m.Type == remote.MessageCredit {
			out = append(out, m)
		}
	}
	return out
}

// closedWith is the error the link ended with.
func (l *rawLink) closedWith(t *testing.T) error {
	t.Helper()
	select {
	case err := <-l.ended:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the link did not close inside the deadline")
		return nil
	}
}

// TestCreditMessages holds the wire of the credit window: the credit, the
// hello's window and watch, and the event keep their field names and read
// back as they were written. Over a live stream both sides agree credit, and
// credit flows both ways: a body larger than either window crosses down and
// back up, which it cannot do on a window nobody replenished.
func TestCreditMessages(t *testing.T) {
	id := remote.NewOperationID()
	state := driver.State{ID: "sbx_1", Name: "one", Owner: "ops", Phase: driver.Running}
	for _, tc := range []struct {
		name   string
		m      remote.Message
		fields map[string]any
	}{
		{"a credit", remote.Message{Type: remote.MessageCredit, Operation: id, Stream: int(remote.StreamStdout), Bytes: 1 << 20},
			map[string]any{"type": "credit", "stream": float64(2), "bytes": float64(1 << 20)}},
		{"a worker's hello", remote.Message{Type: remote.MessageHello, Worker: "wrk_1", Window: remote.DefaultWindow, Watch: true},
			map[string]any{"type": "hello", "worker": "wrk_1", "window": float64(remote.DefaultWindow), "watch": true}},
		{"a control plane's hello", remote.Message{Type: remote.MessageHello, Window: 1 << 16},
			map[string]any{"type": "hello", "window": float64(1 << 16)}},
		{"an event", remote.Message{Type: remote.MessageEvent, Operation: id, Event: &remote.Event{Type: remote.EventAdded, State: state}},
			map[string]any{"type": "event", "event": map[string]any{"type": "added"}}},
		{"a relist", remote.Message{Type: remote.MessageEvent, Operation: id, Event: &remote.Event{Type: remote.EventRelist}},
			map[string]any{"type": "event", "event": map[string]any{"type": "relist"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := remote.EncodeMessage(id, tc.m)
			if err != nil {
				t.Fatalf("the message did not encode: %v", err)
			}
			_, _, payload, err := remote.DecodeFrame(raw)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err = json.Unmarshal(payload, &wire); err != nil {
				t.Fatal(err)
			}
			for key, want := range tc.fields {
				got, held := wire[key]
				if !held {
					t.Errorf("the wire carries no %q: %s", key, payload)
					continue
				}
				if nested, isMap := want.(map[string]any); isMap {
					for k, v := range nested {
						if got.(map[string]any)[k] != v {
							t.Errorf("%s.%s is %v on the wire, want %v", key, k, got.(map[string]any)[k], v)
						}
					}
					continue
				}
				if got != want {
					t.Errorf("%s is %v on the wire, want %v", key, got, want)
				}
			}
			back, err := remote.DecodeMessage(payload)
			if err != nil {
				t.Fatalf("the message did not decode: %v", err)
			}
			if !reflect.DeepEqual(back, tc.m) {
				t.Errorf("the message read back as %+v, want %+v", back, tc.m)
			}
		})
	}
	// A relist says nothing about a sandbox, and the wire does not pretend it
	// does.
	raw, err := json.Marshal(remote.Event{Type: remote.EventRelist})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "state") {
		t.Errorf("a relist carries a state on the wire: %s", raw)
	}

	host, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	const window = 64 << 10
	s := openSeamOver(t, host, remote.HubOptions{Offline: time.Minute, Window: window},
		remote.ServerOptions{Window: window, ReportInterval: time.Minute}, seamWrap{})
	if !s.hub.Credited("env_test") || !s.server.Credited() {
		t.Fatalf("the two sides did not agree credit: control plane %v, worker %v",
			s.hub.Credited("env_test"), s.server.Credited())
	}
	ctx := t.Context()
	ref, err := s.driver.Create(ctx, driver.CreateSpec{ID: "sbx_credit", Name: "credit", Owner: "ops", Command: []string{"sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.driver.Delete(context.Background(), ref.ID) })
	body := bytes.Repeat([]byte("sixteen windows "), 16*window/16)
	if _, err = s.driver.Write(ctx, ref.ID, driver.WriteRequest{Path: "/workspace/credit.bin", Body: bytes.NewReader(body)}); err != nil {
		t.Fatalf("a body of sixteen windows did not cross down: %v", err)
	}
	reader, _, err := s.driver.Open(ctx, ref.ID, "/workspace/credit.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("a body of sixteen windows did not cross back up whole: %d bytes, %v", len(got), err)
	}
	if peak := s.hub.Peak("env_test"); peak > window {
		t.Errorf("the control plane held %d bytes of one sub-stream, past its window of %d", peak, window)
	}
	if peak := s.server.Peak(); peak > window {
		t.Errorf("the worker held %d bytes of one sub-stream, past its window of %d", peak, window)
	}
}

// TestAMisbehavingPeerIsClosed holds the refusals of the window: a peer that
// sends past what it was credited, grants what it cannot, credits control or
// a sub-stream the connection does not credit, or agrees the window twice is
// closed with ErrFrame, because a receiver that tolerated one could no longer
// say what it holds.
func TestAMisbehavingPeerIsClosed(t *testing.T) {
	const window = 4096
	for _, tc := range []struct {
		name   string
		credit bool
		act    func(t *testing.T, l *rawLink, op string)
	}{
		{"bytes past the window", true, func(t *testing.T, l *rawLink, op string) {
			l.frame(t, op, remote.StreamStdout, make([]byte, window))
			l.frame(t, op, remote.StreamStdout, []byte{0})
		}},
		{"a credit of nothing", true, func(t *testing.T, l *rawLink, op string) {
			l.send(t, op, remote.Message{Type: remote.MessageCredit, Stream: int(remote.StreamStdin)})
		}},
		{"a credit below nothing", true, func(t *testing.T, l *rawLink, op string) {
			l.send(t, op, remote.Message{Type: remote.MessageCredit, Stream: int(remote.StreamStdin), Bytes: -1})
		}},
		{"a credit on control", true, func(t *testing.T, l *rawLink, op string) {
			l.send(t, op, remote.Message{Type: remote.MessageCredit, Stream: int(remote.StreamControl), Bytes: 1})
		}},
		{"a credit on no sub-stream", true, func(t *testing.T, l *rawLink, op string) {
			l.send(t, op, remote.Message{Type: remote.MessageCredit, Stream: 256, Bytes: 1})
		}},
		{"a credit past the window", true, func(t *testing.T, l *rawLink, op string) {
			l.send(t, op, remote.Message{Type: remote.MessageCredit, Stream: int(remote.StreamStdin), Bytes: 1})
		}},
		{"a credit on a connection without credit", false, func(t *testing.T, l *rawLink, op string) {
			l.send(t, op, remote.Message{Type: remote.MessageCredit, Stream: int(remote.StreamStdin), Bytes: 1})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := openRawLink(t, remote.LinkOptions{Window: window})
			if tc.credit {
				if err := l.link.Credit(window); err != nil {
					t.Fatal(err)
				}
			}
			op := remote.NewOperationID()
			l.link.Open(op, false)
			tc.act(t, l, op)
			if err := l.closedWith(t); !errors.Is(err, remote.ErrFrame) {
				t.Errorf("the link closed with %v, want ErrFrame", err)
			}
		})
	}

	t.Run("a sub-stream at the window exactly", func(t *testing.T) {
		l := openRawLink(t, remote.LinkOptions{Window: window})
		if err := l.link.Credit(window); err != nil {
			t.Fatal(err)
		}
		op := remote.NewOperationID()
		channel := l.link.Open(op, false)
		l.frame(t, op, remote.StreamStdout, make([]byte, window))
		got := make([]byte, window)
		if _, err := io.ReadFull(channel.Up(remote.StreamStdout), got); err != nil {
			t.Fatalf("a sub-stream at its window did not read: %v", err)
		}
		// What was read was credited back, so the peer may send a window more.
		waitFor(t, "the credit for what was read", func() bool {
			var granted int64
			for _, m := range l.credits() {
				granted += m.Bytes
			}
			return granted == window
		})
		l.frame(t, op, remote.StreamStdout, make([]byte, window))
		if _, err := io.ReadFull(channel.Up(remote.StreamStdout), got); err != nil {
			t.Fatalf("the credited window did not read: %v", err)
		}
		select {
		case err := <-l.ended:
			t.Errorf("a peer inside its window was closed: %v", err)
		default:
		}
	})

	t.Run("the window agreed twice or of nothing", func(t *testing.T) {
		l := openRawLink(t, remote.LinkOptions{})
		for _, bad := range []int{0, -1} {
			if err := l.link.Credit(bad); !errors.Is(err, remote.ErrFrame) {
				t.Errorf("a window of %d was agreed: %v", bad, err)
			}
		}
		if err := l.link.Credit(window); err != nil {
			t.Fatal(err)
		}
		if err := l.link.Credit(window); !errors.Is(err, remote.ErrFrame) {
			t.Errorf("the window was agreed twice: %v", err)
		}
		if l.link.Window() != remote.DefaultWindow {
			t.Errorf("a link given no window grants %d, want DefaultWindow", l.link.Window())
		}
	})

	t.Run("hellos the control plane refuses", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			frames func(worker string) []remote.Message
			// want is what Serve returns: a stream that never bound to a
			// registration is the unknown worker, whatever refused it.
			want error
		}{
			{"a window below nothing", func(worker string) []remote.Message {
				return []remote.Message{{Type: remote.MessageHello, Worker: worker, Window: -1}}
			}, remote.ErrUnknownWorker},
			{"a second hello", func(worker string) []remote.Message {
				return []remote.Message{
					{Type: remote.MessageHello, Worker: worker, Window: window},
					{Type: remote.MessageHello, Worker: worker, Window: window},
				}
			}, remote.ErrFrame},
			{"an event with none in it", func(worker string) []remote.Message {
				return []remote.Message{{Type: remote.MessageHello, Worker: worker}, {Type: remote.MessageEvent}}
			}, remote.ErrFrame},
			{"an event about no sandbox", func(worker string) []remote.Message {
				return []remote.Message{
					{Type: remote.MessageHello, Worker: worker},
					{Type: remote.MessageEvent, Event: &remote.Event{Type: remote.EventAdded}},
				}
			}, remote.ErrFrame},
		} {
			t.Run(tc.name, func(t *testing.T) {
				hub := remote.NewHub(remote.HubOptions{})
				registered, err := hub.Register("env_a", remote.Registration{Driver: "native", Isolation: driver.IsolationNone})
				if err != nil {
					t.Fatal(err)
				}
				control, workerSide := net.Pipe()
				t.Cleanup(func() { _ = workerSide.Close() })
				served := make(chan error, 1)
				go func() { served <- hub.Serve(t.Context(), "env_a", &pipeConn{conn: control}) }()
				peer := &pipeConn{conn: workerSide}
				go func() {
					for {
						if _, readErr := peer.ReadFrame(); readErr != nil {
							return
						}
					}
				}()
				for _, m := range tc.frames(registered.Worker) {
					raw, encodeErr := remote.EncodeMessage(remote.NoOperation, m)
					if encodeErr != nil {
						t.Fatal(encodeErr)
					}
					if writeErr := peer.WriteFrame(raw); writeErr != nil {
						break // the control plane already closed the stream
					}
				}
				select {
				case err = <-served:
					if !errors.Is(err, tc.want) {
						t.Errorf("the stream ended with %v, want %v", err, tc.want)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("the control plane kept a stream that broke the protocol")
				}
			})
		}
	})

	t.Run("a window the worker refuses", func(t *testing.T) {
		control, workerSide := net.Pipe()
		t.Cleanup(func() { _ = control.Close() })
		server := remote.NewServer(&pipeConn{conn: workerSide}, remote.ServerOptions{Worker: "wrk_1", Driver: runtimetest.Nop{}})
		ran := make(chan error, 1)
		go func() { ran <- server.Run(t.Context()) }()
		peer := &pipeConn{conn: control}
		go func() {
			for {
				if _, err := peer.ReadFrame(); err != nil {
					return
				}
			}
		}()
		raw, err := remote.EncodeMessage(remote.NoOperation, remote.Message{Type: remote.MessageHello, Window: -1})
		if err != nil {
			t.Fatal(err)
		}
		if err = peer.WriteFrame(raw); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-ran:
			if !errors.Is(err, remote.ErrFrame) {
				t.Errorf("the worker's stream ended with %v, want ErrFrame", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the worker kept a stream whose window it could not use")
		}
	})
}

// TestAWaitingWriterIsReleased holds that a writer waiting for credit never
// waits on a grant that will not come. It continues on a grant, and returns
// when the caller closes the operation, when the far side answers or cancels
// it, and when the connection ends.
func TestAWaitingWriterIsReleased(t *testing.T) {
	const window = 4096
	for _, tc := range []struct {
		name    string
		release func(t *testing.T, l *rawLink, channel *remote.Channel, op string)
		want    error
	}{
		{"the caller closes", func(t *testing.T, _ *rawLink, channel *remote.Channel, _ string) {
			if err := channel.Close(); err != nil {
				t.Errorf("the close failed: %v", err)
			}
		}, io.ErrClosedPipe},
		{"the far side answers", func(t *testing.T, l *rawLink, _ *remote.Channel, op string) {
			l.send(t, op, remote.Message{Type: remote.MessageResult, Operation: op, OK: true, Response: &remote.Response{}})
		}, io.ErrClosedPipe},
		{"the far side cancels", func(t *testing.T, l *rawLink, _ *remote.Channel, op string) {
			l.send(t, op, remote.Message{Type: remote.MessageCancel, Operation: op})
		}, io.ErrClosedPipe},
		{"the connection ends", func(_ *testing.T, l *rawLink, _ *remote.Channel, _ string) {
			_ = l.peer.Close()
		}, remote.ErrLinkClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := openRawLink(t, remote.LinkOptions{Window: window})
			if err := l.link.Credit(window); err != nil {
				t.Fatal(err)
			}
			op := remote.NewOperationID()
			channel := l.link.Open(op, false)
			type written struct {
				n   int
				err error
			}
			done := make(chan written, 1)
			go func() {
				n, err := channel.Writer(remote.StreamStdin).Write(make([]byte, 4*window))
				done <- written{n, err}
			}()
			waitFor(t, "the first window sent", func() bool { return l.received(op, remote.StreamStdin) == window })
			// A grant is a window more, and no more.
			l.send(t, op, remote.Message{Type: remote.MessageCredit, Stream: int(remote.StreamStdin), Bytes: window})
			waitFor(t, "the granted window sent", func() bool { return l.received(op, remote.StreamStdin) == 2*window })
			select {
			case w := <-done:
				t.Fatalf("the writer returned (%d, %v) with half its body unsent and no credit", w.n, w.err)
			case <-time.After(50 * time.Millisecond):
			}
			tc.release(t, l, channel, op)
			select {
			case w := <-done:
				if w.n != 2*window {
					t.Errorf("the writer reports %d bytes written, want the %d it was credited", w.n, 2*window)
				}
				if !errors.Is(w.err, tc.want) {
					t.Errorf("the writer returned %v, want %v", w.err, tc.want)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("a writer waiting for credit was never released")
			}
		})
	}

	// A body the worker refused part way ends the control plane's copy at
	// once: the worker answered and reads nothing more, and the copy reads
	// the refusal rather than waiting for credit.
	t.Run("a body the worker refused", func(t *testing.T) {
		host, err := native.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = host.Close() })
		s := openSeam(t, host)
		ctx := t.Context()
		ref, err := s.driver.Create(ctx, driver.CreateSpec{ID: "sbx_refused", Name: "refused", Owner: "ops", Command: []string{"sleep", "30"}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.driver.Delete(context.Background(), ref.ID) })
		body := &countingReader{remaining: 16 * remote.DefaultWindow}
		done := make(chan error, 1)
		go func() {
			_, writeErr := s.driver.Write(ctx, ref.ID, driver.WriteRequest{Path: "/workspace/big", MaxBytes: 1024, Body: body})
			done <- writeErr
		}()
		select {
		case err = <-done:
			if !errors.Is(err, driver.ErrTooLarge) {
				t.Errorf("the write ended with %v, want ErrTooLarge", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("a body past the bound held its write after the worker refused it")
		}
		if read := body.read(); read > 2*remote.DefaultWindow {
			t.Errorf("the control plane read %d bytes of a body the worker refused at 1 KiB", read)
		}
	})
}

// countingReader is a body of zeros that counts what was read of it.
type countingReader struct {
	mu        sync.Mutex
	remaining int
	taken     int
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	clear(p[:n])
	r.remaining -= n
	r.taken += n
	return n, nil
}

func (r *countingReader) read() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.taken
}

// TestResizesNeverHoldThePump holds that a terminal's windows never wait on
// the read pump: a queue that is full gives its oldest window way, and every
// other message on the connection keeps moving.
func TestResizesNeverHoldThePump(t *testing.T) {
	l := openRawLink(t, remote.LinkOptions{})
	attach := remote.NewOperationID()
	terminal := l.link.Open(attach, true)
	other := remote.NewOperationID()
	answered := l.link.Open(other, false)
	const sent = 40
	for i := 1; i <= sent; i++ {
		l.send(t, attach, remote.Message{Type: remote.MessageResize, Cols: i, Rows: i})
	}
	l.send(t, other, remote.Message{Type: remote.MessageResult, Operation: other, OK: true, Response: &remote.Response{}})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := answered.Result(ctx); err != nil {
		t.Fatalf("a result behind a burst of resizes never arrived: %v", err)
	}
	var last [2]int
	held := 0
	for {
		select {
		case window := <-terminal.Resizes():
			last = window
			held++
			continue
		default:
		}
		break
	}
	if last != [2]int{sent, sent} {
		t.Errorf("the newest window the terminal holds is %v, want the last one sent", last)
	}
	if held == 0 || held >= sent {
		t.Errorf("the terminal holds %d of %d windows, want the newest few", held, sent)
	}
}

// TestTheWorkerTakesTheHeartbeat holds that the control plane's heartbeat is
// a heartbeat to the worker and not a frame that belongs the other way, and
// that the control plane's hello turns credit on. A frame that does belong
// the other way is still reported, once.
func TestTheWorkerTakesTheHeartbeat(t *testing.T) {
	var mu sync.Mutex
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logged.Write(p)
	}), nil))
	control, workerSide := net.Pipe()
	t.Cleanup(func() { _ = control.Close() })
	server := remote.NewServer(&pipeConn{conn: workerSide}, remote.ServerOptions{Worker: "wrk_1", Driver: runtimetest.Nop{}, Log: log})
	go func() { _ = server.Run(t.Context()) }()
	peer := &pipeConn{conn: control}
	go func() {
		for {
			if _, err := peer.ReadFrame(); err != nil {
				return
			}
		}
	}()
	for _, m := range []remote.Message{
		{Type: remote.MessageHeartbeat},
		{Type: remote.MessageHello, Window: remote.DefaultWindow},
		{Type: remote.MessageState},
	} {
		raw, err := remote.EncodeMessage(remote.NoOperation, m)
		if err != nil {
			t.Fatal(err)
		}
		if err = peer.WriteFrame(raw); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		mu.Lock()
		defer mu.Unlock()
		return logged.String()
	}
	waitFor(t, "the frame that belongs the other way reported", func() bool { return strings.Contains(read(), "frame=state") })
	if strings.Contains(read(), "frame=heartbeat") {
		t.Errorf("the worker reported the control plane's heartbeat as a frame that belongs the other way:\n%s", read())
	}
	if n := strings.Count(read(), "belongs the other way"); n != 1 {
		t.Errorf("the worker reported %d frames that belong the other way, want the one:\n%s", n, read())
	}
	if !server.Credited() {
		t.Errorf("the control plane's hello did not turn credit on")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
