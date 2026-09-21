// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"latere.ai/x/cella/internal/events"
)

// TestFeedReadsOneObjectNewestFirst: the read half of the per-object feed.
// The sequence is the journal's own column and not the payload's, so a record
// that came back with none would be a record no reader could order.
func TestFeedReadsOneObjectNewestFirst(t *testing.T) {
	_, j := journal(t)
	at := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	for i, kind := range []events.Type{events.TypeCreated, events.TypeStarted, events.TypeStopped} {
		write(t, j, at.Add(time.Duration(i)*time.Second), "sbx_a", kind)
	}
	write(t, j, at, "sbx_b", events.TypeCreated)
	emitter := events.NewEmitter(j, slog.New(slog.DiscardHandler))

	page, next, err := emitter.Feed(t.Context(), "sbx_a", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 3 || next != "" {
		t.Fatalf("the feed answered %d record(s) and the cursor %q", len(page), next)
	}
	for i, record := range page {
		if record.Object.ID != "sbx_a" {
			t.Errorf("record %d is of %s", i, record.Object.ID)
		}
		if record.Seq == 0 {
			t.Errorf("record %d carries no sequence", i)
		}
		if i > 0 && page[i-1].Seq < record.Seq {
			t.Errorf("the page is not newest first: %d before %d", page[i-1].Seq, record.Seq)
		}
	}
	if page[0].Type != events.TypeStopped {
		t.Errorf("the newest record is %s", page[0].Type)
	}

	// One at a time, the cursor carries the reader to the end.
	seen, cursor := 0, ""
	for range 4 {
		one, more, err := emitter.Feed(t.Context(), "sbx_a", cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(one) == 0 {
			break
		}
		seen, cursor = seen+1, more
		if cursor == "" {
			break
		}
	}
	if seen != 3 {
		t.Errorf("paging read %d record(s) of 3", seen)
	}
}

// TestFeedWithoutAJournalIsEmpty: an emitter over no journal, and a nil
// emitter, each answer an empty page. A control plane that journals nothing
// has nothing to serve, and that is not a failure a caller can act on.
func TestFeedWithoutAJournalIsEmpty(t *testing.T) {
	for name, emitter := range map[string]*events.Emitter{
		"nil emitter": nil,
		"no journal":  events.NewEmitter(nil, slog.New(slog.DiscardHandler)),
	} {
		t.Run(name, func(t *testing.T) {
			page, next, err := emitter.Feed(t.Context(), "sbx_a", "", 50)
			if err != nil || len(page) != 0 || next != "" {
				t.Fatalf("the feed answered %d record(s), %q, %v", len(page), next, err)
			}
		})
	}
}

// TestFeedReportsTheJournalsRefusal: a journal that cannot answer is an error
// the caller reads, not an empty page it would read as no records.
func TestFeedReportsTheJournalsRefusal(t *testing.T) {
	broken := errors.New("the journal is gone")
	emitter := events.NewEmitter(&refusing{err: broken}, slog.New(slog.DiscardHandler))
	if _, _, err := emitter.Feed(t.Context(), "sbx_a", "", 50); !errors.Is(err, broken) {
		t.Fatalf("the feed answered %v", err)
	}
}
