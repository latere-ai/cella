// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// The refusals a registration can meet. Both are 422 environment_mismatch at
// the API: a worker whose driver or isolation class differs from what the
// environment already recorded is a worker on the wrong environment, and
// admitting it would make one environment two.
var (
	ErrRegistrationMismatch = errors.New("remote: this worker reports another driver or isolation class than the environment's")
	ErrUnknownWorker        = errors.New("remote: this worker has not registered")
	// ErrWorkerLimit is a registration, or a stream, beyond the
	// environment's spec.workers.max, which is 409 worker_limit at the API
	// and the stream closed with that reason (spec 083).
	ErrWorkerLimit = errors.New("remote: the environment holds as many live workers as it takes")
	// ErrServeRefused is a recheck the authorizer refused: the
	// environment may not be served any longer.
	ErrServeRefused = errors.New("remote: the environment may not be served")
)

// MaxRecheck is the longest a live stream goes without environment.serve
// being asked again, whatever TTL the decision named (spec 083).
const MaxRecheck = 5 * time.Minute

// CloseTryAgain is the close code of a stream the control plane ends because
// it could not decide whether the worker may go on serving: 1013, try again
// later. It is no refusal, so the worker reconnects and asks again.
const CloseTryAgain = 1013

// ReasonAuthorizerUnavailable is the reason a stream closed with
// CloseTryAgain carries.
const ReasonAuthorizerUnavailable = "authorizer_unavailable"

// pendingWindow is how long a registration whose stream has not opened yet
// counts as a live worker against spec.workers.max, so two registrations
// racing for the last place do not both take it.
const pendingWindow = HeartbeatTimeout

// Admission is what the control plane knows of a registration beyond what
// the worker sent: the key it registered with, the environment's limit on
// live workers, and how long the decision that admitted it holds.
type Admission struct {
	// JTI is the jti of the key the worker authenticated with, read from
	// the verified bearer and never from the body.
	JTI string
	// MaxWorkers is the environment's spec.workers.max, zero for no limit.
	MaxWorkers int
	// TTL is how long the environment.serve decision that admitted the
	// registration holds, which the stream's first recheck waits.
	TTL time.Duration
}

// Replacement is a registration that named the key of a live worker on an
// environment already holding as many as it takes: the worker it met, both
// declarations, and whether it replaced that worker, which it does only when
// the two report the same host (spec 083).
type Replacement struct {
	Worker   string
	Previous Registration
	Next     Registration
	Replaced bool
}

// Queue is the operations table of design 010 as the hub uses it: the row
// that records who holds an operation, so a dropped connection redelivers to
// whichever worker and replica is live. It is optional. Without one the hub
// keeps the operation in memory alone, which is what a single process with no
// store does.
type Queue interface {
	Enqueue(ctx context.Context, environment, id, sandbox, opType string, payload []byte) error
	Acknowledge(ctx context.Context, id string, result []byte) error
}

// HubOptions configures the control plane's side of every worker stream.
type HubOptions struct {
	// Offline is how long without a heartbeat before an environment's
	// workers are no longer live, from CELLA_ENVIRONMENT_OFFLINE.
	Offline time.Duration
	// NewID mints one operation id: OperationIDLen bytes. Nil takes a
	// built-in generator.
	NewID func() string
	// Now is the clock, for the phase loop's tests. Nil is the wall clock.
	Now   func() time.Time
	Queue Queue
	// Window is the credit the control plane grants per sub-stream, which it
	// announces in answer to a worker's hello. Zero takes DefaultWindow.
	Window int
	Log    *slog.Logger
}

// Hub is every worker stream one control plane holds, grouped by the
// environment each serves. It never dials a worker: a worker connects
// outbound and the hub answers on the stream the worker opened, which is
// invariant 10 of design 001.
type Hub struct {
	offline time.Duration
	newID   func() string
	now     func() time.Time
	queue   Queue
	window  int
	log     *slog.Logger

	mu           sync.Mutex
	environments map[string]*environment
	// creates is every create in flight by its operation id, so a worker
	// that holds one is matched to the sandbox the control plane sent it
	// for, and never to one the worker names.
	creates map[string]pendingCreate
	// onHeld is told of every create a worker holds while its host decides.
	onHeld func(environment, sandbox string)
}

// pendingCreate is one create the hub sent and has not seen answered.
type pendingCreate struct {
	environment string
	sandbox     string
}

// environment is one registered data plane: what its workers declared, the
// connections they hold, and what they report of their sandboxes.
type environment struct {
	registration Registration
	registered   bool
	// maxWorkers is spec.workers.max as the latest registration read it.
	maxWorkers int
	workers    map[string]*worker
	// observed is the last state the workers reported, and reported says
	// whether any of them has. A control plane that has been told nothing
	// asks the worker rather than answering an empty world.
	observed map[string]runtime.State
	reported bool
	// subscribers are the consumers of the environment's events, through
	// the remote driver's Watch.
	subscribers map[*subscription]struct{}
}

