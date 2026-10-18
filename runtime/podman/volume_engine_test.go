// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// engineDriver is a driver over a real engine, or a skip naming why there is
// none, as the conformance suite skips.
func engineDriver(t *testing.T) *Driver {
	t.Helper()
	d, err := New(Options{Socket: os.Getenv("CELLA_PODMAN_SOCKET")})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Preflight(t.Context()); err != nil {
		t.Skipf("no podman engine for the volume case: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// pinnedConformanceImage is the suite's own image pinned by the digest the
// engine holds for it, which is the one form a volume's image source takes.
// The image is pulled when the engine does not have it.
func pinnedConformanceImage(t *testing.T, d *Driver) string {
	t.Helper()
	digest, err := d.imageDigest(t.Context(), conformanceImage)
	if err != nil {
		t.Skipf("the engine cannot provide %s: %v", conformanceImage, err)
	}
	repository, _, _ := strings.Cut(conformanceImage, ":")
	return repository + "@" + digest
}

// run is one command in a sandbox, with what it wrote and how it ended.
func run(t *testing.T, d *Driver, id string, command ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	e, err := d.Exec(ctx, id, driver.ExecRequest{Command: command})
	if err != nil {
		t.Fatalf("exec %v: %v", command, err)
	}
	var out, errs bytes.Buffer
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = io.Copy(&out, e.Stdout()) })
	wg.Go(func() { _, _ = io.Copy(&errs, e.Stderr()) })
	wg.Wait()
	code, err := e.Wait(ctx)
	if err != nil {
		t.Fatalf("exec %v: %v", command, err)
	}
	return out.String() + errs.String(), code
}

// TestVolumesOnARealEngine holds spec 019's volumes to the engine: an empty
// volume and one filled from a pinned image are Available, a second create
// is ErrAlreadyExists, a sandbox reads the image's files through a read-only
// mount and cannot write them while it writes the empty one, an image larger
// than its volume fails SourceTooLarge and leaves nothing, no volume reads as
// a sandbox, a mounted volume is not deleted from under its sandbox, and a
// delete leaves no object of the volume behind.
func TestVolumesOnARealEngine(t *testing.T) {
	d := engineDriver(t)
	ctx := t.Context()
	image := pinnedConformanceImage(t, d)
	const (
		empty   = "vol_engineempty"
		filled  = "vol_engineimage"
		small   = "vol_enginesmall"
		sandbox = "sbx_enginevolumes"
	)
	t.Cleanup(func() {
		clean := context.WithoutCancel(ctx)
		_ = d.Delete(clean, sandbox)
		for _, id := range []string{empty, filled, small} {
			_ = d.DeleteVolume(clean, id)
		}
	})

	if _, err := d.CreateVolume(ctx, driver.VolumeSpec{ID: empty, Name: "scratch", Owner: "alice", Size: "64Mi",
		Access: driver.AccessSingle, Source: driver.VolumeSource{Kind: driver.SourceEmpty}}); err != nil {
		t.Fatalf("the empty volume: %v", err)
	}
	spec := driver.VolumeSpec{ID: filled, Name: "grader", Owner: "alice", Size: "256Mi",
		Access: driver.AccessSharedRead, Source: driver.VolumeSource{Kind: driver.SourceImage, Image: image}}
	if _, err := d.CreateVolume(ctx, spec); err != nil {
		t.Fatalf("the image volume: %v", err)
	}
	for _, id := range []string{empty, filled} {
		state, err := d.InspectVolume(ctx, id)
		if err != nil || state.Phase != driver.VolumeAvailable {
			t.Fatalf("%s reads %+v, %v; want Available", id, state, err)
		}
	}
	if _, err := d.CreateVolume(ctx, spec); !errors.Is(err, driver.ErrAlreadyExists) {
		t.Fatalf("a second create of a filled volume answered %v, want ErrAlreadyExists", err)
	}
	for _, name := range []string{fillSource(filled), fillTarget(filled)} {
		if err := d.client().json(ctx, http.MethodGet, "/containers/"+name+"/json", nil, nil); !notFound(err) {
			t.Errorf("the fill container %s outlived the fill: %v", name, err)
		}
	}

	if _, err := d.Create(ctx, driver.CreateSpec{ID: sandbox, Name: "reader", Owner: "alice", Image: conformanceImage,
		Volumes: []driver.Mount{
			{Name: "grader", VolumeID: filled, Path: "/opt/grader", ReadOnly: true},
			{Name: "scratch", VolumeID: empty, Path: "/data"},
		}}); err != nil {
		t.Fatalf("a sandbox mounting both: %v", err)
	}
	if out, code := run(t, d, sandbox, "cat", "/opt/grader/etc/alpine-release"); code != 0 || strings.TrimSpace(out) == "" {
		t.Errorf("the image's file through the mount: %q, exit %d", out, code)
	}
	if out, code := run(t, d, sandbox, "touch", "/opt/grader/written"); code == 0 {
		t.Errorf("a read-only mount took a write: %q", out)
	}
	if out, code := run(t, d, sandbox, "sh", "-c", "ls -A /data | wc -l"); code != 0 || strings.TrimSpace(out) != "0" {
		t.Errorf("the empty volume holds %q entries, exit %d; want none", out, code)
	}
	if out, code := run(t, d, sandbox, "touch", "/data/written"); code != 0 {
		t.Errorf("a read-write mount refused a write: %q, exit %d", out, code)
	}

	states, err := d.List(ctx, driver.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if strings.HasPrefix(s.ID, "vol_") {
			t.Errorf("the volume %s reads as a sandbox", s.ID)
		}
	}

	if _, err := d.CreateVolume(ctx, driver.VolumeSpec{ID: small, Size: "1Ki", Access: driver.AccessSharedRead,
		Source: driver.VolumeSource{Kind: driver.SourceImage, Image: image}}); err == nil {
		t.Fatal("an image larger than its volume filled it")
	} else if fill, ok := errors.AsType[*driver.FillError](err); !ok || fill.Reason != driver.ReasonSourceTooLarge {
		t.Fatalf("an image larger than its volume answered %v, want SourceTooLarge", err)
	}
	if _, err := d.InspectVolume(ctx, small); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a failed fill left its volume: %v", err)
	}

	if err := d.DeleteVolume(ctx, filled); err == nil {
		t.Error("a volume a sandbox mounts was deleted from under it")
	}
	if _, err := d.Inspect(ctx, sandbox); err != nil {
		t.Errorf("the sandbox went with the refused delete: %v", err)
	}
	if err := d.Delete(ctx, sandbox); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{empty, filled} {
		if err := d.DeleteVolume(ctx, id); err != nil {
			t.Fatalf("deleting %s: %v", id, err)
		}
		if _, err := d.InspectVolume(ctx, id); !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("%s reads %v after its delete", id, err)
		}
		for _, name := range []string{dataVolume(id), volumeRecord(id)} {
			if _, err := d.inspectVolume(ctx, name); !errors.Is(err, driver.ErrNotFound) {
				t.Errorf("%s outlived the delete: %v", name, err)
			}
		}
	}
	if err := d.DeleteVolume(ctx, filled); err != nil {
		t.Errorf("a delete of a volume already gone answered %v", err)
	}
}
