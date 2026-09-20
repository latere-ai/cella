// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command cella is the Cella client: it speaks the /v1 API of a control
// plane from a shell and from an agent inside a sandbox. This file is the
// entry point and holds the process's own edges only: the signals, the
// streams, the environment and the terminal. The commands live in
// internal/cellacli and the calls in internal/cellaclient.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"latere.ai/x/cella/internal/cellacli"
	"latere.ai/x/cella/internal/version"
)

// exit ends the process. It is a variable so a test drives the entry point
// itself rather than a copy of it.
var exit = os.Exit

func main() {
	exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

// run is the command as a function, so a test drives it exactly as a shell
// does: the arguments, the three streams, the environment, and the exit code
// as a value rather than as a call to os.Exit.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	// The two signals end the command, and a session restores the terminal
	// on its way out because the restore is deferred inside the command.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return cellacli.Run(ctx, cellacli.Env{
		Args:     args,
		Stdin:    stdin,
		Stdout:   stdout,
		Stderr:   stderr,
		Getenv:   getenv,
		Terminal: terminalOf(stdin),
		Version:  version.Version,
		Identity: identity(),
	})
}

// terminalOf is the caller's terminal where the input is one, and nothing
// where it is a pipe or a file.
func terminalOf(stdin io.Reader) cellacli.Terminal {
	f, ok := stdin.(*os.File)
	if !ok {
		return nil
	}
	return cellacli.OSTerminal(f)
}

// identity is the line `cella version` prints, in the form the server's own
// binary prints its own.
func identity() string {
	return fmt.Sprintf("cella %s (%s, %s)", version.Version, version.Commit, version.Date)
}