// worker is one registration and, while it holds one, its stream.
type worker struct {
	id            string
	replica       string
	lastHeartbeat time.Time
	link          *Link
	// host is the machine the worker reported, nil where its driver
	// reports none.
	host *v1.HostInfo
	// jti is the key the worker registered with, declared what it
	// registered as, registered when, and ttl how long the decision that
	// admitted it holds.
	jti        string
	declared   Registration
	registered time.Time
	ttl        time.Duration
	// ended marks a registration whose stream was refused, which no longer
	// holds a place while its pending window runs.
	ended bool
}

// NewHub returns a hub holding no environment and no connection.
func NewHub(o HubOptions) *Hub {
	h := &Hub{
		offline: o.Offline, newID: o.NewID, now: o.Now, queue: o.Queue, window: o.Window, log: o.Log,
		environments: map[string]*environment{}, creates: map[string]pendingCreate{},
	}
	if h.offline <= 0 {
		h.offline = DefaultOffline
	}
	// The lease is the floor. A window below it would declare a worker gone
	// while the connection it holds is still inside its own read deadline,
	// so an operator who shortened CELLA_ENVIRONMENT_OFFLINE past the
	// heartbeat would have every operation refused between two heartbeats.
	// The window an environment's phase is computed from is the control
	// plane's and may be shorter; what a worker may be sent is this.
	h.offline = max(h.offline, HeartbeatTimeout)
	if h.newID == nil {
		h.newID = NewOperationID
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	return h
}

// DefaultOffline is how long a worker is held live without a heartbeat, which
// CELLA_ENVIRONMENT_OFFLINE sets and HeartbeatTimeout floors.
const DefaultOffline = 2 * time.Minute

// Register records one worker on one environment and returns the id it claims
// under. The driver name and the isolation class are recorded from the first
// registration and every later worker must report the same, or the
// environment would be two data planes under one name.
func (h *Hub) Register(environmentID string, r Registration) (Registered, error) {
	registered, _, err := h.RegisterWith(environmentID, r, Admission{})
	return registered, err
}

// RegisterWith is Register with what the control plane knows of the
// registration. On an environment holding as many live workers as its
// spec.workers.max takes, a registration is refused with ErrWorkerLimit,
// except one with the key of a live worker that reports the same host: it
// replaces that worker, whose stream is closed with worker_replaced, because
// a worker reconnecting after a dropped connection must not wait out the
// lease. The Replacement says what met what, for the record the control
// plane writes, whether it replaced the worker or was refused.
func (h *Hub) RegisterWith(environmentID string, r Registration, a Admission) (Registered, *Replacement, error) {
	if environmentID == "" || r.Driver == "" || r.Isolation == "" {
		return Registered{}, nil, fmt.Errorf("%w: a registration names an environment, a driver and an isolation class", runtime.ErrInvalid)
	}
	h.mu.Lock()
	registered, replacement, displaced, err := h.registerLocked(environmentID, r, a)
	h.mu.Unlock()
	// The replaced stream is shut after the lock is released: a link's
	// shutdown and the connection that releases it both take that lock.
	if displaced != nil {
		displaced.Shutdown(&CloseError{Code: ClosePolicy, Reason: ReasonWorkerReplaced})
	}
	return registered, replacement, err
}

func (h *Hub) registerLocked(environmentID string, r Registration, a Admission) (Registered, *Replacement, *Link, error) {
	env := h.environmentLocked(environmentID)
	if env.registered && (env.registration.Driver != r.Driver || env.registration.Isolation != r.Isolation) {
		return Registered{}, nil, nil, fmt.Errorf("%w: the environment runs %s with %s isolation and this worker reports %s with %s",
			ErrRegistrationMismatch, env.registration.Driver, env.registration.Isolation, r.Driver, r.Isolation)
	}
	env.maxWorkers = a.MaxWorkers
	replacement, displaced, err := h.admitLocked(env, r, a)
	if err != nil {
		return Registered{}, replacement, nil, err
	}
	switch {
	case !env.registered:
		env.registration = Registration{
			Driver: r.Driver, Isolation: r.Isolation, Capabilities: r.Capabilities,
			Capacity: r.Capacity, Labels: maps.Clone(r.Labels), Version: r.Version,
		}
		env.registered = true
	default:
		// Several workers serve one environment and what it can do is what
		// all of them can do: a caller gated on a capability must not reach
		// a worker that does not provide it.
		env.registration.Capabilities = intersect(env.registration.Capabilities, r.Capabilities)
	}
	id := r.Worker
	if id == "" {
		id = "wrk_" + h.newID()
	}
	now := h.now()
	w := &worker{id: id, replica: r.Version, lastHeartbeat: now, jti: a.JTI, declared: r, registered: now, ttl: a.TTL}
	if r.Host != nil {
		host := cloneHost(*r.Host)
		w.host = &host
		w.declared.Host = &host
	}
	env.workers[id] = w
	return Registered{
		Worker: id, Environment: environmentID,
		HeartbeatInterval: v1.Duration(HeartbeatInterval.String()), Lease: v1.Duration(HeartbeatTimeout.String()),
	}, replacement, displaced, nil
}

// admitLocked holds a registration to spec.workers.max. Below the limit, or
// with no limit, it admits. At the limit it admits only a registration with
// the key of a live worker reporting the same host, which replaces that
// worker: its record is dropped here and its link returned to be shut.
func (h *Hub) admitLocked(env *environment, r Registration, a Admission) (*Replacement, *Link, error) {
	if a.MaxWorkers <= 0 {
		return nil, nil, nil
	}
	live := h.liveWorkersLocked(env, "")
	if len(live) < a.MaxWorkers {
		return nil, nil, nil
	}
	for _, w := range live {
		if a.JTI == "" || w.jti != a.JTI {
			continue
		}
		replacement := &Replacement{Worker: w.id, Previous: w.declared, Next: r}
		if !sameHost(w.host, r.Host) {
			return replacement, nil, fmt.Errorf("%w: a live worker holds this key and reports another host", ErrWorkerLimit)
		}
		replacement.Replaced = true
		delete(env.workers, w.id)
		return replacement, w.link, nil
	}
	return nil, nil, fmt.Errorf("%w: it takes %d", ErrWorkerLimit, a.MaxWorkers)
}

// liveWorkersLocked is every worker of the environment that counts against
// spec.workers.max: one holding a stream inside the offline window, and one
// whose registration is younger than the pending window and whose stream has
// not opened yet. except names one worker left out of the count.
func (h *Hub) liveWorkersLocked(env *environment, except string) []*worker {
	now := h.now()
	var out []*worker
	for _, w := range env.workers {
		switch {
		case w.id == except:
		case w.link != nil && !w.lastHeartbeat.Before(now.Add(-h.offline)):
			out = append(out, w)
		case w.link == nil && w.registered.After(now.Add(-pendingWindow)) && !w.ended:
			out = append(out, w)
		}
	}
	slices.SortFunc(out, func(a, b *worker) int { return strings.Compare(a.id, b.id) })
	return out
}

// sameHost reports whether two registrations report the same host facts:
// both none, or the same operating system, architecture, grants and network.
func sameHost(a, b *v1.HostInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// Revoke closes every stream a key registered, with key_revoked, and drops
// those workers' records, so a revoked key stops serving at once rather than
// at its next request (spec 083). It reports how many streams it closed.
func (h *Hub) Revoke(jti string) int {
	if jti == "" {
		return 0
	}
	h.mu.Lock()
	var links []*Link
	for _, env := range h.environments {
		for id, w := range env.workers {
			if w.jti != jti {
				continue
			}
			if w.link != nil {
				links = append(links, w.link)
			}
			delete(env.workers, id)
		}
	}
	h.mu.Unlock()
	for _, link := range links {
		link.Shutdown(&CloseError{Code: ClosePolicy, Reason: ReasonKeyRevoked})
	}
	return len(links)
}

// Forget drops one worker's registration, which is what a clean disconnect
// writes so the environment's phase moves at once.
func (h *Hub) Forget(environmentID, workerID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if env, held := h.environments[environmentID]; held {
		delete(env.workers, workerID)
	}
}

// Serve runs one worker's stream until it ends. The first frame is the hello
// naming the registration this connection is, per spec 021; a hello naming a
// worker that never registered is ErrUnknownWorker, because the control plane
// has nothing to route to it. Nothing is dispatched to the connection before
// the hello, so a socket that opens and says nothing holds no work.
func (h *Hub) Serve(ctx context.Context, environmentID string, conn FrameConn) error {
	return h.ServeWith(ctx, environmentID, conn, ServeOptions{})
}

// ServeOptions is what the control plane adds to one worker stream.
type ServeOptions struct {
	// Recheck asks environment.serve again for the registration the
	// stream's hello named, and returns how long the answer holds. An
	// error wrapping ErrServeRefused closes the stream with serve_refused;
	// any other error is no decision, and a second in a row closes it with
	// CloseTryAgain so the worker reconnects and is decided anew. Nil asks
	// nothing while the stream lives.
	Recheck func(ctx context.Context, r Registration) (time.Duration, error)
}

// ServeWith is Serve with the control plane's options for the stream.
func (h *Hub) ServeWith(ctx context.Context, environmentID string, conn FrameConn, o ServeOptions) error {
	c := &connection{hub: h, environment: environmentID, recheck: o.Recheck, ctx: ctx}
	c.link = NewLink(conn, LinkOptions{OnMessage: c.onMessage, Window: h.window})
	defer c.release()
	go h.beat(ctx, c.link)
	err := c.link.Run(ctx)
	if err != nil && ctx.Err() == nil && !errors.Is(err, ErrLinkClosed) {
		h.log.WarnContext(ctx, "a worker's stream ended", "environment", environmentID, "err", err)
	}
	if c.bound() == nil {
		return ErrUnknownWorker
	}
	return err
}

// connection is one stream before and after its hello: the link, and the
// registration it named once it arrives.
type connection struct {
	hub         *Hub
	environment string
	link        *Link
	// recheck is the control plane's environment.serve, asked on the
	// decision's TTL while the stream lives, and ctx the stream's.
	recheck func(ctx context.Context, r Registration) (time.Duration, error)
	ctx     context.Context

	mu     sync.Mutex
	worker *worker
	// listing says a List a relist asked for is in flight on this
	// connection, and relistAgain that another relist arrived meanwhile, so
	// a burst of relists costs one List more rather than one each.
	listing     bool
	relistAgain bool
}

func (c *connection) bound() *worker {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.worker
}

// release drops this connection from the worker it bound to, so the
// environment stops being live at once rather than at the end of the lease.
func (c *connection) release() {
	w := c.bound()
	if w == nil {
		return
	}
	c.hub.mu.Lock()
	if w.link == c.link {
		w.link = nil
	}
	c.hub.mu.Unlock()
}

// onMessage takes what a worker sends that the link did not route itself: the
// hello that binds the connection, the heartbeats, the state its driver
// observes, and the events its driver's Watch reports.
func (c *connection) onMessage(operation string, m Message) {
	switch m.Type {
	case MessageHello:
		if err := c.hello(m); err != nil {
			c.hub.log.Warn("a worker's hello was refused",
				"environment", c.environment, "worker", m.Worker, "err", err)
			c.link.Shutdown(err)
		}
	case MessageHeartbeat:
		c.hub.stamp(c.environment, c.workerID())
	case MessageState:
		c.hub.stamp(c.environment, c.workerID())
		c.hub.observe(c.environment, m)
	case MessageHeld:
		c.hub.stamp(c.environment, c.workerID())
		c.hub.held(c.environment, operation)
	case MessageEvent:
		c.hub.stamp(c.environment, c.workerID())
		if err := c.event(m); err != nil {
			c.hub.log.Warn("a worker sent an event this control plane cannot read",
				"environment", c.environment, "err", err)
			c.link.Shutdown(err)
		}
	default:
		c.hub.log.Warn("a worker sent a frame that belongs the other way",
			"environment", c.environment, "frame", m.Type)
		c.link.Shutdown(fmt.Errorf("%w: %s belongs the other way", ErrFrame, m.Type))
	}
}

// hello answers a worker's first frame. A worker that announced a window
// gets this side's in answer, and credit is on for the connection; the answer
// is queued before the registration is bound, and nothing is sent to a worker
// before it is bound, so the worker has credit on before the first frame of
// any operation reaches it. A worker whose driver watches gets the
// connection's Watch operation.
func (c *connection) hello(m Message) error {
	if m.Window != 0 {
		if err := c.link.Credit(m.Window); err != nil {
			return err
		}
		if err := c.link.Send(NoOperation, Message{Type: MessageHello, Window: c.link.Window()}); err != nil {
			return err
		}
	}
	w, err := c.bind(m.Worker)
	if err != nil {
		return err
	}
	if m.Watch {
		go c.watch()
	}
	if c.recheck != nil {
		go c.rechecks(w.declared, w.ttl)
	}
	return nil
}

// rechecks asks environment.serve again on every decision TTL, at most
// MaxRecheck apart, for as long as the stream lives. A refusal closes the
// stream with serve_refused. A recheck that produced no decision keeps the
// stream for one more TTL, and a second one closes it with CloseTryAgain:
// the worker reconnects, and its registration is decided anew.
func (c *connection) rechecks(r Registration, ttl time.Duration) {
	missed := false
	for {
		select {
		case <-c.link.Done():
			return
		case <-time.After(recheckAfter(ttl)):
		}
		next, err := c.recheck(c.ctx, r)
		switch {
		case err == nil:
			missed, ttl = false, next
		case errors.Is(err, ErrServeRefused):
			c.hub.log.Warn("the authorizer refused a live worker's stream", "environment", c.environment, "err", err)
			c.link.Shutdown(&CloseError{Code: ClosePolicy, Reason: ReasonServeRefused})
			return
		case missed:
			c.hub.log.Warn("the authorizer gave no decision on a live worker's stream twice", "environment", c.environment, "err", err)
			c.link.Shutdown(&CloseError{Code: CloseTryAgain, Reason: ReasonAuthorizerUnavailable})
			return
		default:
			missed = true
		}
	}
}

// recheckAfter is how long a stream waits before environment.serve is asked
// again: the decision's TTL, and MaxRecheck where it named none or longer.
func recheckAfter(ttl time.Duration) time.Duration {
	if ttl <= 0 || ttl > MaxRecheck {
		return MaxRecheck
	}
	return ttl
}

// issue sends one operation of the connection's own on this connection
// alone: the Watch, and the List a relist asks for. Neither is a row of the
// operations table, because neither is a caller's and neither is redelivered.
func (c *connection) issue(opType string, req Request) (*Channel, error) {
	id := c.hub.newID()
	channel := c.link.Open(id, false)
	message := withOperationType(Message{Type: MessageOperation, Operation: id, Request: &req}, opType)
	if err := c.link.Send(id, message); err != nil {
		c.link.Drop(id)
		return nil, err
	}
	return channel, nil
}

// maxRewatchDelay bounds how long the control plane waits before it opens a
// worker's Watch again after one ended while the stream stayed up.
const maxRewatchDelay = 30 * time.Second

// watch holds the connection's Watch operation open for as long as the
// connection lasts. Whatever ended one, events may have been missed from then
// until the next opens, so every consumer is told to read the environment
// again. A Watch that ended while the stream stayed up is the worker's driver
// failing to watch, and it is opened again, sooner after one that ran for a
// while than after one that failed at once.
func (c *connection) watch() {
	delay := rewatchDelay
	for {
		opened := time.Now()
		err := c.watchOnce()
		c.hub.publish(c.environment, Event{Type: EventRelist})
		if c.link.Err() != nil {
			return
		}
		c.hub.log.Warn("a worker's watch ended", "environment", c.environment, "err", err, "retryIn", delay)
		if time.Since(opened) > maxRewatchDelay {
			delay = rewatchDelay
		}
		select {
		case <-c.link.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, maxRewatchDelay)
	}
}

// watchOnce opens one Watch and waits for it to end.
func (c *connection) watchOnce() error {
	channel, err := c.issue(OpWatch, Request{})
	if err != nil {
		return err
	}
	_, err = channel.Result(context.Background())
	if closeErr := channel.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = errors.New("remote: the worker ended its watch")
	}
	return err
}

// event applies one change a worker's driver observed. A relist is answered
// with a List on this connection, and the consumers read the relist once its
// answer is in place. An event of a type this release does not know is left
// alone, so a worker of a later release does not lose its connection over
// one.
func (c *connection) event(m Message) error {
	if m.Event == nil {
		return fmt.Errorf("%w: an event message carries an event", ErrFrame)
	}
	e := *m.Event
	switch e.Type {
	case EventRelist:
		c.relist()
	case EventAdded, EventModified, EventLost, EventDeleted:
		if e.State.ID == "" {
			return fmt.Errorf("%w: a %s event names the sandbox it is about", ErrFrame, e.Type)
		}
		c.hub.event(c.environment, e)
	default:
		c.hub.log.Warn("a worker reported an event of a type this control plane does not know",
			"environment", c.environment, "type", e.Type)
	}
	return nil
}

// relist issues the List a relist asks for, beside the read pump that
// delivered the relist, because its answer arrives on that same pump.
func (c *connection) relist() {
	c.mu.Lock()
	if c.listing {
		c.relistAgain = true
		c.mu.Unlock()
		return
	}
	c.listing = true
	c.mu.Unlock()
	go func() {
		for {
			c.list()
			c.mu.Lock()
			if !c.relistAgain {
				c.listing = false
				c.mu.Unlock()
				return
			}
			c.relistAgain = false
			c.mu.Unlock()
		}
	}()
}

// list replaces what the control plane holds of the environment with what
// the worker's driver lists, and then tells the consumers to read it again.
func (c *connection) list() {
	channel, err := c.issue(OpList, Request{Filter: &runtime.Filter{}})
	if err != nil {
		return // the connection ended before the List was sent
	}
	res, err := channel.Result(context.Background())
	if closeErr := channel.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		if c.link.Err() == nil {
			c.hub.log.Warn("the List a relist asked for failed", "environment", c.environment, "err", err)
		}
		return
	}
	c.hub.observe(c.environment, Message{Type: MessageState, States: res.States, Relist: true})
	c.hub.publish(c.environment, Event{Type: EventRelist})
}

