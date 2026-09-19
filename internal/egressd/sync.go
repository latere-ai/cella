// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package egressd

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"latere.ai/x/pkg/authkit/jwt"

	"latere.ai/x/cella/egress"
)

// The bounds of the reconnect. A gateway that cannot reach the control plane
// keeps serving the boundary it already holds and tries again, because a map
// in memory is still the right answer until the control plane says otherwise.
const (
	minBackoff = 250 * time.Millisecond
	maxBackoff = 30 * time.Second
)

// recordBuffer is how many records wait for the stream. Records are
// telemetry: when the buffer is full the oldest is dropped, because a gateway
// that stops serving connections to keep a record has the priority backwards.
const recordBuffer = 1024

// syncClient is the gateway's one outbound stream. The control plane never
// dials a gateway, so everything the gateway learns and everything it reports
// travels here.
type syncClient struct {
	url         string
	key         string
	gatewayID   string
	principal   string
	store       *store
	caPEM       string
	records     chan egress.Record
	log         *slog.Logger
	dialer      *websocket.Dialer
	onConnected func()
}

// Record queues one connection record. It never blocks the connection that
// produced it: a full buffer drops its oldest record and keeps going.
func (c *syncClient) Record(r egress.Record) {
	for {
		select {
		case c.records <- r:
			return
		default:
		}
		select {
		case <-c.records:
		default:
			return
		}
	}
}

// Run holds one stream open until the context ends, reconnecting with
// backoff. Each connection opens with a hello, takes a snapshot that replaces
// everything the gateway held, and then applies what arrives.
func (c *syncClient) Run(ctx context.Context) error {
	backoff := minBackoff
	for {
		if err := c.connect(ctx); err != nil && ctx.Err() == nil {
			c.log.WarnContext(ctx, "the gateway's stream to the control plane ended", "err", err, "retryIn", backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// connect runs one connection to completion.
func (c *syncClient) connect(ctx context.Context) error {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.key)
	conn, resp, err := c.dialer.DialContext(ctx, c.url, header)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
			return errors.New("the control plane refused the stream: " + resp.Status)
		}
		return err
	}
	defer func() { _ = conn.Close() }()
	if resp != nil {
		_ = resp.Body.Close()
	}
	hello := egress.Frame{Type: egress.FrameHello, Hello: &egress.Hello{
		Protocol:  egress.Protocol,
		GatewayID: c.gatewayID,
		Principal: c.principal,
		Versions:  c.store.Versions(),
		CAPEM:     c.caPEM,
	}}
	if err = writeMessage(conn, hello); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	up := make(chan egress.Frame, 64)
	go c.readLoop(ctx, cancel, conn, up)
	return c.writeLoop(ctx, conn, up)
}

// readLoop applies what the control plane sends and queues the answer.
func (c *syncClient) readLoop(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, up chan<- egress.Frame) {
	defer cancel()
	first := true
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
			c.log.WarnContext(ctx, "the control plane sent a frame this gateway does not know", "err", err)
			continue
		}
		switch {
		case f.Type == egress.FrameSnapshot && f.Snapshot != nil:
			// The snapshot is authoritative: after it the gateway holds
			// exactly what the control plane says and nothing it held
			// before, so a purge missed while disconnected still lands.
			c.store.Replace(f.Snapshot.Maps)
			for _, m := range f.Snapshot.Maps {
				queue(ctx, up, ack(m.Principal, m.Version))
			}
			if first && c.onConnected != nil {
				c.onConnected()
			}
			first = false
		case f.Type == egress.FramePut && f.Put != nil:
			m := *f.Put
			c.store.Apply(m)
			// A version at or below the held one is acknowledged too: the
			// control plane asked whether the gateway holds it, and it does.
			held, _ := c.store.Map(m.Principal)
			queue(ctx, up, ack(m.Principal, held.Version))
		case f.Type == egress.FramePurge && f.Purge != nil:
			c.store.Remove(f.Purge.Principal)
			queue(ctx, up, ack(f.Purge.Principal, 0))
		case f.Type == egress.FrameHeartbeat:
		default:
			c.log.WarnContext(ctx, "the control plane sent a frame that belongs the other way", "frame", f.Type)
		}
	}
}

// writeLoop is the connection's one writer: the acknowledgements the read
// loop queued, the records the doors produced, and a heartbeat on an idle
// stream.
func (c *syncClient) writeLoop(ctx context.Context, conn *websocket.Conn, up <-chan egress.Frame) error {
	beat := time.NewTicker(egress.HeartbeatInterval)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case f := <-up:
			if err := writeMessage(conn, f); err != nil {
				return err
			}
		case r := <-c.records:
			if err := writeMessage(conn, egress.Frame{Type: egress.FrameRecord, Record: &r}); err != nil {
				// The record is lost with the connection. It is telemetry:
				// putting it back would let a dead stream hold every later
				// record hostage.
				return err
			}
		case <-beat.C:
			if err := writeMessage(conn, egress.Frame{Type: egress.FrameHeartbeat}); err != nil {
				return err
			}
		}
	}
}

func ack(principal string, version int64) egress.Frame {
	return egress.Frame{Type: egress.FrameAck, Ack: &egress.Ack{Principal: principal, Version: version}}
}

func queue(ctx context.Context, up chan<- egress.Frame, f egress.Frame) {
	select {
	case up <- f:
	case <-ctx.Done():
	}
}

func writeMessage(conn *websocket.Conn, f egress.Frame) error {
	message, err := egress.Encode(f)
	if err != nil {
		return err
	}
	if err = conn.SetWriteDeadline(time.Now().Add(writeDeadline)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, message)
}

// writeDeadline bounds one frame's write, so a control plane that stopped
// reading does not hold the gateway's writer forever.
const writeDeadline = 10 * time.Second

// streamURL turns the control plane's URL and the environment's name into the
// address of the one route a gateway opens.
func streamURL(controlPlane, environment string) (string, error) {
	u, err := url.Parse(strings.TrimRight(controlPlane, "/"))
	if err != nil || u.Host == "" {
		return "", errors.New("CELLA_URL is not an absolute URL")
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", errors.New("CELLA_URL is reached over http or https")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/environments/" + url.PathEscape(environment) + "/egress"
	u.RawQuery = ""
	return u.String(), nil
}

// environmentOf reads the environment out of the key the gateway was given.
// The gateway does not verify the key, which is the control plane's to do; it
// reads its own configuration to learn which environment's stream to open,
// and a key the control plane refuses is refused there. The payload is
// decoded by the shared reader, so no token is ever taken apart here.
func environmentOf(key string) (string, error) {
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := jwt.DecodePayload(strings.TrimSpace(key), &claims); err != nil {
		return "", errors.New("CELLA_ENVIRONMENT_KEY is not a token this server can read: " + err.Error())
	}
	environment, ok := strings.CutPrefix(claims.Sub, environmentSubjectPrefix)
	if !ok || environment == "" {
		return "", errors.New("CELLA_ENVIRONMENT_KEY is not an environment key; its subject names " + claims.Sub)
	}
	return environment, nil
}

// environmentSubjectPrefix is what the subject of an environment key starts
// with. The control plane's own identity package owns the prefix; this role
// imports nothing of the control plane, so it carries the one constant it
// needs and a test holds the two equal.
const environmentSubjectPrefix = "environment:"
