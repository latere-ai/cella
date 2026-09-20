// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake is a server that answers enough of the API for the suite to run
// against it, with one rule it gets wrong on request. It is how the suite's
// own harness is proven: a case that disagrees with a server has to be
// reported as a failure carrying the exchange, and a case that agrees has to
// pass, and neither can be shown against a server that is always right.
type fake struct {
	mu      sync.Mutex
	objects map[string]map[string]any
	names   map[string]string
	deleted []string
	counter int
	// wrongMessage answers a refusal with a sentence outside the table.
	wrongMessage bool
	server       *httptest.Server
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{objects: map[string]map[string]any{}, names: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		f.write(w, http.StatusOK, map[string]any{"version": "fake"})
	})
	mux.Handle("POST /v1/sandboxes", f.authenticated(f.create))
	mux.Handle("GET /v1/sandboxes", f.authenticated(f.list))
	mux.Handle("GET /v1/sandboxes/{id}", f.authenticated(f.read))
	mux.Handle("DELETE /v1/sandboxes/{id}", f.authenticated(f.remove))
	mux.Handle("/", f.authenticated(func(w http.ResponseWriter, _ *http.Request) {
		f.refuse(w, "not_found")
	}))
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fake) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// refuse answers the envelope of design 008, with the sentence of the table
// unless this fake was asked to answer another.
func (f *fake) refuse(w http.ResponseWriter, code string) {
	r := errorTable[code]
	message := r.Message
	if f.wrongMessage {
		message = "Something went wrong."
	}
	f.write(w, r.Status, map[string]any{"error": map[string]any{
		"code": code, "message": message,
		"details": map[string]any{"request_id": "req_fake", "detail": "the fake refused"},
	}})
}

// authenticated is the door of design 008: every route under /v1 needs a
// bearer, and one that is not this fake's is no bearer at all.
func (f *fake) authenticated(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-token" {
			f.refuse(w, "unauthenticated")
			return
		}
		next(w, r)
	})
}

// decode is the decoding order of design 003: the media type, the size, one
// document, the version, the kind, then the fields.
func (f *fake) decode(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	if r.Header.Get("Content-Type") != "application/json" {
		f.refuse(w, "unsupported_media_type")
		return nil, false
	}
	var body map[string]any
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	if err := decoder.Decode(&body); err != nil {
		f.refuse(w, "body_too_large")
		return nil, false
	}
	if decoder.More() {
		f.refuse(w, "multi_document")
		return nil, false
	}
	if version, _ := body["apiVersion"].(string); version != APIVersion {
		f.refuse(w, "unsupported_version")
		return nil, false
	}
	if kind, _ := body["kind"].(string); kind != "Sandbox" {
		f.refuse(w, "unsupported_kind")
		return nil, false
	}
	for key := range body {
		if !slices.Contains([]string{"apiVersion", "kind", "metadata", "spec", "status"}, key) {
			f.refuse(w, "unknown_field")
			return nil, false
		}
	}
	if metadata, ok := body["metadata"].(map[string]any); ok {
		for key := range metadata {
			if !slices.Contains([]string{"name", "labels"}, key) {
				f.refuse(w, "unknown_field")
				return nil, false
			}
		}
	}
	return body, true
}

func (f *fake) create(w http.ResponseWriter, r *http.Request) {
	body, ok := f.decode(w, r)
	if !ok {
		return
	}
	metadata, _ := body["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, taken := f.names[name]; taken && name != "" {
		f.refuse(w, "name_taken")
		return
	}
	f.counter++
	id := fmt.Sprintf("sbx_fake%022d", f.counter)
	if name == "" {
		name = id
	}
	if metadata == nil {
		metadata = map[string]any{}
		body["metadata"] = metadata
	}
	metadata["name"] = name
	body["status"] = map[string]any{
		"id": id, "owner": "fake", "environment": "default",
		"driver": "fake", "isolation": "none", "phase": "Running",
	}
	f.objects[id], f.names[name] = body, id
	f.write(w, http.StatusCreated, body)
}

func (f *fake) read(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.lookup(r.PathValue("id"))
	if !ok {
		f.refuse(w, "not_found")
		return
	}
	f.write(w, http.StatusOK, obj)
}

func (f *fake) remove(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := r.PathValue("id")
	obj, ok := f.lookup(ref)
	if !ok {
		f.refuse(w, "not_found")
		return
	}
	id, _ := obj["status"].(map[string]any)["id"].(string)
	f.deleted = append(f.deleted, id)
	delete(f.objects, id)
	obj["status"].(map[string]any)["phase"] = "Deleting"
	f.write(w, http.StatusAccepted, obj)
}

