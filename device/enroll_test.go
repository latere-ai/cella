// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/client"
	v1 "latere.ai/x/cella/manifest/v1"
)

// stubCella answers the environment routes an Enrollment and a CellaAccount
// call, with the refusal a test sets.
type stubCella struct {
	mu      sync.Mutex
	created []v1.Environment
	revoked []string
	// refuse, when set, answers every call with this status, code and
	// detail.
	refuse *struct {
		status       int
		code, detail string
	}
	missing bool
}

func (s *stubCella) refuseWith(status int, code, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse = &struct {
		status       int
		code, detail string
	}{status, code, detail}
}

func (s *stubCella) client(t *testing.T) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	c, err := client.New(client.Config{URL: srv.URL, Token: client.StaticToken("person-token")})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (s *stubCella) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	refuse, missing := s.refuse, s.missing
	s.mu.Unlock()
	if refuse != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(refuse.status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": refuse.code, "message": "refused", "details": map[string]any{"detail": refuse.detail},
		}})
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/environments":
		var env v1.Environment
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &env); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.created = append(s.created, env)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/keys"):
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "env-key", "jti": "jti-1", "exp": time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)})
	case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/keys/"):
		s.mu.Lock()
		s.revoked = append(s.revoked, r.URL.Path)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/environments/"):
		if missing {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no such environment"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"cella/v1","kind":"Environment","metadata":{"name":"dev-x"}}`))
	default:
		http.NotFound(w, r)
	}
}

// fakePlatform is a plane's part of a connect, as a test sets it.
type fakePlatform struct {
	mu              sync.Mutex
	labels          map[string]string
	labelsErr       error
	org             string
	renameErr       error
	renamed         []string
	removed         []string
	have, limit     int
	devicesErr      error
	contextReadOnce bool
}

func (p *fakePlatform) Labels(context.Context) (map[string]string, error) {
	return p.labels, p.labelsErr
}

func (p *fakePlatform) Context(context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.contextReadOnce = true
	return p.org, nil
}

func (p *fakePlatform) Rename(_ context.Context, device, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.renamed = append(p.renamed, device+"="+name)
	return p.renameErr
}

func (p *fakePlatform) Remove(_ context.Context, device string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removed = append(p.removed, device)
	return nil
}

func (p *fakePlatform) Devices(context.Context) (int, int, error) {
	return p.have, p.limit, p.devicesErr
}

func enrollmentOn(t *testing.T, cella *stubCella, platform *fakePlatform) (*Enrollment, Paths) {
	t.Helper()
	paths := testPaths(t)
	return NewEnrollment(EnrollmentOptions{
		Paths: paths, Cella: cella.client(t), Platform: platform,
		Labels: map[string]string{"example.com/kind": "device"},
		Name:   func() string { return "Test Mac" },
	}), paths
}

func TestEnrollmentConnectCreatesTheEnvironment(t *testing.T) {
	cella := &stubCella{}
	platform := &fakePlatform{labels: map[string]string{"example.com/tenant": "t1"}}
	e, paths := enrollmentOn(t, cella, platform)
	p := Policy{Name: "Desk", SessionStart: SessionStartAllow, Folders: []Folder{{Path: "~/code", Write: true}},
		Network: Network{Mode: NetworkAllowlist, Hosts: []string{"github.com"}}}
	done, err := e.Connect(t.Context(), p)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !strings.HasPrefix(done.Policy.Device, "dev-") || len(done.Policy.Device) != len("dev-")+16 || done.NameError != nil {
		t.Fatalf("Connect answered %+v", done)
	}
	if len(cella.created) != 1 {
		t.Fatalf("Cella was asked %d creates", len(cella.created))
	}
	env := cella.created[0]
	spec := env.Spec
	if env.Metadata.Name != done.Policy.Device || spec.Mode != v1.EnvironmentWorker || spec.Isolation != v1.IsolationProcess ||
		spec.Workers.Max != 1 || spec.Capacity.Sandboxes != Sandboxes {
		t.Errorf("the environment created is %+v", env)
	}
	if env.Metadata.Labels["example.com/tenant"] != "t1" || env.Metadata.Labels["example.com/kind"] != "device" {
		t.Errorf("the environment carries %v, want the plane's labels and the embedder's", env.Metadata.Labels)
	}
	saved, ok, err := LoadPolicy(paths.PolicyFile())
	if err != nil || !ok || saved.Device != done.Policy.Device {
		t.Errorf("the host policy file holds %+v, %v, %v", saved, ok, err)
	}
	if !slices.Equal(platform.renamed, []string{done.Policy.Device + "=Desk"}) {
		t.Errorf("the plane was asked to name %v", platform.renamed)
	}
}

