// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
)

// workerEnv is the smallest environment a worker starts in: the control plane
// it connects outbound to, and the key that authenticates the stream.
func workerEnv(extra map[string]string) Getenv {
	values := map[string]string{
		"CELLA_URL":             "https://control.example.test",
		"CELLA_ENVIRONMENT_KEY": "a-key",
		"CELLA_RUNTIME":         "podman",
	}
	for k, v := range extra {
		if v == "" {
			delete(values, k)
			continue
		}
		values[k] = v
	}
	return func(name string) string { return values[name] }
}

func TestLoadWorker(t *testing.T) {
	c, err := LoadWorker(workerEnv(nil))
	if err != nil {
		t.Fatalf("the smallest worker configuration was refused: %v", err)
	}
	switch {
	case c.URL != "https://control.example.test":
		t.Errorf("the control plane URL is %q", c.URL)
	case c.EnvironmentKey != "a-key":
		t.Errorf("the environment key is %q", c.EnvironmentKey)
	case c.Runtime != RuntimePodman:
		t.Errorf("the driver is %q", c.Runtime)
	case c.DataDir != DefaultDataDir:
		t.Errorf("the data directory is %q, want the default", c.DataDir)
	}
	if !c.Capacity.IsZero() {
		t.Errorf("a worker that declares no capacity carries %+v", c.Capacity)
	}
	if c.Labels != nil {
		t.Errorf("a worker that declares no labels carries %v", c.Labels)
	}
}

// TestLoadWorkerCapacityAndLabels holds what a worker declares about itself:
// placement admits against the lesser of it and the environment's ceiling.
func TestLoadWorkerCapacityAndLabels(t *testing.T) {
	c, err := LoadWorker(workerEnv(map[string]string{
		"CELLA_CAPACITY_CPU":       "128",
		"CELLA_CAPACITY_MEMORY":    "512Gi",
		"CELLA_CAPACITY_DISK":      "5Ti",
		"CELLA_CAPACITY_SANDBOXES": "100",
		"CELLA_WORKER_LABELS":      "region=eu, gpu=true",
	}))
	if err != nil {
		t.Fatalf("the configuration was refused: %v", err)
	}
	want := v1.Capacity{CPU: "128", Memory: "512Gi", Disk: "5Ti", Sandboxes: 100}
	if c.Capacity != want {
		t.Errorf("the capacity is %+v, want %+v", c.Capacity, want)
	}
	if c.Labels["region"] != "eu" || c.Labels["gpu"] != "true" || len(c.Labels) != 2 {
		t.Errorf("the labels are %v", c.Labels)
	}
}

// TestLoadWorkerRefusals holds every configuration a worker cannot start on,
// each naming the variable an operator fixes.
func TestLoadWorkerRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		names string
	}{
		{"no control plane URL", map[string]string{"CELLA_URL": ""}, "CELLA_URL"},
		{"no environment key", map[string]string{"CELLA_ENVIRONMENT_KEY": ""}, "CELLA_ENVIRONMENT_KEY"},
		{"a control plane URL that is not one", map[string]string{"CELLA_URL": "control.example.test"}, "CELLA_URL"},
		{"http on a host that is not loopback", map[string]string{"CELLA_URL": "http://control.example.test"}, "CELLA_INSECURE_CONTROL_PLANE"},
		{"a scheme a control plane is not reached over", map[string]string{"CELLA_URL": "ftp://control.example.test"}, "CELLA_URL"},
		{"a driver this binary does not run", map[string]string{"CELLA_RUNTIME": "vm"}, "CELLA_RUNTIME"},
		{"native without the explicit consent", map[string]string{"CELLA_RUNTIME": "native"}, "CELLA_ALLOW_UNSAFE_NATIVE"},
		{"a podman socket that is not a path", map[string]string{"CELLA_PODMAN_SOCKET": "podman.sock"}, "CELLA_PODMAN_SOCKET"},
		{"a sandbox count that is not one", map[string]string{"CELLA_CAPACITY_SANDBOXES": "many"}, "CELLA_CAPACITY_SANDBOXES"},
		{"a negative sandbox count", map[string]string{"CELLA_CAPACITY_SANDBOXES": "-1"}, "CELLA_CAPACITY_SANDBOXES"},
		{"labels that are not pairs", map[string]string{"CELLA_WORKER_LABELS": "region"}, "CELLA_WORKER_LABELS"},
		{"a consent that is not a boolean", map[string]string{"CELLA_ALLOW_UNSAFE_NATIVE": "yes"}, "CELLA_ALLOW_UNSAFE_NATIVE"},
		{"a hatch that is not a boolean", map[string]string{"CELLA_INSECURE_CONTROL_PLANE": "yes"}, "CELLA_INSECURE_CONTROL_PLANE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadWorker(workerEnv(tc.env))
			if err == nil {
				t.Fatalf("this configuration was accepted and names no %s", tc.names)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the problem is %q and does not name %s", err, tc.names)
			}
		})
	}
}

