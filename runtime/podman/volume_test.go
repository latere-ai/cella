// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	driver "latere.ai/x/cella/runtime"
)

// testDigest is a digest of the right shape for a pinned reference the fake
// resolves by name.
const testDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

// archiveOf builds a tar of the given headers, each regular file's body its
// own name repeated to its size.
func archiveOf(t *testing.T, headers ...tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range headers {
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// entries reads an archive back as its headers.
func entries(t *testing.T, archive []byte) []tar.Header {
	t.Helper()
	var out []tar.Header
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, *h)
	}
}

func fillReason(err error) string {
	if fill, ok := errors.AsType[*driver.FillError](err); ok {
		return fill.Reason
	}
	return ""
}

// TestReframeRootKeepsFilesAndDropsDevices: directories, regular files and
// both kinds of link pass, with their modes and owners; device nodes, fifos
// and sockets are dropped; extended attributes are dropped; the root entry
// and a leading slash are taken off the names.
func TestReframeRootKeepsFilesAndDropsDevices(t *testing.T) {
	in := archiveOf(t,
		tar.Header{Name: "./", Mode: 0o755, Typeflag: tar.TypeDir},
		tar.Header{Name: "/etc/", Mode: 0o755, Typeflag: tar.TypeDir},
		tar.Header{Name: "etc/config", Mode: 0o640, Uid: 7, Gid: 8, Typeflag: tar.TypeReg, Size: 3,
			PAXRecords: map[string]string{"SCHILY.xattr.security.capability": "cap"}},
		tar.Header{Name: "etc/link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/etc/config"},
		tar.Header{Name: "etc/hard", Mode: 0o640, Typeflag: tar.TypeLink, Linkname: "./etc/config"},
		tar.Header{Name: "dev/zero", Mode: 0o666, Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 5},
		tar.Header{Name: "dev/sda", Mode: 0o660, Typeflag: tar.TypeBlock},
		tar.Header{Name: "run/fifo", Mode: 0o600, Typeflag: tar.TypeFifo},
	)
	var out bytes.Buffer
	if err := reframeRoot(t.Context(), bytes.NewReader(in), &out, 1<<20); err != nil {
		t.Fatal(err)
	}
	got := entries(t, out.Bytes())
	var names []string
	for _, h := range got {
		names = append(names, h.Name)
		if len(h.PAXRecords) > 0 {
			t.Errorf("%s kept its extended attributes %v", h.Name, h.PAXRecords)
		}
	}
	if want := []string{"etc/", "etc/config", "etc/link", "etc/hard"}; !slices.Equal(names, want) {
		t.Fatalf("the reframed root holds %v, want %v", names, want)
	}
	if h := got[1]; h.Mode != 0o640 || h.Uid != 7 || h.Gid != 8 || h.Size != 3 {
		t.Errorf("the file lost its mode, owner or size: %+v", h)
	}
	if got[2].Linkname != "/etc/config" {
		t.Errorf("a symbolic link's target was rewritten to %q", got[2].Linkname)
	}
	if got[3].Linkname != "etc/config" {
		t.Errorf("a hard link's target reads %q, want etc/config", got[3].Linkname)
	}
}

// TestReframeRootRefusesWhatLeavesTheVolume: a name or a hard link's target
// that climbs out of the root is SourceUnsupported, and so is a root that is
// a file.
func TestReframeRootRefusesWhatLeavesTheVolume(t *testing.T) {
	for name, header := range map[string]tar.Header{
		"a name that climbs":        {Name: "../escape", Mode: 0o644, Typeflag: tar.TypeReg},
		"a name that climbs inside": {Name: "etc/../../escape", Mode: 0o644, Typeflag: tar.TypeReg},
		"a hard link that climbs":   {Name: "etc/hard", Mode: 0o644, Typeflag: tar.TypeLink, Linkname: "../etc/passwd"},
		"a hard link to the root":   {Name: "etc/hard", Mode: 0o644, Typeflag: tar.TypeLink, Linkname: "./"},
		"a root that is a file":     {Name: ".", Mode: 0o644, Typeflag: tar.TypeReg},
	} {
		t.Run(name, func(t *testing.T) {
			err := reframeRoot(t.Context(), bytes.NewReader(archiveOf(t, header)), io.Discard, 1<<20)
			if fillReason(err) != driver.ReasonSourceUnsupported {
				t.Fatalf("answered %v, want SourceUnsupported", err)
			}
		})
	}
}

