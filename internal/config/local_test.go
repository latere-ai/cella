// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"reflect"
	"strings"
	"testing"

	"latere.ai/x/cella/runtime/local"
)

// TestLoadLocal: the CELLA_LOCAL_* variables fill the local driver's host
// policy in both roles, a grant ending in neither :rw nor :ro is read-only,
// the network defaults to none, a network the driver does not offer stops
// the start, and so does a deny list on a network other than any. They are
// not read for another runtime.
func TestLoadLocal(t *testing.T) {
	vars := map[string]string{
		"CELLA_RUNTIME":          "local",
		"CELLA_LOCAL_GRANTS":     "/Users/dev/site:rw, /Users/dev/docs:ro,/Users/dev/notes",
		"CELLA_LOCAL_READ_PATHS": "~/.gitconfig,~/.rustup",
		"CELLA_LOCAL_DENY":       "~/site/.env.production",
		"CELLA_LOCAL_NETWORK":    "allowlist",
		"CELLA_LOCAL_HOSTS":      "github.com,proxy.golang.org",
	}
	want := local.Options{
		Grants:    []local.Grant{{Path: "/Users/dev/site", Write: true}, {Path: "/Users/dev/docs"}, {Path: "/Users/dev/notes"}},
		ReadPaths: []string{"~/.gitconfig", "~/.rustup"},
		Deny:      []string{"~/site/.env.production"},
		Network:   local.Network{Mode: local.NetworkAllowlist, Hosts: []string{"github.com", "proxy.golang.org"}},
	}
	served, err := Load(env(identity(t, vars)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(served.Local, want) {
		t.Errorf("the control plane read %+v, want %+v", served.Local, want)
	}
	worker, err := LoadWorker(workerEnv(vars))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(worker.Local, want) {
		t.Errorf("the worker read %+v, want %+v", worker.Local, want)
	}

	quiet, err := LoadWorker(workerEnv(map[string]string{"CELLA_RUNTIME": "local"}))
	if err != nil || quiet.Local.Network.Mode != local.NetworkNone || len(quiet.Local.Grants) != 0 {
		t.Errorf("a local worker with no policy read %+v, %v", quiet.Local, err)
	}
	if _, err = LoadWorker(workerEnv(map[string]string{"CELLA_RUNTIME": "local", "CELLA_LOCAL_NETWORK": "open"})); err == nil ||
		!strings.Contains(err.Error(), "CELLA_LOCAL_NETWORK") {
		t.Errorf("an open network answered %v", err)
	}
	anyHost, err := LoadWorker(workerEnv(map[string]string{
		"CELLA_RUNTIME": "local", "CELLA_LOCAL_NETWORK": "any", "CELLA_LOCAL_HOSTS": "github.com:22", "CELLA_LOCAL_BLOCKED": "pastebin.com, *.example.net",
	}))
	if want := (local.Network{Mode: local.NetworkAny, Hosts: []string{"github.com:22"}, Blocked: []string{"pastebin.com", "*.example.net"}}); err != nil ||
		!reflect.DeepEqual(anyHost.Local.Network, want) {
		t.Errorf("a network of any host read %+v, %v", anyHost.Local.Network, err)
	}
	if _, err = LoadWorker(workerEnv(map[string]string{"CELLA_RUNTIME": "local", "CELLA_LOCAL_NETWORK": "allowlist", "CELLA_LOCAL_BLOCKED": "pastebin.com"})); err == nil ||
		!strings.Contains(err.Error(), "CELLA_LOCAL_BLOCKED") {
		t.Errorf("a deny list on an allow list answered %v", err)
	}
	other, err := LoadWorker(workerEnv(map[string]string{"CELLA_LOCAL_GRANTS": "/Users/dev/site:rw"}))
	if err != nil || len(other.Local.Grants) != 0 {
		t.Errorf("a podman worker read the local policy %+v, %v", other.Local, err)
	}
}