func (c *connection) workerID() string {
	if w := c.bound(); w != nil {
		return w.id
	}
	return ""
}

// bind attaches this connection to the registration its hello named. A second
// hello on one connection is refused: one stream is one worker. A stream that
// would hold one worker more than spec.workers.max takes is refused with
// worker_limit, which catches two registrations that raced for one place.
func (c *connection) bind(workerID string) (*worker, error) {
	if workerID == "" {
		return nil, ErrUnknownWorker
	}
	if c.bound() != nil {
		return nil, fmt.Errorf("%w: one stream is one worker", ErrFrame)
	}
	c.hub.mu.Lock()
	defer c.hub.mu.Unlock()
	env, held := c.hub.environments[c.environment]
	if !held {
		return nil, ErrUnknownWorker
	}
	w, has := env.workers[workerID]
	if !has {
		return nil, ErrUnknownWorker
	}
	if env.maxWorkers > 0 {
		streams := 0
		for _, other := range c.hub.liveWorkersLocked(env, w.id) {
			if other.link != nil {
				streams++
			}
		}
		if streams >= env.maxWorkers {
			w.ended = true
			return nil, &CloseError{Code: ClosePolicy, Reason: ReasonWorkerLimit}
		}
	}
	w.link, w.lastHeartbeat = c.link, c.hub.now()
	c.mu.Lock()
	c.worker = w
	c.mu.Unlock()
	return w, nil
}

