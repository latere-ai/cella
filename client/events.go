// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"latere.ai/x/pkg/httpjson"
)

// eventsPath is the feed's one route.
const eventsPath = "/v1/events"

// Event is one record of design 009 as the feed answers it: what happened, to
// which object, who asked, and when. The package does not import the server's
// record type; these are the fields of the wire, and Raw is the record's own
// bytes for a caller that forwards them.
type Event struct {
	// ID names the record across every object.
	ID string `json:"id"`
	// Seq counts one object's records from 1 without a gap, which is what a
	// following feed resumes from.
	Seq int64 `json:"seq"`
	// Type is design 009's type, such as sandbox.started.
	Type string `json:"type"`
	// Time is when the act committed.
	Time time.Time `json:"time"`
	// Object is what the record is about. Sandbox names the sandbox in
	// context where the object is not itself the whole story, which is
	// every operation.
	Object EventObject `json:"object"`
	// Sandbox is the sandbox in context, nil where Object is itself one.
	Sandbox *EventObject `json:"sandbox,omitempty"`
	// Subject is who asked, and Workload the sandbox whose own token asked
	// where one did.
	Subject string `json:"subject"`
	// Workload is the sandbox whose own token asked, nil where a subject
	// asked.
	Workload *EventWorkload `json:"workload,omitempty"`
	// RequestID is the request that caused the act, empty for an act the
	// control plane took on its own.
	RequestID string `json:"requestId"`
	// Reason is set on every terminal transition.
	Reason string `json:"reason,omitempty"`
	// Data is the per-type payload of design 009, redacted.
	Data json.RawMessage `json:"data,omitempty"`
	// Raw is the record's bytes as the server sent them.
	Raw json.RawMessage `json:"-"`
}

// EventObject is the identity of the object a record is about, with the
// labels it carried.
type EventObject struct {
	// Kind is the object's kind, such as Sandbox.
	Kind string `json:"kind"`
	// ID and Name are the object's id and its name.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Owner is the rendered subject that owns it.
	Owner string `json:"owner"`
	// Labels are the labels it carried when the act committed.
	Labels map[string]string `json:"labels,omitempty"`
}

// EventWorkload is the sandbox whose own token made a request.
type EventWorkload struct {
	// ID is the sandbox's id.
	ID string `json:"id"`
}

// EventPage is one page of an object's history, newest first. Next is the
// cursor of the next older page, empty at the end.
type EventPage struct {
	// Items are the page's records, newest first.
	Items []Event
	// Next is the cursor of the next older page.
	Next string
}

// EventOptions select one page of an object's history.
type EventOptions struct {
	// Cursor is a previous page's Next; empty is the newest page.
	Cursor string
	// Limit is the page size, 1 to 200; zero is the server's default.
	Limit int
}

// Events reads one page of an object's history: a sandbox, a secret or an
// environment, by id or by name. The object's read decides whether the
// caller may, so a page tells a caller nothing a read of the object would
// not.
func (c *Client) Events(ctx context.Context, object string, o EventOptions) (EventPage, []byte, error) {
	q := url.Values{"object": []string{object}}
	if o.Cursor != "" {
		q.Set("cursor", o.Cursor)
	}
	if o.Limit > 0 {
		q.Set("limit", limitValue(o.Limit))
	}
	raw, err := c.send(ctx, http.MethodGet, eventsPath, q, nil, "")
	if err != nil {
		return EventPage{}, nil, err
	}
	var answer Page
	if err = json.Unmarshal(raw, &answer); err != nil {
		return EventPage{}, raw, fmt.Errorf("the events answer is no page: %w", err)
	}
	page := EventPage{Next: answer.Next, Items: make([]Event, 0, len(answer.Items))}
	for _, item := range answer.Items {
		e, err := eventOf(item)
		if err != nil {
			return EventPage{}, raw, err
		}
		page.Items = append(page.Items, e)
	}
	return page, raw, nil
}

// FollowOptions select a following feed.
type FollowOptions struct {
	// Object names one object's feed. Empty follows every object the
	// caller may read, from now.
	Object string
	// Cursor is the newest Seq of the object the caller already holds, in
	// decimal: the feed sends every record after it and then each new one.
	// Empty starts from now, and "0" replays what the server still keeps. A
	// page's Next is a position in the other direction and is not a follow
	// cursor; resume from the newest item's Seq instead.
	Cursor string
}

// After is the Cursor that resumes after one record.
func After(seq int64) string { return strconv.FormatInt(seq, 10) }

// FollowEvents opens a following feed. It stays open until the caller closes
// it or cancels the context; one object's feed also ends after that object's
// delete record, and a server that stops ends it without an error.
func (c *Client) FollowEvents(ctx context.Context, o FollowOptions) (*EventStream, error) {
	q := url.Values{"follow": []string{"1"}}
	if o.Object != "" {
		q.Set("object", o.Object)
	}
	if o.Cursor != "" {
		q.Set("cursor", o.Cursor)
	}
	body, _, err := c.stream(ctx, http.MethodGet, eventsPath, q, nil, "")
	if err != nil {
		return nil, err
	}
	return &EventStream{body: body, lines: bufio.NewReader(body)}, nil
}

// EventStream is a following feed: one record per line, as each commits.
// One goroutine reads it.
type EventStream struct {
	body  io.ReadCloser
	lines *bufio.Reader
}

// Next returns the next record. It passes over the empty lines the server
// sends while nothing happens, answers io.EOF where the feed closed, and an
// *Error where the feed's last line is the error envelope of design 008,
// which is how a failure after the first byte is reported. That Error's
// Status is 500, since a failure inside a stream carries no status of its
// own; its Code is what to decide on.
func (s *EventStream) Next() (Event, error) {
	for {
		line, err := s.line()
		if err != nil {
			return Event{}, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var failure struct {
			Error *httpjson.Error `json:"error"`
		}
		if json.Unmarshal(line, &failure) == nil && failure.Error != nil {
			return Event{}, frameError(*failure.Error)
		}
		return eventOf(line)
	}
}

// Close ends the feed.
func (s *EventStream) Close() error { return s.body.Close() }

// line reads one line, bounded as a socket message is. A feed that ends
// part way through a line was cut short.
func (s *EventStream) line() ([]byte, error) {
	var line []byte
	for {
		chunk, err := s.lines.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxMessageBytes {
			return nil, errors.New("the feed sent a line past the client's bound")
		}
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return nil, &StreamError{Err: io.ErrUnexpectedEOF}
		default:
			return nil, err
		}
	}
}

// eventOf decodes one record and keeps its bytes.
func eventOf(raw []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(raw, &e); err != nil {
		return Event{}, fmt.Errorf("the feed sent something that is no record: %w", err)
	}
	e.Raw = append(json.RawMessage(nil), bytes.TrimSpace(raw)...)
	return e, nil
}
