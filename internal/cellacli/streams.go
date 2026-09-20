// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellacli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"latere.ai/x/cella/internal/cellaclient"
)

// defaultWindow is the terminal a session asks for when the caller has none
// of its own and still wants one, which is what -t without a terminal is.
const defaultWindow = 80

const defaultWindowRows = 24

// exec runs a command inside a sandbox. Without -i and without -t it is the
// synchronous route of design 008, whose answer carries the two outputs and
// the exit code; with either it is the exec socket, where the bytes travel
// as they are produced.
func exec(ctx context.Context, c *invocation, args []string) error {
	fs := c.flags("cella exec <ref> [-i] [-t] [--timeout d] [--workdir w] [--env K=V]... -- <command>")
	stdin := fs.Bool("i", false, "send this command's standard input to the command inside")
	tty := fs.Bool("t", false, "run the command under a terminal")
	timeout := fs.Duration("timeout", 0, "end the command after this long")
	workdir := fs.String("workdir", "", "the directory to run in")
	var environment stringList
	fs.Var(&environment, "env", "an environment entry K=V, repeatable")
	ref, command, err := refAndCommand(fs, args)
	if err != nil {
		return err
	}
	if len(command) == 0 {
		return usagef("exec needs a command after --")
	}
	env, err := environmentOf(environment)
	if err != nil {
		return err
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	req := cellaclient.ExecRequest{Command: command, Env: env, Workdir: *workdir}
	if *timeout > 0 {
		req.Timeout = timeout.String()
	}
	if !*stdin && !*tty {
		return c.execWait(ctx, client, ref, req)
	}
	c.session = true
	if *tty {
		req.Cols, req.Rows = c.window()
	}
	session, err := client.ExecSession(ctx, ref, req)
	if err != nil {
		return err
	}
	return c.drive(session, *stdin, *tty)
}

// execWait is the synchronous route: one answer carrying both outputs, the
// exit code, and whether the answer was cut at design 008's cap.
func (c *invocation) execWait(ctx context.Context, client *cellaclient.Client, ref string, req cellaclient.ExecRequest) error {
	c.session = true
	result, raw, err := client.Exec(ctx, ref, req)
	if err != nil {
		return err
	}
	if c.json {
		if err = writeRaw(c.Stdout, raw); err != nil {
			return err
		}
		return exitError{code: result.ExitCode}
	}
	if _, err = io.WriteString(c.Stdout, result.Stdout); err != nil {
		return err
	}
	if _, err = io.WriteString(c.Stderr, result.Stderr); err != nil {
		return err
	}
	if result.Truncated {
		_, _ = fmt.Fprintln(c.Stderr, "cella: the output was cut at this server's cap")
	}
	return exitError{code: result.ExitCode}
}

// attach opens a terminal on a sandbox. The window follows the caller's own
// and the terminal is restored on every exit path.
func attach(ctx context.Context, c *invocation, args []string) error {
	fs := c.flags("cella attach <ref> [-- <command>]")
	ref, command, err := refAndCommand(fs, args)
	if err != nil {
		return err
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	c.session = true
	req := cellaclient.ExecRequest{Command: command}
	req.Cols, req.Rows = c.window()
	session, err := client.AttachSession(ctx, ref, req)
	if err != nil {
		return err
	}
	return c.drive(session, true, true)
}

// drive runs one session to its end: the caller's input goes up, the output
// comes down, the window follows, and the exit code of the command inside is
// the exit code of this one.
func (c *invocation) drive(session *cellaclient.Session, sendStdin, terminal bool) error {
	defer func() { _ = session.Close() }()
	if terminal && c.Terminal != nil {
		restore, err := c.Terminal.MakeRaw()
		if err != nil {
			return err
		}
		// Restored on every exit path, including a failure and a signal
		// that ends the process through the deferred close above.
		defer func() { _ = restore() }()
		changed, stop := c.Terminal.Resized()
		defer stop()
		go func() {
			for range changed {
				cols, rows, err := c.Terminal.Size()
				if err != nil {
					return
				}
				if err = session.Resize(cols, rows); err != nil {
					return
				}
			}
		}()
	}
	if sendStdin && c.Stdin != nil {
		go func() { _, _ = io.Copy(session, c.Stdin) }()
	}
	_, copyErr := io.Copy(c.Stdout, session)
	code, err := session.Wait()
	c.started = session.Started()
	if err != nil {
		return err
	}
	if copyErr != nil {
		return copyErr
	}
	if c.json {
		if err = writeValue(c.Stdout, map[string]int{"exitCode": code}); err != nil {
			return err
		}
	}
	return exitError{code: code}
}

// window is the terminal the session asks the server for: the caller's own
// where there is one, and a fixed one otherwise, so -t on a pipe still runs
// the command under a terminal.
func (c *invocation) window() (cols, rows int) {
	if c.Terminal != nil {
		if cols, rows, err := c.Terminal.Size(); err == nil && cols > 0 && rows > 0 {
			return cols, rows
		}
	}
	return defaultWindow, defaultWindowRows
}

// logs writes a sandbox's main process output as it arrives.
func logs(ctx context.Context, c *invocation, args []string) error {
	fs := c.flags("cella logs <ref> [-f] [--since <RFC3339>] [--tail n]")
	follow := fs.Bool("f", false, "keep writing lines as they arrive")
	since := fs.String("since", "", "only lines after this instant, RFC 3339")
	tail := fs.Int("tail", 0, "only the last n lines")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("logs needs one sandbox")
	}
	o := cellaclient.LogOptions{Follow: *follow, Tail: *tail}
	if *since != "" {
		if o.Since, err = time.Parse(time.RFC3339, *since); err != nil {
			return usagef("--since is an RFC 3339 instant: %v", err)
		}
	}
	client, err := c.client()
	if err != nil {
		return err
	}
	body, err := client.Logs(ctx, rest[0], o)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	_, err = io.Copy(c.Stdout, body)
	return err
}

// refAndCommand reads the reference and the command line after `--`, which
// is cut from the arguments before the flags are parsed because the flag
// package consumes the marker.
func refAndCommand(fs *flag.FlagSet, args []string) (string, []string, error) {
	before, after, separated := splitArgs(args)
	rest, err := parse(fs, before)
	if err != nil {
		return "", nil, err
	}
	if len(rest) == 0 {
		return "", nil, usagef("this command needs one sandbox")
	}
	if len(rest) > 1 && !separated {
		return "", nil, usagef("the command to run goes after --")
	}
	if len(rest) > 1 {
		return "", nil, usagef("%s takes one sandbox", fs.Name())
	}
	return rest[0], after, nil
}

// environmentOf reads the repeated K=V flags into the map the request
// carries.
func environmentOf(entries stringList) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, usagef("--env takes K=V, and %q is not one", entry)
		}
		out[key] = value
	}
	return out, nil
}
