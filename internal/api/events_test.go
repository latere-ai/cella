// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/auth"
	"latere.ai/x/cella/internal/events"
	"latere.ai/x/cella/internal/store"
	"latere.ai/x/cella/internal/store/memory"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
)

// recorded is a fixture whose API journals records, with the journal the test
// reads them back from.
type recorded struct {
	*fixture
	store store.Store
}

// setupRecorded is setup with design 009's emitter wired in. It repeats the
// fixture rather than changing it, so a test about records is not also a
// change to every other test's wiring.
func setupRecorded(t *testing.T) *recorded { return setupRecordedDriver(t, nil) }

// setupRecordedDriver is setupRecorded over a driver a case wraps, for a route
// whose records depend on a capability the native driver does not have.
func setupRecordedDriver(t *testing.T, wrap func(runtime.Driver) runtime.Driver) *recorded {
	t.Helper()
	issuer := issuertest.New(t, issuertest.WithDefaultAudience("cella"))
	verifier, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{
		Issuers: []string{issuer.URL()}, Audience: "cella",
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	var runtimeDriver runtime.Driver = d
	if wrap != nil {
		runtimeDriver = wrap(d)
	}
	s, err := memory.Open(memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	emitter := events.NewEmitter(store.EventJournal(s, store.Delivered), slog.New(slog.DiscardHandler))
	c, err := controller.Open(controller.Options{
		DataDir: t.TempDir(), Driver: runtimeDriver, Environment: "default", Events: emitter,
		TouchInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	h, err := New(Options{
		Controller: c, Verifier: verifier,
		Authorizer: auth.NewAuthorizer(&auth.OwnerPolicy{DefaultEnvironment: "default"}),
		Events:     emitter, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return &recorded{fixture: &fixture{
		t: t, url: server.URL, issuerURL: issuer.URL(),
		alice: issuer.Mint(issuertest.Claims{Sub: "alice"}),
		bob:   issuer.Mint(issuertest.Claims{Sub: "bob"}), h: h, c: c,
	}, store: s}
}

// records reads one sandbox's journal, oldest first.
func (r *recorded) records(id string) []events.Record {
	r.t.Helper()
	var out []events.Record
	if err := r.store.Tx(r.t.Context(), func(tx store.Tx) error {
		rows, _, err := tx.Journal().ByObject(r.t.Context(), id, store.Page{})
		if err != nil {
			return err
		}
		for _, row := range slices.Backward(rows) {
			record, err := events.Rebuild(row.Payload, row.ID, row.Seq, row.Type, row.At)
			if err != nil {
				return err
			}
			out = append(out, record)
		}
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// TestOperationsAreRecorded: an exec and both directions of a transfer each
// produce one record that names the operation, the request that asked for it,
// and nothing it carried.
func TestOperationsAreRecorded(t *testing.T) {
	f := setupRecorded(t)
	var obj struct {
		Status struct{ ID string } `json:"status"`
	}
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj); err != nil {
		t.Fatal(err)
	}
	base := "/v1/sandboxes/" + obj.Status.ID
	f.upload(base+"/files?dest=/workspace", f.alice, "application/x-tar",
		archive(t, "canary.txt", "a-file-body-nobody-should-see"), 204)
	f.request("POST", base+"/exec?wait=1", f.alice,
		`{"command":["sh","-c","printf a-command-nobody-should-see; exit 3"]}`, 200)
	f.request("GET", base+"/files?path=/workspace/canary.txt", f.alice, "", 200)
	// A log read is a read, and design 009 names no type for one.
	f.request("GET", base+"/logs", f.alice, "", 200)

	var seen []events.Record
	for _, r := range f.records(obj.Status.ID) {
		if r.Type == events.TypeExec || r.Type == events.TypeFiles {
			seen = append(seen, r)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("the journal holds %d operation record(s): %+v", len(seen), seen)
	}
	want := []struct {
		kind events.Type
		data string
	}{
		{events.TypeFiles, `"direction":"import"`},
		{events.TypeExec, `"exitCode":3`},
		{events.TypeFiles, `"direction":"export"`},
	}
	for i, r := range seen {
		if r.Type != want[i].kind || !strings.Contains(string(r.Data), want[i].data) {
			t.Errorf("record %d is %s %s, want %s carrying %s", i, r.Type, r.Data, want[i].kind, want[i].data)
		}
		if r.Sandbox == nil || r.Sandbox.ID != obj.Status.ID {
			t.Errorf("record %d names no sandbox in context: %+v", i, r)
		}
		if r.Subject != f.issuerURL+"|alice" {
			t.Errorf("record %d names the subject %q", i, r.Subject)
		}
		if r.RequestID == "" {
			t.Errorf("record %d carries no request id", i)
		}
		if r.Seq == 0 {
			t.Errorf("record %d took no sequence", i)
		}
	}
	// The paths a transfer names are workspace paths and not content.
	var files events.Files
	if err := json.Unmarshal(seen[2].Data, &files); err != nil {
		t.Fatal(err)
	}
	if len(files.Paths) != 1 || files.Paths[0] != "/workspace/canary.txt" || files.Bytes == 0 {
		t.Errorf("the export record is %+v", files)
	}
}

// TestNoContentInOperationRecords: the three canaries of design 009 reach no
// record of a whole session, whichever route carried them.
func TestNoContentInOperationRecords(t *testing.T) {
	const (
		command = "a-command-nobody-should-see"
		body    = "a-file-body-nobody-should-see"
		secret  = "sk-ant-avaluenobodyshouldsee0000"
	)
	f := setupRecorded(t)
	created := strings.Replace(createBody, `"spec":{}`,
		`"spec":{"env":{"API_TOKEN":"`+secret+`"}}`, 1)
	var obj struct {
		Status struct{ ID string } `json:"status"`
	}
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, created, 201), &obj); err != nil {
		t.Fatalf("%v; body was %s", err, created)
	}
	base := "/v1/sandboxes/" + obj.Status.ID
	f.upload(base+"/files?dest=/workspace", f.alice, "application/x-tar", archive(t, "canary.txt", body), 204)
	f.request("POST", base+"/exec?wait=1", f.alice, `{"command":["sh","-c","printf `+command+`"]}`, 200)
	f.request("GET", base+"/files?path=/workspace/canary.txt", f.alice, "", 200)
	f.request("POST", base+"/stop", f.alice, "", 200)
	f.request("DELETE", base, f.alice, "", 202)

	records := f.records(obj.Status.ID)
	if len(records) == 0 {
		t.Fatal("the session produced no record at all")
	}
	for _, r := range records {
		encoded, err := events.Body(r)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{command, body, secret} {
			if strings.Contains(string(encoded), canary) {
				t.Errorf("%s carries a canary: %s", r.Type, encoded)
			}
		}
	}
}

// TestRecordsAreOffWithoutAnEmitter: the routes work with no journal wired,
// which is what a process with no sink and no store runs as.
func TestRecordsAreOffWithoutAnEmitter(t *testing.T) {
	f := setup(t, nil)
	var obj struct {
		Status struct{ ID string } `json:"status"`
	}
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj); err != nil {
		t.Fatal(err)
	}
	f.request("POST", "/v1/sandboxes/"+obj.Status.ID+"/exec?wait=1", f.alice, `{"command":["true"]}`, 200)
}

// TestActorOfReadsTheBearerAndTheRequest: a workload token names its sandbox
// and a person names none.
func TestActorOfReadsTheBearerAndTheRequest(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-ID", "req_9")
	person := actorOf(w, auth.Caller{Subject: "alice"})
	if person.Subject != "alice" || person.Workload != "" || person.RequestID != "req_9" {
		t.Errorf("a person's actor is %+v", person)
	}
	load := actorOf(w, auth.Caller{Subject: "sandbox:sbx_a", Sub: auth.SandboxPrefix + "sbx_a", Minted: true})
	if load.Workload != "sbx_a" {
		t.Errorf("a workload's actor is %+v", load)
	}
}
