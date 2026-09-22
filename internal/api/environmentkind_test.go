// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"

	"latere.ai/x/cella/controller"
	"latere.ai/x/cella/internal/auth"
	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
)

// kinds is a control plane that stores the Environment kind, with a driver
// behind every environment an administrator applies. The driver is the same
// native one the control plane drives itself, because these cases are about
// the object and its routes rather than about where a sandbox runs.
type kinds struct {
	*fixture
	mu            sync.Mutex
	registrations []controller.Registration
}

func setupEnvironments(t *testing.T) *kinds {
	t.Helper()
	return setupEnvironmentsOn(t, func(d runtime.Driver) runtime.Driver { return d })
}

// setupEnvironmentsOn is setupEnvironments with the control plane's own
// environment behind a wrapped driver, so the default and an applied
// environment can declare different capabilities over one native runtime.
func setupEnvironmentsOn(t *testing.T, own func(runtime.Driver) runtime.Driver) *kinds {
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
	k := &kinds{}
	c, err := controller.Open(t.Context(), controller.Options{
		DataDir: t.TempDir(), Driver: own(d), Environment: "default",
		NewDriver:     func(v1.Environment) (runtime.Driver, error) { return d, nil },
		Registrations: func(string) []controller.Registration { return k.live() },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	policy := &auth.OwnerPolicy{DefaultEnvironment: "default", Admins: []string{issuer.URL() + "|admin"}}
	h, err := New(Options{
		Controller: c, Verifier: verifier, Authorizer: auth.NewAuthorizer(policy),
		Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	k.fixture = &fixture{
		t: t, url: server.URL, issuerURL: issuer.URL(),
		alice: issuer.Mint(issuertest.Claims{Sub: "admin"}),
		bob:   issuer.Mint(issuertest.Claims{Sub: "bob"}),
		h:     h, c: c,
	}
	return k
}

func (k *kinds) live() []controller.Registration {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.registrations
}

// arrive puts one worker on every environment and runs the phase loop once,
// which is what makes an applied environment placeable.
func (k *kinds) arrive(t *testing.T) {
	t.Helper()
	k.mu.Lock()
	k.registrations = []controller.Registration{{Worker: "wrk_1", LastHeartbeat: time.Now(), Connected: true}}
	k.mu.Unlock()
	if err := k.c.Phases(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// send is request with the response's headers kept, for the routes whose
// answer carries the ETag an If-Match is built from.
func (k *kinds) send(method, path, token, body string, headers map[string]string, status int) ([]byte, http.Header) {
	k.t.Helper()
	req, err := http.NewRequest(method, k.url+path, strings.NewReader(body))
	if err != nil {
		k.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		k.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		k.t.Fatal(err)
	}
	if res.StatusCode != status {
		k.t.Fatalf("%s %s: got %d want %d: %s", method, path, res.StatusCode, status, b)
	}
	return b, res.Header
}

// environmentBody is one worker environment as an operator writes it.
const environmentBody = `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Environment",` +
	`"metadata":{"name":"eu-gpu","labels":{"region":"eu"}},` +
	`"spec":{"mode":"worker","isolation":"none","capacity":{"cpu":"8","memory":"16Gi","sandboxes":10}}}`

// TestEnvironmentRoutes is the grammar of design 008 over the Environment
// kind: applied by name, read back, listed beside the control plane's own,
// and deleted.
func TestEnvironmentRoutes(t *testing.T) {
	k := setupEnvironments(t)

	body, header := k.send(http.MethodPut, "/v1/environments/eu-gpu", k.alice, environmentBody, nil, http.StatusCreated)
	var applied v1.Environment
	if err := json.Unmarshal(body, &applied); err != nil {
		t.Fatalf("the applied environment did not decode: %v", err)
	}
	switch {
	case applied.Status.ID != "eu-gpu":
		t.Errorf("the environment's id is %q, want its name", applied.Status.ID)
	case applied.Status.Owner != k.issuerURL+"|admin":
		t.Errorf("the environment is owned by %q", applied.Status.Owner)
	case applied.Status.Phase != v1.EnvironmentPending:
		t.Errorf("an environment nothing has registered on is %q", applied.Status.Phase)
	case header.Get("ETag") == "":
		t.Errorf("the answer carries no ETag, which is what an If-Match is built from")
	case header.Get("Location") != "/v1/environments/eu-gpu":
		t.Errorf("the create answers Location %q", header.Get("Location"))
	}

	// A second apply of the same name is an update and not a second object.
	updated := strings.Replace(environmentBody, `"sandboxes":10`, `"sandboxes":25`, 1)
	body, _ = k.send(http.MethodPut, "/v1/environments/eu-gpu", k.alice, updated, nil, http.StatusOK)
	if err := json.Unmarshal(body, &applied); err != nil {
		t.Fatal(err)
	}
	if applied.Spec.Capacity.Sandboxes != 25 {
		t.Errorf("the update wrote capacity %+v", applied.Spec.Capacity)
	}

	// The read answers the stored object, with the phase the loop wrote.
	k.arrive(t)
	body, header = k.send(http.MethodGet, "/v1/environments/eu-gpu", k.alice, "", nil, http.StatusOK)
	var read v1.Environment
	if err := json.Unmarshal(body, &read); err != nil {
		t.Fatal(err)
	}
	switch {
	case read.Spec.Capacity.Sandboxes != 25:
		t.Errorf("the read answers capacity %+v", read.Spec.Capacity)
	case read.Status.Phase != v1.EnvironmentReady:
		t.Errorf("a registered environment reads %q", read.Status.Phase)
	case read.Status.Workers != 1:
		t.Errorf("the environment reports %d workers", read.Status.Workers)
	case header.Get("ETag") == "":
		t.Errorf("the read carries no ETag")
	}

	// The list carries both the applied environment and the one this control
	// plane drives itself.
	body, _ = k.send(http.MethodGet, "/v1/environments", k.alice, "", nil, http.StatusOK)
	var page struct {
		Items []v1.Environment `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].Metadata.Name != "default" || page.Items[1].Metadata.Name != "eu-gpu" {
		t.Errorf("the list answers %d environments: %+v", len(page.Items), page.Items)
	}

	// A POST names the environment in its body, and a name another
	// environment holds is name_taken.
	k.send(http.MethodPost, "/v1/environments", k.alice, environmentBody, nil, http.StatusConflict)
	second := strings.Replace(environmentBody, "eu-gpu", "us-east", 1)
	k.send(http.MethodPost, "/v1/environments", k.alice, second, nil, http.StatusCreated)

	// The delete ends the object and the read that follows is not found.
	k.send(http.MethodDelete, "/v1/environments/us-east", k.alice, "", nil, http.StatusNoContent)
	k.send(http.MethodGet, "/v1/environments/us-east", k.alice, "", nil, http.StatusNotFound)
}

// TestEnvironmentConcurrency is design 008's rule over this kind: an If-Match
// at the version the read returned writes, and a stale one is refused rather
// than overwriting what moved.
func TestEnvironmentConcurrency(t *testing.T) {
	k := setupEnvironments(t)
	_, header := k.send(http.MethodPut, "/v1/environments/eu-gpu", k.alice, environmentBody, nil, http.StatusCreated)
	tag := header.Get("ETag")
	if tag == "" {
		t.Fatal("the apply carries no ETag")
	}

	updated := strings.Replace(environmentBody, `"sandboxes":10`, `"sandboxes":25`, 1)
	_, header = k.send(http.MethodPut, "/v1/environments/eu-gpu", k.alice, updated,
		map[string]string{"If-Match": tag}, http.StatusOK)
	if header.Get("ETag") == tag {
		t.Errorf("the version did not move: %s", header.Get("ETag"))
	}

	// The tag the first read returned names a row that has moved.
	k.send(http.MethodPut, "/v1/environments/eu-gpu", k.alice, updated,
		map[string]string{"If-Match": tag}, http.StatusConflict)
	// An If-Match that is not a version at all is a field refusal.
	k.send(http.MethodPut, "/v1/environments/eu-gpu", k.alice, updated,
		map[string]string{"If-Match": `"not-a-version"`}, http.StatusBadRequest)
	// No If-Match at all is a read-modify-write, which succeeds.
	k.send(http.MethodPut, "/v1/environments/eu-gpu", k.alice, updated, nil, http.StatusOK)
	// The wildcard is any version, which is the same.
	k.send(http.MethodPut, "/v1/environments/eu-gpu", k.alice, updated,
		map[string]string{"If-Match": "*"}, http.StatusOK)
}

// TestEnvironmentRefusals holds the table of refusals this kind answers.
func TestEnvironmentRefusals(t *testing.T) {
	k := setupEnvironments(t)
	k.send(http.MethodPut, "/v1/environments/eu-gpu", k.alice, environmentBody, nil, http.StatusCreated)
	k.arrive(t)

	// The path names the object, so a body that names another is refused
	// rather than quietly renamed.
	k.send(http.MethodPut, "/v1/environments/us-east", k.alice, environmentBody, nil, http.StatusBadRequest)

	// The control plane's own environment is not a caller's to apply or to
	// delete: it describes the process that holds it. On an environment that
	// already exists the mode is immutable, which is the nearer refusal.
	inprocess := strings.Replace(environmentBody, `"mode":"worker"`, `"mode":"inprocess"`, 1)
	k.send(http.MethodPut, "/v1/environments/eu-gpu", k.alice, inprocess, nil, http.StatusConflict)
	k.send(http.MethodPut, "/v1/environments/us-east", k.alice,
		strings.Replace(inprocess, "eu-gpu", "us-east", 1), nil, http.StatusBadRequest)
	k.send(http.MethodDelete, "/v1/environments/default", k.alice, "", nil, http.StatusBadRequest)

	// An environment holding a sandbox is not deleted: the sandboxes would
	// be left with nothing driving them.
	created := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",` +
		`"metadata":{"name":"there"},"spec":{"environment":"eu-gpu"}}`
	k.send(http.MethodPost, "/v1/sandboxes", k.alice, created, nil, http.StatusCreated)
	k.send(http.MethodDelete, "/v1/environments/eu-gpu", k.alice, "", nil, http.StatusConflict)

	// Every field rule of spec 021's table is the resolver's, and the route
	// answers it with the code the table names.
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"a mode outside the enum", strings.Replace(environmentBody, `"mode":"worker"`, `"mode":"somewhere"`, 1), http.StatusBadRequest},
		{"no isolation class", strings.Replace(environmentBody, `"isolation":"none",`, "", 1), http.StatusBadRequest},
		{"auto capacity on a worker", strings.Replace(environmentBody, `{"cpu":"8","memory":"16Gi","sandboxes":10}`, `"auto"`, 1), http.StatusBadRequest},
		{"a field the schema does not know", strings.Replace(environmentBody, `"mode":"worker"`, `"mode":"worker","teleport":true`, 1), http.StatusBadRequest},
		{"a body that is not JSON", "not json", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k.send(http.MethodPut, "/v1/environments/us-east", k.alice, tc.body, nil, tc.status)
		})
	}
}

// TestEnvironmentAuthorization holds spec 006's rows: every environment
// mutation is an administrator's under the built-in owner policy, and a
// sandbox's create is decided against the environment it names.
func TestEnvironmentAuthorization(t *testing.T) {
	k := setupEnvironments(t)
	k.send(http.MethodPut, "/v1/environments/eu-gpu", k.alice, environmentBody, nil, http.StatusCreated)
	k.arrive(t)

	for _, tc := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"a create", http.MethodPost, "/v1/environments", environmentBody, http.StatusForbidden},
		{"an apply", http.MethodPut, "/v1/environments/us-east", strings.Replace(environmentBody, "eu-gpu", "us-east", 1), http.StatusForbidden},
		{"an update", http.MethodPut, "/v1/environments/eu-gpu", environmentBody, http.StatusForbidden},
		{"a delete", http.MethodDelete, "/v1/environments/eu-gpu", "", http.StatusForbidden},
		{"a read", http.MethodGet, "/v1/environments/eu-gpu", "", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k.send(tc.method, tc.path, k.bob, tc.body, nil, tc.status)
		})
	}

	// Every subject may use the environment a manifest that names none gets,
	// and none but an administrator may use another.
	onTheDefault := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"here"},"spec":{}}`
	k.send(http.MethodPost, "/v1/sandboxes", k.bob, onTheDefault, nil, http.StatusCreated)
	// A deny on environment.use is not_found: a caller learns no more about
	// an environment it may not use than that there is none (spec 021).
	elsewhere := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",` +
		`"metadata":{"name":"there"},"spec":{"environment":"eu-gpu"}}`
	k.send(http.MethodPost, "/v1/sandboxes", k.bob, elsewhere, nil, http.StatusNotFound)
	k.send(http.MethodPost, "/v1/sandboxes", k.alice, elsewhere, nil, http.StatusCreated)
}

