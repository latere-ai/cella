// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/cella/authorizer"
	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/egress"
	"latere.ai/x/cella/manifest"
)

// DefaultAckTimeout is how long a create waits for one gateway of the
// environment to acknowledge the sandbox's map, and DefaultRecordsCap how
// many connection records the control plane keeps per sandbox in memory.
// CELLA_EGRESS_ACK_TIMEOUT and CELLA_EGRESS_RECORDS_CAP set them.
const (
	DefaultAckTimeout = 5 * time.Second
	DefaultRecordsCap = 1000
)

// retryInterval is how often an unacknowledged map is sent again while the
// create waits. A gateway that dropped the frame gets another without the
// create having to fail first.
const retryInterval = time.Second

// EgressHub is the control plane's half of the gateway protocol. It holds the
// connected gateways of one environment and the map each of them should have,
// and it never dials one: a gateway connects outbound and the hub answers on
// the stream the gateway opened, which is invariant 10 of the architecture.
//
// The hub is also where the connection records land, in a ring per sandbox,
// so a busy sandbox evicts its own oldest record and nothing else.
type EgressHub struct {
	environment string
	ackTimeout  time.Duration
	recordsCap  int
	log         *slog.Logger

	mu      sync.Mutex
	maps    map[string]egress.Map
	conns   map[*gatewayConn]struct{}
	waiters map[*ackWaiter]struct{}
	records map[string][]egress.Record
	ca      string
}

// EgressHubOptions configures the hub.
type EgressHubOptions struct {
	// Environment is the one environment this control plane drives. A
	// gateway that names another is refused.
	Environment string
	// AckTimeout is how long Send waits for one acknowledgement.
	AckTimeout time.Duration
	// RecordsCap is how many records are kept per sandbox.
	RecordsCap int
	Log        *slog.Logger
}

// NewEgressHub returns a hub holding no map and no connection. Seed it with
// desired state before serving, so a gateway that connects to a control plane
// that has just restarted is still handed every map.
func NewEgressHub(o EgressHubOptions) *EgressHub {
	h := &EgressHub{
		environment: o.Environment,
		ackTimeout:  o.AckTimeout,
		recordsCap:  o.RecordsCap,
		log:         o.Log,
		maps:        map[string]egress.Map{},
		conns:       map[*gatewayConn]struct{}{},
		waiters:     map[*ackWaiter]struct{}{},
		records:     map[string][]egress.Record{},
	}
	if h.ackTimeout <= 0 {
		h.ackTimeout = DefaultAckTimeout
	}
	if h.recordsCap <= 0 {
		h.recordsCap = DefaultRecordsCap
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	return h
}

// Seed replaces the hub's set of maps with what desired state says. It runs
// once at start-up, before the listeners, so the first gateway to connect
// receives every live sandbox's map with the credential that sandbox already
// holds rather than an empty world.
func (h *EgressHub) Seed(maps []egress.Map) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.maps = make(map[string]egress.Map, len(maps))
	for _, m := range maps {
		h.maps[m.Principal] = m
	}
}

// CA is the authority the environment's gateways terminate TLS with, learned
// from the first gateway that connected. It is empty until one has.
func (h *EgressHub) CA() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ca
}

// Connected is how many gateways hold a stream open.
func (h *EgressHub) Connected() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// Send pushes one sandbox's map to every gateway of the environment and
// returns when one has acknowledged it. The map is kept whatever happens, so
// a gateway that connects later receives it in its snapshot; what the
// controller waits for here is only the promise that a gateway holds it
// before the sandbox exists.
func (h *EgressHub) Send(ctx context.Context, m egress.Map) error {
	w := &ackWaiter{principal: m.Principal, version: m.Version, done: make(chan struct{})}
	h.mu.Lock()
	h.maps[m.Principal] = m
	h.waiters[w] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.waiters, w)
		h.mu.Unlock()
	}()

	frame := egress.Frame{Type: egress.FramePut, Put: &m}
	if h.broadcast(frame, m.Principal) == 0 {
		return controller.ErrNoGateway
	}
	deadline := time.NewTimer(h.ackTimeout)
	defer deadline.Stop()
	retry := time.NewTicker(retryInterval)
	defer retry.Stop()
	for {
		select {
		case <-w.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return controller.ErrNoGateway
		case <-retry.C:
			// A gateway that dropped the frame, or connected in between,
			// gets it again rather than the create failing on one attempt.
			if h.broadcast(frame, m.Principal) == 0 {
				return controller.ErrNoGateway
			}
		}
	}
}

// Purge drops a principal from the hub and from every connected gateway. A
// gateway that is not connected learns it from the next snapshot, which is
// authoritative, so a purge missed while disconnected still lands.
func (h *EgressHub) Purge(_ context.Context, principal string) {
	h.mu.Lock()
	delete(h.maps, principal)
	delete(h.records, egress.SandboxOf(principal))
	h.mu.Unlock()
	h.broadcast(egress.Frame{Type: egress.FramePurge, Purge: &egress.Purge{Principal: principal}}, principal)
}