// TestLoadWorkerAccepts holds the shapes a worker does start on: loopback
// over http, the explicit hatch, and native with its consent.
func TestLoadWorkerAccepts(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"http on loopback", map[string]string{"CELLA_URL": "http://127.0.0.1:8080"}},
		{"http on localhost", map[string]string{"CELLA_URL": "http://localhost:8080"}},
		{"http elsewhere with the hatch set", map[string]string{
			"CELLA_URL": "http://control.example.test", "CELLA_INSECURE_CONTROL_PLANE": "1"}},
		{"native with the consent", map[string]string{
			"CELLA_RUNTIME": "native", "CELLA_ALLOW_UNSAFE_NATIVE": "true"}},
		{"labels with an empty value", map[string]string{"CELLA_WORKER_LABELS": "spot="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadWorker(workerEnv(tc.env)); err != nil {
				t.Errorf("this configuration was refused: %v", err)
			}
		})
	}
}

// TestLoadWorkerReadsTheK8sOptionsOnlyForThatDriver holds spec 002's rule:
// a role carries no opinion about a cluster it does not drive.
func TestLoadWorkerReadsTheK8sOptionsOnlyForThatDriver(t *testing.T) {
	c, err := LoadWorker(workerEnv(map[string]string{"CELLA_K8S_NAMESPACE": "elsewhere"}))
	if err != nil {
		t.Fatalf("the configuration was refused: %v", err)
	}
	if c.K8s.Namespace != "" {
		t.Errorf("a podman worker read the namespace %q", c.K8s.Namespace)
	}
	c, err = LoadWorker(workerEnv(map[string]string{"CELLA_RUNTIME": "k8s", "CELLA_K8S_NAMESPACE": "elsewhere"}))
	if err != nil {
		t.Fatalf("the configuration was refused: %v", err)
	}
	if c.K8s.Namespace != "elsewhere" {
		t.Errorf("a k8s worker read the namespace %q", c.K8s.Namespace)
	}
}

// TestEnvironmentOfflineWindow holds spec 021's variable: how long an
// environment is live without a heartbeat.
func TestEnvironmentOfflineWindow(t *testing.T) {
	c, err := Load(env(identity(t, map[string]string{})))
	if err != nil {
		t.Fatalf("the configuration was refused: %v", err)
	}
	if c.EnvironmentOffline != DefaultEnvironmentOffline {
		t.Errorf("the offline window defaults to %s, want %s", c.EnvironmentOffline, DefaultEnvironmentOffline)
	}
	c, err = Load(env(identity(t, map[string]string{"CELLA_ENVIRONMENT_OFFLINE": "30s"})))
	if err != nil {
		t.Fatalf("the configuration was refused: %v", err)
	}
	if c.EnvironmentOffline.String() != "30s" {
		t.Errorf("the offline window is %s, want 30s", c.EnvironmentOffline)
	}
	if _, err = Load(env(identity(t, map[string]string{"CELLA_ENVIRONMENT_OFFLINE": "never"}))); err == nil ||
		!strings.Contains(err.Error(), "CELLA_ENVIRONMENT_OFFLINE") {
		t.Errorf("a window that is not a duration was accepted: %v", err)
	}
}
