// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/httpjson"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/manifest"
)

// The bounds of design 009's following feed. They are constants and not
// configuration: Options overrides the first two for a test.
const (
	// DefaultFollowLimit is how many following feeds one process holds
	// open at once. Each holds a subscription with its buffer and, on a
	// store another replica shares, a read of the journal a second.
	DefaultFollowLimit = 256
	// DefaultFollowHeartbeat is how long a following feed stays silent
	// before it writes an empty line, which is below the idle timeout of
	// every common proxy.
	DefaultFollowHeartbeat = 15 * time.Second
	// readCache bounds the read decisions one feed of every object holds.
	readCache = 1024
)

// The causes a following feed's wait ends with, read with context.Cause.
var (
	errHeartbeat    = errors.New("api: the heartbeat interval passed")
	errDraining     = errors.New("api: the server is shutting down")
	errTokenExpired = errors.New("api: the bearer the feed was authorized under expired")
)

// ndjson is the media type of a following feed.
const ndjson = "application/x-ndjson"

// followFeed serves GET /v1/events?follow=1. With object= it is one object's
// records after the cursor and then as they commit; without, every record the
// caller may read from now. Everything that refuses the follow runs before
// the status goes out, so a refusal is an ordinary error envelope.
func (h *handler) followFeed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if h.following.Add(1) > int64(h.followLimit) {
		h.following.Add(-1)
		respondError(w, &manifest.Error{Code: "rate_limited",
			Detail: "this server holds as many following feeds open as it serves; retry shortly"})
		return
	}
	defer h.following.Add(-1)

	var (
		f    *events.Follower
		gate *recordGate
		err  error
	)
	object := q.Get("object")
	if object == "" {
		if q.Get("cursor") != "" {
			respondError(w, &manifest.Error{Code: "invalid_field", Path: "cursor",
				Detail: "a cursor is a position in one object's feed; name the object with ?object="})
			return
		}
		// Every object's feed is a list of changes: it is opened under the
		// list action, and each record is decided as the list decides a row.
		if _, err = h.decide(r, authorizer.ActionSandboxList, auth.List(authorizer.ActionSandboxList)); err != nil {
			respondError(w, err)
			return
		}
		f, err = h.Events.FollowAll()
		gate = &recordGate{h: h, r: r, defaultEnvironment: h.Controller.Environment(),
			lists: map[string]listVerdict{}, reads: map[string]bool{}}
	} else {
		after := events.FromNow
		if raw := q.Get("cursor"); raw != "" {
			after, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || after < 0 {
				respondError(w, &manifest.Error{Code: "invalid_field", Path: "cursor",
					Detail: "the cursor is the newest seq you hold, a whole number from 0"})
				return
			}
		}
		id, resource, action, lookup := h.feedObject(r, object)
		if lookup != nil {
			respondError(w, lookup)
			return
		}
		if _, err = h.decide(r, action, resource); err != nil {
			respondError(w, err)
			return
		}
		f, err = h.Events.Follow(r.Context(), id, after)
	}
	if err != nil {
		respondError(w, followError(err))
		return
	}
	defer f.Close()
	h.serveFollow(w, r, f, gate, object != "")
}

// followError is the refusal a follower's error is answered with. A cursor
// past the newest record is the caller's field; the rest are in the table.
func followError(err error) error {
	if errors.Is(err, events.ErrAhead) {
		return &manifest.Error{Code: "invalid_field", Path: "cursor",
			Detail: "the cursor is past the newest record of the object: " + err.Error()}
	}
	return err
}

// serveFollow writes the feed until it ends: one record per line, an empty line
// when nothing was written for a heartbeat, and an error envelope as the last
// line where the feed ended on a failure. A caller that went away, and a
// server that stops taking requests, end it without a line: the caller
// reconnects with the newest seq it holds.
func (h *handler) serveFollow(w http.ResponseWriter, r *http.Request, f *events.Follower, gate *recordGate, object bool) {
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	if exp, ok := expiry(caller(r)); ok {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadlineCause(ctx, exp, errTokenExpired)
		defer stop()
	}
	if h.Draining != nil {
		go func() {
			select {
			case <-h.Draining:
				cancel(errDraining)
			case <-ctx.Done():
			}
		}()
	}
	w.Header().Set("Content-Type", ndjson)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flush(w)
	requestID := w.Header().Get(RequestIDHeader)
	for {
		wait, stop := context.WithTimeoutCause(ctx, h.heartbeat, errHeartbeat)
		records, err := f.Next(wait)
		beat := errors.Is(context.Cause(wait), errHeartbeat)
		stop()
		switch {
		case err == nil:
		case beat && ctx.Err() == nil:
			if !writeLine(w, nil) {
				return
			}
			continue
		case r.Context().Err() != nil, errors.Is(context.Cause(ctx), errDraining):
			return
		case errors.Is(context.Cause(ctx), errTokenExpired):
			writeErrorLine(w, &auth.Error{Code: auth.CodeUnauthenticated,
				Detail: "the bearer expired while the feed was open; follow again with a fresh token and the newest seq you hold"}, requestID)
			return
		default:
			writeErrorLine(w, err, requestID)
			return
		}
		for _, record := range records {
			if gate != nil {
				allowed, err := gate.allows(record)
				if err != nil {
					writeErrorLine(w, err, requestID)
					return
				}
				if !allowed {
					continue
				}
			}
			line, err := events.Body(record)
			if err != nil {
				writeErrorLine(w, err, requestID)
				return
			}
			if !writeLine(w, line) {
				return
			}
			if object && events.Ends(record.Type) {
				return
			}
		}
	}
}