// TestReframeRootStopsAtTheSize: regular files are counted against the
// volume's size, and the file that passes it is SourceTooLarge; a link counts
// nothing.
func TestReframeRootStopsAtTheSize(t *testing.T) {
	fits := archiveOf(t,
		tar.Header{Name: "a", Mode: 0o644, Typeflag: tar.TypeReg, Size: 6},
		tar.Header{Name: "b", Mode: 0o644, Typeflag: tar.TypeLink, Linkname: "a"},
		tar.Header{Name: "c", Mode: 0o644, Typeflag: tar.TypeReg, Size: 4},
	)
	if err := reframeRoot(t.Context(), bytes.NewReader(fits), io.Discard, 10); err != nil {
		t.Fatalf("files that fill the volume exactly were refused: %v", err)
	}
	if err := reframeRoot(t.Context(), bytes.NewReader(fits), io.Discard, 9); fillReason(err) != driver.ReasonSourceTooLarge {
		t.Fatalf("files past the size answered %v, want SourceTooLarge", err)
	}
}

// TestReframeRootStopsWithItsContext: a fill whose caller has gone stops
// copying.
func TestReframeRootStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	in := archiveOf(t, tar.Header{Name: "a", Mode: 0o644, Typeflag: tar.TypeReg, Size: 1})
	if err := reframeRoot(ctx, bytes.NewReader(in), io.Discard, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("answered %v, want the context's error", err)
	}
	if err := reframeRoot(t.Context(), strings.NewReader("not an archive at all, long enough to be a header"), io.Discard, 10); err == nil {
		t.Fatal("a body that is not an archive was copied")
	}
}

// imageSpec is a volume filled from the fake's image of this name.
func imageSpec(id, image, size string) driver.VolumeSpec {
	return driver.VolumeSpec{ID: id, Name: "grader", Owner: "alice", Size: size, Access: driver.AccessSharedRead,
		Source: driver.VolumeSource{Kind: driver.SourceImage, Image: image + "@" + testDigest}}
}

