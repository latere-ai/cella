// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// FrameConn is the connection under the protocol, as either side holds it.
// One WebSocket implements it on the worker and one on the control plane, and
// a pair of pipes implements it in a test, so the protocol above is proven
// without a listener.
type FrameConn interface {
	// ReadFrame returns the next whole frame, or the error that ended the
	// connection.
	ReadFrame() ([]byte, error)
	// WriteFrame sends one whole frame. It is called by one writer only,
	// which is the WebSocket contract and is what the link guarantees.
	WriteFrame([]byte) error
	Close() error
}

// outBuffer is how many frames may queue for one side before the link treats
// it as gone. A peer that cannot keep up is dropped rather than allowed to
// grow the other side's memory; its reconnect starts over.
const outBuffer = 256

// ErrLinkClosed is every call on a link whose connection ended.
var ErrLinkClosed = errors.New("remote: the worker stream is closed")

// LinkOptions configures one link.
type LinkOptions struct {
	// OnMessage receives every control message that is not a sub-stream
	// frame and not an answer the link routed itself. It runs on the read
	// pump, so a handler that blocks holds the connection.
	OnMessage func(operation string, m Message)
}

// Link is one worker stream, whichever side holds it: the frames in and out,
// the sub-streams of every operation on it, and the control messages. It has
// exactly one writer, which is what a WebSocket requires.
type Link struct {
	conn      FrameConn
	onMessage func(string, Message)
	out       chan []byte
	done      chan struct{}
	closeOnce sync.Once
	failure   atomic.Pointer[error]

	mu       sync.Mutex
	channels map[string]*Channel
}

// NewLink wraps one connection. Run drives it; nothing is read or written
// before.
func NewLink(conn FrameConn, o LinkOptions) *Link {
	return &Link{
		conn: conn, onMessage: o.OnMessage,
		out: make(chan []byte, outBuffer), done: make(chan struct{}),
		channels: map[string]*Channel{},
	}
}

// Run holds the connection open until it ends or the context does, and
// returns what ended it. It is the link's read pump; the write pump runs
// beside it.
func (l *Link) Run(ctx context.Context) error {
	go l.writePump(ctx)
	defer l.Shutdown(nil)
	for {
		raw, err := l.conn.ReadFrame()
		if err != nil {
			l.Shutdown(err)
			return err
		}
		operation, stream, payload, err := DecodeFrame(raw)
		if err != nil {
			l.Shutdown(err)
			return err
		}
		if stream != StreamControl {
			l.deliver(operation, stream, payload)
			continue
		}
		m, err := DecodeMessage(payload)
		if err != nil {
			l.Shutdown(err)
			return err
		}
		l.route(operation, m)
	}
}

// route answers the messages the link owns and hands the rest to the caller.
// A message about an operation this side has already dropped is discarded:
// the operation is over here and the peer has not learned it yet, which is
// ordinary on every stream a caller closed early.
func (l *Link) route(operation string, m Message) {
	switch m.Type {
	case MessageResult, MessageStarted, MessageResize, MessageCancel:
		c, held := l.channel(operation)
		if !held {
			return
		}
		switch m.Type {
		case MessageResult:
			c.answer(m)
		case MessageStarted:
			c.accepted(m)
		case MessageResize:
			if c.resizes == nil {
				return
			}
			select {
			case c.resizes <- [2]int{m.Cols, m.Rows}:
			case <-l.done:
			}
		case MessageCancel:
			c.cancel()
		}
		return
	}
	if l.onMessage != nil {
		l.onMessage(operation, m)
	}
}

// deliver writes one sub-stream frame into the channel's pipe. A zero-length
// frame ends the sub-stream. A frame for an operation this side has dropped
// is discarded: the operation is over and the peer has not learned it yet.
func (l *Link) deliver(operation string, stream byte, payload []byte) {
	c, held := l.channel(operation)
	if !held {
		return
	}
	c.write(stream, payload)
}

func (l *Link) channel(operation string) (*Channel, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, held := l.channels[operation]
	return c, held
}

// Open registers one operation's channel. withResizes makes the resize
// channel, which only an attach needs.
func (l *Link) Open(operation string, withResizes bool) *Channel {
	c := &Channel{
		link: l, operation: operation,
		readers: map[byte]*io.PipeReader{}, writers: map[byte]*io.PipeWriter{},
		result: make(chan Message, 1), started: make(chan Message, 1),
		cancelled: make(chan struct{}),
	}
	if withResizes {
		c.resizes = make(chan [2]int, 8)
	}
	l.mu.Lock()
	l.channels[operation] = c
	l.mu.Unlock()
	return c
}

// Drop forgets one operation's channel and ends every sub-stream it holds,
// without telling the peer. Close is what tells the peer.
func (l *Link) Drop(operation string) {
	l.mu.Lock()
	c, held := l.channels[operation]
	delete(l.channels, operation)
	l.mu.Unlock()
	if held {
		c.end(ErrLinkClosed)
	}
}

