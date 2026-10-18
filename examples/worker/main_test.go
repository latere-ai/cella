// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/worker"
)

// output is what the program prints while the test reads it.
type output struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}

// controlPlane is the two routes a worker reaches, served by the hub cellad
// serves, and the remote driver over them.
func controlPlane(t *testing.T) (string, *remote.Hub, *remote.Driver) {
	t.Helper()
	hub := remote.NewHub(remote.HubOptions{})
	upgrader := websocket.Upgrader{Subprotocols: []string{remote.Protocol}, CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/environments/{id}/workers", func(w http.ResponseWriter, r *http.Request) {
		var registration remote.Registration
		if err := json.NewDecoder(r.Body).Decode(&registration); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		registered, err := hub.Register("env_example", registration)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		registered.Environment = "env_example"
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(registered)
	})
	mux.HandleFunc("GET /v1/environments/{id}/operations", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = hub.Serve(r.Context(), "env_example", worker.NewSocket(conn))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	d, err := remote.New(remote.Options{Environment: "env_example", Transport: hub.Transport("env_example")})
	if err != nil {
		t.Fatal(err)
	}
	return server.URL, hub, d
}

// TestTheExampleAsksAndRecords runs the program against a control plane: the
// first create is allowed on the terminal and runs, the second is declined
// with the person's reason, and every operation is printed as a record.
func TestTheExampleAsksAndRecords(t *testing.T) {
	url, _, d := controlPlane(t)
	var out output
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, config{URL: url, Key: "an-environment-key", DataDir: t.TempDir()}, strings.NewReader("y\nn\n"), &out)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for d.Ready(t.Context()) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the example never connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_first", Name: "first", Owner: "alice", Command: []string{"sleep", "30"}}); err != nil {
		t.Fatalf("the allowed create failed: %v", err)
	}
	_, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_second", Name: "second", Owner: "alice", Command: []string{"sleep", "30"}})
	if !errors.Is(err, driver.ErrHostRefused) || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("the declined create answered %v", err)
	}
	for !strings.Contains(out.String(), `"Outcome":"refused"`) {
		if time.Now().After(deadline) {
			t.Fatalf("the refusal was never recorded: %s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("the example ended with %v", err)
	}
	printed := out.String()
	for _, want := range []string{"allow sandbox first, owned by alice? [y/N]", `"Type":"Create"`, `"Outcome":"ok"`} {
		if !strings.Contains(printed, want) {
			t.Errorf("the example did not print %q:\n%s", want, printed)
		}
	}
	if (&terminal{}).Check(t.Context(), worker.Operation{}).Allow != true {
		t.Error("the example refuses operations in a sandbox it allowed")
	}
}

// TestTheExampleRefusesWithoutAControlPlane: with no URL there is nothing to
// connect to, which the program says rather than waiting.
func TestTheExampleRefusesWithoutAControlPlane(t *testing.T) {
	var errOut output
	getenv := func(name string) string {
		if name == "WORKER_DATA_DIR" {
			return t.TempDir()
		}
		return ""
	}
	if code := command(t.Context(), getenv, strings.NewReader(""), &output{}, &errOut); code != 1 || !strings.Contains(errOut.String(), "control plane URL") {
		t.Fatalf("the program exited %d: %s", code, errOut.String())
	}
	if err := run(t.Context(), config{DataDir: t.TempDir()}, strings.NewReader(""), &output{}); err == nil {
		t.Fatal("the example ran with no control plane")
	}
	if err := run(t.Context(), config{URL: "http://127.0.0.1:1", DataDir: "/dev/null/not-a-dir"}, strings.NewReader(""), &output{}); err == nil {
		t.Fatal("the example ran with a data directory it cannot make")
	}
}

// TestTheProgramStopsCleanly: the program connected to a control plane exits
// zero when it is asked to stop.
func TestTheProgramStopsCleanly(t *testing.T) {
	url, _, d := controlPlane(t)
	dir := t.TempDir()
	env := map[string]string{"CELLA_URL": url, "CELLA_ENVIRONMENT_KEY": "k", "WORKER_DATA_DIR": dir}
	ctx, cancel := context.WithCancel(t.Context())
	code := make(chan int, 1)
	go func() {
		code <- command(ctx, func(name string) string { return env[name] }, strings.NewReader(""), &output{}, &output{})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for d.Ready(t.Context()) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the program never connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if got := <-code; got != 0 {
		t.Fatalf("the program exited %d when asked to stop", got)
	}
}