// TestCreateVolumeFillsFromAnImage: the image is pulled, its export lands in
// the data volume without the device node and the fifo, the two fill
// containers are gone, the record exists, the volume reads Available at its
// size, and a second create is ErrAlreadyExists.
func TestCreateVolumeFillsFromAnImage(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	spec := imageSpec("vol_a", "registry.example/grader", "1Mi")
	if _, err := d.CreateVolume(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	labels := f.labelsOf(t, dataVolume("vol_a"))
	if labels[labelKind] != kindVolume || labels[labelID] != "vol_a" || labels[labelImage] != spec.Source.Image ||
		labels[labelSize] != "1Mi" || labels[labelAccess] != driver.AccessSharedRead || labels[labelOwner] != "alice" {
		t.Errorf("the data volume carries %v", labels)
	}
	if f.labelsOf(t, volumeRecord("vol_a"))[labelKind] != kindVolumeRecord {
		t.Error("the record does not name its kind")
	}
	f.mu.Lock()
	data := f.volumeData[dataVolume("vol_a")]
	f.mu.Unlock()
	if got := slices.Sorted(maps.Keys(data)); !slices.Equal(got, []string{"bin", "bin/check", "bin/grade", "bin/score", "dev"}) {
		t.Errorf("the volume holds %v", got)
	}
	if string(data["bin/grade"].body) != "grade" || data["bin/check"].symlink != "/bin/grade" || data["bin/score"].hardlink != "bin/grade" {
		t.Errorf("the volume's files are %+v", data)
	}
	if n := f.containerCount(); n != 0 {
		t.Errorf("the fill left %d containers", n)
	}
	state, err := d.InspectVolume(t.Context(), "vol_a")
	if err != nil || state != (driver.VolumeState{ID: "vol_a", Phase: driver.VolumeAvailable, Capacity: "1Mi"}) {
		t.Errorf("the volume reads %+v, %v", state, err)
	}
	if _, err := d.CreateVolume(t.Context(), spec); !errors.Is(err, driver.ErrAlreadyExists) {
		t.Errorf("a second create answered %v, want ErrAlreadyExists", err)
	}
}

// TestCreateVolumeEmpty: an empty volume is the data volume and its record,
// with no container and no pull.
func TestCreateVolumeEmpty(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	if _, err := d.CreateVolume(t.Context(), driver.VolumeSpec{ID: "vol_e", Size: "1Gi", Access: driver.AccessSingle,
		Source: driver.VolumeSource{Kind: driver.SourceEmpty}}); err != nil {
		t.Fatal(err)
	}
	if got := f.volumeNames(); !slices.Equal(got, []string{dataVolume("vol_e"), volumeRecord("vol_e")}) {
		t.Errorf("the engine holds %v", got)
	}
	if f.pulls != 0 || f.containerCount() != 0 {
		t.Errorf("an empty volume pulled %d images and left %d containers", f.pulls, f.containerCount())
	}
}

// TestCreateVolumeRefillsWhatACrashLeft: a data volume with no record and the
// fill containers of a process that died are removed, and the fill runs again
// to the end; a record with no data volume is removed before a fill as well.
func TestCreateVolumeRefillsWhatACrashLeft(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	spec := imageSpec("vol_c", "registry.example/grader", "1Mi")
	f.mu.Lock()
	f.images[spec.Source.Image] = testDigest
	f.volumes[dataVolume("vol_c")] = map[string]string{labelID: "vol_c", labelKind: kindVolume}
	f.volumeData[dataVolume("vol_c")] = map[string]fakeFile{"half-written": {body: []byte("x")}}
	f.containers[fillSource("vol_c")] = &fakeContainer{image: spec.Source.Image, labels: map[string]string{}, released: make(chan struct{})}
	f.containers[fillTarget("vol_c")] = &fakeContainer{image: spec.Source.Image, labels: map[string]string{}, released: make(chan struct{}),
		volumes: []namedVolume{{Name: dataVolume("vol_c"), Dest: fillMount}}}
	f.volumes[volumeRecord("vol_s")] = map[string]string{labelID: "vol_s", labelKind: kindVolumeRecord}
	f.mu.Unlock()
	if state, err := d.InspectVolume(t.Context(), "vol_c"); err != nil || state.Phase != driver.VolumePending {
		t.Fatalf("a volume with no record reads %+v, %v; want Pending", state, err)
	}
	if _, err := d.CreateVolume(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	_, stale := f.volumeData[dataVolume("vol_c")]["half-written"]
	f.mu.Unlock()
	if stale {
		t.Error("the refill kept what the dead fill wrote")
	}
	if state, err := d.InspectVolume(t.Context(), "vol_c"); err != nil || state.Phase != driver.VolumeAvailable {
		t.Errorf("the refilled volume reads %+v, %v", state, err)
	}
	if _, err := d.CreateVolume(t.Context(), driver.VolumeSpec{ID: "vol_s", Size: "1Gi", Access: driver.AccessSingle,
		Source: driver.VolumeSource{Kind: driver.SourceEmpty}}); err != nil {
		t.Fatalf("a create over a stale record: %v", err)
	}
}

// TestCreateVolumeFailures: an image that cannot be pulled is
// SourceUnreachable, an image larger than its volume SourceTooLarge, and an
// engine that refuses a step is that step's error; each leaves no data
// volume, no record and no container behind.
func TestCreateVolumeFailures(t *testing.T) {
	big := archiveOf(t, tar.Header{Name: "big", Mode: 0o644, Typeflag: tar.TypeReg, Size: 2048})
	for _, tc := range []struct {
		name, image, size, fault, reason string
		skip                             int
	}{
		{name: "unpullable", image: "unpullable", size: "1Mi", reason: driver.ReasonSourceUnreachable},
		{name: "too large", image: "registry.example/big", size: "1Ki", reason: driver.ReasonSourceTooLarge},
		{name: "the source container", image: "registry.example/grader", size: "1Mi", fault: "POST " + libpod + "/containers/create"},
		{name: "the target container", image: "registry.example/grader", size: "1Mi", fault: "POST " + libpod + "/containers/create", skip: 1},
		{name: "the export", image: "registry.example/grader", size: "1Mi", fault: "GET " + libpod + "/containers/" + fillSource("vol_f") + "/export"},
		{name: "the archive", image: "registry.example/grader", size: "1Mi", fault: "PUT " + compat + "/containers/" + fillTarget("vol_f") + "/archive"},
		{name: "the record", image: "registry.example/grader", size: "1Mi", fault: "POST " + libpod + "/volumes/create", skip: 1},
		{name: "the data volume", image: "registry.example/grader", size: "1Mi", fault: "POST " + libpod + "/volumes/create"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.roots["registry.example/big@"+testDigest] = big
			d := f.driver(t)
			if tc.fault != "" {
				f.faultAfter(tc.fault, http.StatusInternalServerError, tc.skip)
			}
			_, err := d.CreateVolume(t.Context(), imageSpec("vol_f", tc.image, tc.size))
			if err == nil {
				t.Fatal("the fill succeeded")
			}
			if fillReason(err) != tc.reason {
				t.Errorf("answered %v, want the reason %q", err, tc.reason)
			}
			if got := f.volumeNames(); len(got) != 0 {
				t.Errorf("the failed fill left %v", got)
			}
			if n := f.containerCount(); n != 0 {
				t.Errorf("the failed fill left %d containers", n)
			}
		})
	}
}

// TestCreateVolumeRefusesWhatNoDriverActsOn: a spec Check refuses, a size
// that is no quantity, an id the engine cannot name, a source this driver
// does not fill, and a caller that has gone.
func TestCreateVolumeRefusesWhatNoDriverActsOn(t *testing.T) {
	d := newFake(t).driver(t)
	empty := driver.VolumeSource{Kind: driver.SourceEmpty}
	for name, spec := range map[string]driver.VolumeSpec{
		"no size":         {ID: "vol_x", Access: driver.AccessSingle, Source: empty},
		"a bad size":      {ID: "vol_x", Size: "lots", Access: driver.AccessSingle, Source: empty},
		"a zero size":     {ID: "vol_x", Size: "0", Access: driver.AccessSingle, Source: empty},
		"a bad id":        {ID: "vol x", Size: "1Gi", Access: driver.AccessSingle, Source: empty},
		"an unpinned one": {ID: "vol_x", Size: "1Gi", Access: driver.AccessSingle, Source: driver.VolumeSource{Kind: driver.SourceImage, Image: "alpine:3"}},
	} {
		if _, err := d.CreateVolume(t.Context(), spec); !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("%s: answered %v, want ErrInvalid", name, err)
		}
	}
	if _, err := d.CreateVolume(t.Context(), driver.VolumeSpec{ID: "vol_x", Size: "1Gi", Access: driver.AccessSingle,
		Source: driver.VolumeSource{Kind: "snapshot"}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("a snapshot source answered %v, want ErrUnsupported", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := d.CreateVolume(ctx, driver.VolumeSpec{ID: "vol_x", Size: "1Gi", Access: driver.AccessSingle, Source: empty}); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled create answered %v", err)
	}
	for _, err := range []error{
		func() error { _, err := d.InspectVolume(t.Context(), "vol x"); return err }(),
		d.DeleteVolume(t.Context(), "vol x"),
	} {
		if !errors.Is(err, driver.ErrInvalid) {
			t.Errorf("a bad id answered %v, want ErrInvalid", err)
		}
	}
	if _, err := d.InspectVolume(ctx, "vol_x"); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled read answered %v", err)
	}
	if err := d.DeleteVolume(ctx, "vol_x"); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled delete answered %v", err)
	}
	if _, err := d.InspectVolume(t.Context(), "vol_absent"); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("an absent volume reads %v, want ErrNotFound", err)
	}
}

