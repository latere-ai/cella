// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
)

const bearer = "admission-token"

// endpointRequest is the envelope as the endpoint of spec 007 decodes it,
// copied field for field from the type the platform's own webhook
// declares so that this repository drives the shape that endpoint reads
// and never imports it. testdata/endpoint-request.json is that webhook's
// own test fixture, copied the same way.
//
// The decode is deliberately not strict: an endpoint ignores a member a
// later core adds rather than failing an apply, and a test that refused
// one would be stricter than the contract.
type endpointRequest struct {
	Subject     string          `json:"subject"`
	Issuer      string          `json:"issuer"`
	Sub         string          `json:"sub"`
	Claims      map[string]any  `json:"claims"`
	Workload    json.RawMessage `json:"workload"`
	Action      string          `json:"action"`
	Existing    *map[string]any `json:"existing"`
	Parent      *map[string]any `json:"parent"`
	Environment json.RawMessage `json:"environment"`
	Set         json.RawMessage `json:"set"`
	Manifest    map[string]any  `json:"manifest"`
	RequestRef  struct {
		ID string `json:"id"`
	} `json:"request"`
}

// endpoint is an httptest server standing in for the operator's own. It
// counts the requests it received, so a failure mode proves that nothing
// was resent.
type endpoint struct {
	*httptest.Server
	calls  atomic.Int64
	bodies chan []byte
	answer func(w http.ResponseWriter, body []byte)
}

func serve(t *testing.T, answer func(w http.ResponseWriter, body []byte)) *endpoint {
	t.Helper()
	e := &endpoint{answer: answer, bodies: make(chan []byte, 16)}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		select {
		case e.bodies <- body:
		default:
		}
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		e.answer(w, body)
	}))
	t.Cleanup(e.Close)
	return e
}

// allow writes a 200 carrying the manifest it was given, with mutate
// applied to it as a decoded map. It is how the platform's endpoint
// works: named fields are written into the document it received, so a
// field it does not read survives byte for byte.
func allow(t *testing.T, mutate func(map[string]any), warnings ...string) func(http.ResponseWriter, []byte) {
	t.Helper()
	return func(w http.ResponseWriter, body []byte) {
		var in endpointRequest
		if err := json.Unmarshal(body, &in); err != nil {
			t.Error(err)
			return
		}
		if mutate != nil {
			mutate(in.Manifest)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"allow": true, "manifest": in.Manifest, "warnings": warnings,
		})
	}
}