// beat sends the heartbeat an idle stream needs, so a worker that has nothing
// to do is still known to be there.
func (h *Hub) beat(ctx context.Context, link *Link) {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-link.Done():
			return
		case <-ticker.C:
			if err := link.Send(NoOperation, Message{Type: MessageHeartbeat}); err != nil {
				return
			}
		}
	}
}

func (h *Hub) stamp(environmentID, workerID string) {
	if workerID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if env, held := h.environments[environmentID]; held {
		if w, has := env.workers[workerID]; has {
			w.lastHeartbeat = h.now()
		}
	}
}

// observe records what a worker reported. A relist is the whole environment
// and replaces what the control plane held; anything else is merged, and a
// state the worker no longer has is dropped by the next relist.
func (h *Hub) observe(environmentID string, m Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	env := h.environmentLocked(environmentID)
	if m.Relist {
		env.observed = make(map[string]runtime.State, len(m.States))
	}
	for _, s := range m.States {
		env.observed[s.ID] = s
	}
	env.reported = true
}

// event applies one change to what the control plane holds of the
// environment and hands it to every consumer.
func (h *Hub) event(environmentID string, e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	env := h.environmentLocked(environmentID)
	if e.Type == EventDeleted {
		delete(env.observed, e.State.ID)
	} else {
		env.observed[e.State.ID] = e.State
	}
	env.publishLocked(e)
}