// TestDeleteVolume: a delete removes the data volume and the record, a volume
// already gone is not an error, and a volume a sandbox's container mounts is
// refused by the engine and left as it was, the container with it.
func TestDeleteVolume(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	empty := driver.VolumeSource{Kind: driver.SourceEmpty}
	for _, id := range []string{"vol_d", "vol_m"} {
		if _, err := d.CreateVolume(t.Context(), driver.VolumeSpec{ID: id, Size: "1Gi", Access: driver.AccessSingle, Source: empty}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.DeleteVolume(t.Context(), "vol_d"); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteVolume(t.Context(), "vol_d"); err != nil {
		t.Errorf("a second delete answered %v", err)
	}
	if _, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_m", Image: "img",
		Volumes: []driver.Mount{{Name: "state", VolumeID: "vol_m", Path: "/data"}}}); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteVolume(t.Context(), "vol_m"); err == nil {
		t.Fatal("a mounted volume was deleted")
	}
	if state, err := d.InspectVolume(t.Context(), "vol_m"); err != nil || state.Phase != driver.VolumeAvailable {
		t.Errorf("the refused delete left the volume reading %+v, %v", state, err)
	}
	if _, err := d.Inspect(t.Context(), "sbx_m"); err != nil {
		t.Errorf("the refused delete took the sandbox: %v", err)
	}
	if got := f.volumeNames(); slices.Contains(got, dataVolume("vol_d")) || slices.Contains(got, volumeRecord("vol_d")) {
		t.Errorf("the deleted volume left %v", got)
	}
	f.fault("DELETE "+libpod+"/volumes/"+volumeRecord("vol_m"), http.StatusInternalServerError)
	if err := d.Delete(t.Context(), "sbx_m"); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteVolume(t.Context(), "vol_m"); err == nil {
		t.Error("a record the engine would not remove was reported removed")
	}
	f.fault("DELETE "+libpod+"/containers/"+fillSource("vol_m"), http.StatusInternalServerError)
	if err := d.DeleteVolume(t.Context(), "vol_m"); err == nil {
		t.Error("a fill container the engine would not remove was reported removed")
	}
}

