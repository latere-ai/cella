// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

// output is what the plane prints while the test reads it.
type output struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}

// start runs the plane on a port the kernel picks and answers its address.
func start(t *testing.T, cfg config) string {
	t.Helper()
	cfg.Addr, cfg.Root = "127.0.0.1:0", t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	var out output
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, &out) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the plane exited with %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("the plane did not shut down")
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, address, found := strings.Cut(out.String(), "plane listening on "); found {
			return "http://" + strings.TrimSpace(strings.SplitN(address, "\n", 2)[0])
		}
		select {
		case err := <-done:
			t.Fatalf("the plane exited before listening: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the plane never printed its address: %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// call is one request to the plane, with the subject as the basic-auth user
// unless it is empty.
func call(t *testing.T, base, method, path, subject, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if subject != "" {
		req.SetBasicAuth(subject, "any-password")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(answer)
}

const manifestBody = `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","spec":{"command":["/bin/sh","-c","sleep 30"]}}`

// TestThePlaneServesItsOwnAPI drives the example the way its reader will:
// create, read and delete a sandbox through a platform's own routes, with
// the platform's own identity in front and its own plan behind.
func TestThePlaneServesItsOwnAPI(t *testing.T) {
	base := start(t, config{Image: "registry.example/base:1"})

	if status, body := call(t, base, http.MethodPost, "/sandboxes", "", manifestBody); status != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated create answered %d %s", status, body)
	}
	status, body := call(t, base, http.MethodPost, "/sandboxes", "alice@elsewhere.example", manifestBody)
	if status != http.StatusCreated {
		t.Fatalf("the create answered %d %s", status, body)
	}
	var created struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Image string `json:"image"`
		} `json:"spec"`
		Status struct {
			ID    string `json:"id"`
			Owner string `json:"owner"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	switch {
	case created.Status.ID == "":
		t.Fatalf("the create answered %s", body)
	case created.Status.Owner != "alice@elsewhere.example":
		t.Fatalf("the sandbox is owned by %q, want the subject the plane authenticated", created.Status.Owner)
	case created.Spec.Image != "":
		t.Fatalf("the image is %q, and this plane's environment runs none", created.Spec.Image)
	case created.Metadata.Labels["plane.example.com/account"] != "alice-at-elsewhere.example":
		t.Fatalf("the admission step stamped %v", created.Metadata.Labels)
	}

	if status, body = call(t, base, http.MethodGet, "/sandboxes/"+created.Status.ID, "alice@elsewhere.example", ""); status != http.StatusOK {
		t.Fatalf("the read answered %d %s", status, body)
	}
	// One subject's object is another subject's absence.
	if status, body = call(t, base, http.MethodGet, "/sandboxes/"+created.Status.ID, "bob@elsewhere.example", ""); status != http.StatusNotFound {
		t.Fatalf("another subject's read answered %d %s", status, body)
	}
	if status, body = call(t, base, http.MethodDelete, "/sandboxes/"+created.Status.ID, "alice@elsewhere.example", ""); status != http.StatusAccepted {
		t.Fatalf("the delete answered %d %s", status, body)
	}
	if status, _ = call(t, base, http.MethodGet, "/sandboxes/sbx_00000000000000000000000000", "alice@elsewhere.example", ""); status != http.StatusNotFound {
		t.Fatalf("an unknown id answered %d", status)
	}
	if status, _ = call(t, base, http.MethodDelete, "/sandboxes/sbx_00000000000000000000000000", "alice@elsewhere.example", ""); status != http.StatusNotFound {
		t.Fatalf("a delete of an unknown id answered %d", status)
	}
	// Every route asks who is calling before it asks the core anything.
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if status, _ = call(t, base, method, "/sandboxes/"+created.Status.ID, "", ""); status != http.StatusUnauthorized {
			t.Fatalf("an unauthenticated %s answered %d", method, status)
		}
	}
	// And another subject's delete is the same absence its read is.
	second := start(t, config{Image: "registry.example/base:1"})
	status, body = call(t, second, http.MethodPost, "/sandboxes", "alice@example.com", manifestBody)
	if status != http.StatusCreated {
		t.Fatalf("the create answered %d %s", status, body)
	}
	var other struct {
		Status struct {
			ID string `json:"id"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &other); err != nil {
		t.Fatal(err)
	}
	if status, body = call(t, second, http.MethodDelete, "/sandboxes/"+other.Status.ID, "bob@example.com", ""); status != http.StatusNotFound {
		t.Fatalf("another subject's delete answered %d %s", status, body)
	}
}

// TestThePlanesOwnRulesRefuse holds the three places a platform fills to
// their answers: the schema's refusal, the catalogue's, and the plan's.
func TestThePlanesOwnRulesRefuse(t *testing.T) {
	base := start(t, config{Image: "registry.example/base:1"})
	for _, tc := range []struct {
		name, subject, body string
		status              int
		carries             string
	}{
		{
			name: "a field the schema does not have", subject: "alice@elsewhere.example",
			body:    `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","spec":{"nonesuch":1}}`,
			status:  http.StatusUnprocessableEntity,
			carries: "unknown_field",
		},
		{
			name: "a value the schema refuses", subject: "alice@elsewhere.example",
			body:    `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"Not A Name"}}`,
			status:  http.StatusUnprocessableEntity,
			carries: "invalid_field",
		},
		{
			name: "an image where the environment runs none", subject: "alice@elsewhere.example",
			body:    `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","spec":{"image":"docker.io/library/alpine"}}`,
			status:  http.StatusUnprocessableEntity,
			carries: "capability_unsupported",
		},
		{
			name: "a life longer than the plan", subject: "alice@elsewhere.example",
			body:    `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","spec":{"lifecycle":{"ttl":"48h"}}}`,
			status:  http.StatusForbidden,
			carries: "ceiling_exceeded",
		},
		{
			name: "a life the larger plan allows", subject: "alice@example.com",
			body:   `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","spec":{"lifecycle":{"ttl":"48h"},"command":["/bin/sh","-c","sleep 30"]}}`,
			status: http.StatusCreated,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := call(t, base, http.MethodPost, "/sandboxes", tc.subject, tc.body)
			if status != tc.status {
				t.Fatalf("answered %d %s, want %d", status, body, tc.status)
			}
			if tc.carries != "" && !strings.Contains(body, tc.carries) {
				t.Fatalf("the answer is %s, want %s in it", body, tc.carries)
			}
		})
	}
}