// publish hands one event to every consumer of the environment.
func (h *Hub) publish(environmentID string, e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if env, held := h.environments[environmentID]; held {
		env.publishLocked(e)
	}
}

func (env *environment) publishLocked(e Event) {
	for s := range env.subscribers {
		s.push(e)
	}
}

// Transport is the driver's side of one environment. It is valid before any
// worker has registered: a driver built over it answers that nothing is
// registered rather than failing to exist.
func (h *Hub) Transport(environmentID string) Transport {
	return &transport{hub: h, environment: environmentID}
}

// Workers is every registration of one environment with its last heartbeat,
// which is what the environment's phase is computed from.
func (h *Hub) Workers(environmentID string) []WorkerState {
	h.mu.Lock()
	defer h.mu.Unlock()
	env, held := h.environments[environmentID]
	if !held {
		return nil
	}
	out := make([]WorkerState, 0, len(env.workers))
	for _, w := range env.workers {
		state := WorkerState{Worker: w.id, LastHeartbeat: w.lastHeartbeat, Connected: w.link != nil}
		if w.host != nil {
			host := cloneHost(*w.host)
			state.Host = &host
		}
		out = append(out, state)
	}
	slices.SortFunc(out, func(a, b WorkerState) int { return strings.Compare(a.Worker, b.Worker) })
	return out
}