// TestCreateMountsVolumes: a sandbox's mounts reach the container as named
// volumes, read-only where asked, with nothing copied into them from the
// image, and its workspace stays first.
func TestCreateMountsVolumes(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	empty := driver.VolumeSource{Kind: driver.SourceEmpty}
	for _, id := range []string{"vol_r", "vol_w"} {
		if _, err := d.CreateVolume(t.Context(), driver.VolumeSpec{ID: id, Size: "1Gi", Access: driver.AccessSingle, Source: empty}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_v", Image: "img", Volumes: []driver.Mount{
		{Name: "tools", VolumeID: "vol_r", Path: "/opt/tools", ReadOnly: true},
		{Name: "state", VolumeID: "vol_w", Path: "/data"},
	}}); err != nil {
		t.Fatal(err)
	}
	want := []namedVolume{
		{Name: workspaceVolume("sbx_v"), Dest: driver.DefaultWorkdir, Options: []string{"rw"}},
		{Name: dataVolume("vol_r"), Dest: "/opt/tools", Options: []string{"ro", "nocopy"}},
		{Name: dataVolume("vol_w"), Dest: "/data", Options: []string{"rw", "nocopy"}},
	}
	if got := f.containerOf(t, "sbx_v").volumes; !slices.EqualFunc(got, want, func(a, b namedVolume) bool {
		return a.Name == b.Name && a.Dest == b.Dest && slices.Equal(a.Options, b.Options)
	}) {
		t.Errorf("the container mounts %+v, want %+v", got, want)
	}
}

