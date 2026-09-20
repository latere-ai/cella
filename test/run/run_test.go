// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// Package run_test is the tier that drives the development stack of spec
// 049 end to end: the bootstrap `make run` names, run once with -smoke, so
// the composition is proved and not described. It is behind the e2e tag,
// because it compiles two binaries and starts two processes, which is
// outside the gate's untagged suite.
package run_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRunBootstrap: a clean state directory, no CELLA_OIDC_ISSUERS and no
// issuer of the developer's own, and the stack comes up, mints a token and
// creates a sandbox with it.
func TestRunBootstrap(t *testing.T) {
	for _, tool := range []string{"go", "openssl", "curl", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("the bootstrap needs %s on PATH: %v", tool, err)
		}
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join("tools", "run", "up.sh"), "-smoke")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"CELLA_RUN_DIR="+t.TempDir(),
		"CELLA_RUN_PORT="+strconv.Itoa(freePort(t)),
		// The bootstrap is what supplies the issuer, so the variable the
		// old target demanded is explicitly absent here.
		"CELLA_OIDC_ISSUERS=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the bootstrap failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"export CELLA_URL=",
		"/mint",
		"the stack created and deleted one sandbox",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the bootstrap printed no %q:\n%s", want, out)
		}
	}
}

// freePort is a port nothing holds, so two runs of this tier do not meet.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}
