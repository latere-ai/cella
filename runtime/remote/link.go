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
	// connection. The frame is the caller's to keep: the link holds a
	// sub-stream's bytes until its reader takes them, so an implementation
	// never reuses the slice it returned.
	ReadFrame() ([]byte, error)
	// WriteFrame sends one whole frame. It is called by one writer only,
	// which is the WebSocket contract and is what the link guarantees.
	WriteFrame([]byte) error
	Close() error
}

// outBuffer is how many frames may queue for the writer. Under credit a
// sub-stream never has more than its window queued, in flight and held by the
// far side together, so the queue is bounded by what the operations on the
// connection were credited rather than by this figure.
const outBuffer = 256

// ErrLinkClosed is every call on a link whose connection ended.
var ErrLinkClosed = errors.New("remote: the worker stream is closed")

// LinkOptions configures one link.
type LinkOptions struct {
	// OnMessage receives every control message that is not a sub-stream
	// frame and not an answer the link routed itself. It runs on the read
	// pump, so a handler that blocks holds the connection.
	OnMessage func(operation string, m Message)
	// Window is the credit this side grants per sub-stream, which its hello
	// announces. Zero takes DefaultWindow.
	Window int
}

// Link is one worker stream, whichever side holds it: the frames in and out,
// the sub-streams of every operation on it, and the control messages. It has
// exactly one writer, which is what a WebSocket requires.
//
// Once both sides have agreed on credit, a sub-stream's bytes are bounded by
// the window each way: the receiver holds at most its window of one
// sub-stream, the sender waits for credit rather than queueing more, and the
// read pump never waits on a reader. Without credit, which is a peer of a
// release before it, the read pump waits for a reader whose sub-stream holds
// a window already, which is the back pressure the stream had before.
type Link struct {
	conn      FrameConn
	onMessage func(string, Message)
	out       chan []byte
	done      chan struct{}
	closeOnce sync.Once
	failure   atomic.Pointer[error]

	// window is what this side grants per sub-stream and peerWindow what the
	// far side does, zero until credit is agreed. peak is the most one
	// sub-stream has held here, which is what the window bounds.
	window     int64
	peerWindow atomic.Int64
	peak       atomic.Int64

	mu       sync.Mutex
	channels map[string]*Channel
}

// NewLink wraps one connection. Run drives it; nothing is read or written
// before.
func NewLink(conn FrameConn, o LinkOptions) *Link {
	window := int64(o.Window)
	if window <= 0 {
		window = DefaultWindow
	}
	return &Link{
		conn: conn, onMessage: o.OnMessage,
		out: make(chan []byte, outBuffer), done: make(chan struct{}),
		window: window, channels: map[string]*Channel{},
	}
}

// Window is the credit this side grants per sub-stream, which it announces in
// its hello.
func (l *Link) Window() int { return int(l.window) }

// Credit turns flow control on with the window the far side announced. It is
// agreed once, on the hello, before the first frame of any operation: a
// sub-stream opened before it runs without credit for its whole life. A
// window below one byte, or a second agreement, is ErrFrame.
func (l *Link) Credit(peerWindow int) error {
	if peerWindow <= 0 {
		return fmt.Errorf("%w: a window is at least one byte, not %d", ErrFrame, peerWindow)
	}
	if !l.peerWindow.CompareAndSwap(0, int64(peerWindow)) {
		return fmt.Errorf("%w: the window is agreed once per connection", ErrFrame)
	}
	return nil
}

// notePeak records what one sub-stream holds here, so the bound the window
// sets is something a reader of the link can measure.
func (l *Link) notePeak(held int64) {
	for {
		peak := l.peak.Load()
		if held <= peak || l.peak.CompareAndSwap(peak, held) {
			return
		}
	}
}

// Run holds the connection open until it ends or the context does, and
// returns what ended it. It is the link's read pump; the write pump runs
// beside it.
//
// What ended it is the first reason recorded. A handler that refused a
// message shuts the link down with its refusal, and the read that then fails
// on the closed connection is a consequence of that refusal, not the reason.
func (l *Link) Run(ctx context.Context) error {
	go l.writePump(ctx)
	defer l.Shutdown(nil)
	for {
		raw, err := l.conn.ReadFrame()
		if err == nil {
			err = l.read(raw)
		}
		if err != nil {
			l.Shutdown(err)
			return l.Err()
		}
	}
}

