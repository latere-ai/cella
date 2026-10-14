// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
)

func TestServeRefusesUnavailableRuntime(t *testing.T) {
	var errOut bytes.Buffer
	// Every runtime the configuration admits is built, so a name outside
	// the list is refused by the configuration, before any driver opens.
	cfg := identity(t, map[string]string{"CELLA_RUNTIME": "vm", "CELLA_DATA_DIR": t.TempDir()})
	if code := run(t.Context(), nil, env(cfg), io.Discard, &errOut); code != 1 || !strings.Contains(errOut.String(), "one of k8s, podman, native") {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
}

// TestPodmanRuntimeSelected proves CELLA_RUNTIME=podman reaches the podman
// driver: the start-up fails at its preflight, naming the socket it was given,
// rather than at the selection.
func TestPodmanRuntimeSelected(t *testing.T) {
	var errOut bytes.Buffer
	socket := filepath.Join(t.TempDir(), "absent.sock")
	cfg := identity(t, map[string]string{"CELLA_RUNTIME": "podman", "CELLA_PODMAN_SOCKET": socket, "CELLA_DATA_DIR": t.TempDir()})
	code := run(t.Context(), nil, env(cfg), io.Discard, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "runtime preflight") || !strings.Contains(errOut.String(), socket) {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
}

// The Kubernetes driver is selected and built, so a node whose cluster is
// unreachable fails at start-up with the cluster named rather than serving an
// API over a backend that is not there.
func TestServeNeedsTheClusterItIsPointedAt(t *testing.T) {
	var errOut bytes.Buffer
	cfg := identity(t, map[string]string{
		"CELLA_RUNTIME":        "k8s",
		"CELLA_DATA_DIR":       t.TempDir(),
		"CELLA_K8S_KUBECONFIG": filepath.Join(t.TempDir(), "absent"),
	})
	code := run(t.Context(), nil, env(cfg), io.Discard, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "kubeconfig") {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
}

func TestNativeServerEndToEnd(t *testing.T) {
	issuer := issuertest.New(t)
	cfg := map[string]string{"CELLA_OIDC_ISSUERS": issuer.URL(), "CELLA_OIDC_AUDIENCE": "cella,platform.example", "CELLA_DATA_DIR": t.TempDir()}
	base, _, _, stop := startServeWithLog(t, cfg)
	defer func() { stop() }()
	alice := issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"platform.example"}})
	bob := issuer.Mint(issuertest.Claims{Sub: "bob", Aud: issuertest.StringList{"platform.example"}})
	request := func(method, path, token, body string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, resp.StatusCode, data, want)
		}
		return data
	}
	request("GET", "/v1/sandboxes", "", "", 401)
	body := request("POST", "/v1/sandboxes?wait=1", alice, `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"e2e"},"spec":{}}`, 201)
	var obj struct {
		Status struct {
			ID    string `json:"id"`
			Owner string `json:"owner"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Status.ID == "" || obj.Status.Owner != issuer.URL()+"|alice" {
		t.Fatalf("object = %s", body)
	}
	path := "/v1/sandboxes/" + obj.Status.ID
	request("GET", path, bob, "", 403)
	if list := request("GET", "/v1/sandboxes", bob, "", 200); strings.Contains(string(list), obj.Status.ID) {
		t.Fatalf("foreign object in list: %s", list)
	}
	result := request("POST", path+"/exec?wait=1", alice, `{"command":["sh","-c","printf migrated; printf diagnostic >&2; exit 7"]}`, 200)
	var output struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal(result, &output); err != nil {
		t.Fatal(err)
	}
	if output.ExitCode != 7 || output.Stdout != "migrated" || output.Stderr != "diagnostic" {
		t.Fatalf("exec = %s", result)
	}
	request("POST", path+"/stop", alice, "", 200)
	request("POST", path+"/exec?wait=1", alice, `{"command":["sh","-c","exit 0"]}`, 409)
	// Stop the server and reopen the same runtime and desired-state records.
	if code := stop(); code != 0 {
		t.Fatalf("shutdown: %d", code)
	}
	var internal string
	base, internal, _, stop = startServeWithLog(t, cfg)
	recovered := request("GET", path, alice, "", 200)
	if !strings.Contains(string(recovered), `"phase":"Stopped"`) {
		t.Fatalf("restart: %s", recovered)
	}
	request("POST", path+"/start", alice, "", 200)
	deleting := request("DELETE", path, alice, "", 202)
	if !strings.Contains(string(deleting), `"phase":"Deleting"`) {
		t.Fatalf("delete response: %s", deleting)
	}
	request("GET", path, alice, "", 404)
	if code, _ := get(t, fmt.Sprintf("%s/v1/sandboxes", internal)); code != 404 {
		t.Fatalf("internal API exposed: %d", code)
	}
}

func TestSecondServerCannotMutateRunningWorkload(t *testing.T) {
	data := t.TempDir()
	store, err := controller.OpenFileStore(filepath.Join(data, "controller"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	driver, err := native.New(filepath.Join(data, "native"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = driver.Close() }()
	_, err = driver.Create(t.Context(), runtime.CreateSpec{ID: "sbx_running", Owner: "alice", Command: []string{"sh", "-c", "sleep 60"}})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := run(t.Context(), nil, env(identity(t, map[string]string{"CELLA_DATA_DIR": data})), io.Discard, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "already in use") {
		t.Fatalf("second start: %d %s", code, stderr.String())
	}
	state, err := driver.Inspect(t.Context(), "sbx_running")
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != runtime.Running {
		t.Fatalf("second start mutated active workload: %+v", state)
	}
	if err := driver.Stop(t.Context(), "sbx_running"); err != nil {
		t.Fatalf("running workload no longer stoppable: %v", err)
	}
}

func TestReadinessIncludesRuntime(t *testing.T) {
	data := t.TempDir()
	_, internal, _, stop := startServeWithLog(t, map[string]string{"CELLA_DATA_DIR": data})
	defer stop()
	if err := os.Remove(filepath.Join(data, "native")); err != nil {
		t.Fatal(err)
	}
	if code, body := get(t, internal+"/readyz"); code != 503 {
		t.Fatalf("missing runtime ready: %d %s", code, body)
	}
}

// mover is a driver that moves credentials out of its records, answering
// the count and the failure it is given.
type mover struct {
	runtime.Driver
	moved int
	err   error
	calls int
}

func (m *mover) MoveCredentials(context.Context) (int, error) {
	m.calls++
	return m.moved, m.err
}

// TestMoveCredentialsIsLoggedAndFailsNothing: a driver that moves
// credentials is asked once, what it moved and what it could not are logged,
// and one that moves nothing is not asked.
func TestMoveCredentialsIsLoggedAndFailsNothing(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	m := &mover{moved: 2, err: errors.New("sandbox sbx_a: claim patch: forbidden")}
	moveCredentials(t.Context(), m, log)
	if m.calls != 1 {
		t.Fatalf("the driver was asked %d times", m.calls)
	}
	for _, want := range []string{"count=2", "sbx_a", "level=WARN"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the log %q does not carry %q", out.String(), want)
		}
	}

	out.Reset()
	moveCredentials(t.Context(), &mover{}, log)
	if out.Len() != 0 {
		t.Errorf("a move with nothing to do logged %q", out.String())
	}
	// A driver with no records to move is not asked at all.
	plain, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	moveCredentials(t.Context(), plain, log)
	if out.Len() != 0 {
		t.Errorf("a driver with no move logged %q", out.String())
	}
}

// sealer is a store that seals the credentials its rows hold in the clear,
// answering the count and the failure it is given.
type sealer struct {
	store.Store
	sealed int
	err    error
	calls  int
}

func (s *sealer) SealCredentials(context.Context) (int, error) {
	s.calls++
	return s.sealed, s.err
}

// TestSealCredentialsIsLoggedAndFailsNothing: a store that seals is asked
// once, what it sealed and what it could not are logged, and one that seals
// nothing is not asked.
func TestSealCredentialsIsLoggedAndFailsNothing(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	s := &sealer{sealed: 3, err: errors.New("sandbox sbx_a: store: opening the credentials of sbx_a")}
	sealCredentials(t.Context(), s, log)
	if s.calls != 1 {
		t.Fatalf("the store was asked %d times", s.calls)
	}
	for _, want := range []string{"count=3", "sbx_a", "level=WARN"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the log %q does not carry %q", out.String(), want)
		}
	}

	out.Reset()
	sealCredentials(t.Context(), &sealer{}, log)
	if out.Len() != 0 {
		t.Errorf("a rewrite with nothing to do logged %q", out.String())
	}
	// A store with no rows of an earlier release to seal is not asked.
	plain, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	sealCredentials(t.Context(), plain, log)
	if out.Len() != 0 {
		t.Errorf("a store with no rewrite logged %q", out.String())
	}
}