// Records is one sandbox's connections, newest first, at most limit of them.
func (h *EgressHub) Records(sandboxID string, limit int) []egress.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := h.records[sandboxID]
	out := make([]egress.Record, 0, len(kept))
	for _, k := range slices.Backward(kept) {
		if limit > 0 && len(out) == limit {
			break
		}
		out = append(out, k)
	}
	return out
}

// ackWaiter is one Send waiting for its map to be acknowledged.
type ackWaiter struct {
	principal string
	version   int64
	done      chan struct{}
	once      sync.Once
}

func (w *ackWaiter) satisfy() { w.once.Do(func() { close(w.done) }) }

// broadcast queues a frame on every connection whose scope holds the
// principal and reports how many took it.
func (h *EgressHub) broadcast(f egress.Frame, principal string) int {
	h.mu.Lock()
	conns := make([]*gatewayConn, 0, len(h.conns))
	for c := range h.conns {
		if c.holds(principal) {
			conns = append(conns, c)
		}
	}
	h.mu.Unlock()
	sent := 0
	for _, c := range conns {
		if c.send(f) {
			sent++
		}
	}
	return sent
}

// onAck satisfies every Send waiting for this principal at or below the
// acknowledged version.
func (h *EgressHub) onAck(a egress.Ack) {
	h.mu.Lock()
	var satisfied []*ackWaiter
	for w := range h.waiters {
		if w.principal == a.Principal && a.Version >= w.version {
			satisfied = append(satisfied, w)
		}
	}
	h.mu.Unlock()
	for _, w := range satisfied {
		w.satisfy()
	}
}

// onRecord keeps one connection record in the sandbox's ring. A record for a
// sandbox the hub holds no map for is kept as well: the map may have been
// purged while the connection was closing, and the record is what says what
// left before that.
func (h *EgressHub) onRecord(r egress.Record) error {
	if err := r.Normalize(); err != nil {
		return err
	}
	id := egress.SandboxOf(r.Principal)
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := append(h.records[id], r)
	if len(kept) > h.recordsCap {
		kept = slices.Delete(kept, 0, len(kept)-h.recordsCap)
	}
	h.records[id] = kept
	return nil
}

// snapshot is every map in the connection's scope.
func (h *EgressHub) snapshot(principal string) []egress.Map {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]egress.Map, 0, len(h.maps))
	for _, m := range h.maps {
		if principal == "" || m.Principal == principal {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b egress.Map) int { return strings.Compare(a.Principal, b.Principal) })
	return out
}

// gatewayConn is one gateway's stream. Frames leave through out, which one
// writer drains, so the WebSocket has exactly one writer as its contract
// requires.
type gatewayConn struct {
	id        string
	principal string
	out       chan egress.Frame
	done      chan struct{}
	once      sync.Once
}

// outBuffer is how many frames may queue for one gateway before the hub
// treats it as gone. A gateway that cannot keep up is dropped rather than
// allowed to grow the control plane's memory; its reconnect gets a snapshot.
const outBuffer = 256

func (c *gatewayConn) holds(principal string) bool {
	return c.principal == "" || c.principal == principal
}

func (c *gatewayConn) send(f egress.Frame) bool {
	select {
	case c.out <- f:
		return true
	case <-c.done:
		return false
	default:
		// The queue is full: close the connection rather than block the
		// controller on one slow gateway.
		c.close()
		return false
	}
}

func (c *gatewayConn) close() { c.once.Do(func() { close(c.done) }) }

// upgrader turns the request into a WebSocket. The origin check is the
// environment key: a browser cannot hold one, so there is no cross-origin
// case to defend against here.
var upgrader = websocket.Upgrader{
	Subprotocols: []string{egress.Protocol},
	CheckOrigin:  func(*http.Request) bool { return true },
}

// ErrEgressEnvironment is a gateway that connected to an environment this
// control plane does not drive.
var ErrEgressEnvironment = errors.New("the key names another environment")

// ServeGateway runs one gateway's stream: the hello it opens with, the
// snapshot that makes it whole, then the puts and purges down and the
// acknowledgements and records up until either side stops.
func (h *EgressHub) ServeGateway(w http.ResponseWriter, r *http.Request, environment string) {
	if environment != h.environment {
		respondError(w, &manifest.Error{Code: "not_found", Detail: ErrEgressEnvironment.Error()})
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has answered
	}
	defer func() { _ = conn.Close() }()
	if conn.Subprotocol() != egress.Protocol {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "this server speaks "+egress.Protocol),
			time.Now().Add(time.Second))
		return
	}
	hello, err := readHello(conn)
	if err != nil {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, err.Error()),
			time.Now().Add(time.Second))
		return
	}
	c := &gatewayConn{id: hello.GatewayID, principal: hello.Principal, out: make(chan egress.Frame, outBuffer), done: make(chan struct{})}
	h.join(c, hello)
	defer h.leave(c)

	// The snapshot is the first frame down and is authoritative: whatever
	// the gateway held before this connection, it holds exactly this after.
	if !c.send(egress.Frame{Type: egress.FrameSnapshot, Snapshot: &egress.Snapshot{Maps: h.snapshot(c.principal)}}) {
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go h.readPump(ctx, cancel, conn, c)
	h.writePump(ctx, conn, c)
}