// read takes one frame off the connection: a sub-stream's bytes into its
// buffer, a control message to where it belongs.
func (l *Link) read(raw []byte) error {
	operation, stream, payload, err := DecodeFrame(raw)
	if err != nil {
		return err
	}
	if stream != StreamControl {
		return l.deliver(operation, stream, payload)
	}
	m, err := DecodeMessage(payload)
	if err != nil {
		return err
	}
	return l.route(operation, m)
}

// errCancelled is what a sub-stream coming into an operation the far side
// cancelled reads: a driver blocked reading a body or a terminal's input
// learns the caller went away rather than waiting for the connection to end.
var errCancelled = fmt.Errorf("remote: the far side cancelled the operation: %w", context.Canceled)

// route answers the messages the link owns and hands the rest to the caller.
// A message about an operation this side has already dropped is discarded:
// the operation is over here and the peer has not learned it yet, which is
// ordinary on every stream a caller closed early. An error is a message this
// protocol cannot accept, and it ends the connection.
func (l *Link) route(operation string, m Message) error {
	switch m.Type {
	case MessageResult, MessageStarted, MessageResize, MessageCancel, MessageCredit:
		c, held := l.channel(operation)
		if !held {
			return nil
		}
		switch m.Type {
		case MessageResult:
			c.answer(m)
		case MessageStarted:
			c.accepted(m)
		case MessageResize:
			c.resize(m.Cols, m.Rows)
		case MessageCancel:
			c.end(errCancelled)
		case MessageCredit:
			return c.grant(m.Stream, m.Bytes)
		}
		return nil
	}
	if l.onMessage != nil {
		l.onMessage(operation, m)
	}
	return nil
}

// deliver puts one sub-stream frame into the operation's buffer for that
// sub-stream. A zero-length frame ends the sub-stream. A frame for an
// operation this side has dropped is discarded: the operation is over and the
// peer has not learned it yet.
func (l *Link) deliver(operation string, stream byte, payload []byte) error {
	c, held := l.channel(operation)
	if !held {
		return nil
	}
	return c.inbound(stream).push(payload)
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
		incoming: map[byte]*inbound{}, outgoing: map[byte]*outbound{},
		granted: make(chan struct{}), finished: make(chan struct{}),
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

	mu       sync.Mutex
	incoming map[byte]*inbound
	outgoing map[byte]*outbound
	// granted is closed and replaced whenever credit arrives, which wakes
	// every writer of this operation waiting for some.
	granted chan struct{}
	// ended is why the operation stopped, once it has. A sub-stream asked
	// for after that is handed back already ended: a caller that starts
	// reading an operation the connection has already lost must read the
	// reason rather than wait for bytes nobody will send.
	ended error

	resizes      chan [2]int
	result       chan Message
	answered     sync.Once
	finished     chan struct{}
	sent         sync.Once
	started      chan Message
	acceptedOnce sync.Once
	cancelled    chan struct{}
	cancelOnce   sync.Once
	closed       atomic.Bool
}

// Reader is one sub-stream coming in, ending in io.EOF at its zero-length
// frame. Asking for a sub-stream twice returns the same reader.
func (c *Channel) Reader(stream byte) io.Reader { return c.inbound(stream) }

// Up is Reader under the name the control plane's Stream contract uses.
func (c *Channel) Up(stream byte) io.Reader { return c.inbound(stream) }