// TestASandboxNamesAnEnvironmentThisServerDoesNotHold: the resolver answers
// from the registry, so a manifest naming an absent environment is not_found
// at spec.environment rather than placed on the default.
func TestASandboxNamesAnEnvironmentThisServerDoesNotHold(t *testing.T) {
	k := setupEnvironments(t)
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox",` +
		`"metadata":{"name":"nowhere"},"spec":{"environment":"us-east"}}`
	answer, _ := k.send(http.MethodPost, "/v1/sandboxes", k.alice, body, nil, http.StatusNotFound)
	if !strings.Contains(string(answer), "spec.environment") {
		t.Errorf("the refusal does not name the field: %s", answer)
	}
}

// TestApplyingAnEnvironmentWithoutAStore: a control plane that stores no
// Environment kind refuses the apply as a capability rather than failing
// somewhere below it.
func TestApplyingAnEnvironmentWithoutAStore(t *testing.T) {
	f := setup(t, &auth.OwnerPolicy{DefaultEnvironment: "default", Admins: []string{"alice"}})
	f.request(http.MethodPut, "/v1/environments/eu-gpu", f.alice, environmentBody, http.StatusForbidden)
}

// desktopOnlyDriver declares a desktop and nothing else, so the control
// plane's own environment and a native one applied beside it differ in every
// capability a route's gate reads.
type desktopOnlyDriver struct{ runtime.Driver }