func (f *fake) list(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := []map[string]any{}
	for _, obj := range f.objects {
		items = append(items, obj)
	}
	f.write(w, http.StatusOK, map[string]any{"items": items, "next": ""})
}

// lookup resolves an id or a name. The caller holds the lock.
func (f *fake) lookup(ref string) (map[string]any, bool) {
	if obj, ok := f.objects[ref]; ok {
		return obj, true
	}
	if id, ok := f.names[ref]; ok {
		obj, ok := f.objects[id]
		return obj, ok
	}
	return nil, false
}

// config is a run against the fake with no capability and no control, which
// is the smallest configuration the suite takes. The case deadline is short
// on purpose: a case whose state the fake never reaches waits for its
// deadline by design, and these tests are about the harness and not about
// how long a case may wait.
func (f *fake) config() Config {
	return Config{URL: f.server.URL, Caller: "fake-token", Timeout: 500 * time.Millisecond}
}

// TestAWrongServerIsReportedFailed: a server that answers a refusal with a
// sentence outside the table fails the case that provoked it, and the report
// carries the request and the answer that disagreed.
func TestAWrongServerIsReportedFailed(t *testing.T) {
	f := newFake(t)
	f.wrongMessage = true
	report, err := Execute(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(report.Failed, "case008NameTaken") {
		t.Fatalf("the wrong sentence did not fail the case; failed: %v", report.Failed)
	}
	if report.OK() {
		t.Error("a report with a failed case is green")
	}
	var found *Result
	for i, res := range report.Results {
		if res.Name == "case008NameTaken" {
			found = &report.Results[i]
		}
	}
	var d *Disagreement
	if found == nil || !asDisagreement(found.Err, &d) {
		t.Fatalf("the failure carries no disagreement: %+v", found)
	}
	for _, want := range []string{"POST", "/v1/sandboxes", "message", "Something went wrong."} {
		if !strings.Contains(d.Error(), want) {
			t.Errorf("the disagreement does not name %q:\n%s", want, d.Error())
		}
	}
	if !strings.Contains(report.String(), "fail case008NameTaken"[:4]) {
		t.Error("the report does not print the failure")
	}
}

// TestAServerThatAgreesPasses: the same fake, answering the table, passes the
// cases it serves and fails none of them, so a failure above is the wrong
// answer and not the suite.
func TestAServerThatAgreesPasses(t *testing.T) {
	f := newFake(t)
	report, err := Execute(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"case008NameTaken", "case003DefaultsAreReturned", "case006Unauthenticated"} {
		if !slices.Contains(report.Passed, name) {
			t.Errorf("%s did not pass against a server that answers it", name)
		}
	}
}

// TestGroupsAndSkips: every group of design 015 exists, and a case whose
// capability or input the configuration does not carry is reported skipped
// with what is missing named, never silently and never as a pass.
func TestGroupsAndSkips(t *testing.T) {
	want := []string{
		"decode", "resolve", "lifecycle", "identity", "list", "streams", "errors",
		"events", "secrets", "volumes", "sets", "environments", "egress", "spawn",
		"agent", "computer use", "indistinguishability", "capability",
	}
	var got []string
	for _, group := range Groups() {
		got = append(got, group.Name)
		if len(group.Cases) == 0 {
			t.Errorf("the %s group holds no case", group.Name)
		}
		for _, c := range group.Cases {
			if c.Group != group.Name {
				t.Errorf("%s is filed under %s and listed in %s", c.Name, c.Group, group.Name)
			}
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("the groups are %v, want %v", got, want)
	}
	f := newFake(t)
	report, err := Execute(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	skips := map[string]string{
		"case008Dial":                  "dial",
		"case019VolumeLifecycle":       "volumes",
		"case020SetRunsToCompletion":   "QueuedEnvironment",
		"case021EnvironmentRead":       "Admin",
		"case011AgentScenario":         "Cella",
		"case023BrowserReady":          "DisplayImage",
		"case001Indistinguishable":     "WorkerEnvironment",
		"case006AuthorizerUnavailable": "AuthorizerControl",
		"case007AdmissionRefused":      "AdmissionControl",
		"case009DeliveredInOrder":      "SinkControl",
	}
	for name, input := range skips {
		if !slices.Contains(report.Skipped, name) {
			t.Errorf("%s did not skip where %s is empty", name, input)
			continue
		}
		if reason := report.Reasons[name]; !strings.Contains(reason, input) {
			t.Errorf("%s skipped with %q, which does not name %s", name, reason, input)
		}
	}
	// A case skipped by request names that, so a report says why a case did
	// not run whatever the reason was.
	cfg := f.config()
	cfg.Skip = []string{"decode", "case005StopAndStart"}
	report, err = Execute(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"case003MultiDocument", "case005StopAndStart"} {
		if !strings.Contains(report.Reasons[name], "by request") {
			t.Errorf("%s was not skipped by request: %q", name, report.Reasons[name])
		}
	}
}

// TestADeclaredGapIsNotAFailure: a server may declare the cases it fails,
// and the run stays green while the report names each one. A declaration
// that no longer describes a gap fails the run instead, so it cannot outlive
// what it describes.
func TestADeclaredGapIsNotAFailure(t *testing.T) {
	f := newFake(t)
	f.wrongMessage = true
	cfg := f.config()
	cfg.Known = map[string]string{"case008NameTaken": "the fake answers its own sentence"}
	report, err := Execute(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(report.Known, "case008NameTaken") {
		t.Fatalf("the declared case is not in the report's declarations: %v", report.Known)
	}
	if slices.Contains(report.Failed, "case008NameTaken") {
		t.Error("a declared gap is counted as a failure")
	}
	if report.Reasons["case008NameTaken"] == "" {
		t.Error("the report does not carry the reason the gap was declared with")
	}
	f.wrongMessage = false
	report, err = Execute(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(report.Undeclared, "case008NameTaken") {
		t.Fatal("a declared case that passed is not reported")
	}
	if report.OK() {
		t.Error("a declaration that has outlived its gap leaves the run green")
	}
	if !strings.Contains(report.String(), "declared as failing but passed") {
		t.Error("the report does not print the declaration that outlived its gap")
	}
}

// TestRunCleansUp: a run deletes every object it made and nothing else, and
// two runs against one server carry different prefixes, so neither reaches
// the other's objects.
func TestRunCleansUp(t *testing.T) {
	f := newFake(t)
	report, err := Execute(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) == 0 {
		t.Fatal("the run created nothing, so there is no cleanup to prove")
	}
	f.mu.Lock()
	left, deleted := len(f.objects), slices.Clone(f.deleted)
	f.mu.Unlock()
	if left != 0 {
		f.mu.Lock()
		names := keys(f.objects)
		f.mu.Unlock()
		t.Errorf("%d object(s) outlived the run: %v; created %v; deleted %v", left, names, report.Created, deleted)
	}
	for _, id := range report.Created {
		if !slices.Contains(deleted, id) {
			t.Errorf("%s was created and not deleted", id)
		}
	}
	for _, id := range deleted {
		if !slices.Contains(report.Created, id) {
			t.Errorf("%s was deleted and not created by this run", id)
		}
	}
	first, err := newEnv(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	second, err := newEnv(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	if first.name() == second.name() {
		t.Error("two runs name their objects alike, so one would reach the other's")
	}
}

// TestTheReportCarriesTheMarker: a report names the version the server
// reports and the suite's own, which is what makes two reports comparable.
func TestTheReportCarriesTheMarker(t *testing.T) {
	f := newFake(t)
	report, err := Execute(t.Context(), f.config())
	if err != nil {
		t.Fatal(err)
	}
	if report.Marker.Server != "fake" || report.Marker.Suite != Version {
		t.Errorf("the marker is %+v, want the server's version and %s", report.Marker, Version)
	}
	if !strings.Contains(report.String(), "conformance suite "+Version+" against server fake") {
		t.Errorf("the report does not open with the marker:\n%s", report.String())
	}
}

// TestASuiteWithNoServerRefusesToRun: a configuration that names no server
// or no caller is a usage error and never an empty green report.
func TestASuiteWithNoServerRefusesToRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"no URL", Config{Caller: "t"}, "no server"},
		{"no scheme", Config{URL: "cella.example.com", Caller: "t"}, "no http"},
		{"no caller", Config{URL: "http://127.0.0.1:1"}, "no caller"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Execute(t.Context(), tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the suite answered %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// TestSubtestNames: a case runs as <NNN>/<Name>, which is what a Go test run
// and the report agree on.
func TestSubtestNames(t *testing.T) {
	for name, want := range map[string]string{
		"case008ExecWait":   "008/ExecWait",
		"case001Indistinct": "001/Indistinct",
		"notACase":          "notACase",
		"case01":            "case01",
	} {
		if got := subtestName(name); got != want {
			t.Errorf("%s runs as %s, want %s", name, got, want)
		}
	}
}

// asDisagreement is errors.As without the import in every assertion.
func asDisagreement(err error, into **Disagreement) bool {
	d, ok := err.(*Disagreement)
	if ok {
		*into = d
	}
	return ok
}
