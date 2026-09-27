// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"latere.ai/x/pkg/authkit/issuertest"

	v1 "latere.ai/x/cella/manifest/v1"
)

// replicaDatabase starts one Postgres for the replica test and returns its
// URL, or skips the test where no container runtime answers, as the store's
// own Postgres suite does.
func replicaDatabase(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	containerSocket(ctx)
	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("cella"),
		tcpostgres.WithUsername("cella"),
		tcpostgres.WithPassword("cella"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		t.Skipf("no container runtime answered, so the replica test did not run: %v", err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := ctr.Terminate(stop); err != nil {
			t.Logf("terminating the database: %v", err)
		}
	})
	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	return url
}

// containerSocket points the container library at a Podman machine where no
// Docker socket answers, and turns its reaper off there, as the store's
// Postgres suite does.
func containerSocket(ctx context.Context) {
	podman := func(path string) {
		if resolved, err := filepath.EvalSymlinks(path); err == nil && strings.Contains(resolved, "podman") {
			_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
		}
	}
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		podman(strings.TrimPrefix(host, "unix://"))
		return
	}
	for _, path := range []string{"/var/run/docker.sock", filepath.Join(os.Getenv("HOME"), ".docker/run/docker.sock")} {
		if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			podman(path)
			return
		}
	}
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := osexec.CommandContext(probe, "podman", "machine", "inspect", "--format", "{{.ConnectionInfo.PodmanSocket.Path}}").Output()
	if err != nil {
		return
	}
	path := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if info, err := os.Stat(path); err != nil || info.Mode()&os.ModeSocket == 0 {
		return
	}
	_ = os.Setenv("DOCKER_HOST", "unix://"+path)
	_ = os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
}

// replica is one `cellad serve` of the replica set: its two listeners, and the
// stop that ends it as SIGTERM would and returns its exit code.
type replica struct {
	public, internal string
	stop             context.CancelFunc
	done             chan struct{}
	code             int
	errOut           *syncBuffer
}

// exited waits up to within for the replica's serve to return and reports its
// exit code.
func (r *replica) exited(within time.Duration) (int, bool) {
	select {
	case <-r.done:
		return r.code, true
	case <-time.After(within):
		return 0, false
	}
}

