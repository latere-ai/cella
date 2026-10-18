// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Command worker is a program that embeds Cella's worker: it serves one
// environment from the machine it runs on, asks on its terminal before the
// control plane creates a sandbox there, and prints one line for every
// operation it answers. It is the embedding of docs/workers.md as code.
//
// It runs the native driver so it runs anywhere, and the native driver
// confines nothing: a program that serves a person's computer runs the
// local driver instead, which confines each sandbox to the folders the
// person grants.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/worker"
)

func main() {
	os.Exit(command(context.Background(), os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

// command is the program: its configuration read from the environment, run
// until a signal or a refusal, and the exit code.
func command(ctx context.Context, getenv func(string) string, in io.Reader, out, errOut io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg := config{URL: getenv("CELLA_URL"), Key: getenv("CELLA_ENVIRONMENT_KEY"), DataDir: getenv("WORKER_DATA_DIR")}
	if err := run(ctx, cfg, in, out); err != nil {
		_, _ = fmt.Fprintln(errOut, "worker:", err)
		return 1
	}
	return 0
}

// config is what this program reads from its environment: the control
// plane's URL, the environment key an administrator minted, and where the
// driver keeps its sandboxes.
type config struct {
	URL, Key, DataDir string
}

// run serves the environment until ctx ends or the control plane refuses
// this worker.
func run(ctx context.Context, cfg config, in io.Reader, out io.Writer) error {
	if cfg.DataDir == "" {
		cfg.DataDir = "./worker-data"
	}
	d, err := native.New(cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	ask := &terminal{answers: bufio.NewScanner(in), out: out}
	return worker.Run(ctx, worker.Options{
		URL: cfg.URL,
		// A program that keeps the key in the operating system's key store
		// reads it here, once per connection.
		Key:    func(context.Context) (string, error) { return cfg.Key, nil },
		Driver: d,
		Policy: ask,
		Record: ask.record,
	})
}

// terminal is the host's policy: it asks on the terminal before each create
// and lets every other operation through, since the sandbox it runs in was
// already allowed.
type terminal struct {
	mu      sync.Mutex
	answers *bufio.Scanner
	out     io.Writer
}

func (t *terminal) Admit(_ context.Context, c worker.Create) worker.Verdict {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = fmt.Fprintf(t.out, "allow sandbox %s, owned by %s? [y/N] ", c.Sandbox.Metadata.Name, c.Sandbox.Status.Owner)
	if !t.answers.Scan() || strings.TrimSpace(strings.ToLower(t.answers.Text())) != "y" {
		return worker.Verdict{Reason: "the person at this computer declined"}
	}
	return worker.Verdict{Allow: true}
}

func (*terminal) Check(context.Context, worker.Operation) worker.Verdict {
	return worker.Verdict{Allow: true}
}

// record prints one operation as a line of JSON. A record carries no file
// content, no stdin and no output.
func (t *terminal) record(r worker.Record) {
	line, err := json.Marshal(r)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = fmt.Fprintln(t.out, string(line))
}
