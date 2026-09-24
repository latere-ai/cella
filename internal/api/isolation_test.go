// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// containerDriver stands in for a container backend over the native one: it
// reports the isolation class and the name a podman or k8s driver would, and
// hands the native driver a spec with the image cleared, since the native
// driver runs none.
type containerDriver struct {
	runtime.Driver
	mu     sync.Mutex
	images []string
}

func (d *containerDriver) Name() string      { return "fake-container" }
func (d *containerDriver) Isolation() string { return v1.IsolationContainer }
func (d *containerDriver) Create(ctx context.Context, s runtime.CreateSpec) (runtime.Ref, error) {
	d.mu.Lock()
	d.images = append(d.images, s.Image)
	d.mu.Unlock()
	s.Image = ""
	return d.Driver.Create(ctx, s)
}

// TestAContainerEnvironmentRunsAnImage: the server resolves a create against
// the environment its driver provides, so a container backend accepts
// spec.image and reports its own isolation. Before this the server resolved
// every create as native and a k8s installation refused every image.
func TestAContainerEnvironmentRunsAnImage(t *testing.T) {
	d := &containerDriver{}
	f := setupDriver(t, nil, func(inner runtime.Driver) runtime.Driver {
		d.Driver = inner
		return d
	})
	body := strings.Replace(strings.Replace(createBody, `"name":"work"`, `"name":"imaged"`, 1), `"spec":{}`, `"spec":{"image":"registry.example/app:1"}`, 1)
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes?wait=1", f.alice, body, 201), &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Status.Isolation != v1.IsolationContainer || obj.Status.Driver != "fake-container" {
		t.Fatalf("status reads driver %q isolation %q, want the backend's", obj.Status.Driver, obj.Status.Isolation)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.images) != 1 || d.images[0] != "registry.example/app:1" {
		t.Fatalf("the driver was asked for %v, want the manifest's image", d.images)
	}
}