// inbound is one sub-stream's buffer, made the first time either the reader
// or the far side's first frame names it. Whether it is credited is fixed
// here, from whether the connection had agreed credit, so one sub-stream never
// changes rules half way.
func (c *Channel) inbound(stream byte) *inbound {
	c.mu.Lock()
	defer c.mu.Unlock()
	if in, held := c.incoming[stream]; held {
		return in
	}
	in := &inbound{
		channel: c, stream: stream,
		credited: c.link.peerWindow.Load() > 0, window: c.link.window,
		err: c.ended,
	}
	in.ready = sync.NewCond(&in.mu)
	c.incoming[stream] = in
	return in
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

// resize queues one window for the terminal without waiting, on the read
// pump. A queue that is full holds windows the terminal has not applied yet,
// and only the newest of them is the terminal's, so the oldest gives way.
func (c *Channel) resize(cols, rows int) {
	if c.resizes == nil {
		return
	}
	window := [2]int{cols, rows}
	for {
		select {
		case c.resizes <- window:
			return
		default:
		}
		select {
		case <-c.resizes:
		default:
		}
	}
}

// Event sends one change the worker's driver observed, on the Watch
// operation.
func (c *Channel) Event(e Event) error {
	return c.link.Send(c.operation, Message{Type: MessageEvent, Operation: c.operation, Event: &e})
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

// answer records the far side's result. It also ends every writer of this
// operation waiting for credit: an operation that answered is over on the far
// side, which reads nothing more of it and grants nothing more.
func (c *Channel) answer(m Message) {
	c.answered.Do(func() {
		c.result <- m
		close(c.finished)
	})
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
// reader blocked on one learns it rather than waiting forever, and a writer
// waiting for credit learns there will be none. The first reason is kept,
// because a caller may ask for a sub-stream after the operation ended and
// must be handed one that is already over.
func (c *Channel) end(err error) {
	c.cancel()
	if err == nil {
		err = ErrLinkClosed
	}
	c.mu.Lock()
	if c.ended == nil {
		c.ended = err
	}
	incoming := make([]*inbound, 0, len(c.incoming))
	for _, in := range c.incoming {
		incoming = append(incoming, in)
	}
	c.mu.Unlock()
	for _, in := range incoming {
		in.fail(err)
	}
}

// outbound is what one sub-stream going out may still send: the far side's
// window less what it has not credited back. A sub-stream opened before the
// connection agreed credit is not credited, and sends as the stream did before.
type outbound struct {
	credited bool
	credit   int64
}

func (c *Channel) outboundLocked(stream byte) *outbound {
	if o, held := c.outgoing[stream]; held {
		return o
	}
	peer := c.link.peerWindow.Load()
	o := &outbound{credited: peer > 0, credit: peer}
	c.outgoing[stream] = o
	return o
}

// spend takes up to want bytes of credit on one sub-stream, waiting for a
// grant when none is left. The wait ends without credit when the operation
// can no longer use the bytes: the caller closed it or the far side cancelled
// it, the far side answered it, or the connection ended.
func (c *Channel) spend(stream byte, want int) (int, error) {
	for {
		c.mu.Lock()
		o := c.outboundLocked(stream)
		if !o.credited {
			c.mu.Unlock()
			return want, nil
		}
		if o.credit > 0 {
			n := int(min(int64(want), o.credit))
			o.credit -= int64(n)
			c.mu.Unlock()
			return n, nil
		}
		granted := c.granted
		c.mu.Unlock()
		select {
		case <-granted:
		case <-c.cancelled:
			return 0, c.stopped()
		case <-c.finished:
			return 0, io.ErrClosedPipe
		case <-c.link.done:
			return 0, c.link.closedErr()
		}
	}
}

// stopped is what a writer of an operation that ended reads: the connection's
// end where that is what ended it, and a closed pipe where the operation alone
// is over, because the far side will read nothing more of it.
func (c *Channel) stopped() error {
	if c.link.Err() != nil {
		return c.link.closedErr()
	}
	return io.ErrClosedPipe
}

// grant adds the far side's credit to one sub-stream going out. A correct
// peer never leaves a sub-stream more credit than its own window, never grants
// nothing, and never credits control or a sub-stream the connection does not
// credit, so each of those is ErrFrame.
func (c *Channel) grant(stream int, bytes int64) error {
	if stream <= int(StreamControl) || stream > 0xff {
		return fmt.Errorf("%w: a credit names a sub-stream, not %d", ErrFrame, stream)
	}
	if bytes <= 0 {
		return fmt.Errorf("%w: a credit grants at least one byte, not %d", ErrFrame, bytes)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	o := c.outboundLocked(byte(stream))
	if !o.credited {
		return fmt.Errorf("%w: a credit on sub-stream %d, which this connection does not credit", ErrFrame, stream)
	}
	if peer := c.link.peerWindow.Load(); bytes > peer-o.credit {
		return fmt.Errorf("%w: a credit of %d leaves sub-stream %d more than the window of %d", ErrFrame, bytes, stream, peer)
	}
	o.credit += bytes
	close(c.granted)
	c.granted = make(chan struct{})
	return nil
}

// inbound is one sub-stream coming in: the bytes the far side sent that the
// reader has not taken yet. The read pump appends and the reader takes, and
// on a credited sub-stream the reader credits the far side back as it takes,
// so the buffer never holds more than the window and the pump never waits.
type inbound struct {
	channel  *Channel
	stream   byte
	credited bool
	window   int64

	mu    sync.Mutex
	ready *sync.Cond
	// frames is what arrived and was not taken, held is its size, received
	// every byte the far side sent, granted every byte credited back beyond
	// the first window, and taken what was read and not credited back yet.
	frames   [][]byte
	held     int64
	received int64
	granted  int64
	taken    int64
	eof      bool
	err      error
}

// grantAt is how much a reader takes before it credits the far side back:
// half the window, and never more than one frame, so a reader taking a few
// bytes at a time sends one credit per frame rather than one per read.
func (in *inbound) grantAt() int64 { return max(1, min(in.window/2, MaxFrameBytes)) }

// push appends one frame on the read pump. A zero-length frame ends the
// sub-stream. On a credited sub-stream a frame past the window is the far
// side breaking the protocol, and the connection closes; on one that is not,
// the pump waits for the reader to make room, which is the back pressure a
// peer without credit expects.
func (in *inbound) push(payload []byte) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.eof || in.err != nil {
		// The sub-stream is over here and the far side has not learned it.
		return nil
	}
	if len(payload) == 0 {
		in.eof = true
		in.ready.Broadcast()
		return nil
	}
	size := int64(len(payload))
	if in.credited {
		if outstanding := in.received + size - in.granted; outstanding > in.window {
			return fmt.Errorf("%w: sub-stream %d of %s holds %d bytes the far side was never credited for, past the window of %d",
				ErrFrame, in.stream, in.channel.operation, outstanding, in.window)
		}
	} else {
		for in.held >= in.window && in.err == nil {
			in.ready.Wait()
		}
		if in.err != nil {
			return nil
		}
	}
	in.frames = append(in.frames, payload)
	in.held += size
	in.received += size
	in.channel.link.notePeak(in.held)
	in.ready.Broadcast()
	return nil
}

// Read takes what arrived, waiting for bytes, the end of the sub-stream, or
// the reason the operation stopped. Bytes that arrived before the operation
// stopped are read first.
func (in *inbound) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	in.mu.Lock()
	for len(in.frames) == 0 && !in.eof && in.err == nil {
		in.ready.Wait()
	}
	if len(in.frames) == 0 {
		err := in.err
		if in.eof {
			err = io.EOF
		}
		in.mu.Unlock()
		return 0, err
	}
	n := copy(p, in.frames[0])
	if n == len(in.frames[0]) {
		in.frames[0] = nil
		in.frames = in.frames[1:]
	} else {
		in.frames[0] = in.frames[0][n:]
	}
	in.held -= int64(n)
	var grant int64
	if in.credited && in.err == nil {
		in.taken += int64(n)
		if in.taken >= in.grantAt() {
			grant, in.taken = in.taken, 0
			in.granted += grant
		}
	}
	in.ready.Broadcast()
	in.mu.Unlock()
	if grant > 0 {
		// A credit that cannot be sent is a connection that ended, which
		// this reader learns on its next read from the reason the
		// operation stopped.
		_ = in.channel.link.Send(in.channel.operation, Message{
			Type: MessageCredit, Operation: in.channel.operation, Stream: int(in.stream), Bytes: grant,
		})
	}
	return n, nil
}

// fail ends the sub-stream with the reason the operation stopped. A sub-stream
// that already ended whole keeps its end, because what it carried is complete.
func (in *inbound) fail(err error) {
	in.mu.Lock()
	if !in.eof && in.err == nil {
		in.err = err
	}
	in.ready.Broadcast()
	in.mu.Unlock()
}

// substream is one outgoing sub-stream: frames of at most MaxFrameBytes, each
// within the credit the far side granted, and a zero-length frame at the end.
type substream struct {
	channel *Channel
	stream  byte
	closed  atomic.Bool
}

func (s *substream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n, err := s.channel.spend(s.stream, min(len(p), MaxFrameBytes))
		if err != nil {
			return written, err
		}
		if err = s.channel.link.SendFrame(s.channel.operation, s.stream, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// Close ends the sub-stream. The zero-length frame costs no credit, so a
// writer that spent its window still ends what it sent.
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
