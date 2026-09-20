// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/internal/stubs"
	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/test/conformance"
)

// TestTheConformanceSuiteHoldsAgainstThisServer runs the suite of design 015
// against a `cellad serve` in this process, with the stubs of design 012
// beside it: the issuer that mints every subject, the authorizer and the
// admission endpoint behind a control the suite drives, and the sink that
// receives what the server delivered.
//
// It is untagged, so the bar runs it on every push: the suite is the
// contract, and a contract that is only checked in a tier is a contract this
// repository learns about late. The gaps this server has are declared in
// test/conformance/known.json, and the declaration is exact: a case that
// fails without being declared fails this test, and a declared case that
// passes fails it too.
func TestTheConformanceSuiteHoldsAgainstThisServer(t *testing.T) {
	stack := startStack(t)
	known, err := conformance.LoadDeclaration(filepath.Join("..", "..", "test", "conformance", "known.json"))
	if err != nil {
		t.Fatal(err)
	}
	report := conformance.Run(t, conformance.Config{
		URL:               stack.url,
		Token:             stack.mint,
		Admin:             "",
		Capabilities:      stack.capabilities,
		AuthorizerControl: stack.authorizer.URL,
		AdmissionControl:  stack.admission.URL,
		SinkControl:       stack.sink,
		Known:             known,
	})
	if report.Marker.Server == "" {
		t.Error("the report carries no server version; the marker of design 015 names both versions")
	}
	if len(report.Passed) == 0 {
		t.Fatal("no case passed, which is a suite that did not run")
	}
	// Every object the run made is deleted by the run itself, so what is left
	// on the server is what was there before it.
	for _, id := range report.Created {
		x, err := stack.read(t, "/v1/sandboxes/"+id)
		if err != nil {
			t.Fatal(err)
		}
		if x == http.StatusOK {
			t.Errorf("%s outlived the run", id)
		}
	}
}

// stack is the node and the stubs one suite run drives.
type stack struct {
	url          string
	issuer       string
	sink         string
	capabilities []string
	authorizer   *httptest.Server
	admission    *httptest.Server
}