func (desktopOnlyDriver) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{Display: true, Input: true}
}

// TestTheGatesReadTheSandboxsOwnEnvironment: a route's capability gate
// decides on what the environment the sandbox runs on provides, not on what
// the control plane's own environment provides. The default here declares a
// desktop and no file operations or terminal; the environment beside it is
// native and declares the opposite.
func TestTheGatesReadTheSandboxsOwnEnvironment(t *testing.T) {
	k := setupEnvironmentsOn(t, func(d runtime.Driver) runtime.Driver { return desktopOnlyDriver{d} })
	k.send(http.MethodPut, "/v1/environments/eu-gpu", k.alice, environmentBody, nil, http.StatusCreated)
	k.arrive(t)
	create := func(name, environment string) string {
		t.Helper()
		body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"` + name + `"},` +
			`"spec":{"environment":"` + environment + `"}}`
		out, _ := k.send(http.MethodPost, "/v1/sandboxes", k.alice, body, nil, http.StatusCreated)
		var obj v1.Sandbox
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatal(err)
		}
		return "/v1/sandboxes/" + obj.Status.ID
	}
	there, here := create("there", "eu-gpu"), create("here", "default")
	refused := func(path string) {
		t.Helper()
		out, _ := k.send(http.MethodGet, path, k.alice, "", nil, http.StatusUnprocessableEntity)
		if !strings.Contains(string(out), "capability_unsupported") {
			t.Fatalf("%s answered %s", path, out)
		}
	}

	// The archive route: served where the sandbox's environment has files,
	// refused where it has none.
	k.send(http.MethodGet, there+"/files?path=/workspace", k.alice, "", nil, http.StatusOK)
	refused(here + "/files?path=/workspace")

	// The desktop routes: refused where the sandbox's environment has no
	// desktop, whatever the control plane's own declares.
	refused(there + "/display")
	refused(there + "/screenshot")

	// The terminal: a request that passes the gate reaches the upgrade, which
	// refuses a plain GET with 400; one that fails it is refused first.
	if k.c.CapabilitiesOf("eu-gpu").Attach {
		k.send(http.MethodGet, there+"/attach", k.alice, "", nil, http.StatusBadRequest)
	}
	refused(here + "/attach")
}