// TestCreateRefusesAMountItCannotHonor: a mount path that is not a clean
// absolute path below the root, one that overlaps the workspace, another
// mount or the control plane's prefix, a volume id the engine cannot name,
// and a volume that is absent or not filled each refuse the create with
// ErrInvalid and leave nothing behind.
func TestCreateRefusesAMountItCannotHonor(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	if _, err := d.CreateVolume(t.Context(), driver.VolumeSpec{ID: "vol_ok", Size: "1Gi", Access: driver.AccessSingle,
		Source: driver.VolumeSource{Kind: driver.SourceEmpty}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.volumes[dataVolume("vol_half")] = map[string]string{labelID: "vol_half", labelKind: kindVolume}
	f.mu.Unlock()
	mount := func(p, id string) driver.Mount { return driver.Mount{Name: "m", VolumeID: id, Path: p} }
	for name, mounts := range map[string][]driver.Mount{
		"a relative path":           {mount("data", "vol_ok")},
		"the root":                  {mount("/", "vol_ok")},
		"an unclean path":           {mount("/data/", "vol_ok")},
		"the workspace":             {mount("/workspace", "vol_ok")},
		"inside the workspace":      {mount("/workspace/data", "vol_ok")},
		"the control plane's":       {mount("/run/cella/x", "vol_ok")},
		"over the control plane's":  {mount("/run", "vol_ok")},
		"two mounts, one in other":  {mount("/data", "vol_ok"), mount("/data/more", "vol_ok")},
		"an id the engine refuses":  {mount("/data", "vol x")},
		"an absent volume":          {mount("/data", "vol_absent")},
		"a volume still being made": {mount("/data", "vol_half")},
	} {
		t.Run(name, func(t *testing.T) {
			before := f.volumeNames()
			if _, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_bad", Image: "img", Volumes: mounts}); !errors.Is(err, driver.ErrInvalid) {
				t.Fatalf("answered %v, want ErrInvalid", err)
			}
			if got := f.volumeNames(); !slices.Equal(got, before) || f.containerCount() != 0 {
				t.Errorf("the refused create left %v and %d containers", got, f.containerCount())
			}
		})
	}
	f.fault("GET "+libpod+"/volumes/"+volumeRecord("vol_ok"), http.StatusInternalServerError)
	if _, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_bad", Image: "img", Volumes: []driver.Mount{mount("/data", "vol_ok")}}); err == nil || errors.Is(err, driver.ErrInvalid) {
		t.Errorf("an engine that could not be read answered %v, want its own error", err)
	}
}

// TestVolumesNeverReadAsSandboxes: List over an engine holding volumes, their
// records and a fill's containers answers the sandboxes alone.
func TestVolumesNeverReadAsSandboxes(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	if _, err := d.CreateVolume(t.Context(), imageSpec("vol_l", "registry.example/grader", "1Mi")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(t.Context(), driver.CreateSpec{ID: "sbx_l", Image: "img",
		Volumes: []driver.Mount{{Name: "grader", VolumeID: "vol_l", Path: "/opt/grader", ReadOnly: true}}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.containers[fillSource("vol_x")] = &fakeContainer{labels: map[string]string{labelID: "vol_x", labelKind: kindFill},
		state: "created", released: make(chan struct{})}
	f.mu.Unlock()
	states, err := d.List(t.Context(), driver.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].ID != "sbx_l" {
		t.Fatalf("List answered %+v, want the one sandbox", states)
	}
}

// TestCapabilitiesDeclareVolumes: the driver keeps volumes and fills them
// from an image.
func TestCapabilitiesDeclareVolumes(t *testing.T) {
	caps := newFake(t).driver(t).Capabilities()
	if !caps.Volumes || !caps.VolumeImages {
		t.Fatalf("the podman driver declares %+v", caps)
	}
}
