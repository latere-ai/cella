// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"latere.ai/x/pkg/otel"
)

// The delivery loop's shape, from design 009.
const (
	// LeaseName is the lease of design 010 delivery runs under, so one
	// replica of a set delivers and the others journal.
	LeaseName = "journal"
	// LeaseTTL is the term, renewed by the store underneath.
	LeaseTTL = 15 * time.Second
	// BatchSize is how many records one pass takes, at most one per object.
	BatchSize = 64
	// Concurrency is how many objects one pass delivers at a time, so a
	// slow object holds only its own successors.
	Concurrency = 16
	// MinBackoff and MaxBackoff bound the wait after a failed attempt,
	// doubling per attempt in between.
	MinBackoff = time.Second
	MaxBackoff = 5 * time.Minute
	// Interval is how often a pass runs when the last one had nothing.
	Interval = time.Second
	// DefaultTimeout is one attempt's deadline, CELLA_EVENTS_TIMEOUT.
	DefaultTimeout = 10 * time.Second
	// DefaultRetryWindow is how long a record is retried before it is
	// dropped, CELLA_EVENTS_RETRY_WINDOW.
	DefaultRetryWindow = 24 * time.Hour
	// maxAnswer is how much of a sink's answer is drained before the body
	// is closed. Only the status decides anything, and a sink that replies
	// with a stream does not hold this process open.
	maxAnswer = 1 << 12
)

// Lease is the single-writer seam of design 010, as delivery needs it.
type Lease interface {
	Acquire(ctx context.Context, name string, ttl time.Duration) (held bool, err error)
}

// Clock is the deliverer's view of time, so a test drives the backoff and
// the retry window without waiting for either.
type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

// DelivererOptions configures one deliverer.
type DelivererOptions struct {
	Journal Journal
	Lease   Lease
	// URL is CELLA_EVENTS_URL and Secrets the one or two halves of
	// CELLA_EVENTS_SECRET, in the order the variable lists them.
	URL     string
	Secrets []string
	// Timeout bounds one attempt and RetryWindow how long a record is
	// retried before it is dropped. Zero takes the default.
	Timeout     time.Duration
	RetryWindow time.Duration
	Client      *http.Client
	Clock       Clock
	Log         *slog.Logger
	// Metrics is design 017's recorder. It is optional: with none delivery
	// keeps only the counts Stats reports.
	Metrics Metrics
}

// Deliverer takes records off the journal and posts them to the sink, in
// sequence order per object, at least once.
type Deliverer struct {
	o         DelivererOptions
	delivered atomic.Int64
	dropped   atomic.Int64
	deferred  atomic.Int64
}

