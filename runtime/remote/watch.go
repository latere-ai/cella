// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"sync"

	"latere.ai/x/cella/runtime"
)

// Event is one change a driver observed in its substrate, design 004's Event.
// State is the sandbox after the change. It is empty on a relist, which tells
// the consumer to List because what it holds may have missed changes.
type Event struct {
	Type  string        `json:"type"`
	State runtime.State `json:"state,omitzero"`
}

// The event types of design 004.
const (
	EventAdded    = "added"
	EventModified = "modified"
	EventDeleted  = "deleted"
	EventLost     = "lost"
	EventRelist   = "relist"
)

// Watcher is design 004's Watch: the changes a driver observes in its
// substrate, pushed as they happen rather than found by listing. The channel
// closes when the driver stops watching, and the consumer then lists and
// watches again.
//
// The runtime contract does not declare Watch yet, so the seam's half of it is
// declared here: a worker's driver implements Watcher, and the remote driver
// implements it for the consumer on the control plane. When the contract
// declares it, Event becomes an alias of the contract's type and nothing on
// the wire changes.
type Watcher interface {
	Watch(ctx context.Context) (<-chan Event, error)
}

// watchQueue is how many events one consumer of Watch may leave undelivered.
// Past it the undelivered events are replaced by one relist: the consumer
// re-reads what it missed, and the read pump that delivers events never waits
// for it.
const watchQueue = 256

// subscription is one consumer of an environment's events on the control
// plane: a bounded queue the hub pushes into without waiting, and the
// goroutine that hands the queue to the consumer's channel.
type subscription struct {
	out  chan Event
	wake chan struct{}
	stop chan struct{}

	stopOnce sync.Once
	mu       sync.Mutex
	queue    []Event
}

func newSubscription() *subscription {
	return &subscription{out: make(chan Event), wake: make(chan struct{}, 1), stop: make(chan struct{})}
}

// push queues one event and never waits. A consumer that has fallen
// watchQueue events behind loses them to one relist, which is what tells it to
// read the environment again.
func (s *subscription) push(e Event) {
	s.mu.Lock()
	if len(s.queue) >= watchQueue {
		s.queue = append(s.queue[:0], Event{Type: EventRelist})
	} else {
		s.queue = append(s.queue, e)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// close ends the subscription, which closes the consumer's channel.
func (s *subscription) close() { s.stopOnce.Do(func() { close(s.stop) }) }

// run hands the queue to the consumer until its context ends or the
// subscription is closed, then closes the consumer's channel.
func (s *subscription) run(ctx context.Context) {
	defer close(s.out)
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.mu.Unlock()
			select {
			case <-s.wake:
				continue
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			}
		}
		e := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		select {
		case s.out <- e:
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		}
	}
}
