// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"errors"
	"time"
)

// The ways a following read of design 009 is refused or ends.
var (
	// ErrExpired is a position whose next record the journal no longer
	// holds while it holds a later one: the retention or the memory ring
	// took the records between, and a follower that went on would skip
	// them without saying so.
	ErrExpired = errors.New("events: the journal no longer holds the records after this position")
	// ErrAhead is a position above the object's newest record, which names
	// a record the object never had.
	ErrAhead = errors.New("events: the position is past the object's newest record")
	// ErrBehind is a subscription that dropped a record because its reader
	// did not keep up. A follower of one object reads the journal again;
	// a follower of every object has no position to read from and ends.
	ErrBehind = errors.New("events: the follower fell behind and a record was dropped")
	// ErrNoJournal is a follow asked of an emitter over no journal: a
	// control plane that journals nothing has nothing to follow.
	ErrNoJournal = errors.New("events: this control plane journals nothing")
)

// FromNow is the position of a follower that starts at the object's newest
// record: only what is appended after the follow opened is sent.
const FromNow int64 = -1

const (
	// FollowBatch is how many records one read of the journal hands a
	// follower that is catching up.
	FollowBatch = 200
	// FollowPoll is how often a follower of one object reads a shared
	// journal, which is how a record another replica appended arrives.
	FollowPoll = time.Second
)

// errPoll is the cause of a wait the poll interval ended, which sends the
// follower to the journal rather than out of Next.
var errPoll = errors.New("events: the poll interval passed")

// Follower is one following read of the journal: an object's records after
// a position and then as they commit, or every object's records from the
// moment it opened.
//
// It subscribes before it reads, so a record committed while the history is
// read arrives from both and the second copy is dropped by its sequence. A
// committed record it is handed is sent only when its sequence is the next
// one; anything else sends the follower back to the journal, whose read is
// in sequence order. That is what restores the order where a store publishes
// after its commit rather than under it.
type Follower struct {
	journal Journal
	object  string
	sub     Subscription
	// last is the sequence of the newest record sent, for a follower of
	// one object.
	last int64
	// behind says the journal may hold records past last that the
	// subscription will not deliver: the history not read yet, a record
	// that arrived out of order, or a subscription that fell behind.
	behind bool
	// poll is the interval a shared journal is read at, and due when the
	// next read is.
	poll time.Duration
	due  time.Time
}

// Follow opens a follower of one object after a position: the newest
// sequence the caller holds, or FromNow. A position whose next record is
// gone is ErrExpired and one above the newest is ErrAhead, both before
// anything is sent.
func (e *Emitter) Follow(ctx context.Context, objectID string, after int64) (*Follower, error) {
	if e == nil || e.journal == nil {
		return nil, ErrNoJournal
	}
	f := &Follower{journal: e.journal, object: objectID, sub: e.journal.Watch(objectID)}
	if err := f.start(ctx, after); err != nil {
		f.sub.Close()
		return nil, err
	}
	if e.journal.Shared() {
		f.poll = e.poll
		if f.poll <= 0 {
			f.poll = FollowPoll
		}
		f.due = time.Now().Add(f.poll)
	}
	return f, nil
}

// start sets the follower's position. It runs after the subscription is
// open, so a record committed from here on is either at or below the newest
// read below, or on the subscription.
func (f *Follower) start(ctx context.Context, after int64) error {
	newest := int64(0)
	page, _, err := f.journal.ByObject(ctx, f.object, "", 1)
	if err != nil {
		return err
	}
	if len(page) > 0 {
		newest = page[0].Seq
	}
	switch {
	case after == FromNow:
		f.last = newest
		return nil
	case after < 0 || after > newest:
		return ErrAhead
	}
	next, err := f.journal.After(ctx, f.object, after, 1)
	if err != nil {
		return err
	}
	if len(next) > 0 && next[0].Seq != after+1 {
		return ErrExpired
	}
	f.last, f.behind = after, true
	return nil
}

// FollowAll opens a follower of every object's records from now. There is no
// position to resume from: a sequence counts within one object, and the
// journal orders nothing across objects.
func (e *Emitter) FollowAll() (*Follower, error) {
	if e == nil || e.journal == nil {
		return nil, ErrNoJournal
	}
	return &Follower{journal: e.journal, sub: e.journal.Watch("")}, nil
}

// Next blocks until there are records to send and returns them in sequence
// order, oldest first, or returns why the follower cannot go on: ErrExpired
// where the records after its position are gone, ErrBehind for a follower of
// every object that dropped one, or the context's error.
func (f *Follower) Next(ctx context.Context) ([]Record, error) {
	for {
		if f.behind {
			records, err := f.catchUp(ctx)
			if err != nil || len(records) > 0 {
				return records, err
			}
		}
		record, err := f.receive(ctx)
		switch {
		case err == nil && f.object == "":
			return []Record{record}, nil
		case err == nil && record.Seq == f.last+1:
			f.last = record.Seq
			return []Record{record}, nil
		case err == nil && record.Seq > f.last:
			// A later record arrived first: the ones between are in the
			// journal, committed, and the read returns them in order.
			f.behind = true
		case err == nil:
			// A record at or below the position was already sent.
		case errors.Is(err, errPoll):
			f.behind = true
		case errors.Is(err, ErrBehind) && f.object != "":
			// Subscribe again before reading, so nothing committed
			// between the read and the new subscription is missed.
			f.sub.Close()
			f.sub = f.journal.Watch(f.object)
			f.behind = true
		default:
			return nil, err
		}
	}
}

// catchUp reads the records after the position and sends the run of them
// that follows it without a gap. A first record that does not follow the
// position is ErrExpired: what was between is gone.
func (f *Follower) catchUp(ctx context.Context) ([]Record, error) {
	records, err := f.journal.After(ctx, f.object, f.last, FollowBatch)
	if err != nil {
		return nil, err
	}
	f.behind = len(records) == FollowBatch
	if len(records) == 0 {
		return nil, nil
	}
	if records[0].Seq != f.last+1 {
		return nil, ErrExpired
	}
	n := 1
	for n < len(records) && records[n].Seq == records[n-1].Seq+1 {
		n++
	}
	if n < len(records) {
		records, f.behind = records[:n], true
	}
	f.last = records[n-1].Seq
	return records, nil
}

// receive waits for the subscription's next record, and on a shared journal
// for no longer than the next poll is due.
func (f *Follower) receive(ctx context.Context) (Record, error) {
	if f.poll <= 0 {
		return f.sub.Next(ctx)
	}
	wait, cancel := context.WithDeadlineCause(ctx, f.due, errPoll)
	defer cancel()
	record, err := f.sub.Next(wait)
	if err != nil && ctx.Err() == nil && errors.Is(context.Cause(wait), errPoll) {
		f.due = time.Now().Add(f.poll)
		return Record{}, errPoll
	}
	return record, err
}

// Close ends the follower's subscription.
func (f *Follower) Close() { f.sub.Close() }

// Ends reports whether a record is the last one its object has: the delete
// that ended it. A follower of one object ends after sending it, which is
// design 008's "the connection closes with the sandbox".
func Ends(t Type) bool {
	switch t {
	case TypeDeleted, TypeSecretDeleted, TypeEnvironmentDeleted:
		return true
	}
	return false
}
