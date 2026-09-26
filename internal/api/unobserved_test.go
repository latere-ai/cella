// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/controller"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// readFailingDriver answers the read of the sandboxes the case names the way
// the case needs: with an error of its own, or, for a held one, only once the
// read's caller has gone. Every other call is the wrapped driver's.
type readFailingDriver struct {
	runtime.Driver
	mu      sync.Mutex
	fail    map[string]error
	held    map[string]bool
	entered chan struct{}
}

func (d *readFailingDriver) failRead(id string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fail[id] = err
}

func (d *readFailingDriver) holdRead(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.held[id] = true
}

func (d *readFailingDriver) Inspect(ctx context.Context, id string) (runtime.State, error) {
	d.mu.Lock()
	err, failing := d.fail[id]
	holding := d.held[id]
	d.mu.Unlock()
	switch {
	case holding:
		select {
		case d.entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return runtime.State{}, fmt.Errorf("pods get: %w", ctx.Err())
	case failing:
		return runtime.State{}, err
	}
	return d.Driver.Inspect(ctx, id)
}

// listPage reads one page of the sandbox list as alice and decodes it.
func (f *fixture) listPage(query string) []v1.Sandbox {
	f.t.Helper()
	var page struct {
		Items []v1.Sandbox `json:"items"`
	}
	body := f.request(http.MethodGet, "/v1/sandboxes"+query, f.alice, "", http.StatusOK)
	if err := json.Unmarshal(body, &page); err != nil {
		f.t.Fatalf("the list is %s: %v", body, err)
	}
	return page.Items
}

// observedCondition is the Observed condition a row carries, if any.
func observedCondition(obj v1.Sandbox) (v1.Condition, bool) {
	for _, c := range obj.Status.Conditions {
		if c.Type == v1.ConditionObserved {
			return c, true
		}
	}
	return v1.Condition{}, false
}

// TestAListAnswersARowWhoseRuntimeDidNotAnswer is spec 075: one sandbox
// whose driver read fails does not fail the page. The page answers every
// row; the one whose read failed carries the status last written, its phase
// included, with Observed False and the reason, and the others carry what
// the runtime answered and no Observed condition. The phase selector reads
// the phase the row answers. The failure is logged with the sandbox and the
// error, and the caller reads a fixed sentence and never the driver's error.
func TestAListAnswersARowWhoseRuntimeDidNotAnswer(t *testing.T) {
	d := &readFailingDriver{fail: map[string]error{}, held: map[string]bool{}, entered: make(chan struct{}, 1)}
	f := setupDriver(t, nil, func(inner runtime.Driver) runtime.Driver { d.Driver = inner; return d })
	sink := &logSink{}
	f.h.(*handler).log = slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelInfo}))
	first, second, third := f.sandbox("first"), f.sandbox("second"), f.sandbox("third")
	for _, obj := range []v1.Sandbox{first, second, third} {
		if obj.Status.Phase != runtime.Running {
			t.Fatalf("%s is %s after a held create, want Running", obj.Metadata.Name, obj.Status.Phase)
		}
	}
	d.failRead(second.Status.ID, errors.New("pods get: dial tcp 10.0.0.1:443: connect: connection refused"))
	sink.clear()

	items := f.listPage("")
	if len(items) != 3 {
		t.Fatalf("the page carries %d rows, want all three", len(items))
	}
	for _, obj := range items {
		cond, marked := observedCondition(obj)
		if obj.Status.ID != second.Status.ID {
			if marked {
				t.Errorf("%s was read and still carries %+v", obj.Metadata.Name, cond)
			}
			continue
		}
		if !marked || cond.Status != v1.ConditionFalse || cond.Reason != v1.ReasonDriverUnavailable || cond.Since.IsZero() {
			t.Fatalf("the row whose read failed carries %+v, want Observed False DriverUnavailable", obj.Status.Conditions)
		}
		if obj.Status.Phase != runtime.Running {
			t.Errorf("the row whose read failed is %s, want the Running last written", obj.Status.Phase)
		}
		if cond.Message == "" || containsAny(cond.Message, "10.0.0.1", "connection refused") {
			t.Errorf("the row's sentence is %q, want the fixed one without the driver's error", cond.Message)
		}
	}
	if running := f.listPage("?phase=Running"); len(running) != 3 {
		t.Errorf("phase=Running carries %d rows, want all three with the last written phase for the unread one", len(running))
	}

	var warned bool
	for _, line := range parseLines(t, sink.String()) {
		if line["level"] == "WARN" && line["sandbox"] == second.Status.ID && containsAny(fmt.Sprint(line["err"]), "connection refused") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("the failed read was not logged with its sandbox and error: %s", sink.String())
	}

	// The single read of that sandbox is one object's one answer, and is
	// refused rather than answered from what was last written.
	f.request(http.MethodGet, "/v1/sandboxes/"+second.Status.ID, f.alice, "", http.StatusServiceUnavailable)
}

// TestAListWhoseCallerLeftIsNotAnsweredStale: a read that failed because the
// caller closed the request ends the page with that failure rather than
// marking the row and going on, so the request is counted client_closed and
// not answered 200 to nobody.
func TestAListWhoseCallerLeftIsNotAnsweredStale(t *testing.T) {
	const route = "GET /v1/sandboxes"
	d := &readFailingDriver{fail: map[string]error{}, held: map[string]bool{}, entered: make(chan struct{}, 1)}
	f := setupDriver(t, nil, func(inner runtime.Driver) runtime.Driver { d.Driver = inner; return d })
	obj := f.sandbox("abandoned")
	d.holdRead(obj.Status.ID)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+"/v1/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.alice)
	done := make(chan error, 1)
	go func() {
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = res.Body.Close()
		}
		done <- err
	}()
	select {
	case <-d.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the list never reached the driver's read")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the aborted list ended with %v", err)
	}
	if got := f.metrics.awaitCount(t, route); got != (request{route, "4xx", ClientClosed}) {
		t.Errorf("counted %+v, want the 4xx class and %s, not a page answered to nobody", got, ClientClosed)
	}
}

// TestUnobservedNamesTheCause: a row on an environment this control plane
// does not hold is marked with its own reason, and a row marked twice
// carries one Observed condition.
func TestUnobservedNamesTheCause(t *testing.T) {
	now := time.Now().UTC()
	obj := unobserved(v1.Sandbox{}, fmt.Errorf("refresh: %w", controller.ErrNoEnvironment), now)
	obj = unobserved(obj, fmt.Errorf("refresh: %w", controller.ErrNoEnvironment), now)
	if len(obj.Status.Conditions) != 1 {
		t.Fatalf("the row carries %+v, want one Observed condition", obj.Status.Conditions)
	}
	if cond := obj.Status.Conditions[0]; cond.Reason != v1.ReasonEnvironmentNotHeld || cond.Status != v1.ConditionFalse {
		t.Errorf("the condition is %+v, want Observed False EnvironmentNotHeld", cond)
	}
}

// containsAny reports whether s holds any of the fragments.
func containsAny(s string, fragments ...string) bool {
	return slices.ContainsFunc(fragments, func(f string) bool { return strings.Contains(s, f) })
}