// Send queues one control message. A link whose connection ended is
// ErrLinkClosed.
func (l *Link) Send(operation string, m Message) error {
	raw, err := EncodeMessage(operation, m)
	if err != nil {
		return err
	}
	return l.queue(raw)
}

// SendFrame queues one sub-stream frame.
func (l *Link) SendFrame(operation string, stream byte, payload []byte) error {
	raw, err := EncodeFrame(operation, stream, payload)
	if err != nil {
		return err
	}
	return l.queue(raw)
}

func (l *Link) queue(raw []byte) error {
	select {
	case <-l.done:
		return l.closedErr()
	default:
	}
	select {
	case l.out <- raw:
		return nil
	case <-l.done:
		return l.closedErr()
	}
}

// closedErr is what a call on a link that ended reads: ErrLinkClosed, so a
// caller tells the connection being gone from a failure of its own act, and
// the reason the connection ended beside it.
func (l *Link) closedErr() error {
	err := l.Err()
	if err == nil || errors.Is(err, ErrLinkClosed) {
		return ErrLinkClosed
	}
	return fmt.Errorf("%w: %w", ErrLinkClosed, err)
}

// Shutdown ends the link and every operation on it. The first error recorded
// is the one every caller reads, because it is the one that ended the
// connection.
func (l *Link) Shutdown(err error) {
	l.closeOnce.Do(func() {
		if err == nil {
			err = ErrLinkClosed
		}
		l.failure.Store(&err)
		close(l.done)
		_ = l.conn.Close()
		l.mu.Lock()
		channels := make([]*Channel, 0, len(l.channels))
		for _, c := range l.channels {
			channels = append(channels, c)
		}
		l.channels = map[string]*Channel{}
		l.mu.Unlock()
		for _, c := range channels {
			c.end(err)
		}
	})
}

// Done is closed when the link ends.
func (l *Link) Done() <-chan struct{} { return l.done }

// Err is what ended the link, or nil while it is open.
func (l *Link) Err() error {
	if e := l.failure.Load(); e != nil {
		return *e
	}
	return nil
}

// writePump is the link's one writer: every frame queued, until the link ends
// or the context does.
func (l *Link) writePump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			l.Shutdown(ctx.Err())
			return
		case <-l.done:
			return
		case raw := <-l.out:
			if err := l.conn.WriteFrame(raw); err != nil {
				l.Shutdown(err)
				return
			}
		}
	}
}

// Channel is one operation's sub-streams and its answer. It satisfies Stream
// on the control plane and Sink on the worker, which is the same set of
// sub-streams read from the two ends.
type Channel struct {
	link      *Link
	operation string

	mu      sync.Mutex
	readers map[byte]*io.PipeReader
	writers map[byte]*io.PipeWriter
	// ended is why the operation stopped, once it has. A sub-stream asked
	// for after that is handed back already ended: a caller that starts
	// reading an operation the connection has already lost must read the
	// reason rather than wait for bytes nobody will send.
	ended error

	resizes      chan [2]int
	result       chan Message
	answered     sync.Once
	sent         sync.Once
	started      chan Message
	acceptedOnce sync.Once
	cancelled    chan struct{}
	cancelOnce   sync.Once
	closed       atomic.Bool
}

// Reader is one sub-stream coming in, ending in io.EOF at its zero-length
// frame. Asking for a sub-stream twice returns the same reader.
func (c *Channel) Reader(stream byte) io.Reader { return c.pipe(stream) }

// Up is Reader under the name the control plane's Stream contract uses.
func (c *Channel) Up(stream byte) io.Reader { return c.pipe(stream) }

func (c *Channel) pipe(stream byte) *io.PipeReader {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, held := c.readers[stream]; held {
		return r
	}
	r, w := io.Pipe()
	c.readers[stream], c.writers[stream] = r, w
	if c.ended != nil {
		_ = w.CloseWithError(c.ended)
	}
	return r
}

// write puts one incoming frame into a sub-stream's pipe. A zero-length frame
// ends it.
//
// The write runs on the read pump and an io.Pipe blocks until it is read, so
// a sub-stream whose reader has stopped holds this connection. That is the
// only back pressure the protocol has until the credit window of design 021
// lands, and it is why an operation that answers and then streams sends its
// answer first: the far side must never be waiting for a result that is
// queued behind bytes it has not started reading.
func (c *Channel) write(stream byte, payload []byte) {
	c.pipe(stream)
	c.mu.Lock()
	w := c.writers[stream]
	c.mu.Unlock()
	if w == nil {
		return
	}
	if len(payload) == 0 {
		_ = w.Close()
		return
	}
	if _, err := w.Write(payload); err != nil {
		_ = w.CloseWithError(err)
	}
}

// Writer is one sub-stream going out. Closing it writes the zero-length frame
// that ends the sub-stream for the other side.
func (c *Channel) Writer(stream byte) io.WriteCloser {
	return &substream{channel: c, stream: stream}
}

