// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"io"
	"net"

	"latere.ai/x/cella/runtime"
)

// ExportTar and ImportTar move an archive out of and into a sandbox. Each is
// counted as a session open in it while it runs, so a move to a larger size
// does not cut a transfer in half (spec 078).
func (c *Controller) ExportTar(ctx context.Context, id string, paths []string, dst io.Writer) error {
	d, err := c.driverOf(id)
	if err != nil {
		return err
	}
	defer c.session(id)()
	return d.ExportTar(ctx, id, paths, dst)
}
func (c *Controller) ImportTar(ctx context.Context, id, dest string, src io.Reader) error {
	d, err := c.driverOf(id)
	if err != nil {
		return err
	}
	defer c.session(id)()
	return d.ImportTar(ctx, id, dest, src)
}
func (c *Controller) Logs(ctx context.Context, id string, req runtime.LogsRequest) (io.ReadCloser, error) {
	d, err := c.driverOf(id)
	if err != nil {
		return nil, err
	}
	return d.Logs(ctx, id, req)
}

// Attach opens one terminal in a sandbox. A driver that declares no Attach has
// no Attacher, which the API reports as the capability the environment lacks.
func (c *Controller) Attach(ctx context.Context, id string, req runtime.AttachRequest) (runtime.Session, error) {
	d, err := c.driverOf(id)
	if err != nil {
		return nil, err
	}
	a, ok := d.(runtime.Attacher)
	if !ok {
		return nil, runtime.ErrUnsupported
	}
	session, err := a.Attach(ctx, id, req)
	if err != nil {
		return nil, err
	}
	return &trackedSession{Session: session, closed: c.session(id)}, nil
}

// Files returns the driver's per-file half. A driver that declares no Files
// has no FileStore, which the API reports as the capability the environment
// lacks.
func (c *Controller) Files(id string) (runtime.FileStore, error) {
	d, err := c.driverOf(id)
	if err != nil {
		return nil, err
	}
	store, ok := d.(runtime.FileStore)
	if !ok {
		return nil, runtime.ErrUnsupported
	}
	return store, nil
}

// Dialer returns the dial half of the driver serving one sandbox's
// environment. It is the interface and not a connection, because the API
// decides what to answer before it upgrades a socket or writes a response. A
// driver that declares no Dial has no Dialer, which the API reports as the
// capability the environment lacks.
func (c *Controller) Dialer(id string) (runtime.Dialer, error) {
	d, err := c.driverOf(id)
	if err != nil {
		return nil, err
	}
	dialer, ok := d.(runtime.Dialer)
	if !ok {
		return nil, runtime.ErrUnsupported
	}
	return trackedDialer{Dialer: dialer, c: c}, nil
}

// trackedDialer counts each connection it opens as a session in its sandbox
// until the connection is closed.
type trackedDialer struct {
	runtime.Dialer
	c *Controller
}

func (t trackedDialer) Dial(ctx context.Context, id string, port int) (net.Conn, error) {
	conn, err := t.Dialer.Dial(ctx, id, port)
	if err != nil {
		return nil, err
	}
	return &trackedConn{Conn: conn, closed: t.c.session(id)}, nil
}

type trackedConn struct {
	net.Conn
	closed func()
}

func (t *trackedConn) Close() error {
	t.closed()
	return t.Conn.Close()
}