func fixed(status int, body string) func(http.ResponseWriter, []byte) {
	return func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func client(t *testing.T, e *endpoint, o Options) *Client {
	t.Helper()
	if o.URL == "" {
		o.URL = e.URL
	}
	if o.Token == "" {
		o.Token = bearer
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sandbox() *v1.Sandbox {
	return &v1.Sandbox{
		APIVersion: v1.APIVersion, Kind: "Sandbox",
		Metadata: v1.Metadata{Name: "dev", Labels: map[string]string{"team": "research"}},
		Spec: v1.SandboxSpec{
			Environment: "default", Image: "base",
			Command: []string{"/bin/bash", "-l"}, Workdir: "/workspace",
			Env:       map[string]string{"LOG_LEVEL": "debug"},
			Workspace: v1.Workspace{Path: "/workspace", Source: v1.WorkspaceSourceEmpty},
			Network:   v1.Network{Egress: v1.Egress{Mode: v1.EgressOpen, DeniedHosts: []string{"evil.example"}}},
		},
	}
}

func admitRequest() manifest.AdmitRequest {
	return manifest.AdmitRequest{
		Actor: manifest.Actor{
			Subject: "https://auth.example|alice", Issuer: "https://auth.example", Sub: "alice",
		},
		Claims: map[string]any{"org_id": "org-one", "roles": []any{"member"}, "principal_type": "user"},
		Action: "create",
		Environment: &v1.Environment{
			APIVersion: v1.APIVersion, Kind: "Environment",
			Metadata: v1.Metadata{Name: "default"},
			Spec:     v1.EnvironmentSpec{Isolation: v1.IsolationContainer},
			Status: v1.EnvironmentStatus{
				ID: "env_01J9", Driver: "k8s", Isolation: v1.IsolationContainer,
				Capabilities: v1.Capabilities{Egress: []v1.EgressMode{v1.EgressOpen, v1.EgressAllowlist}},
			},
		},
		RequestID: "req_01J9",
	}
}

// TestEnvelopeMatchesTheEndpoint drives the body this client sends into
// the type the operator's endpoint decodes with, and holds it beside that
// endpoint's own fixture: every member of the fixture is a member the
// client sends, with the same type and the same meaning.
func TestEnvelopeMatchesTheEndpoint(t *testing.T) {
	e := serve(t, allow(t, nil))
	if _, _, err := client(t, e, Options{}).Admit(t.Context(), sandbox(), admitRequest()); err != nil {
		t.Fatal(err)
	}
	sent := <-e.bodies

	var got, want endpointRequest
	if err := json.Unmarshal(sent, &got); err != nil {
		t.Fatalf("the endpoint could not decode the body: %v", err)
	}
	fixture, err := os.ReadFile("testdata/endpoint-request.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(fixture, &want); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"subject", got.Subject, want.Subject},
		{"issuer", got.Issuer, want.Issuer},
		{"sub", got.Sub, want.Sub},
		{"action", got.Action, want.Action},
		{"request id", got.RequestRef.ID, want.RequestRef.ID},
		{"claims", got.Claims["org_id"].(string), want.Claims["org_id"].(string)},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	// The four absent members are present and null, not omitted: an
	// endpoint reads a fixed shape.
	var body map[string]json.RawMessage
	if err = json.Unmarshal(sent, &body); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"subject", "issuer", "sub", "claims", "workload", "action",
		"existing", "parent", "environment", "set", "manifest", "request"} {
		if _, ok := body[name]; !ok {
			t.Errorf("the body has no %q member", name)
		}
	}
	for _, name := range []string{"workload", "existing", "parent", "set"} {
		if string(body[name]) != "null" {
			t.Errorf("%s = %s, want null", name, body[name])
		}
	}
	// The environment is the summary of spec 007 and not the whole object.
	var env map[string]any
	if err = json.Unmarshal(body["environment"], &env); err != nil {
		t.Fatal(err)
	}
	if env["id"] != "env_01J9" || env["name"] != "default" || env["isolation"] != "container" {
		t.Errorf("environment = %v", env)
	}
	if _, ok := env["capabilities"]; !ok {
		t.Error("the environment carries no capabilities")
	}
	if _, ok := env["spec"]; ok {
		t.Error("the environment carries the whole object")
	}
	// The request member carries the id alone: the peer address and the
	// user agent belong to the authorizer's envelope.
	if string(body["request"]) != `{"id":"req_01J9"}` {
		t.Errorf("request = %s", body["request"])
	}
	// The manifest is the stage 2 object, and the fixture's members are
	// members of it.
	for key := range want.Manifest["spec"].(map[string]any) {
		if _, ok := got.Manifest["spec"].(map[string]any)[key]; !ok {
			t.Errorf("the manifest sent has no spec.%s", key)
		}
	}
}

// TestAllowMutationReachesTheCaller: the manifest an allow carries is what
// the caller reads back, with the alias rewritten, the annotations stamped
// and the egress narrowed.
func TestAllowMutationReachesTheCaller(t *testing.T) {
	const pinned = "registry.example/base@sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	e := serve(t, allow(t, func(m map[string]any) {
		spec := m["spec"].(map[string]any)
		spec["image"] = pinned
		spec["resources"] = map[string]any{"cpu": "50m", "memory": "256Mi", "disk": "1Gi"}
		spec["network"] = map[string]any{"egress": map[string]any{"mode": "allowlist"}}
		m["metadata"].(map[string]any)["annotations"] = map[string]any{
			"platform.latere.ai/image":      "base",
			"platform.latere.ai/image-tier": "warm",
		}
	}, "The plan narrowed this sandbox's egress."))

	out, warnings, err := client(t, e, Options{}).Admit(t.Context(), sandbox(), admitRequest())
	if err != nil {
		t.Fatal(err)
	}
	if out.Spec.Image != pinned {
		t.Errorf("image = %q, want the pinned reference", out.Spec.Image)
	}
	if out.Metadata.Annotations["platform.latere.ai/image"] != "base" {
		t.Errorf("annotations = %v", out.Metadata.Annotations)
	}
	if out.Spec.Network.Egress.Mode != v1.EgressAllowlist || len(out.Spec.Network.Egress.DeniedHosts) != 0 {
		t.Errorf("egress = %+v", out.Spec.Network.Egress)
	}
	if out.Spec.Resources.CPU != "50m" || out.Spec.Lifecycle.TTL != "" {
		t.Errorf("resources = %+v", out.Spec.Resources)
	}
	// A field the endpoint did not touch survives.
	if out.Spec.Env["LOG_LEVEL"] != "debug" || out.Spec.Workspace.Path != "/workspace" {
		t.Errorf("spec = %+v", out.Spec)
	}
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "The plan narrowed") {
		t.Errorf("warnings = %v", warnings)
	}
}