// TestTheCatalogueIsTheAdmissionStep drives the seam a platform fills
// directly, on an environment that runs images: the catalogue supplies what
// a caller left out, refuses what is not in it, and says when it cannot
// decide at all, which is a 503 and never a no.
func TestTheCatalogueIsTheAdmissionStep(t *testing.T) {
	running := &v1.Environment{Status: v1.EnvironmentStatus{Isolation: v1.IsolationContainer}}
	request := manifest.AdmitRequest{
		Actor:       manifest.Actor{Sub: "alice@elsewhere.example"},
		Environment: running,
	}
	p := &plane{cfg: config{Image: "registry.example/base:1"}}
	out, _, err := p.catalogue(t.Context(), &v1.Sandbox{}, request)
	if err != nil {
		t.Fatalf("the catalogue refused a manifest that names no image: %v", err)
	}
	if out.Spec.Image != "registry.example/base:1" {
		t.Fatalf("the catalogue supplied %q", out.Spec.Image)
	}
	elsewhere := &v1.Sandbox{Spec: v1.SandboxSpec{Image: "docker.io/library/alpine"}}
	if _, _, err = p.catalogue(t.Context(), elsewhere, request); err == nil {
		t.Fatal("the catalogue took an image that is not in it")
	} else if code := (&manifest.Error{}); errors.As(err, &code) {
		t.Fatalf("a policy refusal carried the code %s, and a plain error is what the core reads as a refusal", code.Code)
	}
	var known *manifest.Error
	_, _, err = (&plane{}).catalogue(t.Context(), &v1.Sandbox{}, request)
	if !errors.As(err, &known) || known.Code != manifest.CodeAdmissionUnavailable {
		t.Fatalf("a plane with no catalogue answered %v, want no decision at all", err)
	}
	// Where the environment runs no image, the catalogue has nothing to say
	// and the contract's own rule is what refuses one.
	native := manifest.NativeEnvironment("default")
	out, _, err = p.catalogue(t.Context(), &v1.Sandbox{}, manifest.AdmitRequest{Environment: &native})
	if err != nil || out.Spec.Image != "" {
		t.Fatalf("the catalogue wrote %q on an environment that runs none: %v", out.Spec.Image, err)
	}
}

