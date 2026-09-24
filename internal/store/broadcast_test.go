// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"testing"

	"latere.ai/x/cella/internal/store"
)

// TestBroadcastDropsASlowSubscriber: a subscription whose buffer is full is
// ended as behind rather than blocking the transaction that published, a
// subscription to another object is not charged for rows it never receives,
// and a closed broadcast ends what is open and every later subscription.
func TestBroadcastDropsASlowSubscriber(t *testing.T) {
	var b store.Broadcast
	slow, other, closed := b.Subscribe(""), b.Subscribe("sbx_a"), b.Subscribe("")
	closed.Close()
	closed.Close()
	if closed.Err() != nil {
		t.Errorf("a subscription its reader closed ended with %v", closed.Err())
	}
	if _, open := <-closed.Rows(); open {
		t.Error("a closed subscription still delivers")
	}

	rows := make([]store.Event, store.SubscriptionBuffer)
	for i := range rows {
		rows[i] = store.Event{ObjectID: "sbx_b", Seq: int64(i + 1)}
	}
	// Publish runs on this goroutine: a send that blocked would hang the test
	// rather than fail it, which is the property.
	b.Publish(rows)
	b.Publish([]store.Event{{ObjectID: "sbx_b", Seq: int64(len(rows) + 1)}})
	b.Publish(nil)

	received := 0
	for range slow.Rows() {
		received++
	}
	if received != store.SubscriptionBuffer {
		t.Errorf("the slow subscription received %d rows before it ended, want the %d it could hold", received, store.SubscriptionBuffer)
	}
	if !errors.Is(slow.Err(), store.ErrBehind) {
		t.Errorf("the slow subscription ended with %v, want ErrBehind", slow.Err())
	}
	slow.Close()

	select {
	case row := <-other.Rows():
		t.Errorf("sbx_a's subscription received %+v", row)
	default:
	}
	b.Publish([]store.Event{{ObjectID: "sbx_a", Seq: 1}})
	if row := <-other.Rows(); row.Seq != 1 {
		t.Errorf("sbx_a's subscription received %+v", row)
	}

	b.Close(nil)
	b.Close(errors.New("a second close"))
	for name, sub := range map[string]*store.Subscription{"open": other, "later": b.Subscribe("")} {
		if _, open := <-sub.Rows(); open {
			t.Errorf("the %s subscription delivers after the close", name)
		}
		if !errors.Is(sub.Err(), store.ErrStoreClosed) {
			t.Errorf("the %s subscription ended with %v, want ErrStoreClosed", name, sub.Err())
		}
	}
}