// TestAllowWithoutAManifestLeavesTheInput: an absent manifest means
// unchanged, which is spec 007's rule, and an endpoint that only warns
// says so without echoing the document.
func TestAllowWithoutAManifestLeavesTheInput(t *testing.T) {
	for _, body := range []string{`{"allow":true}`, `{"allow":true,"manifest":null}`} {
		e := serve(t, fixed(http.StatusOK, body))
		out, warnings, err := client(t, e, Options{}).Admit(t.Context(), sandbox(), admitRequest())
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if out.Spec.Image != "base" || warnings != nil {
			t.Fatalf("%s: out = %+v warnings = %v", body, out.Spec, warnings)
		}
	}
}

// TestWarningsAreBounded holds an endpoint's warnings to spec 007's
// bounds: at most eight sentences of at most 256 characters.
func TestWarningsAreBounded(t *testing.T) {
	long := strings.Repeat("a", 300)
	many := make([]string, 12)
	for i := range many {
		many[i] = long
	}
	body, err := json.Marshal(map[string]any{"allow": true, "warnings": many})
	if err != nil {
		t.Fatal(err)
	}
	e := serve(t, fixed(http.StatusOK, string(body)))
	_, warnings, err := client(t, e, Options{}).Admit(t.Context(), sandbox(), admitRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != MaxWarnings {
		t.Fatalf("warnings = %d, want %d", len(warnings), MaxWarnings)
	}
	for _, w := range warnings {
		if len([]rune(w)) != MaxWarningRunes {
			t.Fatalf("a warning is %d runes", len([]rune(w)))
		}
	}
}

// TestRefusalCarriesTheCode: a policy refusal is a parsed 200 with allow
// false, and the endpoint's code reaches the caller verbatim as the
// developer detail of admission_refused. The codes are the ones the
// platform's endpoint writes.
func TestRefusalCarriesTheCode(t *testing.T) {
	for _, reason := range []string{
		"no_snapshot", "stale_snapshot", "no_catalog", "anonymous", "worker_key",
		"workload_policy_unavailable", "unknown_action", "plan_suspended", "invalid_manifest",
		"ceiling_exceeded: spec.resources.cpu is 8, above the plan's 4",
		"image_floating_tag: ubuntu:latest", "image_not_in_catalog: ubuntu:24.04",
	} {
		t.Run(reason, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"allow": false, "reason": reason})
			if err != nil {
				t.Fatal(err)
			}
			e := serve(t, fixed(http.StatusOK, string(body)))
			_, _, err = client(t, e, Options{}).Admit(t.Context(), sandbox(), admitRequest())
			var known *manifest.Error
			if !errors.As(err, &known) || known.Code != manifest.CodeAdmissionRefused {
				t.Fatalf("err = %v, want admission_refused", err)
			}
			if known.Detail != reason {
				t.Fatalf("detail = %q, want the endpoint's reason verbatim", known.Detail)
			}
			if e.calls.Load() != 1 {
				t.Fatalf("calls = %d, want one", e.calls.Load())
			}
		})
	}
	// A refusal that names nothing still says something.
	e := serve(t, fixed(http.StatusOK, `{"allow":false}`))
	_, _, err := client(t, e, Options{}).Admit(t.Context(), sandbox(), admitRequest())
	var known *manifest.Error
	if !errors.As(err, &known) || known.Detail == "" {
		t.Fatalf("err = %v", err)
	}
}