// startReplica runs one serve over the shared database, advertising its own
// public listener to the others.
func startReplica(t *testing.T, shared map[string]string) *replica {
	t.Helper()
	public, internal := freePort(t), freePort(t)
	vars := identity(t, shared)
	vars["CELLA_DATA_DIR"] = t.TempDir()
	vars["CELLA_PUBLIC_ADDR"] = public
	vars["CELLA_INTERNAL_ADDR"] = internal
	vars["CELLA_ADVERTISE_URL"] = "http://" + public
	ctx, cancel := context.WithCancel(context.Background())
	r := &replica{public: "http://" + public, internal: "http://" + internal, stop: cancel, done: make(chan struct{}), errOut: &syncBuffer{}}
	var out syncBuffer
	go func() {
		defer close(r.done)
		r.code = run(ctx, nil, env(vars), &out, r.errOut)
	}()
	t.Cleanup(func() {
		cancel()
		if _, ok := r.exited(gracePeriod + 30*time.Second); !ok {
			t.Errorf("a replica did not stop")
		}
	})
	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(out.String(), "listening public=") {
		if code, ok := r.exited(0); ok {
			t.Fatalf("a replica exited %d before listening: %s", code, r.errOut.String())
		}
		if time.Now().After(deadline) {
			t.Fatalf("a replica never listened: %s", r.errOut.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return r
}

// writerLeaseHeld reads whether a replica reports the writer lease held.
func (r *replica) writerLeaseHeld(t *testing.T) bool {
	t.Helper()
	res, err := http.Get(r.internal + "/metrics")
	if err != nil {
		return false
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return false
	}
	return strings.Contains(string(body), `cella_lease_held{name="writer"} 1`)
}

// ready reads a replica's readiness.
func (r *replica) ready() bool {
	res, err := http.Get(r.internal + "/readyz")
	if err != nil {
		return false
	}
	_ = res.Body.Close()
	return res.StatusCode == http.StatusOK
}

func awaitCondition(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// probeResult is one request of the probe loop and how it ended.
type probeResult struct {
	at     time.Time
	status int
	code   string
	err    error
}

// TestARollingHandoffAnswersEveryRequest is spec 076 end to end: two `cellad
// serve` over one database, the first the writer and the second a standby
// that forwards to it. A probe writes and reads secrets through the standby,
// which is where a Service sends traffic once the writer stops being ready,
// while the writer is stopped as SIGTERM stops it. The writer hands off, the
// standby promotes and serves, and every request of the probe is answered: a
// success, or control_plane_unavailable inside the hold, and never a refused
// or reset connection. The standby stays ready throughout, the old writer
// exits zero, and what it wrote before the handoff is what its successor
// reads after.
func TestARollingHandoffAnswersEveryRequest(t *testing.T) {
	db := replicaDatabase(t)
	issuer := issuertest.New(t, issuertest.WithDefaultAudience("cella"))
	token := issuer.Mint(issuertest.Claims{Sub: "alice"})
	shared := map[string]string{
		"CELLA_OIDC_ISSUERS":    issuer.URL(),
		"CELLA_DB_URL":          db,
		"CELLA_SECRET_KEY":      base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		"CELLA_FORWARD_HOLD":    "8s",
		"CELLA_HANDOFF_TIMEOUT": "2s",
	}

	first := startReplica(t, shared)
	awaitCondition(t, "the first replica to become the writer", 30*time.Second, func() bool { return first.writerLeaseHeld(t) })
	second := startReplica(t, shared)
	awaitCondition(t, "the standby to be ready", 30*time.Second, second.ready)
	if second.writerLeaseHeld(t) {
		t.Fatal("the second replica holds the writer lease beside the first")
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	call := func(method, path, body string) probeResult {
		req, err := http.NewRequest(method, second.public+path, strings.NewReader(body))
		if err != nil {
			return probeResult{at: time.Now(), err: err}
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		at := time.Now()
		res, err := httpClient.Do(req)
		if err != nil {
			return probeResult{at: at, err: err}
		}
		defer func() { _ = res.Body.Close() }()
		payload, err := io.ReadAll(res.Body)
		if err != nil {
			return probeResult{at: at, status: res.StatusCode, err: err}
		}
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(payload, &envelope)
		return probeResult{at: at, status: res.StatusCode, code: envelope.Error.Code}
	}
	secret := func(name string) string {
		return `{"apiVersion":"` + v1.APIVersion + `","kind":"Secret","metadata":{"name":"` + name + `"},` +
			`"spec":{"scope":{"hosts":["api.example.com"]},"value":"v"}}`
	}
	if r := call(http.MethodPut, "/v1/secrets/before-the-handoff", secret("before-the-handoff")); r.err != nil || r.status != http.StatusCreated {
		t.Fatalf("a write forwarded to the writer answered %d %s %v", r.status, r.code, r.err)
	}

	var (
		mu       sync.Mutex
		results  []probeResult
		notReady []time.Time
	)
	stopProbe := make(chan struct{})
	probed := make(chan struct{})
	go func() {
		defer close(probed)
		for n := 0; ; n++ {
			select {
			case <-stopProbe:
				return
			default:
			}
			name := fmt.Sprintf("probe-%d", n)
			for _, r := range []probeResult{
				call(http.MethodPut, "/v1/secrets/"+name, secret(name)),
				call(http.MethodGet, "/v1/secrets/"+name, ""),
			} {
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}
			if !second.ready() {
				mu.Lock()
				notReady = append(notReady, time.Now())
				mu.Unlock()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	time.Sleep(time.Second)
	handoffStarted := time.Now()
	first.stop()
	code, ok := first.exited(gracePeriod + 30*time.Second)
	if !ok {
		t.Fatal("the old writer did not stop")
	}
	firstExited := time.Now()
	awaitCondition(t, "the standby to become the writer", 30*time.Second, func() bool { return second.writerLeaseHeld(t) })
	time.Sleep(2 * time.Second)
	close(stopProbe)
	<-probed

	if code != 0 {
		t.Errorf("the old writer exited %d: %s", code, first.errOut.String())
	}
	mu.Lock()
	defer mu.Unlock()
	during := 0
	for _, r := range results {
		if !r.at.Before(handoffStarted) && r.at.Before(firstExited) {
			during++
		}
		switch {
		case r.err != nil:
			t.Errorf("a request at %v failed on the wire: %v", r.at.Sub(handoffStarted), r.err)
		case r.status >= 200 && r.status < 300:
		case r.status == http.StatusServiceUnavailable && r.code == "control_plane_unavailable":
		default:
			t.Errorf("a request at %v was answered %d %s", r.at.Sub(handoffStarted), r.status, r.code)
		}
	}
	if during == 0 {
		t.Error("no request of the probe fell between the stop and the old writer's exit, so the handoff was not exercised")
	}
	if len(notReady) > 0 {
		t.Errorf("the standby was not ready %d times during the handoff", len(notReady))
	}
	if r := call(http.MethodGet, "/v1/secrets/before-the-handoff", ""); r.err != nil || r.status != http.StatusOK {
		t.Errorf("the secret the old writer wrote reads %d %s %v from its successor", r.status, r.code, r.err)
	}
	refused := 0
	for _, r := range results {
		if r.code == "control_plane_unavailable" {
			refused++
		}
	}
	t.Logf("%d probe requests, %d during the handoff, %d answered control_plane_unavailable", len(results), during, refused)
}
