// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// full is a configuration that carries every input and declares every
// capability, which is what a server that serves the whole contract is run
// with. Against a fake that serves a quarter of it, every case runs and the
// ones the fake does not answer fail, which is the point of
// TestSuiteCatchesAFalseCapability.
func (f *fake) full() Config {
	cfg := f.config()
	cfg.Capabilities = []string{"attach", "files", "display", "input", "dial", "volumes", "mesh"}
	cfg.Admin = "fake-token"
	cfg.Token = func(context.Context, string) (string, error) { return "fake-token", nil }
	cfg.AuthorizerControl = f.server.URL
	cfg.AdmissionControl = f.server.URL
	cfg.SinkControl = f.server.URL
	cfg.QueuedEnvironment = "queued"
	cfg.WorkerEnvironment = "worker"
	cfg.DisplayImage = "fake/desktop"
	cfg.Cella = "/bin/echo"
	return cfg
}

// TestSuiteCatchesAFalseCapability: an environment that declares a
// capability its server does not honour fails the capability group and the
// cases that capability gates, rather than being excused by the gate.
func TestSuiteCatchesAFalseCapability(t *testing.T) {
	f := newFake(t)
	cfg := f.full()
	report, err := Execute(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"case004CapabilityGates", "case008ExecSocket", "case008AttachSocket", "case008Dial"} {
		if !slices.Contains(report.Failed, name) {
			t.Errorf("%s did not fail although the environment declares a capability the server does not honour", name)
		}
		if slices.Contains(report.Skipped, name) {
			t.Errorf("%s was excused as a skip", name)
		}
	}
	// Every input is configured, so no case skips for want of one.
	for _, name := range report.Skipped {
		if reason := report.Reasons[name]; strings.Contains(reason, "set ") {
			t.Errorf("%s skipped for want of an input the configuration carries: %s", name, reason)
		}
	}
}

// TestAServerThatIsNotThereFailsEveryCase: a run against an address nothing
// answers reports every case it ran as a failure naming the call, and never
// as a pass or a skip.
func TestAServerThatIsNotThereFailsEveryCase(t *testing.T) {
	f := newFake(t)
	cfg := f.full()
	// The agent case shells out to a binary and not to the server, so it is
	// left out of a run that is about a server that is not there.
	cfg.Cella = ""
	f.server.Close()
	report, err := Execute(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Passed) != 0 {
		t.Errorf("%d case(s) passed against a server that is not there: %v", len(report.Passed), report.Passed)
	}
	if len(report.Failed) < len(Names())/2 {
		t.Errorf("only %d of %d cases failed against a server that is not there", len(report.Failed), len(Names()))
	}
	if report.Marker.Server != "" {
		t.Errorf("the marker names server %q from a server that is not there", report.Marker.Server)
	}
}

// TestABodyThatIsNoAnswerIsReportedFailed: a server that answers every call
// with something that is not the shape the design states fails the case that
// read it, and the report says what could not be read.
func TestABodyThatIsNoAnswerIsReportedFailed(t *testing.T) {
	f := newFake(t)
	f.breakMode = "garbage"
	cfg := f.full()
	cfg.Cella = ""
	report, err := Execute(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The two cases a body proves nothing about: the door, where a call with
	// no bearer is refused before anything is written, and the public
	// documents, which are outside the API and outside this mode.
	for _, name := range report.Passed {
		if name != "case006Unauthenticated" && name != "case008PublicDocuments" {
			t.Errorf("%s passed against a server that answers no JSON", name)
		}
	}
	if len(report.Failed) < len(Names())/2 {
		t.Errorf("only %d of %d cases failed", len(report.Failed), len(Names()))
	}
}

// TestEveryStepOfEveryCaseIsReportedFailed: one case at a time, against a
// server that breaks at its first call, then at its second, and so on, in
// the two ways a call can break: the connection goes away, and the answer is
// not one the design allows. Whichever step the case had reached, it is
// reported as a failure that names that step, never as a pass and never as a
// panic. This is what proves every assertion inside every case is reached
// and reported, and not only the first one.
func TestEveryStepOfEveryCaseIsReportedFailed(t *testing.T) {
	for _, name := range Names() {
		others := []string{}
		for _, other := range Names() {
			if other != name {
				others = append(others, other)
			}
		}
		for _, mode := range []string{"gone", "garbage"} {
			noticed := false
			for after := range 12 {
				f := newFake(t)
				f.breakMode, f.breakAfter = mode, after
				cfg := f.full()
				cfg.Cella = ""
				cfg.Skip = others
				report, err := Execute(t.Context(), cfg)
				if err != nil {
					t.Fatalf("%s at step %d of %s: %v", mode, after, name, err)
				}
				noticed = noticed || len(report.Failed) > 0
				f.mu.Lock()
				calls := f.requests
				f.mu.Unlock()
				// A connection that goes away is an answer no case can read,
				// so a case that reached that step has to report it. The run's
				// own cleanup calls one delete per object it made, and those
				// come after every case has finished.
				if mode == "gone" && len(report.Passed) > 0 && calls-len(report.Created) > after {
					t.Errorf("%s at step %d of %d and %s passed", mode, after, calls, name)
				}
			}
			// A case that notices nothing at any step is a case that asserts
			// nothing about what it read. Three read something else: the door
			// answers before anything this fake breaks, the two public
			// documents are outside the door, and the agent case reads a
			// binary rather than an answer.
			outside := map[string]string{
				"case006Unauthenticated": "the door answers before the body is written",
				"case008PublicDocuments": "the two documents are outside the API",
				"case011AgentScenario":   "the case reads a binary and not an answer",
			}
			if _, ok := outside[name]; ok {
				continue
			}
			if !noticed {
				t.Errorf("%s never made %s fail, so the case reads no answer it acts on", mode, name)
			}
		}
	}
}

// TestTheExchangeHelpersReadTheEnvelope: the pieces every case builds its
// verdict from, each over one answer.
func TestTheExchangeHelpersReadTheEnvelope(t *testing.T) {
	x := &exchange{Method: http.MethodGet, Path: "/v1/sandboxes", Status: http.StatusBadRequest,
		Body: []byte(`{"error":{"code":"invalid_field","message":"A field has a value it cannot take.","details":{"request_id":"req_1","paths":["metadata.name","spec.image"]}}}`)}
	if err := x.refusal("invalid_field"); err != nil {
		t.Fatalf("a refusal of the table was not read as one: %v", err)
	}
	if got := x.paths(); !slices.Equal(got, []string{"metadata.name", "spec.image"}) {
		t.Errorf("the paths are %v", got)
	}
	if err := x.refusal("not_a_code"); err == nil || !strings.Contains(err.Error(), "no row in the error table") {
		t.Errorf("a code outside the table was read as one: %v", err)
	}
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"no envelope", `not json`, "no envelope", 400},
		{"another code", `{"error":{"code":"not_found","message":"There is no such object.","details":{"request_id":"r"}}}`, "code invalid_field", 404},
		{"another status", `{"error":{"code":"invalid_field","message":"A field has a value it cannot take.","details":{"request_id":"r"}}}`, "status 400", 422},
		{"another sentence", `{"error":{"code":"invalid_field","message":"Nope.","details":{"request_id":"r"}}}`, "message A field", 400},
		{"no request id", `{"error":{"code":"invalid_field","message":"A field has a value it cannot take.","details":{}}}`, "request_id", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			y := &exchange{Method: http.MethodGet, Path: "/v1/sandboxes", Status: tc.status, Body: []byte(tc.body)}
			err := y.refusal("invalid_field")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the answer was read as a refusal of the table: %v", err)
			}
			if got := y.paths(); got != nil && len(got) != 0 {
				t.Errorf("paths from an answer that names none: %v", got)
			}
		})
	}
	if err := (&exchange{Status: 200}).status(200); err != nil {
		t.Errorf("a status that matches was read as a disagreement: %v", err)
	}
	if _, err := (&exchange{Body: []byte("{")}).object(); err == nil {
		t.Error("a body that is no object was decoded as one")
	}
}