// join registers the connection and takes the gateway's authority, which is
// what the control plane projects into every sandbox of the environment.
func (h *EgressHub) join(c *gatewayConn, hello egress.Hello) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[c] = struct{}{}
	switch {
	case hello.CAPEM == "":
	case h.ca == "":
		h.ca = hello.CAPEM
	case h.ca != hello.CAPEM:
		// Every gateway of one environment signs with one authority, or a
		// sandbox trusts one door and is pointed at another.
		h.log.Warn("a gateway connected with another authority than the environment's", "gateway", c.id)
	}
}

func (h *EgressHub) leave(c *gatewayConn) {
	c.close()
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// readHello takes the first frame, which must be the hello of a gateway
// speaking this protocol.
func readHello(conn *websocket.Conn) (egress.Hello, error) {
	if err := conn.SetReadDeadline(time.Now().Add(egress.HeartbeatTimeout)); err != nil {
		return egress.Hello{}, err
	}
	_, message, err := conn.ReadMessage()
	if err != nil {
		return egress.Hello{}, err
	}
	f, err := egress.Decode(message)
	if err != nil {
		return egress.Hello{}, err
	}
	if f.Type != egress.FrameHello || f.Hello == nil {
		return egress.Hello{}, errors.New("the first frame of a stream is the hello")
	}
	if f.Hello.Protocol != "" && f.Hello.Protocol != egress.Protocol {
		return egress.Hello{}, errors.New("this server speaks " + egress.Protocol)
	}
	return *f.Hello, nil
}

// readPump takes the acknowledgements and records the gateway sends up.
func (h *EgressHub) readPump(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, c *gatewayConn) {
	defer cancel()
	defer c.close()
	for {
		if err := conn.SetReadDeadline(time.Now().Add(egress.HeartbeatTimeout)); err != nil {
			return
		}
		_, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		f, err := egress.Decode(message)
		if err != nil {
			h.log.WarnContext(ctx, "a gateway sent a frame this server does not know", "gateway", c.id, "err", err)
			continue
		}
		switch {
		case f.Type == egress.FrameAck && f.Ack != nil:
			h.onAck(*f.Ack)
		case f.Type == egress.FrameRecord && f.Record != nil:
			if err = h.onRecord(*f.Record); err != nil {
				h.log.WarnContext(ctx, "a gateway sent a record this server will not keep", "gateway", c.id, "err", err)
			}
		case f.Type == egress.FrameHeartbeat:
		default:
			h.log.WarnContext(ctx, "a gateway sent a frame that belongs the other way", "gateway", c.id, "frame", f.Type)
		}
	}
}

// writePump is the connection's one writer: every frame the hub queued, and a
// heartbeat on an idle stream.
func (h *EgressHub) writePump(ctx context.Context, conn *websocket.Conn, c *gatewayConn) {
	beat := time.NewTicker(egress.HeartbeatInterval)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case f := <-c.out:
			if !writeFrame(conn, f) {
				return
			}
		case <-beat.C:
			if !writeFrame(conn, egress.Frame{Type: egress.FrameHeartbeat}) {
				return
			}
		}
	}
}

// writeDeadline bounds one frame's write, so a gateway that stopped reading
// does not hold the writer forever.
const writeDeadline = 10 * time.Second

func writeFrame(conn *websocket.Conn, f egress.Frame) bool {
	message, err := egress.Encode(f)
	if err != nil {
		return false
	}
	if err = conn.SetWriteDeadline(time.Now().Add(writeDeadline)); err != nil {
		return false
	}
	return conn.WriteMessage(websocket.TextMessage, message) == nil
}

// egressStreamPath matches the one route an environment key reaches,
// GET /v1/environments/{id}/egress, and answers the environment it names. It
// is matched before the mux, because every route on the mux decides on a
// subject and an environment key names none.
func egressStreamPath(r *http.Request) (string, bool) {
	if r.Method != http.MethodGet {
		return "", false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/environments/")
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(rest, "/egress")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// egressRecords answers one sandbox's connections, newest first, which is
// what a caller reads to see what left its sandbox and what was refused.
func (h *handler) egressRecords(w http.ResponseWriter, r *http.Request) {
	obj, err := h.Controller.Get(r.Context(), r.PathValue("id"), caller(r).Subject)
	if err != nil {
		respondError(w, err)
		return
	}
	if _, err = h.decide(r, authorizer.ActionSandboxRead, resource(obj)); err != nil {
		respondError(w, err)
		return
	}
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		limit, err = strconv.Atoi(q)
		if err != nil || limit < 1 || limit > 200 {
			respondError(w, &manifest.Error{Code: "invalid_field", Detail: "limit must be between 1 and 200"})
			return
		}
	}
	items := []egress.Record{}
	if h.Egress != nil {
		items = h.Egress.Records(obj.Status.ID, limit)
	}
	respond(w, http.StatusOK, map[string]any{"items": items})
}
