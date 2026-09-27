// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/store"
)

// The cadences of spec 076: how often a standby looks for the writer lease and
// a writer renews it, and how long a read of where the writer is reached is
// trusted.
const (
	leasePoll   = time.Second
	lookupFresh = 100 * time.Millisecond
	idlePoll    = 50 * time.Millisecond
)

// errDemoted is a writer that lost the writer lease: another replica holds it,
// or no renewal succeeded for the lease's whole term. The process exits and
// comes back as a standby.
var errDemoted = errors.New("this process lost the writer lease")

// roleSwitch is this process's half of the API slot of spec 076: whether it
// answers requests itself, which it does once it promoted to writer and until
// it hands off, and the requests it is answering now. The forwarder of the
// API package asks it first and holds or forwards what it declines.
type roleSwitch struct {
	mu sync.Mutex
	// local is the API once this process promoted, and serving whether it
	// serves requests itself now; a writer handing off stops serving before
	// its API is gone.
	local   http.Handler
	serving bool
	// changed is closed and replaced at every change of the two, which is
	// what a held request waits on.
	changed chan struct{}
	// active counts the requests the API is answering now, which a handoff
	// waits for.
	active int
	// cut closes when a handoff's wait runs out, which ends the context of
	// every request the API still answers.
	cut     chan struct{}
	cutOnce sync.Once
}

func newRoleSwitch() *roleSwitch {
	return &roleSwitch{changed: make(chan struct{}), cut: make(chan struct{})}
}

// watch returns the channel closed at the next change of whether this process
// serves.
func (s *roleSwitch) watch() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// serveLocally answers the request with the API when this process serves, and
// reports whether it did. The request's context ends with the caller or when
// a handoff's wait runs out, whichever is first.
func (s *roleSwitch) serveLocally(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	if !s.serving || s.local == nil {
		s.mu.Unlock()
		return false
	}
	local := s.local
	s.active++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	answered := make(chan struct{})
	defer close(answered)
	go func() {
		select {
		case <-s.cut:
			cancel()
		case <-answered:
		}
	}()
	local.ServeHTTP(w, r.WithContext(ctx))
	return true
}

// promoted makes the API this process's answer to every request.
func (s *roleSwitch) promoted(local http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.local, s.serving = local, true
	s.signal()
}

// stopServing is the first step of a handoff: from here a request is held for
// the successor rather than answered here.
func (s *roleSwitch) stopServing() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serving = false
	s.signal()
}

// signal wakes every held request. The caller holds s.mu.
func (s *roleSwitch) signal() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// drain waits until the API answers no request or the timeout passes, then
// ends the context of every request it still answers and waits for those to
// return, for at most the same timeout again. It reports how many were cut.
func (s *roleSwitch) drain(timeout time.Duration) int {
	if s.waitIdle(timeout) {
		return 0
	}
	s.mu.Lock()
	cut := s.active
	s.mu.Unlock()
	s.cutOnce.Do(func() { close(s.cut) })
	s.waitIdle(timeout)
	return cut
}

// waitIdle reports whether the API came to answer no request inside timeout.
func (s *roleSwitch) waitIdle(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		idle := s.active == 0
		s.mu.Unlock()
		if idle {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(idlePoll)
	}
}

// writerLookup reads where the writer is reached from the writer lease, with a
// short memory so a burst of held requests costs one read each poll. The
// lease names no writer to forward to while it lapsed, while it is this
// process's own, or while its holder advertises no address.
type writerLookup struct {
	bound *store.Controlled

	mu     sync.Mutex
	cached string
	read   time.Time
}

func (l *writerLookup) address(ctx context.Context) (string, error) {
	l.mu.Lock()
	if time.Since(l.read) < lookupFresh {
		cached := l.cached
		l.mu.Unlock()
		return cached, nil
	}
	l.mu.Unlock()
	lease, err := l.bound.Lease(ctx, store.WriterLease)
	if err != nil {
		return "", err
	}
	target := lease.Address
	if !lease.Live || lease.Holder == l.bound.Holder() {
		target = ""
	}
	l.mu.Lock()
	l.cached, l.read = target, time.Now()
	l.mu.Unlock()
	return target, nil
}

// leaseHolder is the writer lease as the role needs it: take or renew it, and
// read who holds it.
type leaseHolder interface {
	Acquire(ctx context.Context, name string, ttl time.Duration) (bool, error)
	Lease(ctx context.Context, name string) (store.Lease, error)
	Holder() string
}

// awaitWriterLease takes the writer lease once it is free, polling each
// leasePoll. A standby reads the row first and asks to take it only when no
// live holder has it, so its poll takes no row lock while a writer holds it.
// It returns true once held and false when stop closes first.
func awaitWriterLease(ctx context.Context, lease leaseHolder, stop <-chan struct{}, log *slog.Logger) bool {
	ticker := time.NewTicker(leasePoll)
	defer ticker.Stop()
	for {
		row, err := lease.Lease(ctx, store.WriterLease)
		switch {
		case err != nil:
			log.WarnContext(ctx, "the writer lease could not be read", "err", err)
		case !row.Live || row.Holder == lease.Holder():
			held, err := lease.Acquire(ctx, store.WriterLease, controller.LeaseTTL)
			if err != nil {
				log.WarnContext(ctx, "the writer lease could not be taken", "err", err)
			}
			if held {
				return true
			}
		}
		select {
		case <-stop:
			return false
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// watchWriterLease renews the writer lease each leasePoll for as long as this
// process is the writer, and returns errDemoted once it is not: the store
// reports another holder, or no renewal succeeded for ttl. One failed renewal
// is not a loss, since the next may succeed well inside the term. It returns
// nil when stop closes first.
func watchWriterLease(ctx context.Context, lease leaseHolder, ttl time.Duration, stop <-chan struct{}, log *slog.Logger) error {
	ticker := time.NewTicker(leasePoll)
	defer ticker.Stop()
	renewed := time.Now()
	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
		}
		held, err := lease.Acquire(ctx, store.WriterLease, ttl)
		switch {
		case err == nil && !held:
			return fmt.Errorf("%w: another replica holds it", errDemoted)
		case err == nil:
			renewed = time.Now()
		case time.Since(renewed) > ttl:
			return fmt.Errorf("%w: no renewal succeeded for %s: %w", errDemoted, ttl, err)
		default:
			log.WarnContext(ctx, "the writer lease was not renewed", "err", err, "since", time.Since(renewed))
		}
	}
}
