// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// sizingDriver is a runtime that sizes a sandbox's memory, over the native
// one: it reports the size each sandbox holds, records a change of size,
// reports a sandbox a test killed as its workload killed for memory, and
// holds a start open, reporting a sandbox with no workload, until the test
// lets it finish. It is what a container runtime looks like to the control
// plane across a kill and a restart.
type sizingDriver struct {
	runtime.Driver
	mu         sync.Mutex
	asked      map[string]runtime.Resources
	held       map[string]runtime.Resources
	killed     map[string]bool
	restarting map[string]bool
	// entered is sent on when a start begins and finish is what a start
	// waits for.
	entered chan string
	finish  chan struct{}
}

func newSizingDriver(d runtime.Driver) *sizingDriver {
	return &sizingDriver{
		Driver: d, asked: map[string]runtime.Resources{}, held: map[string]runtime.Resources{},
		killed: map[string]bool{}, restarting: map[string]bool{},
		entered: make(chan string, 8), finish: make(chan struct{}),
	}
}

func (d *sizingDriver) Capabilities() runtime.Capabilities {
	c := d.Driver.Capabilities()
	c.Autosize.CPU, c.Autosize.Memory = true, true
	return c
}

func (d *sizingDriver) Create(ctx context.Context, s runtime.CreateSpec) (runtime.Ref, error) {
	size := s.Resources
	if size.CPULimit == "" {
		size.CPULimit = size.CPU
	}
	d.mu.Lock()
	d.asked[s.ID], d.held[s.ID] = size, size
	d.mu.Unlock()
	return d.Driver.Create(ctx, s)
}

// seen is one state as this runtime reports it.
func (d *sizingDriver) seen(state runtime.State) runtime.State {
	d.mu.Lock()
	defer d.mu.Unlock()
	state.Resources = d.held[state.ID]
	switch {
	case d.restarting[state.ID]:
		state.Phase, state.Reason, state.StoppedAt = runtime.Stopped, "", time.Now().UTC()
	case d.killed[state.ID]:
		code := 137
		state.Phase, state.Reason, state.ExitCode = "Failed", "OOMKilled", &code
	}
	return state
}

func (d *sizingDriver) Inspect(ctx context.Context, id string) (runtime.State, error) {
	state, err := d.Driver.Inspect(ctx, id)
	if err != nil {
		return state, err
	}
	return d.seen(state), nil
}

func (d *sizingDriver) List(ctx context.Context, f runtime.Filter) ([]runtime.State, error) {
	states, err := d.Driver.List(ctx, f)
	for i := range states {
		states[i] = d.seen(states[i])
	}
	return states, err
}

func (d *sizingDriver) Update(ctx context.Context, id string, c runtime.Change) error {
	if c.Resources == nil {
		return d.Driver.Update(ctx, id, c)
	}
	size := *c.Resources
	if size.CPULimit == "" {
		size.CPULimit = size.CPU
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	size.Disk = d.asked[id].Disk
	d.asked[id] = size
	return nil
}

func (d *sizingDriver) Start(ctx context.Context, id string) error {
	d.mu.Lock()
	killed := d.killed[id]
	d.killed[id], d.restarting[id] = false, killed
	d.mu.Unlock()
	if !killed {
		return d.Driver.Start(ctx, id)
	}
	d.entered <- id
	select {
	case <-d.finish:
	case <-ctx.Done():
		return ctx.Err()
	}
	d.mu.Lock()
	d.held[id], d.restarting[id] = d.asked[id], false
	d.mu.Unlock()
	return nil
}

func (d *sizingDriver) kill(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.killed[id] = true
}

// phaseOf reads the phase and the reason one answer carries.
func phaseOf(t *testing.T, body []byte) (phase, reason string) {
	t.Helper()
	var obj v1.Sandbox
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("the answer is no sandbox: %v: %s", err, body)
	}
	return obj.Status.Phase, obj.Status.Reason
}

