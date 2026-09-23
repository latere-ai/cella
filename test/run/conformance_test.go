// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package run_test

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/test/conformance"
)

// TestRunConformance is the suite of design 015 against the development
// stack: `make run` brings the stubs and `cellad serve` up on loopback, the
// suite runs against that server with a token minted at the stub issuer, and
// the stack is stopped. It is the run a contributor can repeat with two
// commands, and the one the install job runs before it builds a cluster.
func TestRunConformance(t *testing.T) {
	for _, tool := range []string{"go", "openssl", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("the development stack needs %s on PATH: %v", tool, err)
		}
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	port := freePortPair(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join("tools", "run", "up.sh"))
	cmd.Dir = root
	// The bootstrap starts two processes of its own, so the tier stops the
	// group and not the shell alone: a control plane that outlived the test
	// would keep writing into a directory the test has removed.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(),
		"CELLA_RUN_DIR="+dir,
		"CELLA_RUN_PORT="+strconv.Itoa(port),
		// The bootstrap supplies the issuer, so the developer's own is
		// explicitly absent.
		"CELLA_OIDC_ISSUERS=",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
		stopped := make(chan struct{})
		go func() { defer close(stopped); _ = cmd.Wait() }()
		select {
		case <-stopped:
		case <-time.After(20 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-stopped
		}
	})
	// The stack prints one line when everything it starts is up; the stub
	// addresses are in the file it wrote for its own use. A bootstrap that
	// ended before that line is a failure to report and not a wait.
	up, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		scanner := bufio.NewScanner(stdout)
		ready := false
		for scanner.Scan() {
			if !ready && strings.Contains(scanner.Text(), "The stack is up.") {
				ready = true
				close(up)
			}
		}
	}()
	select {
	case <-up:
	case <-ended:
		t.Fatal("the bootstrap ended before the stack was up; its output is above")
	case <-ctx.Done():
		t.Fatal("the development stack did not come up")
	}

	issuer := role(t, dir, "issuer")
	sink := role(t, dir, "sink")
	driver, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	caps := driver.Capabilities()
	_ = driver.Close()
	declared := []string{}
	// Every capability the suite reads, named as the in-process run in
	// cmd/cellad names them: a capability the driver declares and the list
	// leaves out is one the suite holds the server to refusing.
	for name, ok := range map[string]bool{
		"files": caps.Files, "attach": caps.Attach, "display": caps.Display,
		"input": caps.Input, "pool": caps.Pool, "dial": caps.Dial,
	} {
		if ok {
			declared = append(declared, name)
		}
	}
	known, err := conformance.LoadDeclaration(filepath.Join(root, "test", "conformance", "known.json"))
	if err != nil {
		t.Fatal(err)
	}
	report := conformance.Run(t, conformance.Config{
		URL:          "http://127.0.0.1:" + strconv.Itoa(port),
		Token:        conformance.Minter(issuer),
		Capabilities: declared,
		SinkControl:  sink,
		Known:        known,
	})
	if len(report.Passed) == 0 {
		t.Fatal("no case passed against the development stack")
	}
}

// freePortPair is a port nothing holds whose neighbour is free as well: the
// bootstrap binds the public listener on the port it is given and the
// internal one on the next.
func freePortPair(t *testing.T) int {
	t.Helper()
	for range 20 {
		port := freePort(t)
		next, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port+1))
		if err != nil {
			continue
		}
		_ = next.Close()
		return port
	}
	t.Fatal("no pair of free ports")
	return 0
}

// role reads one stub's address out of the file the bootstrap wrote.
func role(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "stubs.addrs"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if address, ok := strings.CutPrefix(line, name+" "); ok {
			return strings.TrimSpace(address)
		}
	}
	t.Fatalf("the stack printed no address for %s:\n%s", name, data)
	return ""
}