// TestADisagreementNamesTheExchange: what a failed case prints is the
// request, what the specification says, and what arrived, with a long body
// cut rather than pasted whole.
func TestADisagreementNamesTheExchange(t *testing.T) {
	d := &Disagreement{Method: http.MethodPost, Path: "/v1/sandboxes", Want: "status 201", Got: "status 500",
		Body: strings.Repeat("x", 2000)}
	text := d.Error()
	for _, want := range []string{"POST", "/v1/sandboxes", "status 201", "status 500", "..."} {
		if !strings.Contains(text, want) {
			t.Errorf("the disagreement does not name %q: %s", want, text)
		}
	}
	if len(text) > 1200 {
		t.Errorf("the disagreement is %d bytes; a long body is cut", len(text))
	}
	short := &Disagreement{Method: http.MethodGet, Path: "/v1/sandboxes", Want: "a", Got: "b"}
	if strings.Contains(short.Error(), "body:") {
		t.Error("an exchange with no body prints a body line")
	}
}

// TestASkipCarriesItsReason: a skip is an error type of its own, so a case
// that cannot run says so and is never read as a pass.
func TestASkipCarriesItsReason(t *testing.T) {
	err := skipf("the environment does not declare %s", "dial")
	if !strings.Contains(err.Error(), "does not declare dial") {
		t.Errorf("the skip reads %q", err)
	}
	e := &Env{caps: map[string]bool{"files": true}}
	if err := e.need("files"); err != nil {
		t.Errorf("a declared capability was skipped: %v", err)
	}
	if err := e.need("dial"); err == nil || !strings.Contains(err.Error(), "dial") {
		t.Errorf("an undeclared capability was not skipped: %v", err)
	}
	empty := &Env{caps: map[string]bool{}}
	if err := empty.need("files"); err == nil || !strings.Contains(err.Error(), "Capabilities") {
		t.Errorf("a run with no declared set did not say so: %v", err)
	}
	if _, err := (&Env{}).second(); err == nil {
		t.Error("a case that needs a second subject ran without one")
	}
}

// TestKeysAreSorted: the helper that names what a map held, for a message
// that has to be the same on two runs.
func TestKeysAreSorted(t *testing.T) {
	got := keys(map[string]bool{"b": true, "a": true, "c": true})
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("the keys are %v", got)
	}
}

// TestTheControlContractIsOneRoute: the suite drives a stub through one
// route, and an endpoint that refuses the mode is an error the case reports
// rather than a mode it assumes took effect.
func TestTheControlContractIsOneRoute(t *testing.T) {
	f := newFake(t)
	e, err := newEnv(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := e.control(ctx, f.server.URL, "unavailable"); err != nil {
		t.Fatalf("the control route answered %v", err)
	}
	if err := e.control(ctx, f.server.URL+"/nothing", "unavailable"); err == nil {
		t.Error("a control endpoint that refused the mode was read as one that took it")
	}
}