// TestTheConfigurationFallsBack is the one rule the environment reading has.
func TestTheConfigurationFallsBack(t *testing.T) {
	t.Setenv("PLANE_TEST_VALUE", "set")
	if got := value("PLANE_TEST_VALUE", "fallback"); got != "set" {
		t.Fatalf("value = %q", got)
	}
	if got := value("PLANE_TEST_ABSENT", "fallback"); got != "fallback" {
		t.Fatalf("value = %q", got)
	}
}

// TestTheRootIsWhereTheStateGoes: a plane that cannot open its own state
// directory says so and does not serve.
func TestTheRootIsWhereTheStateGoes(t *testing.T) {
	err := run(t.Context(), config{Addr: "127.0.0.1:0", Root: "/dev/null/plane"}, io.Discard)
	if err == nil {
		t.Fatal("a plane opened a runtime under an unusable path")
	}
	if !strings.Contains(err.Error(), "runtime") && !strings.Contains(err.Error(), "controller") {
		t.Fatalf("the failure is %v, want the runtime or the controller named", err)
	}
}

// TestTheAddressMustBeFree: an address already taken is a start-up failure
// and not a silent one.
func TestTheAddressMustBeFree(t *testing.T) {
	base := start(t, config{Image: "registry.example/base:1"})
	taken := strings.TrimPrefix(base, "http://")
	err := run(t.Context(), config{Addr: taken, Root: t.TempDir(), Image: "registry.example/base:1"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "listening on") {
		t.Fatalf("a second plane on %s failed with %v", taken, err)
	}
}

// TestEveryFailureHasAnAnswer holds the platform's own error mapping: the
// contract's codes and the controller's sentinels each become one status,
// and anything else is the platform's fault and not the caller's.
func TestEveryFailureHasAnAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"a field the schema refuses", &manifest.Error{Code: "invalid_field", Path: "spec.user"}, http.StatusUnprocessableEntity},
		{"a reference the actor cannot see", &manifest.Error{Code: "not_found", Path: "spec.secrets[0].name"}, http.StatusNotFound},
		{"a ceiling", &manifest.Error{Code: "ceiling_exceeded", Path: "spec.lifecycle.ttl"}, http.StatusForbidden},
		{"a step that could not decide", &manifest.Error{Code: manifest.CodeAdmissionUnavailable}, http.StatusServiceUnavailable},
		{"a sandbox that is not there", controller.ErrNotFound, http.StatusNotFound},
		{"a name in use", controller.ErrNameTaken, http.StatusConflict},
		{"a plan that is full", controller.ErrQuota, http.StatusForbidden},
		{"a phase that does not allow it", controller.ErrPhase, http.StatusConflict},
		{"anything else", errors.New("the disk is gone"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			refuse(w, tc.err)
			if w.Code != tc.status {
				t.Fatalf("answered %d %s, want %d", w.Code, w.Body, tc.status)
			}
			if tc.status == http.StatusInternalServerError && strings.Contains(w.Body.String(), "disk is gone") {
				t.Fatalf("the caller was told the platform's own detail: %s", w.Body)
			}
		})
	}
}

// TestOneNameIsOneSandbox: the core keeps one live name per owner, and the
// plane renders that as a conflict rather than a second sandbox.
func TestOneNameIsOneSandbox(t *testing.T) {
	base := start(t, config{Image: "registry.example/base:1"})
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"only-one"},` +
		`"spec":{"command":["/bin/sh","-c","sleep 30"]}}`
	if status, answer := call(t, base, http.MethodPost, "/sandboxes", "alice@example.com", body); status != http.StatusCreated {
		t.Fatalf("the first create answered %d %s", status, answer)
	}
	if status, answer := call(t, base, http.MethodPost, "/sandboxes", "alice@example.com", body); status != http.StatusConflict {
		t.Fatalf("the second create answered %d %s", status, answer)
	}
}

// TestThePlanIsACeilingOnCount: the core counts and the platform decides
// what the count may be, which is one number handed to Create.
func TestThePlanIsACeilingOnCount(t *testing.T) {
	base := start(t, config{Image: "registry.example/base:1"})
	subject := "carol@elsewhere.example" // the smaller plan: three sandboxes
	for i := range ceilings(subject).Sandboxes {
		if status, answer := call(t, base, http.MethodPost, "/sandboxes", subject, manifestBody); status != http.StatusCreated {
			t.Fatalf("create %d answered %d %s", i, status, answer)
		}
	}
	status, answer := call(t, base, http.MethodPost, "/sandboxes", subject, manifestBody)
	if status != http.StatusForbidden {
		t.Fatalf("the create past the plan answered %d %s", status, answer)
	}
}