// startStack brings up the stubs, the two control shims and `cellad serve`,
// and tears each down with the test.
func startStack(t *testing.T) *stack {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	const (
		authorizerToken = "stub-authorizer-token"
		admissionToken  = "stub-admission-token"
		eventsSecret    = "0123456789abcdef0123456789abcdef"
	)
	// The refusing admission endpoint is a second stub rather than a handler
	// of the test's own: what a refusal looks like is the stub's business,
	// and the control only chooses which stub answers.
	allowing, err := stubs.Start(ctx, stubs.Options{
		Issuer:     stubs.IssuerOptions{Addr: "127.0.0.1:0"},
		Authorizer: stubs.AuthorizerOptions{Addr: "127.0.0.1:0", Token: authorizerToken},
		Admission:  stubs.AdmissionOptions{Addr: "127.0.0.1:0", Token: admissionToken},
		Sink:       stubs.SinkOptions{Addr: "127.0.0.1:0", Secrets: []string{eventsSecret}},
	})
	if err != nil {
		t.Fatalf("the stubs did not start: %v", err)
	}
	t.Cleanup(func() { _ = allowing.Close(context.WithoutCancel(ctx)) })
	refusing, err := stubs.Start(ctx, stubs.Options{
		Authorizer: stubs.AuthorizerOptions{Addr: "127.0.0.1:0", Token: authorizerToken, Deny: []string{"sandbox.read", "sandbox.list"}},
		Admission:  stubs.AdmissionOptions{Addr: "127.0.0.1:0", Token: admissionToken, Refuse: "the conformance suite asked for a refusal"},
	})
	if err != nil {
		t.Fatalf("the refusing stubs did not start: %v", err)
	}
	t.Cleanup(func() { _ = refusing.Close(context.WithoutCancel(ctx)) })

	s := &stack{
		issuer: allowing.URL(stubs.RoleIssuer),
		sink:   allowing.URL(stubs.RoleSink),
	}
	s.authorizer = control(t, allowing.URL(stubs.RoleAuthorizer), refusing.URL(stubs.RoleAuthorizer))
	s.admission = control(t, allowing.URL(stubs.RoleAdmission), refusing.URL(stubs.RoleAdmission))

	driver, err := native.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	caps := driver.Capabilities()
	_ = driver.Close()
	for name, declared := range map[string]bool{
		"files": caps.Files, "attach": caps.Attach, "display": caps.Display,
		"input": caps.Input, "pool": caps.Pool,
	} {
		if declared {
			s.capabilities = append(s.capabilities, name)
		}
	}

	var out syncBuffer
	var errOut bytes.Buffer
	codec := make(chan int, 1)
	go func() {
		codec <- run(ctx, nil, env(map[string]string{
			"CELLA_DATA_DIR":            t.TempDir(),
			"CELLA_PUBLIC_ADDR":         "127.0.0.1:0",
			"CELLA_INTERNAL_ADDR":       "127.0.0.1:0",
			"CELLA_PUBLIC_URL":          "http://127.0.0.1:0",
			"CELLA_RUNTIME":             "native",
			"CELLA_ALLOW_UNSAFE_NATIVE": "true",
			"CELLA_TOKEN_KEY":           signingKeyPEM(t, 1),
			"CELLA_SECRET_KEY":          base64.StdEncoding.EncodeToString([]byte(eventsSecret)),
			"CELLA_OIDC_ISSUERS":        s.issuer,
			"CELLA_ADMIN_SUBJECTS":      s.issuer + "|admin",
			"CELLA_AUTHORIZER_URL":      s.authorizer.URL,
			"CELLA_AUTHORIZER_TOKEN":    authorizerToken,
			"CELLA_ADMISSION_URL":       s.admission.URL,
			"CELLA_ADMISSION_TOKEN":     admissionToken,
			"CELLA_EVENTS_URL":          s.sink,
			"CELLA_EVENTS_SECRET":       eventsSecret,
		}), &out, &errOut)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for s.url == "" {
		if m := listening.FindStringSubmatch(out.String()); m != nil {
			s.url = "http://" + m[1]
			break
		}
		select {
		case code := <-codec:
			t.Fatalf("serve exited %d before listening; stderr %q", code, errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never reported its listeners; stdout %q stderr %q", out.String(), errOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-codec:
		case <-time.After(gracePeriod + 10*time.Second):
			t.Error("serve did not stop")
		}
	})
	return s
}

// mint is the suite's Token: a subject's bearer from the stub issuer.
func (s *stack) mint(ctx context.Context, subject string) (string, error) {
	body, err := json.Marshal(map[string]string{"sub": subject})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.issuer+"/mint", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil {
		return "", err
	}
	if minted.Token == "" {
		return "", fmt.Errorf("the issuer minted nothing for %s", subject)
	}
	return minted.Token, nil
}

// read asks the server for one path with an administrator's bearer and
// returns the status, for the assertion that a run leaves nothing behind.
func (s *stack) read(t *testing.T, path string) (int, error) {
	t.Helper()
	token, err := s.mint(t.Context(), "admin")
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.url+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// control is the contract the suite drives a stub with: POST /fail with a
// mode, and every other request proxied to the stub that answers it. The
// modes are the stub's own behaviours, chosen by which upstream answers:
// nothing for the allowing stub, a refusal from the refusing one, and 503
// from the control itself where the mode is an outage.
//
// It lives here and not in `cella-stubs` because the flags of design 012 are
// read at start-up; a control endpoint in the binary is that spec's.
func control(t *testing.T, allow, refuse string) *httptest.Server {
	t.Helper()
	allowing := proxy(t, allow)
	refusing := proxy(t, refuse)
	var mu sync.RWMutex
	mode := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/fail" {
			var asked struct {
				Mode string `json:"mode"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&asked); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			mode = asked.Mode
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mu.RLock()
		current := mode
		mu.RUnlock()
		switch {
		case current == "":
			allowing.ServeHTTP(w, r)
		case current == "unavailable":
			http.Error(w, "the stub is unavailable by request", http.StatusServiceUnavailable)
		case current == "refuse" || strings.HasPrefix(current, "deny:"):
			refusing.ServeHTTP(w, r)
		default:
			http.Error(w, "unknown mode "+current, http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// proxy forwards to one stub.
func proxy(t *testing.T, target string) *httputil.ReverseProxy {
	t.Helper()
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	return httputil.NewSingleHostReverseProxy(parsed)
}
