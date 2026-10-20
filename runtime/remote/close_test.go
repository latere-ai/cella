// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/runtime/remote"
)

// refusedConn is a connection whose peer closed it: every write is refused
// as the close being answered, and the read returns the close's reason a
// moment later, which is the order a WebSocket gives the two.
type refusedConn struct {
	reason    error
	readAfter time.Duration
	closed    chan struct{}
	once      sync.Once
}

func (c *refusedConn) ReadFrame() ([]byte, error) {
	select {
	case <-time.After(c.readAfter):
		return nil, c.reason
	case <-c.closed:
		return nil, errors.New("closed")
	}
}

func (c *refusedConn) WriteFrame([]byte) error {
	return errors.Join(remote.ErrClosing, errors.New("close sent"))
}

func (c *refusedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// TestAWriteRefusedByACloseLeavesTheReasonToTheRead: when the peer closes the
// connection, a write racing the close is refused before the read pump reads
// the close frame. The link ends with the close's reason, which may be a
// refusal a worker must stop on, and not with the refused write.
func TestAWriteRefusedByACloseLeavesTheReasonToTheRead(t *testing.T) {
	reason := errors.New("the peer closed the stream: key_revoked")
	conn := &refusedConn{reason: reason, readAfter: 50 * time.Millisecond, closed: make(chan struct{})}
	link := remote.NewLink(conn, remote.LinkOptions{})
	if err := link.Send(remote.NoOperation, remote.Message{Type: remote.MessageHello}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := link.Run(ctx)
	if !errors.Is(err, reason) {
		t.Fatalf("the link ended with %v, want the close's reason", err)
	}
	if errors.Is(err, remote.ErrClosing) {
		t.Errorf("the link ended with the refused write %v", err)
	}
}

// TestARefusedWriteEndsTheLinkWhenNoCloseFollows: a write refused as closing
// whose read pump never reads a close still ends the link, with the write's
// error, rather than leaving it open.
func TestARefusedWriteEndsTheLinkWhenNoCloseFollows(t *testing.T) {
	conn := &refusedConn{reason: errors.New("never read"), readAfter: time.Hour, closed: make(chan struct{})}
	link := remote.NewLink(conn, remote.LinkOptions{})
	if err := link.Send(remote.NoOperation, remote.Message{Type: remote.MessageHello}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := link.Run(ctx)
	if !errors.Is(err, remote.ErrClosing) {
		t.Fatalf("the link ended with %v, want the refused write", err)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Errorf("the link took %s to end", waited)
	}
}