// Down is Writer under the name the control plane's Stream contract uses.
func (c *Channel) Down(stream byte) io.WriteCloser { return c.Writer(stream) }

// Resizes carries the windows the control plane asked for. It is nil on an
// operation that is not an attach.
func (c *Channel) Resizes() <-chan [2]int { return c.resizes }

// Resize asks the far side for a window.
func (c *Channel) Resize(cols, rows int) error {
	return c.link.Send(c.operation, Message{Type: MessageResize, Cols: cols, Rows: rows})
}

// Result waits for the operation's answer. A link that ended first is the
// error that ended it, so a caller never waits on a worker that went away.
func (c *Channel) Result(ctx context.Context) (Response, error) {
	select {
	case m := <-c.result:
		if m.Error != nil {
			return Response{}, m.Error.Err()
		}
		if m.Response == nil {
			return Response{}, nil
		}
		return *m.Response, nil
	case <-c.cancelled:
		return Response{}, ErrLinkClosed
	case <-c.link.Done():
		return Response{}, c.link.Err()
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

// Accept says the driver call was created and its sub-streams are live, or
// that the driver refused before any of them existed. It is the worker's
// call, on the operations that stream for their whole life.
func (c *Channel) Accept(err error) error {
	m := Message{Type: MessageStarted, Operation: c.operation, OK: err == nil}
	if err != nil {
		m.Error = EncodeError(err)
	}
	return c.link.Send(c.operation, m)
}

func (c *Channel) accepted(m Message) {
	c.acceptedOnce.Do(func() { c.started <- m })
}

// Accepted waits for the worker to say the driver call was created. A worker
// that answered the operation instead refused it before the call existed, and
// that refusal is what a caller of Exec, Attach or Logs reads.
func (c *Channel) Accepted(ctx context.Context) error {
	select {
	case m := <-c.started:
		if m.Error != nil {
			return m.Error.Err()
		}
		return nil
	case m := <-c.result:
		// Put it back: the caller may still wait on the result, and an
		// operation that answered before it started answered with its
		// refusal.
		c.answered.Do(func() {})
		c.result <- m
		if m.Error != nil {
			return m.Error.Err()
		}
		return nil
	case <-c.cancelled:
		return ErrLinkClosed
	case <-c.link.Done():
		return c.link.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Answer sends the operation's result. It is the worker's call, and one
// operation answers once: an operation that answered before it wrote its
// sub-stream has already said what it will say, and a second answer would be
// a second result for one call.
func (c *Channel) Answer(res Response, err error) error {
	var sendErr error
	c.sent.Do(func() {
		m := Message{Type: MessageResult, Operation: c.operation, OK: err == nil}
		if err != nil {
			m.Error = EncodeError(err)
		} else {
			m.Response = &res
		}
		sendErr = c.link.Send(c.operation, m)
	})
	return sendErr
}

func (c *Channel) answer(m Message) {
	c.answered.Do(func() { c.result <- m })
}

// Cancelled is closed when the far side cancelled the operation, which is the
// worker's signal to end the driver call.
func (c *Channel) Cancelled() <-chan struct{} { return c.cancelled }

func (c *Channel) cancel() { c.cancelOnce.Do(func() { close(c.cancelled) }) }

// Close cancels the operation on the far side and releases every sub-stream.
// It is idempotent.
func (c *Channel) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	err := c.link.Send(c.operation, Message{Type: MessageCancel, Operation: c.operation})
	c.link.Drop(c.operation)
	if errors.Is(err, ErrLinkClosed) {
		return nil // the operation is over either way
	}
	return err
}

// end releases every sub-stream with the reason the operation stopped, so a
// reader blocked on one learns it rather than waiting forever. The reason is
// kept, because a caller may ask for a sub-stream after the operation ended
// and must be handed one that is already over.
func (c *Channel) end(err error) {
	c.cancel()
	if err == nil {
		err = ErrLinkClosed
	}
	c.mu.Lock()
	c.ended = err
	writers := make([]*io.PipeWriter, 0, len(c.writers))
	for _, w := range c.writers {
		writers = append(writers, w)
	}
	c.mu.Unlock()
	for _, w := range writers {
		_ = w.CloseWithError(err)
	}
}

// substream is one outgoing sub-stream: frames of at most MaxFrameBytes, and
// a zero-length frame at the end.
type substream struct {
	channel *Channel
	stream  byte
	closed  atomic.Bool
}

func (s *substream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := min(len(p), MaxFrameBytes)
		if err := s.channel.link.SendFrame(s.channel.operation, s.stream, p[:chunk]); err != nil {
			return written, err
		}
		written += chunk
		p = p[chunk:]
	}
	return written, nil
}

func (s *substream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	err := s.channel.link.SendFrame(s.channel.operation, s.stream, nil)
	if errors.Is(err, ErrLinkClosed) {
		return nil
	}
	return err
}
