// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/internal/egressd"
	driver "latere.ai/x/cella/runtime"
)

// TestWorkloadTokenNeverLeavesItsSandbox follows a workload token the way
// TestSecretValuesNeverEnterASandbox follows a secret value, over the same
// tier: one control plane, one gateway, one sandbox. The two canaries differ
// in where the value is allowed to be. A secret's value is allowed nowhere
// inside the sandbox; a workload token is the sandbox's own identity and its
// one home is the projection the driver writes. Everywhere else is a leak:
// the environment, which carries the path and not the bytes, the events, the
// connection records, the API answers, the logs of both processes, and every
// other file the control plane wrote.
func TestWorkloadTokenNeverLeavesItsSandbox(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "the upstream answered "+r.Host+r.URL.Path)
	}))
	defer upstream.Close()
	sink := newStubSink(t, "only-secret")
	proxyLn, reverseLn := doors(t)
	proxyAddr, reverseAddr := proxyLn.Addr().String(), reverseLn.Addr().String()
	plane := startPlaneWith(t, proxyAddr, reverseAddr, map[string]string{
		"CELLA_EVENTS_URL":    sink.server.URL,
		"CELLA_EVENTS_SECRET": "only-secret",
	})

	var gatewayLog syncBuffer
	ready := make(chan struct{})
	startGateway(t, plane, egressd.Options{
		ProxyListener: proxyLn, ReverseListener: reverseLn,
		UpstreamCAPEM: certificatePEM(t, upstream),
		Dial:          dialTo(upstream.Listener.Addr().String()),
		Log:           slog.New(slog.NewTextHandler(&gatewayLog, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Ready:         func() { close(ready) },
	})
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("the gateway never received its first snapshot")
	}

	sandbox := plane.create(t, `{"mode":"allowlist","allowedHosts":["upstream.example.com"]}`)

	// The token this tier follows is the one the sandbox holds, read the way
	// the agent client reads it: out of the file the projection names.
	projection := strings.TrimSpace(plane.exec(t, sandbox, `printenv `+driver.TokenFileEnv))
	token := strings.TrimSpace(plane.exec(t, sandbox, `cat "$`+driver.TokenFileEnv+`"`))
	if strings.Count(token, ".") != 2 || len(token) < 64 {
		t.Fatalf("the projection holds %q, want a compact JWS", token)
	}

	t.Run("theTokenIsTheSandboxsOwnIdentity", func(t *testing.T) {
		// A canary that followed a token the control plane does not honor
		// would prove nothing, so the token is used once before it is
		// looked for.
		var read struct {
			Status struct {
				ID         string          `json:"id"`
				TokenState json.RawMessage `json:"tokenState"`
			} `json:"status"`
		}
		if err := json.Unmarshal(asSandbox(t, plane, sandbox, token), &read); err != nil {
			t.Fatal(err)
		}
		if read.Status.ID != sandbox {
			t.Fatalf("the sandbox read %s with its own token, want %s", read.Status.ID, sandbox)
		}
		if len(read.Status.TokenState) != 0 {
			t.Fatalf("the answer carries the control plane's token record: %s", read.Status.TokenState)
		}
	})

	t.Run("theEnvironmentCarriesThePathAndNotTheToken", func(t *testing.T) {
		environment := plane.exec(t, sandbox, "env")
		if !strings.Contains(environment, driver.TokenFileEnv+"=") {
			t.Fatalf("the sandbox was not told where its token is: %s", environment)
		}
		if strings.Contains(environment, token) {
			t.Fatal("the token itself is in the sandbox's environment")
		}
	})

	t.Run("theTokenIsInNoFileButItsOwnProjection", func(t *testing.T) {
		// Inside the sandbox, the projection is the one file that holds it:
		// the workspace it works in and the directory the control plane's
		// own files are projected into hold nothing else of it.
		found := strings.Fields(plane.exec(t, sandbox,
			`grep -rl '`+token+`' "$PWD" "$(dirname "$`+driver.TokenFileEnv+`")" 2>/dev/null; true`))
		if len(found) != 1 || found[0] != projection {
			t.Fatalf("the token is in %v, want only %s", found, projection)
		}
		// Outside it, the control plane keeps the token's id and its two
		// instants and never the token, so the projection is the only file
		// under the data directory that holds it.
		walked := grepTree(t, plane.dataDir, token)
		for _, path := range walked {
			if filepath.Clean(path) != filepath.Clean(projection) {
				t.Errorf("the token is in %s, which is not the sandbox's own projection", path)
			}
		}
		// The walk did read the tree: where the driver projects under the
		// data directory, the projection is what it found.
		if strings.HasPrefix(projection, plane.dataDir) && len(walked) == 0 {
			t.Fatal("the walk of the data directory found nothing, not even the projection")
		}
	})

	t.Run("theTokenIsInNoRecordAndNoAnswer", func(t *testing.T) {
		gatewayURL := strings.TrimSpace(plane.exec(t, sandbox, "printenv CELLA_GATEWAY_URL"))
		credential := strings.TrimSpace(plane.exec(t, sandbox, "printenv CELLA_GATEWAY_CREDENTIAL"))
		if gatewayURL == "" || credential == "" {
			t.Fatalf("the sandbox was not pointed at a gateway: %q %q", gatewayURL, credential)
		}
		body := reverseGet(t, gatewayURL+"/upstream.example.com/v1/things", credential, "Bearer not-the-workload-token")
		if !strings.Contains(body, "the upstream answered upstream.example.com/v1/things") {
			t.Fatalf("the upstream answered %q", body)
		}
		records := plane.records(t, sandbox, 1)
		encoded, err := json.Marshal(records)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, []byte(token)) {
			t.Fatalf("a connection record carries the token: %s", encoded)
		}
		for _, path := range []string{"/v1/sandboxes/" + sandbox, "/v1/sandboxes"} {
			if strings.Contains(plane.get(t, path), token) {
				t.Fatalf("%s answers with the token", path)
			}
		}
	})

	t.Run("theTokenIsInNoEvent", func(t *testing.T) {
		// The record this run must produce. Waiting for it is what makes the
		// check an assertion rather than a race the sink can win by
		// delivering nothing.
		deadline := time.Now().Add(30 * time.Second)
		for {
			created := false
			for _, record := range sink.records() {
				if bytes.Contains(record.Raw, []byte(token)) {
					t.Fatalf("an event carries the token: %s", record.Raw)
				}
				if record.Type == "sandbox.created" {
					created = true
				}
			}
			if created {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the sink received no sandbox.created record")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("theTokenIsInNoLog", func(t *testing.T) {
		for name, written := range map[string]string{
			"the control plane's output": plane.out.String(),
			"the control plane's errors": plane.errOut.String(),
			"the gateway's log":          gatewayLog.String(),
		} {
			if strings.Contains(written, token) {
				t.Fatalf("%s carries the token", name)
			}
		}
		// The logs did run: a tier that printed nothing would pass the check
		// above without proving anything.
		if plane.out.String() == "" || gatewayLog.String() == "" {
			t.Fatal("neither process logged, so the check above proves nothing")
		}
	})
}

// asSandbox is one call to the control plane as the sandbox itself, with its
// own workload token as the bearer.
func asSandbox(t *testing.T, p *plane, sandbox, token string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, p.url+"/v1/sandboxes/"+sandbox, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the sandbox read itself: %d %s", resp.StatusCode, body)
	}
	return body
}
