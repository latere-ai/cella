// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"

	cellaclient "latere.ai/x/cella/client"
)

// portForward serves design 011's port-forward: a listener on loopback whose
// every accepted connection is carried by one dial socket to a port inside a
// sandbox. It runs until it is interrupted.
func portForward(ctx context.Context, c *invocation, args []string) error {
	fs := c.flags("cella port-forward <ref> <local>:<port>")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return usagef("port-forward needs a sandbox and <local>:<port>")
	}
	ref := positional[0]
	local, remote, err := forwardPorts(positional[1])
	if err != nil {
		return err
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	// One socket opened first answers whether the sandbox can be reached at
	// all, so a sandbox that is missing, stopped or on an environment that
	// reaches no port is this command's exit rather than a listener that
	// fails every connection it accepts. What happens inside after the
	// upgrade is the port's, and a port nothing holds yet is not a refusal.
	probe, err := client.Dial(ctx, ref, remote)
	if err != nil {
		return err
	}
	if err = probe.Close(); err != nil {
		return err
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(local)))
	if err != nil {
		return err
	}
	stopped := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stopped()
	defer func() { _ = listener.Close() }()
	f := &forwarder{c: c, client: client, ref: ref, port: remote}
	if _, err = fmt.Fprintf(c.Stdout, "Forwarding %s to port %d of %s\n", listener.Addr(), remote, ref); err != nil {
		return err
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Go(func() { f.carry(ctx, conn) })
	}
}

// forwardPorts reads `<local>:<port>`: the loopback port to listen on, zero
// for one the kernel picks, and the port inside.
func forwardPorts(arg string) (local, remote int, err error) {
	l, r, ok := strings.Cut(arg, ":")
	if !ok {
		return 0, 0, usagef("port-forward takes <local>:<port>, not %q", arg)
	}
	local, err = strconv.Atoi(l)
	if err != nil || local < 0 || local > 65535 {
		return 0, 0, usagef("the local port %q is not between 0 and 65535", l)
	}
	remote, err = strconv.Atoi(r)
	if err != nil || remote < 1 || remote > 65535 {
		return 0, 0, usagef("the port %q is not between 1 and 65535", r)
	}
	return local, remote, nil
}

// forwarder carries the connections of one port-forward.
type forwarder struct {
	c      *invocation
	client *cellaclient.Client
	ref    string
	port   int
	// mu serializes the lines connections write to standard error.
	mu sync.Mutex
}

// carry pumps one accepted connection both ways through its own socket until
// either end closes. The protocol has no half close, so the end of one
// direction ends the connection.
func (f *forwarder) carry(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	stream, err := f.client.Dial(ctx, f.ref, f.port)
	if err != nil {
		f.report(err)
		return
	}
	defer func() { _ = stream.Close() }()
	ended := make(chan error, 2)
	go func() {
		_, err := io.Copy(stream, conn)
		ended <- err
	}()
	go func() {
		_, err := io.Copy(conn, stream)
		ended <- err
	}()
	// The first direction to end decides; a connection the caller closed
	// and a socket this process closed are the ordinary ends, not failures.
	if err = <-ended; err != nil && !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
		f.report(err)
	}
}

// report writes one connection's failure on its own line.
func (f *forwarder) report(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = fmt.Fprintf(f.c.Stderr, "cella: a connection to port %d of %s ended: %v\n", f.port, f.ref, err)
}