func TestEnrollmentConnectsWhenTheNameIsRefused(t *testing.T) {
	cella := &stubCella{}
	platform := &fakePlatform{labels: map[string]string{"example.com/tenant": "t1"}, renameErr: errors.New("name taken")}
	e, _ := enrollmentOn(t, cella, platform)
	done, err := e.Connect(t.Context(), Policy{Name: "Desk", Network: Network{Mode: NetworkNone}})
	if err != nil || done.Policy.Device == "" || done.NameError == nil {
		t.Fatalf("Connect = %+v, %v; want connected with the name's refusal beside it", done, err)
	}
}

func TestEnrollmentRefusesAnOrganizationsContext(t *testing.T) {
	cella := &stubCella{}
	e, paths := enrollmentOn(t, cella, &fakePlatform{org: "Acme"})
	_, err := e.Connect(t.Context(), Policy{Name: "Desk"})
	if CodeOf(err) != CodeOrgContext || !strings.Contains(err.Error(), "Acme") {
		t.Fatalf("Connect = %v, want %s", err, CodeOrgContext)
	}
	if len(cella.created) != 0 {
		t.Error("an environment was created in an organization's context")
	}
	if _, ok, _ := LoadPolicy(paths.PolicyFile()); ok {
		t.Error("the host policy file was written in an organization's context")
	}
}

func TestEnrollmentWaitsForCredentials(t *testing.T) {
	e, _ := enrollmentOn(t, &stubCella{}, &fakePlatform{labelsErr: fmt.Errorf("%w: platform", ErrCredentialsNeeded)})
	if _, err := e.Connect(t.Context(), Policy{Name: "Desk"}); CodeOf(err) != CodeCredentialsNeeded {
		t.Fatalf("Connect = %v, want %s", err, CodeCredentialsNeeded)
	}
}

func TestEnrollmentCreateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		code, detail string
		want         string
		hint         string
	}{
		{"the limit, told with the plane's numbers", http.StatusForbidden, "forbidden", "forbidden: environment.create: device_limit", CodeLimit, "You have 3 devices, and one person may connect 3."},
		{"personal only", http.StatusForbidden, "forbidden", "forbidden: environment.create: device_personal_only", CodePersonalOnly, ""},
		{"an expired sign-in", http.StatusUnauthorized, "unauthorized", "the token expired", CodeSignInRequired, ""},
		{"anything else", http.StatusBadRequest, "invalid", "the manifest is invalid", CodeRefused, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cella := &stubCella{}
			cella.refuseWith(tc.status, tc.code, tc.detail)
			e, _ := enrollmentOn(t, cella, &fakePlatform{labels: map[string]string{"example.com/tenant": "t1"}, have: 3, limit: 3})
			_, err := e.Connect(t.Context(), Policy{Name: "Desk"})
			if CodeOf(err) != tc.want {
				t.Fatalf("Connect = %v, want %s", err, tc.want)
			}
			var de *Error
			if errors.As(err, &de) && tc.hint != "" && !strings.HasPrefix(de.Hint, tc.hint) {
				t.Errorf("the hint reads %q, want it to start %q", de.Hint, tc.hint)
			}
		})
	}
}

func TestEnrollmentRenameAndRemoveAskThePlane(t *testing.T) {
	platform := &fakePlatform{}
	e, _ := enrollmentOn(t, &stubCella{}, platform)
	if err := e.Rename(t.Context(), "dev-1", "Studio"); err != nil {
		t.Fatal(err)
	}
	if err := e.Remove(t.Context(), "dev-1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(platform.renamed, []string{"dev-1=Studio"}) || !slices.Equal(platform.removed, []string{"dev-1"}) {
		t.Errorf("the plane was asked to rename %v and remove %v", platform.renamed, platform.removed)
	}
}

func TestCellaAccount(t *testing.T) {
	cella := &stubCella{}
	account := NewCellaAccount(cella.client(t))
	key, err := account.MintKey(t.Context(), "dev-1", time.Hour)
	if err != nil || key.Environment != "dev-1" || key.Token != "env-key" || key.JTI != "jti-1" || key.ExpiresAt.IsZero() {
		t.Fatalf("MintKey = %+v, %v", key, err)
	}
	if err = account.RevokeKey(t.Context(), "dev-1", "jti-1"); err != nil || len(cella.revoked) != 1 {
		t.Fatalf("RevokeKey = %v, revoked %v", err, cella.revoked)
	}
	if ok, err := account.Exists(t.Context(), "dev-1"); !ok || err != nil {
		t.Fatalf("Exists = %v, %v; want true", ok, err)
	}
	cella.missing = true
	if ok, err := account.Exists(t.Context(), "dev-1"); ok || err != nil {
		t.Fatalf("Exists of an environment Cella holds none of = %v, %v; want false", ok, err)
	}
	for _, tc := range []struct {
		code, detail string
		removed      bool
	}{
		{"not_found", "", true},
		{"forbidden", "forbidden: environment.key: device_removed", true},
		{"forbidden", "forbidden: environment.key: device_not_owned", true},
		{"forbidden", "forbidden: environment.key: something_else", false},
	} {
		cella.refuseWith(http.StatusForbidden, tc.code, tc.detail)
		_, err := account.MintKey(t.Context(), "dev-1", time.Hour)
		if errors.Is(err, ErrRemoved) != tc.removed {
			t.Errorf("MintKey refused %s %q = %v, removed want %v", tc.code, tc.detail, err, tc.removed)
		}
	}
}