// WorkerState is one worker as the control plane sees it.
type WorkerState struct {
	Worker        string       `json:"worker"`
	LastHeartbeat time.Time    `json:"lastHeartbeat"`
	Connected     bool         `json:"connected"`
	Host          *v1.HostInfo `json:"host,omitempty"`
}

// OnHeld sets what the hub tells of a create a worker holds while its host
// decides: the environment and the sandbox the create is for. The control
// plane's API sets it once it has a controller to tell, and replaces it when
// a new one is built; nil tells nothing.
func (h *Hub) OnHeld(f func(environment, sandbox string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onHeld = f
}

// held passes on one held frame. The sandbox is the one this hub sent the
// operation for, read from its own record, so a worker cannot name another
// sandbox, and a frame for an operation that is not a create in flight on
// this environment is dropped.
func (h *Hub) held(environmentID, operation string) {
	h.mu.Lock()
	pending, ok := h.creates[operation]
	f := h.onHeld
	h.mu.Unlock()
	if !ok || pending.environment != environmentID || f == nil {
		return
	}
	f(pending.environment, pending.sandbox)
}

// cloneHost copies one host's lists, so a caller never writes through the
// hub's own record.
func cloneHost(h v1.HostInfo) v1.HostInfo {
	h.Grants = slices.Clone(h.Grants)
	h.Network.Hosts = slices.Clone(h.Network.Hosts)
	return h
}

// Release drops everything one environment holds, which is what a delete of
// the object writes.
//
// What it ends is read under the hub's lock and ended after it: a link's
// shutdown and a consumer's goroutine both take that lock on their way out.
func (h *Hub) Release(environmentID string) {
	h.mu.Lock()
	env, held := h.environments[environmentID]
	delete(h.environments, environmentID)
	var links []*Link
	var subscribers []*subscription
	if held {
		for _, w := range env.workers {
			if w.link != nil {
				links = append(links, w.link)
			}
		}
		for s := range env.subscribers {
			subscribers = append(subscribers, s)
		}
	}
	h.mu.Unlock()
	// A deleted environment may not be served, which the worker reads as
	// the end of everything it ran for the environment (spec 083).
	for _, link := range links {
		link.Shutdown(&CloseError{Code: ClosePolicy, Reason: ReasonServeRefused})
	}
	for _, s := range subscribers {
		s.close()
	}
}

func (h *Hub) environmentLocked(id string) *environment {
	env, held := h.environments[id]
	if !held {
		env = &environment{
			workers: map[string]*worker{}, observed: map[string]runtime.State{},
			subscribers: map[*subscription]struct{}{},
		}
		h.environments[id] = env
	}
	return env
}

// transport is one environment's half of the hub, which the driver holds.
type transport struct {
	hub         *Hub
	environment string
}

var _ Transport = (*transport)(nil)

func (t *transport) Registration() (Registration, bool) {
	t.hub.mu.Lock()
	defer t.hub.mu.Unlock()
	env, held := t.hub.environments[t.environment]
	if !held || !env.registered {
		return Registration{}, false
	}
	return env.registration, true
}

func (t *transport) Live() bool {
	t.hub.mu.Lock()
	defer t.hub.mu.Unlock()
	return t.hub.liveLocked(t.environment) != nil
}

// liveLocked is a worker of the environment holding a stream open and inside
// the offline window, or nil when none is.
func (h *Hub) liveLocked(environmentID string) *worker {
	env, held := h.environments[environmentID]
	if !held {
		return nil
	}
	cutoff := h.now().Add(-h.offline)
	var chosen *worker
	for _, w := range env.workers {
		if w.link == nil || w.lastHeartbeat.Before(cutoff) {
			continue
		}
		// The worker last heard from is the one with the most recent view of
		// the environment, so work goes there rather than round-robin.
		if chosen == nil || w.lastHeartbeat.After(chosen.lastHeartbeat) {
			chosen = w
		}
	}
	return chosen
}

func (t *transport) Observed(id string) (runtime.State, bool) {
	t.hub.mu.Lock()
	defer t.hub.mu.Unlock()
	env, held := t.hub.environments[t.environment]
	if !held || !env.reported {
		return runtime.State{}, false
	}
	state, has := env.observed[id]
	return state, has
}

func (t *transport) ObservedList() ([]runtime.State, bool) {
	t.hub.mu.Lock()
	defer t.hub.mu.Unlock()
	env, held := t.hub.environments[t.environment]
	if !held || !env.reported {
		return nil, false
	}
	out := make([]runtime.State, 0, len(env.observed))
	for _, s := range env.observed {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b runtime.State) int { return strings.Compare(a.ID, b.ID) })
	return out, true
}

