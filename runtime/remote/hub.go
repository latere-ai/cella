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
)

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
	Log   *slog.Logger
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
	log     *slog.Logger

	mu           sync.Mutex
	environments map[string]*environment
}

// environment is one registered data plane: what its workers declared, the
// connections they hold, and what they report of their sandboxes.
type environment struct {
	registration Registration
	registered   bool
	workers      map[string]*worker
	// observed is the last state the workers reported, and reported says
	// whether any of them has. A control plane that has been told nothing
	// asks the worker rather than answering an empty world.
	observed map[string]runtime.State
	reported bool
}

// worker is one registration and, while it holds one, its stream.
type worker struct {
	id            string
	replica       string
	lastHeartbeat time.Time
	link          *Link
}

// NewHub returns a hub holding no environment and no connection.
func NewHub(o HubOptions) *Hub {
	h := &Hub{
		offline: o.Offline, newID: o.NewID, now: o.Now, queue: o.Queue, log: o.Log,
		environments: map[string]*environment{},
	}
	if h.offline <= 0 {
		h.offline = DefaultOffline
	}
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

// DefaultOffline is how long an environment is held live without a heartbeat,
// which CELLA_ENVIRONMENT_OFFLINE sets.
const DefaultOffline = 2 * time.Minute

// Register records one worker on one environment and returns the id it claims
// under. The driver name and the isolation class are recorded from the first
// registration and every later worker must report the same, or the
// environment would be two data planes under one name.
func (h *Hub) Register(environmentID string, r Registration) (Registered, error) {
	if environmentID == "" || r.Driver == "" || r.Isolation == "" {
		return Registered{}, fmt.Errorf("%w: a registration names an environment, a driver and an isolation class", runtime.ErrInvalid)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	env := h.environmentLocked(environmentID)
	switch {
	case !env.registered:
		env.registration = Registration{
			Driver: r.Driver, Isolation: r.Isolation, Capabilities: r.Capabilities,
			Capacity: r.Capacity, Labels: maps.Clone(r.Labels), Version: r.Version,
		}
		env.registered = true
	case env.registration.Driver != r.Driver || env.registration.Isolation != r.Isolation:
		return Registered{}, fmt.Errorf("%w: the environment runs %s with %s isolation and this worker reports %s with %s",
			ErrRegistrationMismatch, env.registration.Driver, env.registration.Isolation, r.Driver, r.Isolation)
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
	env.workers[id] = &worker{id: id, replica: r.Version, lastHeartbeat: h.now()}
	return Registered{
		Worker: id, Environment: environmentID,
		HeartbeatInterval: v1.Duration(HeartbeatInterval.String()), Lease: v1.Duration(HeartbeatTimeout.String()),
	}, nil
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
	c := &connection{hub: h, environment: environmentID}
	c.link = NewLink(conn, LinkOptions{OnMessage: c.onMessage})
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

	mu     sync.Mutex
	worker *worker
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
// hello that binds the connection, the heartbeats, and the state its driver
// observes.
func (c *connection) onMessage(_ string, m Message) {
	switch m.Type {
	case MessageHello:
		if err := c.bind(m.Worker); err != nil {
			c.hub.log.Warn("a worker opened a stream without a registration",
				"environment", c.environment, "worker", m.Worker)
			c.link.Shutdown(err)
		}
	case MessageHeartbeat:
		c.hub.stamp(c.environment, c.workerID())
	case MessageState:
		c.hub.stamp(c.environment, c.workerID())
		c.hub.observe(c.environment, m)
	default:
		c.hub.log.Warn("a worker sent a frame that belongs the other way",
			"environment", c.environment, "frame", m.Type)
		c.link.Shutdown(fmt.Errorf("%w: %s belongs the other way", ErrFrame, m.Type))
	}
}

func (c *connection) workerID() string {
	if w := c.bound(); w != nil {
		return w.id
	}
	return ""
}

// bind attaches this connection to the registration its hello named. A second
// hello on one connection is refused: one stream is one worker.
func (c *connection) bind(workerID string) error {
	if workerID == "" {
		return ErrUnknownWorker
	}
	if c.bound() != nil {
		return fmt.Errorf("%w: one stream is one worker", ErrFrame)
	}
	c.hub.mu.Lock()
	defer c.hub.mu.Unlock()
	env, held := c.hub.environments[c.environment]
	if !held {
		return ErrUnknownWorker
	}
	w, has := env.workers[workerID]
	if !has {
		return ErrUnknownWorker
	}
	w.link, w.lastHeartbeat = c.link, c.hub.now()
	c.mu.Lock()
	c.worker = w
	c.mu.Unlock()
	return nil
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
		out = append(out, WorkerState{
			Worker: w.id, LastHeartbeat: w.lastHeartbeat, Connected: w.link != nil,
		})
	}
	slices.SortFunc(out, func(a, b WorkerState) int { return strings.Compare(a.Worker, b.Worker) })
	return out
}

// WorkerState is one worker as the control plane sees it.
type WorkerState struct {
	Worker        string    `json:"worker"`
	LastHeartbeat time.Time `json:"lastHeartbeat"`
	Connected     bool      `json:"connected"`
}

// Release drops everything one environment holds, which is what a delete of
// the object writes.
func (h *Hub) Release(environmentID string) {
	h.mu.Lock()
	env, held := h.environments[environmentID]
	delete(h.environments, environmentID)
	h.mu.Unlock()
	if !held {
		return
	}
	for _, w := range env.workers {
		if w.link != nil {
			w.link.Shutdown(errors.New("remote: the environment was deleted"))
		}
	}
}

func (h *Hub) environmentLocked(id string) *environment {
	env, held := h.environments[id]
	if !held {
		env = &environment{workers: map[string]*worker{}, observed: map[string]runtime.State{}}
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

// Open enqueues one operation and sends it to a live worker. The row records
// the claim, so a worker that went away has its lifecycle operations
// redelivered; the frames travel on the connection the worker opened.
func (t *transport) Open(ctx context.Context, opType string, req Request) (Stream, error) {
	t.hub.mu.Lock()
	w := t.hub.liveLocked(t.environment)
	t.hub.mu.Unlock()
	if w == nil {
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
	channel := w.link.Open(id, opType == OpAttach)
	message := Message{Type: MessageOperation, Operation: id, Request: &req}
	if err = w.link.Send(id, withOperationType(message, opType)); err != nil {
		w.link.Drop(id)
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
func (s *hubStream) Close() error                    { return s.channel.Close() }

func (s *hubStream) Accepted(ctx context.Context) error {
	err := s.channel.Accepted(ctx)
	if opened := s.opened.Err(); opened != nil {
		return opened
	}
	return err
}

func (s *hubStream) Result(ctx context.Context) (Response, error) {
	res, err := s.channel.Result(ctx)
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
	}
	for _, mode := range a.Egress {
		if slices.Contains(b.Egress, mode) {
			out.Egress = append(out.Egress, mode)
		}
	}
	return out
}