// NewDeliverer builds the loop. It refuses a configuration a start-up should
// not have accepted, so a misconfigured process fails here rather than
// signing with no secret.
func NewDeliverer(o DelivererOptions) (*Deliverer, error) {
	switch {
	case o.Journal == nil:
		return nil, errors.New("events: delivery needs a journal")
	case o.Lease == nil:
		return nil, errors.New("events: delivery needs a lease")
	case o.URL == "":
		return nil, errors.New("events: delivery needs CELLA_EVENTS_URL")
	case len(o.Secrets) == 0:
		return nil, errors.New("events: delivery needs CELLA_EVENTS_SECRET")
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.RetryWindow <= 0 {
		o.RetryWindow = DefaultRetryWindow
	}
	if o.Client == nil {
		// Every outbound client this repository builds is instrumented, so
		// one delivery is a span on the same trace as the act it reports.
		o.Client = &http.Client{Timeout: o.Timeout, Transport: otel.Transport(nil)}
	}
	if o.Clock == nil {
		o.Clock = wallClock{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Metrics == nil {
		o.Metrics = nopMetrics{}
	}
	return &Deliverer{o: o}, nil
}

// Stats is what the deliverer counts, until the metric names of design 017
// land: records the sink took, records dropped, and attempts deferred.
type Stats struct {
	Delivered int64
	Dropped   int64
	Deferred  int64
}

// Stats reports the counts.
func (d *Deliverer) Stats() Stats {
	return Stats{Delivered: d.delivered.Load(), Dropped: d.dropped.Load(), Deferred: d.deferred.Load()}
}

// Run delivers until the context ends. A pass that moved a full batch runs
// again at once, so a backlog drains as fast as the sink takes it.
func (d *Deliverer) Run(ctx context.Context) {
	for {
		moved, err := d.Pass(ctx)
		if err != nil && ctx.Err() == nil {
			d.o.Log.WarnContext(ctx, "the event delivery pass failed", "error", err)
		}
		if moved >= BatchSize && ctx.Err() == nil {
			continue
		}
		timer := time.NewTimer(Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Pass is one round: take the lease, read what is due, and deliver it. It
// reports how many records it attempted, which is what Run paces on.
func (d *Deliverer) Pass(ctx context.Context) (int, error) {
	held, err := d.o.Lease.Acquire(ctx, LeaseName, LeaseTTL)
	d.o.Metrics.LeaseHeld(MetricLeaseJournal, err == nil && held)
	if err != nil {
		return 0, fmt.Errorf("events: taking the %s lease: %w", LeaseName, err)
	}
	if !held {
		return 0, nil
	}
	now := d.o.Clock.Now()
	rows, err := d.o.Journal.Pending(ctx, BatchSize, now)
	if err != nil {
		return 0, fmt.Errorf("events: reading what is pending: %w", err)
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, Concurrency)
	for _, row := range rows {
		wg.Add(1)
		slots <- struct{}{}
		go func(p Pending) {
			defer wg.Done()
			defer func() { <-slots }()
			d.deliver(ctx, p, now)
		}(row)
	}
	wg.Wait()
	return len(rows), nil
}

// deliver posts one record and records the outcome on its row. now is the
// pass's instant, so every record of one pass is signed and aged by one
// clock.
func (d *Deliverer) deliver(ctx context.Context, p Pending, now time.Time) {
	body, err := Body(p.Record)
	if err != nil {
		// A record that does not marshal will not marshal tomorrow.
		d.drop(ctx, p, now, "the record could not be encoded: "+err.Error())
		return
	}
	if now.Sub(p.Record.Time) >= d.o.RetryWindow {
		d.drop(ctx, p, now, fmt.Sprintf("unacknowledged for %s, past the %s retry window",
			now.Sub(p.Record.Time).Round(time.Second), d.o.RetryWindow))
		return
	}
	attempt, cancel := context.WithTimeout(ctx, d.o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attempt, http.MethodPost, d.o.URL, bytes.NewReader(body))
	if err != nil {
		d.drop(ctx, p, now, "the sink URL is not usable: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, Header(now, body, d.o.Secrets...))
	attempted := time.Now()
	resp, err := d.o.Client.Do(req)
	d.o.Metrics.EventDeliveryDuration(time.Since(attempted))
	if err != nil {
		d.retry(ctx, p, now, "the sink did not answer: "+err.Error(), false)
		return
	}
	// The answer is drained and closed so the connection is reused. Nothing
	// in it but the status decides anything.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswer))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if err := d.o.Journal.Acknowledge(ctx, p.Record.ID, now); err != nil {
			d.o.Log.WarnContext(ctx, "the delivered event was not acknowledged",
				"event", p.Record.ID, "error", err)
			return
		}
		d.delivered.Add(1)
		d.o.Metrics.EventDelivered(MetricAcknowledged)
	case resp.StatusCode == http.StatusUnauthorized:
		// A 401 is the one 4xx that says nothing about these bytes: the two
		// ends hold different secrets. Retrying the same body cannot help
		// until an operator repairs the configuration, and dropping would
		// discard every record emitted during a botched rotation, so the
		// record is held and the fault is made loud.
		d.retry(ctx, p, now, "the sink refused the signature; CELLA_EVENTS_SECRET and the sink's disagree", true)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		d.drop(ctx, p, now, fmt.Sprintf("the sink refused the record with %d", resp.StatusCode))
	default:
		d.retry(ctx, p, now, fmt.Sprintf("the sink answered %d", resp.StatusCode), false)
	}
}

// retry defers one record, or drops it where the next attempt would fall
// outside the window.
func (d *Deliverer) retry(ctx context.Context, p Pending, now time.Time, why string, loud bool) {
	next := now.Add(backoff(p.Attempts))
	if next.Sub(p.Record.Time) >= d.o.RetryWindow {
		d.drop(ctx, p, now, why+", and the retry window has closed")
		return
	}
	level := slog.LevelWarn
	if loud {
		level = slog.LevelError
	}
	d.o.Log.Log(ctx, level, "the event was not delivered",
		"event", p.Record.ID, "type", p.Record.Type, "object", p.Record.Object.ID,
		"attempts", p.Attempts+1, "next", next, "reason", why)
	if err := d.o.Journal.Defer(ctx, p.Record.ID, next); err != nil {
		d.o.Log.WarnContext(ctx, "the event's next attempt was not recorded",
			"event", p.Record.ID, "error", err)
		return
	}
	d.deferred.Add(1)
	d.o.Metrics.EventDelivered(MetricDeferred)
}

// drop ends a record and says so, because a dropped record is the one
// failure this pipeline cannot make good later.
func (d *Deliverer) drop(ctx context.Context, p Pending, at time.Time, why string) {
	d.o.Log.ErrorContext(ctx, "the event was dropped",
		"event", p.Record.ID, "type", p.Record.Type, "object", p.Record.Object.ID,
		"attempts", p.Attempts, "reason", why)
	if err := d.o.Journal.Drop(ctx, p.Record.ID, at); err != nil {
		d.o.Log.WarnContext(ctx, "the dropped event was not marked", "event", p.Record.ID, "error", err)
		return
	}
	d.dropped.Add(1)
	d.o.Metrics.EventDelivered(MetricDropped)
}

// backoff is the wait before attempt n+1, doubling from MinBackoff and held
// at MaxBackoff.
func backoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	wait := MinBackoff
	for range attempts {
		wait *= 2
		if wait >= MaxBackoff {
			return MaxBackoff
		}
	}
	return wait
}