// TestFailsClosedWithoutRetry: everything that is not a parsed 200 with
// allow is admission_unavailable and never a pass, and nothing is resent.
// An admission call may rewrite the manifest, so a resend could apply one
// mutation twice.
func TestFailsClosedWithoutRetry(t *testing.T) {
	oversized, err := json.Marshal(map[string]any{
		"allow": true, "reason": strings.Repeat("x", MaxResponseBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		answer func(http.ResponseWriter, []byte)
		wrong  bool // the bearer the endpoint expects is not the one sent
	}{
		{name: "a bad bearer", answer: fixed(http.StatusOK, `{"allow":true}`), wrong: true},
		{name: "a rejected envelope", answer: fixed(http.StatusBadRequest, `{"code":"bad_request"}`)},
		{name: "a server failure", answer: fixed(http.StatusInternalServerError, "")},
		{name: "a redirect", answer: fixed(http.StatusFound, "")},
		{name: "a body that is not JSON", answer: fixed(http.StatusOK, "not json")},
		{name: "a body with no allow", answer: fixed(http.StatusOK, `{"manifest":null}`)},
		{name: "an oversized body", answer: fixed(http.StatusOK, string(oversized))},
		{name: "a timeout", answer: func(w http.ResponseWriter, _ []byte) {
			time.Sleep(200 * time.Millisecond)
			_, _ = w.Write([]byte(`{"allow":true}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := serve(t, tc.answer)
			token := bearer
			if tc.wrong {
				token = "another core's token"
			}
			c := client(t, e, Options{Token: token, Timeout: MinTimeout})
			_, _, err := c.Admit(t.Context(), sandbox(), admitRequest())
			var known *manifest.Error
			if !errors.As(err, &known) || known.Code != manifest.CodeAdmissionUnavailable {
				t.Fatalf("err = %v, want admission_unavailable", err)
			}
			if got := e.calls.Load(); got != 1 {
				t.Fatalf("calls = %d, want exactly one", got)
			}
		})
	}
	// A connection that is refused before a response line arrived is the
	// case the authorizer retries and this one does not. The endpoint is
	// closed, so nothing counts the calls but the dial itself fails.
	closed := serve(t, fixed(http.StatusOK, `{"allow":true}`))
	url := closed.URL
	closed.Close()
	c := client(t, closed, Options{URL: url})
	_, _, err = c.Admit(t.Context(), sandbox(), admitRequest())
	var known *manifest.Error
	if !errors.As(err, &known) || known.Code != manifest.CodeAdmissionUnavailable {
		t.Fatalf("err = %v, want admission_unavailable", err)
	}
	if closed.calls.Load() != 0 {
		t.Fatalf("calls = %d, want none", closed.calls.Load())
	}
}

// TestReturnedManifestIsDecodedStrictly: a manifest the endpoint wrote
// goes through the decode a caller's manifest goes through, so a field
// the schema does not know is refused and never dropped.
func TestReturnedManifestIsDecodedStrictly(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		mutate     func(map[string]any)
	}{
		{"an unknown field", "unknown_field", func(m map[string]any) {
			m["spec"].(map[string]any)["nonesuch"] = "x"
		}},
		{"another kind", "unsupported_kind", func(m map[string]any) { m["kind"] = "Secret" }},
		{"another version", "unsupported_version", func(m map[string]any) { m["apiVersion"] = "cella.latere.ai/v2" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := serve(t, allow(t, tc.mutate))
			_, _, err := client(t, e, Options{}).Admit(t.Context(), sandbox(), admitRequest())
			var known *manifest.Error
			if !errors.As(err, &known) || known.Code != tc.code {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
		})
	}
}

// TestObserveSeparatesTheThreeOutcomes: the metric of spec 017 tells an
// allow, a refusal and an outage apart, because an operator's alarm is on
// the third and not on the second.
func TestObserveSeparatesTheThreeOutcomes(t *testing.T) {
	var results []string
	observe := func(result string, seconds float64) {
		if seconds < 0 {
			t.Errorf("duration = %v", seconds)
		}
		results = append(results, result)
	}
	for _, answer := range []func(http.ResponseWriter, []byte){
		fixed(http.StatusOK, `{"allow":true}`),
		fixed(http.StatusOK, `{"allow":false,"reason":"plan_suspended"}`),
		fixed(http.StatusInternalServerError, ""),
	} {
		e := serve(t, answer)
		_, _, _ = client(t, e, Options{Observe: observe}).Admit(t.Context(), sandbox(), admitRequest())
	}
	want := []string{ResultAllow, ResultRefused, ResultError}
	if strings.Join(results, ",") != strings.Join(want, ",") {
		t.Fatalf("results = %v, want %v", results, want)
	}
}

// TestNewRefusesAnEndpointWithoutABearer: an endpoint that decides what a
// caller may run is never asked unauthenticated.
func TestNewRefusesAnEndpointWithoutABearer(t *testing.T) {
	if _, err := New(Options{URL: "https://admission.example/hook"}); err == nil {
		t.Fatal("a URL without a token was accepted")
	}
	if _, err := New(Options{Token: bearer}); err == nil {
		t.Fatal("a token without a URL was accepted")
	}
	c, err := New(Options{URL: "https://admission.example/hook", Token: bearer})
	if err != nil {
		t.Fatal(err)
	}
	if c.URL() != "https://admission.example/hook" || c.timeout != DefaultTimeout {
		t.Fatalf("client = %+v", c)
	}
}

// TestAdmitCarriesTheWorkloadAndTheExisting: an update sends the stored
// object and a workload's apply says that it is one, because an endpoint
// that would not grant a sandbox the authority of its owner has to see
// the difference.
func TestAdmitCarriesTheWorkloadAndTheExisting(t *testing.T) {
	e := serve(t, allow(t, nil))
	req := admitRequest()
	req.Action = "update"
	req.Existing = sandbox()
	req.Workload = &v1.SandboxStatus{ID: "sb_1", Phase: "Running"}
	if _, _, err := client(t, e, Options{}).Admit(t.Context(), sandbox(), req); err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(<-e.bodies, &body); err != nil {
		t.Fatal(err)
	}
	if string(body["workload"]) == "null" || !strings.Contains(string(body["workload"]), `"sb_1"`) {
		t.Fatalf("workload = %s", body["workload"])
	}
	if string(body["existing"]) == "null" {
		t.Fatal("an update sent no existing object")
	}
	var action string
	if err := json.Unmarshal(body["action"], &action); err != nil || action != "update" {
		t.Fatalf("action = %q", action)
	}
}

// TestEndpointResponseFixture reads the answer shape the platform's
// endpoint returns, copied into testdata beside its request, and proves
// the client reads all four members of it.
func TestEndpointResponseFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/endpoint-response.json")
	if err != nil {
		t.Fatal(err)
	}
	e := serve(t, fixed(http.StatusOK, string(body)))
	out, warnings, err := client(t, e, Options{}).Admit(t.Context(), sandbox(), admitRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Spec.Image, "registry.example/base@sha256:") {
		t.Errorf("image = %q", out.Spec.Image)
	}
	if out.Metadata.Annotations["platform.latere.ai/image-tier"] != "warm" {
		t.Errorf("annotations = %v", out.Metadata.Annotations)
	}
	if out.Spec.Lifecycle.TTL != "24h" || out.Spec.Resources.Memory != "256Mi" {
		t.Errorf("spec = %+v", out.Spec)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v", warnings)
	}
}

// TestContextEndsTheCall: the caller's own cancellation ends the call and
// is answered as no decision, the same as a timeout.
func TestContextEndsTheCall(t *testing.T) {
	e := serve(t, func(w http.ResponseWriter, _ []byte) {
		time.Sleep(time.Second)
		_, _ = w.Write([]byte(`{"allow":true}`))
	})
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, _, err := client(t, e, Options{}).Admit(ctx, sandbox(), admitRequest())
	var known *manifest.Error
	if !errors.As(err, &known) || known.Code != manifest.CodeAdmissionUnavailable {
		t.Fatalf("err = %v, want admission_unavailable", err)
	}
}

// TestEnvelopeThatCannotBeBuilt: a manifest that does not marshal is no
// decision rather than a panic. Nothing in v1 can produce one, so the
// case is driven through the renderer directly.
func TestEnvelopeThatCannotBeBuilt(t *testing.T) {
	req := admitRequest()
	req.Claims = map[string]any{"cycle": make(chan int)}
	if _, err := envelopeOf(sandbox(), req); err == nil {
		t.Fatal("a body that cannot be marshalled was built")
	}
	c := client(t, serve(t, allow(t, nil)), Options{})
	_, _, err := c.Admit(t.Context(), sandbox(), req)
	var known *manifest.Error
	if !errors.As(err, &known) || known.Code != manifest.CodeAdmissionUnavailable {
		t.Fatalf("err = %v, want admission_unavailable", err)
	}
	// A URL that is not one is the same answer, and no call is made.
	broken := client(t, nil, Options{URL: "://", Token: bearer})
	if _, _, err = broken.Admit(t.Context(), sandbox(), admitRequest()); err == nil {
		t.Fatal("a broken URL was accepted")
	}
}

// TestSummaryOfNoEnvironment: an admission step called with no
// environment sends null rather than an empty object, so an endpoint
// tells "no environment" from "an environment with no name".
func TestSummaryOfNoEnvironment(t *testing.T) {
	if summaryOf(nil) != nil {
		t.Fatal("a nil environment rendered an object")
	}
	env := &v1.Environment{Metadata: v1.Metadata{Name: "default"}, Spec: v1.EnvironmentSpec{Isolation: v1.IsolationContainer}}
	if got := summaryOf(env); got.Isolation != string(v1.IsolationContainer) {
		t.Fatalf("isolation = %q, want the operator's declaration where no driver answered", got.Isolation)
	}
}