// Watch is the environment's events as its workers' drivers report them,
// until the context ends or the environment is released. A consumer that
// falls behind receives a relist in place of what it missed.
func (t *transport) Watch(ctx context.Context) (<-chan Event, error) {
	s := newSubscription()
	t.hub.mu.Lock()
	env := t.hub.environmentLocked(t.environment)
	env.subscribers[s] = struct{}{}
	t.hub.mu.Unlock()
	go func() {
		s.run(ctx)
		t.hub.mu.Lock()
		delete(env.subscribers, s)
		t.hub.mu.Unlock()
	}()
	return s.out, nil
}

// Open enqueues one operation and sends it to a live worker. The row records
// the claim, so a worker that went away has its lifecycle operations
// redelivered; the frames travel on the connection the worker opened.
//
// The link is read under the hub's lock, with the worker it belongs to: a
// stream that ends clears it under that lock, and an operation issued at that
// moment reaches the link it read, which refuses it as closed, rather than
// one that is no longer there.
func (t *transport) Open(ctx context.Context, opType string, req Request) (Stream, error) {
	t.hub.mu.Lock()
	var link *Link
	if w := t.hub.liveLocked(t.environment); w != nil {
		link = w.link
	}
	t.hub.mu.Unlock()
	if link == nil {
		return nil, ErrNoWorker
	}
	id := t.hub.newID()
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("remote: %s: %w", opType, err)
	}
	if t.hub.queue != nil {
		if err = t.hub.queue.Enqueue(ctx, t.environment, OperationIDPrefix+id, req.ID, opType, payload); err != nil {
			return nil, err
		}
	}
	channel := link.Open(id, opType == OpAttach)
	if opType == OpCreate {
		t.hub.mu.Lock()
		t.hub.creates[id] = pendingCreate{environment: t.environment, sandbox: req.ID}
		if req.Spec != nil {
			t.hub.creates[id] = pendingCreate{environment: t.environment, sandbox: req.Spec.ID}
		}
		t.hub.mu.Unlock()
	}
	message := Message{Type: MessageOperation, Operation: id, Request: &req}
	if err = link.Send(id, withOperationType(message, opType)); err != nil {
		link.Drop(id)
		t.hub.forgetCreate(id)
		return nil, ErrNoWorker
	}
	stream := &hubStream{hub: t.hub, channel: channel, id: id, opened: ctx}
	// The caller's own context ends the operation on the worker: a caller
	// that went away is a caller whose work is nobody's, which is spec 021's
	// cancel crossing the seam.
	go func() {
		select {
		case <-ctx.Done():
			_ = channel.Close()
		case <-channel.Cancelled():
		}
	}()
	return stream, nil
}

