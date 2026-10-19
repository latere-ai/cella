// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package local

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/worker"
)

// TestAMissingFileIsWordedByItsPath: a file operation that finds nothing
// answers "not found:" and the path it looked for, which is what the host's
// record of the operation shows, and is still ErrNotFound and the
// filesystem's own error, so the API answers it as it did.
func TestAMissingFileIsWordedByItsPath(t *testing.T) {
	shim(t)
	h := newHost(t)
	d := h.open(t, Options{Lookup: os.LookupEnv})
	sandbox(t, d, driver.CreateSpec{ID: "sbx_missing", Workspace: driver.Workspace{Path: h.project, Host: h.project}})
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"Stat": func() error { _, err := d.Stat(ctx, "sbx_missing", filepath.Join(h.project, "AGENTS.md")); return err },
		"Open": func() error {
			_, _, err := d.Open(ctx, "sbx_missing", filepath.Join(h.project, "AGENTS.md"))
			return err
		},
		"Move": func() error {
			return d.Move(ctx, "sbx_missing", filepath.Join(h.project, "AGENTS.md"), filepath.Join(h.project, "moved.md"))
		},
	} {
		err := call()
		if err == nil || err.Error() != "not found: AGENTS.md" || !errors.Is(err, driver.ErrNotFound) || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s of a missing file answered %v", name, err)
		}
	}
	if err := missing(fmt.Errorf("the walk: %w", fs.ErrNotExist)); err.Error() != "not found: the walk: file does not exist" ||
		!errors.Is(err, driver.ErrNotFound) {
		t.Errorf("an error that names no path reads %v", err)
	}

	// The worker's record of the operation, which is the host's own log,
	// carries the same words.
	p := newStubPlane(t)
	var mu sync.Mutex
	var records []worker.Record
	p.serve(t, worker.Options{Driver: d, Record: func(r worker.Record) {
		mu.Lock()
		defer mu.Unlock()
		records = append(records, r)
	}})
	files, ok := p.driver.(driver.FileStore)
	if !ok {
		t.Fatal("the remote driver has no file operations")
	}
	if _, err := files.Stat(ctx, "sbx_missing", filepath.Join(h.project, "AGENTS.md")); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("the stat across the stream answered %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(records) == 0 {
		t.Fatal("the worker recorded nothing")
	}
	if last := records[len(records)-1]; last.Outcome != worker.OutcomeFailed || last.Reason != "not found: AGENTS.md" {
		t.Errorf("the host's record of a missing file reads %s %q", last.Outcome, last.Reason)
	}
}
