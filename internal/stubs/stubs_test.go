// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package stubs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/internal/stubs"
)

// buffer is a log a test reads while the roles are still serving.
type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// loopback is every role on a port the kernel chooses, which is what lets
// two tests run at once and what the tiers bind.
func loopback(o stubs.Options) stubs.Options {
	if o.Issuer.Addr == "" {
		o.Issuer.Addr = "127.0.0.1:0"
	}
	if o.Authorizer.Addr == "" {
		o.Authorizer.Addr = "127.0.0.1:0"
	}
	if o.Admission.Addr == "" {
		o.Admission.Addr = "127.0.0.1:0"
	}
	if o.Sink.Addr == "" {
		o.Sink.Addr = "127.0.0.1:0"
	}
	if len(o.Sink.Secrets) == 0 {
		o.Sink.Secrets = []string{stubs.DefaultSinkSecret}
	}
	return o
}

// start runs the roles for one test and stops them with it.
func start(t *testing.T, o stubs.Options) *stubs.Stubs {
	t.Helper()
	s, err := stubs.Start(loopback(o))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// post sends one JSON body and returns the status and the body read back.
func post(t *testing.T, url string, body any, header map[string]string) (int, []byte) {
	t.Helper()
	var payload []byte
	switch v := body.(type) {
	case nil:
	case string:
		payload = []byte(v)
	case []byte:
		payload = v
	default:
		var err error
		if payload, err = json.Marshal(v); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	read, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp.StatusCode, read
}

// get reads one URL and returns the status and the body.
func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	read, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp.StatusCode, read
}

// TestEveryRoleListensAndIsLogged: the four roles answer on the addresses
// Start resolved, and each request leaves one line naming its role.
func TestEveryRoleListensAndIsLogged(t *testing.T) {
	log := &buffer{}
	s := start(t, stubs.Options{Log: log})
	for _, role := range stubs.RoleOrder {
		if s.URL(role) == "" {
			t.Fatalf("the %s role was not started", role)
		}
		if !strings.HasPrefix(s.URL(role), "http://127.0.0.1:") {
			t.Errorf("the %s role listens at %s, and a stub binds loopback", role, s.URL(role))
		}
	}
	if code, _ := get(t, s.URL(stubs.RoleIssuer)+"/.well-known/openid-configuration"); code != http.StatusOK {
		t.Fatalf("the issuer's discovery document answered %d", code)
	}
	if code, _ := get(t, s.URL(stubs.RoleSink)+"/events"); code != http.StatusOK {
		t.Fatalf("the sink's feed answered %d", code)
	}
	for _, want := range []string{"issuer GET /.well-known/openid-configuration 200", "sink GET /events 200"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log holds %q and not the line %q", log.String(), want)
		}
	}
}

// TestARoleWithNoAddressIsNotStarted: a tier that needs one endpoint runs
// one listener, and the roles it turned off answer nowhere.
func TestARoleWithNoAddressIsNotStarted(t *testing.T) {
	s, err := stubs.Start(stubs.Options{Sink: stubs.SinkOptions{Addr: "127.0.0.1:0", Secrets: []string{"one"}}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	if s.URL(stubs.RoleSink) == "" {
		t.Fatal("the sink was not started")
	}
	for _, role := range []stubs.Role{stubs.RoleIssuer, stubs.RoleAuthorizer, stubs.RoleAdmission} {
		if got := s.URL(role); got != "" {
			t.Errorf("the %s role was not asked for and listens at %s", role, got)
		}
		if got := s.Addr(role); got != "" {
			t.Errorf("the %s role reports the address %s", role, got)
		}
	}
}

// TestStartRefusesWhatItCannotServe: every option that cannot produce a
// role is a failure to start and never a role that answers wrongly.
func TestStartRefusesWhatItCannotServe(t *testing.T) {
	for name, o := range map[string]stubs.Options{
		"no role at all":       {},
		"an unknown algorithm": {Issuer: stubs.IssuerOptions{Addr: "127.0.0.1:0", Algorithm: "hs256"}},
		"limits that are not JSON": {Authorizer: stubs.AuthorizerOptions{
			Addr: "127.0.0.1:0", Limits: "{not json",
		}},
		"a filter that is not JSON": {Authorizer: stubs.AuthorizerOptions{
			Addr: "127.0.0.1:0", Filter: "[",
		}},
		"an unknown authorizer failure mode": {Authorizer: stubs.AuthorizerOptions{
			Addr: "127.0.0.1:0", Fail: "explode",
		}},
		"an unknown admission failure mode": {Admission: stubs.AdmissionOptions{
			Addr: "127.0.0.1:0", Fail: "status:nine",
		}},
		"a sink with no secret": {Sink: stubs.SinkOptions{Addr: "127.0.0.1:0"}},
		"an address in use":     {Issuer: stubs.IssuerOptions{Addr: "256.0.0.1:1"}},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := stubs.Start(o)
			if err == nil {
				_ = s.Close(context.Background())
				t.Fatal("the options were accepted, and a stub that cannot answer must not start")
			}
		})
	}
}

// TestCloseTwiceIsOneStop: a binary that defers Close and calls it on a
// signal calls it twice, and the second is not an error.
func TestCloseTwiceIsOneStop(t *testing.T) {
	s, err := stubs.Start(loopback(stubs.Options{}))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("the first Close: %v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("the second Close: %v", err)
	}
}

// TestTheTimeoutModeIsReleasedByClose: a role told to answer nothing
// parks the request, and closing the process releases it rather than
// leaking the goroutine that serves it.
func TestTheTimeoutModeIsReleasedByClose(t *testing.T) {
	s, err := stubs.Start(loopback(stubs.Options{
		Admission: stubs.AdmissionOptions{Addr: "127.0.0.1:0", Fail: stubs.FailTimeout},
	}))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	url := s.URL(stubs.RoleAdmission)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{}`))
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	// The request is on its way before the close, so the close is what
	// releases it.
	time.Sleep(50 * time.Millisecond)
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the parked request outlived the close")
	}
}
