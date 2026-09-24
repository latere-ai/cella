// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"sync"
)

// ErrBehind ends a subscription whose reader did not keep up: a row was
// committed while its buffer was full, so what the reader holds is not every
// row that committed. The reader reads the journal again from its own
// position, or ends where it has no position to read from.
var ErrBehind = errors.New("store: the subscription fell behind and a committed row was not delivered")

// ErrStoreClosed is why a subscription ended when its broadcast was closed
// with no error of the adapter's own.
var ErrStoreClosed = errors.New("store: the store closed")

// SubscriptionBuffer is how many committed rows one subscription holds for a
// reader that has not taken them yet. A reader writing to a slow client falls
// this far behind before its subscription ends.
const SubscriptionBuffer = 256

// Broadcast hands the journal rows one process commits to the subscriptions
// open in that process. It is what a following reader of design 009 waits on
// instead of a timer.
//
// Publish never blocks: a subscription whose buffer is full is ended with
// ErrBehind rather than holding the transaction that committed. A row is
// handed over as the adapter wrote it, and a reader must not change its
// payload.
type Broadcast struct {
	mu     sync.Mutex
	subs   map[*Subscription]struct{}
	closed error
}

// Subscribe opens one subscription: to one object's rows, or to every row
// where objectID is empty. A broadcast that was closed answers a subscription
// that has already ended with the close's error.
func (b *Broadcast) Subscribe(objectID string) *Subscription {
	s := &Subscription{broadcast: b, object: objectID, rows: make(chan Event, SubscriptionBuffer)}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed != nil {
		s.end(b.closed)
		return s
	}
	if b.subs == nil {
		b.subs = map[*Subscription]struct{}{}
	}
	b.subs[s] = struct{}{}
	return s
}

// Publish hands rows, in the order given, to every subscription they belong
// to. The adapter calls it once per committed transaction and never for one
// that rolled back.
func (b *Broadcast) Publish(rows []Event) {
	if len(rows) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		for _, row := range rows {
			if s.object != "" && s.object != row.ObjectID {
				continue
			}
			select {
			case s.rows <- row:
				continue
			default:
			}
			delete(b.subs, s)
			s.end(ErrBehind)
			break
		}
	}
}

// Close ends every subscription with err and every later one at once. The
// adapter closes its broadcast when the store closes, so a reader waiting on
// a store that is gone learns it rather than waiting forever.
func (b *Broadcast) Close(err error) {
	if err == nil {
		err = ErrStoreClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed != nil {
		return
	}
	b.closed = err
	for s := range b.subs {
		s.end(err)
	}
	b.subs = nil
}

// Subscription is one reader's view of what its process commits to the
// journal from the moment it was opened.
type Subscription struct {
	broadcast *Broadcast
	object    string
	rows      chan Event
	// err is why rows closed. It is written under the broadcast's lock
	// before the channel closes, and read under the same lock.
	err error
}

// Rows delivers the committed rows in the order the process published them.
// It is closed when the subscription ends, and Err says why.
func (s *Subscription) Rows() <-chan Event { return s.rows }

// Err is why the subscription ended: ErrBehind, the store's own error for a
// store that closed, or nil where the reader closed it itself or it is still
// open.
func (s *Subscription) Err() error {
	s.broadcast.mu.Lock()
	defer s.broadcast.mu.Unlock()
	return s.err
}

// Close ends the subscription. It is safe to call more than once and after
// the subscription ended on its own.
func (s *Subscription) Close() {
	s.broadcast.mu.Lock()
	defer s.broadcast.mu.Unlock()
	if _, open := s.broadcast.subs[s]; !open {
		return
	}
	delete(s.broadcast.subs, s)
	s.end(nil)
}

// end records why and closes the channel. The caller holds the broadcast's
// lock and has taken the subscription out of the set, which is what makes
// this run once.
func (s *Subscription) end(err error) {
	s.err = err
	close(s.rows)
}
