// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"latere.ai/x/cella/internal/check"
	"latere.ai/x/cella/internal/config"
	"latere.ai/x/cella/internal/version"
	"latere.ai/x/cella/runtime"
)

// checkRole is the check subcommand of spec 014: one line per requirement of
// this installation, exit 1 on any failure. It runs before the first
// manifest, as a Job or as an init container beside the Deployment, and it
// reads the same variables serve does, so the Pod that runs it is the Pod
// that would serve with one argument changed.
func checkRole(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cellad check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the build identity and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version.String())
		return 0
	}
	// The runtime line asks the driver's preflight alone, which reads no
	// trust file, so the driver is opened without the public roots.
	open := func(cfg config.Config) (runtime.Driver, func() error, error) { return openRuntime(cfg, nil) }
	lines := check.Run(ctx, check.Options{Getenv: getenv, Open: open})
	if check.Report(stdout, lines) {
		return 1
	}
	return 0
}

// versionRole prints the build identity a release pins: the version, the
// commit and the date -ldflags set. It is the -version flag as a
// subcommand, because a Job is one image and one argument and a flag on an
// entry point that also takes a subcommand reads as neither.
func versionRole(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cellad version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_, _ = fmt.Fprintln(stdout, version.String())
	return 0
}