// withOperationType writes the operation's type where the worker reads it.
// The type rides in the message rather than beside it, so one frame carries
// the whole call.
func withOperationType(m Message, opType string) Message {
	m.Worker = opType
	return m
}

// OperationType reads the type one operation message carries.
func OperationType(m Message) string { return m.Worker }

// hubStream is one operation on the control plane's side, which acknowledges
// the row when the answer arrives.
type hubStream struct {
	hub     *Hub
	channel *Channel
	id      string
	// opened is the context the caller issued the operation under. It is
	// read again at every wait, so a caller that cancelled reads its own
	// error rather than the stream ending underneath it.
	opened context.Context
}

func (s *hubStream) Down(stream byte) io.WriteCloser { return s.channel.Down(stream) }
func (s *hubStream) Up(stream byte) io.Reader        { return s.channel.Up(stream) }
func (s *hubStream) Resize(cols, rows int) error     { return s.channel.Resize(cols, rows) }
func (s *hubStream) Close() error {
	s.hub.forgetCreate(s.id)
	return s.channel.Close()
}

// forgetCreate drops one create from the record of those in flight, once it
// answered or its caller closed it.
func (h *Hub) forgetCreate(operation string) {
	h.mu.Lock()
	delete(h.creates, operation)
	h.mu.Unlock()
}

func (s *hubStream) Accepted(ctx context.Context) error {
	err := s.channel.Accepted(ctx)
	if opened := s.opened.Err(); opened != nil {
		return opened
	}
	return err
}

func (s *hubStream) Result(ctx context.Context) (Response, error) {
	res, err := s.channel.Result(ctx)
	s.hub.forgetCreate(s.id)
	if opened := s.opened.Err(); opened != nil && err != nil {
		err = opened
	}
	if s.hub.queue != nil {
		record, marshalErr := json.Marshal(struct {
			OK    bool      `json:"ok"`
			Res   *Response `json:"response,omitempty"`
			Error string    `json:"error,omitempty"`
		}{OK: err == nil, Res: &res, Error: errorText(err)})
		if marshalErr == nil {
			if ackErr := s.hub.queue.Acknowledge(ctx, OperationIDPrefix+s.id, record); ackErr != nil {
				s.hub.log.WarnContext(ctx, "an operation's result was not recorded", "operation", s.id, "err", ackErr)
			}
		}
	}
	return res, err
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// intersect is what all of an environment's workers can do. A capability one
// worker lacks is one the environment does not have, because a caller gated
// on it must not reach that worker.
func intersect(a, b runtime.Capabilities) runtime.Capabilities {
	out := runtime.Capabilities{
		Mesh: a.Mesh && b.Mesh, Ingress: a.Ingress && b.Ingress,
		Volumes: a.Volumes && b.Volumes, Snapshots: a.Snapshots && b.Snapshots,
		Attach: a.Attach && b.Attach, Dial: a.Dial && b.Dial,
		Display: a.Display && b.Display, Input: a.Input && b.Input,
		Resize: a.Resize && b.Resize, Pool: a.Pool && b.Pool,
		Files: a.Files && b.Files, Detach: a.Detach && b.Detach,
		HostWorkspace: a.HostWorkspace && b.HostWorkspace,
		EgressLocal:   a.EgressLocal && b.EgressLocal,
		ExecStdin:     a.ExecStdin && b.ExecStdin,
		Autosize:      a.Autosize.And(b.Autosize),
	}
	for _, mode := range a.Egress {
		if slices.Contains(b.Egress, mode) {
			out.Egress = append(out.Egress, mode)
		}
	}
	return out
}
