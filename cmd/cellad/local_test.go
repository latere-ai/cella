// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/cella/manifest"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/local"
)

// TestLocalWorkerEndToEnd is spec 081 through every layer: cellad's worker
// role on CELLA_RUNTIME=local, configured by the CELLA_LOCAL_* variables,
// registers its host; a sandbox on a granted folder resolves with its egress
// narrowed to the host's list, runs a command under the operating system's
// sandbox that reads and writes the folder in place, and leaves the folder
// whole when it is deleted; and a folder outside the grants is refused at
// resolve. It needs the sandbox runtime, so it is skipped where none is
// installed.
func TestLocalWorkerEndToEnd(t *testing.T) {
	probe, err := local.New(local.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err = probe.Preflight(t.Context()); err != nil {
		t.Skipf("the sandbox runtime is not installed here, so the local worker is unchecked end to end:\n%v", err)
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(home, "project")
	if err = os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(project, "notes.txt"), []byte("from the host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The driver reads the home directory from the process, which is where a
	// person's own worker finds it.
	t.Setenv("HOME", home)

	p := startPlane(t, "", "")
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Environment","metadata":{"name":"laptop"},` +
		`"spec":{"mode":"worker","isolation":"process","capacity":{"sandboxes":4}}}`
	if status, answer := p.do(t, http.MethodPut, "/v1/environments/laptop", strings.NewReader(body)); status != http.StatusCreated {
		t.Fatalf("the apply answered %d: %s", status, answer)
	}
	status, answer := p.do(t, http.MethodPost, "/v1/environments/laptop/keys", nil)
	if status != http.StatusCreated {
		t.Fatalf("the key mint answered %d: %s", status, answer)
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err = json.Unmarshal([]byte(answer), &minted); err != nil {
		t.Fatal(err)
	}
	w := startWorker(t, p, minted.Token, map[string]string{
		"CELLA_RUNTIME":             "local",
		"CELLA_ALLOW_UNSAFE_NATIVE": "",
		"CELLA_LOCAL_GRANTS":        project + ":rw",
		"CELLA_LOCAL_NETWORK":       "allowlist",
		"CELLA_LOCAL_HOSTS":         "github.com",
	})
	waitFor(t, "the environment to list the local worker's host", func() bool {
		return len(p.environmentNamed(t, "laptop").Status.Hosts) == 1
	})
	host := p.environmentNamed(t, "laptop").Status.Hosts[0].Host
	if len(host.Grants) != 1 || host.Grants[0].Path != project || !host.Grants[0].Write ||
		host.Network.Mode != "allowlist" || strings.Join(host.Network.Hosts, ",") != "github.com" {
		t.Fatalf("the host registered %+v; worker stderr %q", host, w.errOut.String())
	}

	outside := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"outside"},` +
		`"spec":{"environment":"laptop","workspace":{"host":"` + home + `/elsewhere"}}}`
	if status, answer = p.do(t, http.MethodPost, "/v1/sandboxes", strings.NewReader(outside)); status != http.StatusUnprocessableEntity ||
		!strings.Contains(answer, "workspace_not_granted") {
		t.Errorf("a folder outside the grants answered %d: %s", status, answer)
	}

	create := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"site"},` +
		`"spec":{"environment":"laptop","workspace":{"host":"` + project + `"},"network":{"egress":{"mode":"open"}}}}`
	status, answer = p.do(t, http.MethodPost, "/v1/sandboxes", strings.NewReader(create))
	if status != http.StatusCreated {
		t.Fatalf("the create answered %d: %s", status, answer)
	}
	var obj v1.Sandbox
	if err = json.Unmarshal([]byte(answer), &obj); err != nil {
		t.Fatal(err)
	}
	id := obj.Status.ID
	waitFor(t, "the sandbox to run on the host", func() bool { return sandboxNow(t, p, id).Status.Phase == driver.Running })
	obj = sandboxNow(t, p, id)
	if egress := obj.Spec.Network.Egress; egress.Mode != v1.EgressAllowlist || strings.Join(egress.AllowedHosts, ",") != "github.com" {
		t.Errorf("the sandbox resolved with the egress %+v, want the host's list", egress)
	}
	narrowed := false
	for _, warning := range obj.Status.Warnings {
		narrowed = narrowed || strings.HasPrefix(warning, manifest.WarningEgressNarrowed)
	}
	if !narrowed {
		t.Errorf("the sandbox carries no narrowing warning: %v", obj.Status.Warnings)
	}
	if obj.Status.Paths == nil || obj.Status.Paths.Home == "" {
		t.Errorf("the sandbox reports no scratch paths: %+v", obj.Status.Paths)
	}

	status, answer = p.do(t, http.MethodPost, "/v1/sandboxes/"+id+"/exec?wait=1",
		strings.NewReader(`{"command":["/bin/sh","-c","cat notes.txt; echo made here > made.txt"]}`))
	if status != http.StatusOK || !strings.Contains(answer, "from the host") {
		t.Fatalf("the exec answered %d: %s", status, answer)
	}
	if status, answer = p.do(t, http.MethodDelete, "/v1/sandboxes/"+id, nil); status != http.StatusNoContent && status != http.StatusAccepted {
		t.Fatalf("the delete answered %d: %s", status, answer)
	}
	waitFor(t, "the sandbox to be deleted", func() bool {
		status, _ := p.do(t, http.MethodGet, "/v1/sandboxes/"+id, nil)
		return status == http.StatusNotFound
	})
	for name, want := range map[string]string{"notes.txt": "from the host\n", "made.txt": "made here\n"} {
		if got, err := os.ReadFile(filepath.Join(project, name)); err != nil || string(got) != want {
			t.Errorf("after the delete the host folder holds %s as %q, %v", name, got, err)
		}
	}
}