// TestAKilledSandboxUnderACeilingNeverAnswersFailed is the promise a client
// of the API acts on, held over HTTP. A sandbox whose workload was killed
// for memory with room under its ceiling is read by GET and by the list,
// over and over from several clients, from before the control plane has
// read it for its size until it runs again at the larger size, and every
// answer is 200 with phase Running or Starting, the second with reason
// MemoryRaised: never Failed, never Stopped, never not found. A start or a
// stop of it in between is phase_conflict, because the control plane is
// starting it. At its ceiling the same kill answers Failed with OOMKilled,
// and a start of that sandbox is phase_conflict as a start of any failed
// sandbox is.
func TestAKilledSandboxUnderACeilingNeverAnswersFailed(t *testing.T) {
	var d *sizingDriver
	f := setupDriver(t, nil, func(native runtime.Driver) runtime.Driver {
		d = newSizingDriver(native)
		return d
	})
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"sized"},
		"spec":{"resources":{"cpu":"500m","memory":"256Mi","max":{"memory":"768Mi"}}}}`
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes?wait=1", f.alice, body, http.StatusCreated), &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Status.Phase != runtime.Running || obj.Status.Resources.Memory != "256Mi" {
		t.Fatalf("the sandbox was created %s at %+v", obj.Status.Phase, obj.Status.Resources)
	}
	base := "/v1/sandboxes/" + obj.Status.ID

	// Clients read the sandbox, by id and through the list, until told to
	// stop, and keep what they saw.
	var (
		mu    sync.Mutex
		seen  = map[string]int{}
		reads atomic.Int64
		stop  = make(chan struct{})
		group sync.WaitGroup
	)
	note := func(what string) {
		mu.Lock()
		seen[what]++
		mu.Unlock()
		reads.Add(1)
	}
	get := func(path string) (int, []byte) {
		req, err := http.NewRequest(http.MethodGet, f.url+path, nil)
		if err != nil {
			return 0, nil
		}
		req.Header.Set("Authorization", "Bearer "+f.alice)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, []byte(err.Error())
		}
		defer func() { _ = res.Body.Close() }()
		answer, _ := io.ReadAll(res.Body)
		return res.StatusCode, answer
	}
	for i := range 4 {
		group.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if i%2 == 0 {
					status, answer := get(base)
					if status != http.StatusOK {
						note("GET " + http.StatusText(status))
						continue
					}
					var one v1.Sandbox
					if json.Unmarshal(answer, &one) != nil {
						note("GET unreadable")
						continue
					}
					note(strings.TrimSpace(one.Status.Phase + " " + one.Status.Reason))
					continue
				}
				status, answer := get("/v1/sandboxes")
				var page struct {
					Items []v1.Sandbox `json:"items"`
				}
				if status != http.StatusOK || json.Unmarshal(answer, &page) != nil || len(page.Items) != 1 {
					note("list " + http.StatusText(status))
					continue
				}
				note(strings.TrimSpace(page.Items[0].Status.Phase + " " + page.Items[0].Status.Reason))
			}
		})
	}
	await := func(more int64) {
		t.Helper()
		target := reads.Load() + more
		deadline := time.Now().Add(20 * time.Second)
		for reads.Load() < target {
			if time.Now().After(deadline) {
				t.Fatal("the readers stopped reading")
			}
			time.Sleep(time.Millisecond)
		}
	}
	await(20)

	// The kill. Nothing has read the sandbox for its size: the clients
	// alone meet it.
	d.kill(obj.Status.ID)
	await(60)
	if phase, reason := phaseOf(t, f.request("GET", base, f.alice, "", http.StatusOK)); phase != "Starting" || reason != v1.ReasonMemoryRaised {
		t.Fatalf("after the kill the sandbox answers %s %q, want Starting MemoryRaised", phase, reason)
	}
	// It is the control plane's to start, so a client's start and stop of
	// it are refused as a conflict with its phase.
	for _, verb := range []string{"start", "stop"} {
		refused := f.request("POST", base+"/"+verb, f.alice, "", http.StatusConflict)
		if !strings.Contains(string(refused), `"phase_conflict"`) {
			t.Fatalf("a %s of a sandbox being raised answered %s", verb, refused)
		}
	}
	// The control plane reads it and starts it again; the start is held
	// open, with the runtime holding no workload, while the clients read.
	passed := make(chan error, 1)
	go func() {
		_, err := f.c.Autosize(context.Background())
		passed <- err
	}()
	select {
	case <-d.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the control plane never started the killed sandbox")
	}
	await(60)
	close(d.finish)
	if err := <-passed; err != nil {
		t.Fatal(err)
	}
	// It runs again at the next size.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var now v1.Sandbox
		if err := json.Unmarshal(f.request("GET", base, f.alice, "", http.StatusOK), &now); err != nil {
			t.Fatal(err)
		}
		if now.Status.Phase == runtime.Running && now.Status.Resources.Memory == "768Mi" && now.Status.Moves == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sandbox did not come back: %s %q at %+v with %d moves", now.Status.Phase, now.Status.Reason, now.Status.Resources, now.Status.Moves)
		}
		time.Sleep(5 * time.Millisecond)
	}
	await(20)
	close(stop)
	group.Wait()
	starting := "Starting " + v1.ReasonMemoryRaised
	for what, n := range seen {
		if what != runtime.Running && what != starting {
			t.Errorf("clients were answered %q %d times; a sandbox being started again answers Running or Starting only", what, n)
		}
	}
	if seen[starting] == 0 || seen[runtime.Running] == 0 {
		t.Fatalf("clients were answered %v, want both Running and Starting", seen)
	}

	// Now at its ceiling, the same kill is terminal: Failed with the
	// reason, until a client acts, and a start of it is refused.
	d.kill(obj.Status.ID)
	if _, err := f.c.Autosize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if phase, reason := phaseOf(t, f.request("GET", base, f.alice, "", http.StatusOK)); phase != "Failed" || reason != "OOMKilled" {
		t.Fatalf("at its ceiling the killed sandbox answers %s %q, want Failed OOMKilled", phase, reason)
	}
	refused := f.request("POST", base+"/start", f.alice, "", http.StatusConflict)
	if !strings.Contains(string(refused), `"phase_conflict"`) {
		t.Fatalf("a start of a failed sandbox answered %s", refused)
	}
	// Raising the ceiling is an act that keeps its files: it has room
	// again and answers Starting from that update on.
	raised := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"sized"},
		"spec":{"resources":{"cpu":"500m","memory":"256Mi","max":{"memory":"2Gi"}}}}`
	f.request("PUT", "/v1/sandboxes/sized", f.alice, raised, http.StatusOK)
	if phase, reason := phaseOf(t, f.request("GET", base, f.alice, "", http.StatusOK)); phase != "Starting" || reason != v1.ReasonMemoryRaised {
		t.Fatalf("with its ceiling raised the killed sandbox answers %s %q, want Starting MemoryRaised", phase, reason)
	}
}
