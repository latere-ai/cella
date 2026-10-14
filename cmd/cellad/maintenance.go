// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"latere.ai/x/cella/internal/config"
	"latere.ai/x/cella/internal/store/postgres"
	"latere.ai/x/cella/internal/version"
)

// unsealRole is the rollback of spec 014 across the release that seals a
// sandbox's credentials in the database: every sealed row is put back in the
// clear and the schema is migrated down to the one an earlier release starts
// on. It runs with no replica of cellad serve running, since a replica of
// this release seals a row again at its next write, and refuses while one
// holds the writer lease. A second run finds nothing to do. It prints counts
// and versions and never a value.
func unsealRole(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	if code, done := maintenanceFlags("cellad unseal", args, stdout, stderr); done {
		return code
	}
	cfg, err := config.LoadMaintenance(getenv, false)
	if err != nil {
		return fail(stderr, err)
	}
	result, err := postgres.Unseal(ctx, postgres.Options{URL: cfg.DBURL, Key: cfg.SecretKey})
	if err != nil {
		return fail(stderr, fmt.Errorf("unseal: %d sandbox row(s) unsealed before: %w", result.Unsealed, err))
	}
	_, _ = fmt.Fprintf(stdout, "cellad: %s unsealed %d sandbox row(s); the schema is at version %d\n",
		version.Version, result.Unsealed, result.Schema)
	return 0
}

// rotateRole moves every data key the database holds wrapped from
// CELLA_SECRET_KEY to CELLA_SECRET_KEY_NEXT, in one transaction, so a key can
// be replaced without reading or rewriting any value. It runs with no replica
// serving, each of which holds the old key until it restarts under the new
// one, and refuses while one holds the writer lease.
func rotateRole(ctx context.Context, args []string, getenv config.Getenv, stdout, stderr io.Writer) int {
	if code, done := maintenanceFlags("cellad rotate", args, stdout, stderr); done {
		return code
	}
	cfg, err := config.LoadMaintenance(getenv, true)
	if err != nil {
		return fail(stderr, err)
	}
	n, err := postgres.Rotate(ctx, postgres.Options{URL: cfg.DBURL, Key: cfg.SecretKey}, cfg.NextSecretKey)
	if err != nil {
		return fail(stderr, fmt.Errorf("rotate: %w", err))
	}
	_, _ = fmt.Fprintf(stdout, "cellad: %s moved %d data key(s) to CELLA_SECRET_KEY_NEXT; start cellad serve with it as CELLA_SECRET_KEY\n",
		version.Version, n)
	return 0
}

// maintenanceFlags parses the flags both maintenance runs take, and reports
// done with the exit code where the run ends there.
func maintenanceFlags(name string, args []string, stdout, stderr io.Writer) (int, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the build identity and exit")
	if err := fs.Parse(args); err != nil {
		return 2, true
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, version.String())
		return 0, true
	}
	return 0, false
}