func TestProposePolicy(t *testing.T) {
	home := "/h"
	named := func() string { return "  Studio\tMac  " }
	t.Run("defaults", func(t *testing.T) {
		p, err := ProposePolicy(Policy{}, ConnectQuery{}, home, "/h/code/site", named)
		if err != nil {
			t.Fatal(err)
		}
		if p.Name != CleanName("  Studio\tMac  ") || p.SessionStart != SessionStartAllow || p.Network.Mode != NetworkAllowlist ||
			!slices.Equal(p.Network.Hosts, DefaultHosts) || len(p.Folders) != 1 || p.Folders[0].Path != "~/code/site" {
			t.Errorf("the defaults are %+v", p)
		}
	})
	t.Run("the previous file, then the query", func(t *testing.T) {
		until := time.Now().Add(24 * time.Hour).UTC()
		previous := Policy{Name: "Desk", SessionStart: SessionStartAsk, Folders: []Folder{{Path: "~/notes"}},
			Network: Network{Mode: NetworkAllowlist, Hosts: []string{"pypi.org"}}, ReadPaths: []string{"/opt/tools"},
			RememberedTriggers: []Remembered{{Trigger: "trg_1", AgentVersion: "4", Until: until}}}
		kept, err := ProposePolicy(previous, ConnectQuery{}, home, "/", named)
		if err != nil {
			t.Fatal(err)
		}
		if kept.Name != "Desk" || kept.SessionStart != SessionStartAsk || kept.Folders[0].Path != "~/notes" ||
			!slices.Equal(kept.Network.Hosts, []string{"pypi.org"}) || !slices.Equal(kept.ReadPaths, []string{"/opt/tools"}) ||
			len(kept.RememberedTriggers) != 1 {
			t.Errorf("the previous file's values were not kept: %+v", kept)
		}
		asked, err := ProposePolicy(previous, ConnectQuery{Name: "Studio", SessionStart: SessionStartAllow,
			Folders: []string{"~/code:ro"}, Network: NetworkNone, Hosts: []string{"github.com"}}, home, "/", named)
		if err != nil {
			t.Fatal(err)
		}
		if asked.Name != "Studio" || asked.SessionStart != SessionStartAllow || len(asked.Folders) != 1 ||
			asked.Folders[0].Path != "~/code" || asked.Folders[0].Write || asked.Network.Mode != NetworkNone || asked.Network.Hosts != nil {
			t.Errorf("the query was not applied over the file: %+v", asked)
		}
	})
	t.Run("hosts named", func(t *testing.T) {
		p, err := ProposePolicy(Policy{}, ConnectQuery{Hosts: []string{"github.com", "github.com:22"}}, home, "/h", named)
		if err != nil || !slices.Equal(p.Network.Hosts, []string{"github.com", "github.com:22"}) {
			t.Errorf("ProposePolicy = %+v, %v", p, err)
		}
	})
	t.Run("a folder that does not parse", func(t *testing.T) {
		if _, err := ProposePolicy(Policy{}, ConnectQuery{Folders: []string{":ro"}}, home, "/h", named); err == nil {
			t.Error("a malformed folder was proposed")
		}
	})
}

func TestEnrollmentProposeUsesTheWorkingDirectory(t *testing.T) {
	e, paths := enrollmentOn(t, &stubCella{}, &fakePlatform{})
	dir := filepath.Join(paths.Home, "project")
	t.Chdir(t.TempDir())
	p, err := e.Propose(Policy{}, ConnectQuery{Folders: []string{dir}})
	if err != nil || len(p.Folders) != 1 || p.Folders[0].Path != "~/project" || p.Name != "Test Mac" {
		t.Errorf("Propose = %+v, %v", p, err)
	}
}

func TestDefaultComputerName(t *testing.T) {
	if name := DefaultComputerName(); strings.TrimSpace(name) == "" || strings.HasSuffix(name, ".local") {
		t.Errorf("DefaultComputerName = %q", name)
	}
}

func TestPlatformReason(t *testing.T) {
	for detail, want := range map[string]string{
		"forbidden: environment.create: device_limit": "device_limit",
		"device_limit":                 "",
		"forbidden: Device Limit":      "",
		"forbidden: environment.key: ": "",
	} {
		if got := platformReason(detail); got != want {
			t.Errorf("platformReason(%q) = %q, want %q", detail, got, want)
		}
	}
}
