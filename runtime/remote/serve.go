// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"latere.ai/x/cella/runtime"
)

// ServerOptions configures the worker's side of one stream.
type ServerOptions struct {
	// Worker is the id the control plane minted at registration, which the
	// hello names.
	Worker string
	// Driver is what the operations are executed with.
	Driver runtime.Driver
	// ReportInterval is how often the worker sends its driver's whole list
	// up, so the control plane reads a sandbox without waking it. Zero takes
	// the heartbeat's interval.
	ReportInterval time.Duration
	Log            *slog.Logger
}

// Server is the worker's side of one stream: every operation the control
// plane sends, executed with the worker's own driver and answered, and the
// state that driver observes, reported up.
//
// It holds nothing across a connection. A worker that reconnects registers
// again and reports its whole list, so the control plane's view is rebuilt
// from the driver rather than carried over.
type Server struct {
	options ServerOptions
	link    *Link

	mu      sync.Mutex
	running map[string]context.CancelFunc
	wg      sync.WaitGroup
}

// NewServer wraps one connection with the worker's side of the protocol.
func NewServer(conn FrameConn, o ServerOptions) *Server {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.ReportInterval <= 0 {
		o.ReportInterval = HeartbeatInterval
	}
	s := &Server{options: o, running: map[string]context.CancelFunc{}}
	s.link = NewLink(conn, LinkOptions{OnMessage: s.onMessage})
	return s
}

// Run holds the stream open until it ends: the hello, the first report, then
// the operations as they arrive.
func (s *Server) Run(ctx context.Context) error {
	if err := s.link.Send(NoOperation, Message{Type: MessageHello, Worker: s.options.Worker}); err != nil {
		return err
	}
	go s.beat(ctx)
	s.report(ctx, true)
	err := s.link.Run(ctx)
	// Every operation still running belongs to a connection that is gone;
	// the control plane redelivers what it can and fails the rest.
	s.mu.Lock()
	for _, cancel := range s.running {
		cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return err
}

// beat sends the heartbeat and the periodic report. They ride the same ticker
// because a report is a heartbeat that also carries what the driver sees.
func (s *Server) beat(ctx context.Context) {
	heartbeat := time.NewTicker(HeartbeatInterval)
	defer heartbeat.Stop()
	reports := time.NewTicker(s.options.ReportInterval)
	defer reports.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.link.Done():
			return
		case <-heartbeat.C:
			if err := s.link.Send(NoOperation, Message{Type: MessageHeartbeat}); err != nil {
				return
			}
		case <-reports.C:
			s.report(ctx, true)
		}
	}
}

// report sends what the driver observes. A relist is the whole environment
// and replaces what the control plane held, which is how a sandbox the driver
// no longer has stops being reported.
func (s *Server) report(ctx context.Context, relist bool) {
	states, err := s.options.Driver.List(ctx, runtime.Filter{})
	if err != nil {
		s.options.Log.WarnContext(ctx, "the worker could not list what its driver holds", "err", err)
		return
	}
	if err = s.link.Send(NoOperation, Message{Type: MessageState, States: states, Relist: relist}); err != nil &&
		!errors.Is(err, ErrLinkClosed) {
		s.options.Log.WarnContext(ctx, "the worker could not report what its driver holds", "err", err)
	}
}

// onMessage takes what the control plane sends: one operation at a time. The
// handler returns at once and the operation runs beside the read pump, so a
// long call never holds the connection.
func (s *Server) onMessage(operation string, m Message) {
	if m.Type != MessageOperation {
		s.options.Log.Warn("the control plane sent a frame that belongs the other way", "frame", m.Type)
		return
	}
	opType := OperationType(m)
	var req Request
	if m.Request != nil {
		req = *m.Request
	}
	channel := s.link.Open(operation, opType == OpAttach)
	ctx, cancel := context.WithCancel(context.WithoutCancel(context.Background()))
	s.mu.Lock()
	s.running[operation] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer func() {
			s.mu.Lock()
			delete(s.running, operation)
			s.mu.Unlock()
			s.link.Drop(operation)
		}()
		// A cancel from the control plane, or a connection that ended, ends
		// the driver call: the caller is gone and the work is nobody's.
		go func() {
			select {
			case <-channel.Cancelled():
				cancel()
			case <-s.link.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
		res, err := Execute(ctx, s.options.Driver, opType, req, channel)
		// The report goes up before the answer, on the one writer both
		// share, so a caller that reads the sandbox the instant its
		// operation returns reads what the operation did and not what was
		// there before it.
		if changes(opType) {
			s.report(ctx, true)
		}
		if sendErr := channel.Answer(res, err); sendErr != nil && !errors.Is(sendErr, ErrLinkClosed) {
			s.options.Log.Warn("the worker could not answer an operation", "operation", operation, "err", sendErr)
		}
	}()
}

// changes reports whether an operation can have altered what the driver
// holds, and so whether the control plane's view needs rebuilding at once
// rather than at the next report.
func changes(opType string) bool {
	switch opType {
	case OpCreate, OpStart, OpStop, OpDelete, OpUpdate, OpTouch:
		return true
	}
	return false
}
