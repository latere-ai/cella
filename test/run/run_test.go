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
	"strings"
	"testing"
	"time"
)

// TestRunBootstrap: a clean state directory, no CELLA_OIDC_ISSUERS and no
// issuer of the developer's own, and the stack comes up, mints a token and
// creates a sandbox with it.
//
// A port picked by listening on port 0 and closing the listener is free only
// at that moment: another process can bind it before cellad does, and
// cellad's bind then fails with the address in use. The test picks such a
// port and holds it for the whole run, as that other process would, and the
// stack comes up all the same, because cellad binds port 0 itself and the
// bootstrap reads back where it landed.
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
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = taken.Close() }()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join("tools", "run", "up.sh"), "-smoke")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"CELLA_RUN_DIR="+dir,
		"CELLA_RUN_PORT=0",
		// The bootstrap is what supplies the issuer, so the variable the
		// old target demanded is explicitly absent here.
		"CELLA_OIDC_ISSUERS=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the bootstrap failed: %v\n%s", err, out)
	}
	public := role(t, dir, "cellad.addrs", "public")
	host, port, err := net.SplitHostPort(public)
	if err != nil {
		t.Fatalf("the bootstrap wrote the public address %q: %v", public, err)
	}
	if host != "127.0.0.1" || port == "0" || public == taken.Addr().String() {
		t.Fatalf("cellad reports the public address %s beside the held %s; want a loopback port of its own", public, taken.Addr())
	}
	for _, want := range []string{
		"export CELLA_URL=http://" + public + "\n",
		"/mint",
		"the stack created and deleted one sandbox",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the bootstrap printed no %q:\n%s", want, out)
		}
	}
}