// writeLine writes one line and flushes it, and reports whether the caller is
// still reading.
func writeLine(w http.ResponseWriter, line []byte) bool {
	if _, err := w.Write(append(line, '\n')); err != nil {
		return false
	}
	flush(w)
	return true
}

// writeErrorLine is the last line of a feed that ended on a failure: the
// envelope design 008's table gives the error, on one line.
func writeErrorLine(w http.ResponseWriter, err error, requestID string) {
	_, envelope := errorEnvelope(err, requestID)
	line, marshal := json.Marshal(httpjson.ErrorEnvelope{Error: envelope})
	if marshal != nil {
		line = []byte(`{"error":{"code":"` + envelope.Code + `"}}`)
	}
	writeLine(w, line)
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// expiry is the bearer's exp claim. Every token the verifier accepts carries
// one, as a JSON number.
func expiry(c auth.Caller) (time.Time, bool) {
	var seconds float64
	switch exp := c.Claims["exp"].(type) {
	case float64:
		seconds = exp
	case int64:
		seconds = float64(exp)
	case json.Number:
		parsed, err := exp.Float64()
		if err != nil {
			return time.Time{}, false
		}
		seconds = parsed
	default:
		return time.Time{}, false
	}
	whole, frac := math.Modf(seconds)
	return time.Unix(int64(whole), int64(frac*1e9)), true
}

// listVerdict is one kind's list decision for a feed of every object: whether
// the caller may list the kind at all, and the filter the decision carried.
type listVerdict struct {
	allowed bool
	filter  *authz.Filter
}

// recordGate applies the list route's rule to a feed of every object, one
// record at a time: the kind's list decision and its filter on owner and
// labels, then the kind's read on the record's own object. Each decision is
// asked once per kind or per object and held for the feed. A record about the
// default environment passes the filter as that environment does in the list,
// and its read decides it.
//
// The resource is built from the record and never from a read of the object,
// because the object a deleted record is about no longer exists.
type recordGate struct {
	h                  *handler
	r                  *http.Request
	defaultEnvironment string
	lists              map[string]listVerdict
	reads              map[string]bool
}

// allows reports whether the caller may read the record. A refusal is a
// record the feed skips; any other failure ends the feed.
func (g *recordGate) allows(record events.Record) (bool, error) {
	list, read, resource, known := actionsOf(record.Object)
	if !known {
		return false, nil
	}
	verdict, decided := g.lists[list]
	if !decided {
		d, err := g.h.decide(g.r, list, auth.List(list))
		switch {
		case err == nil:
			verdict = listVerdict{allowed: true, filter: d.Filter}
		case auth.CodeOf(err) != auth.CodeForbidden:
			return false, err
		}
		g.lists[list] = verdict
	}
	if !verdict.allowed || !g.admits(verdict.filter, record.Object) {
		return false, nil
	}
	key := fmt.Sprint(record.Object.Kind, "\x00", record.Object.ID, "\x00", record.Object.Owner, "\x00", record.Object.Labels)
	if allowed, decided := g.reads[key]; decided {
		return allowed, nil
	}
	allowed := true
	if _, err := g.h.decide(g.r, read, resource); err != nil {
		if auth.CodeOf(err) != auth.CodeForbidden {
			return false, err
		}
		allowed = false
	}
	if len(g.reads) >= readCache {
		clear(g.reads)
	}
	g.reads[key] = allowed
	return allowed, nil
}

// admits is the list's filter step over one record's object, the same step
// the list route takes over one row.
func (g *recordGate) admits(filter *authz.Filter, o events.Object) bool {
	if o.Kind == events.KindEnvironment {
		return admitsEnvironment(filter, g.defaultEnvironment, o.Name, o.Owner, o.Labels)
	}
	return admits(filter, o.Owner, o.Labels)
}

// actionsOf is the list and read actions of a record's kind and the resource
// the read is decided on, built from what the record carries.
func actionsOf(o events.Object) (string, string, authz.Resource, bool) {
	switch o.Kind {
	case events.KindSandbox:
		return authorizer.ActionSandboxList, authorizer.ActionSandboxRead,
			(auth.Sandbox{ID: o.ID, Name: o.Name, Owner: o.Owner, Labels: o.Labels}).Resource(), true
	case events.KindSecret:
		return authorizer.ActionSecretList, authorizer.ActionSecretRead,
			(auth.Secret{ID: o.ID, Name: o.Name, Owner: o.Owner, Labels: o.Labels}).Resource(), true
	case events.KindEnvironment:
		return authorizer.ActionEnvironmentList, authorizer.ActionEnvironmentRead,
			(auth.Environment{ID: o.ID, Name: o.Name, Owner: o.Owner, Labels: o.Labels}).Resource(), true
	}
	return "", "", authz.Resource{}, false
}
