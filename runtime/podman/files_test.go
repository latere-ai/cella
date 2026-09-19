// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"testing"

	driver "latere.ai/x/cella/runtime"
)

// entry is one archive member a test writes or reads back.
type entry struct {
	name, body, link string
	mode             int64
	typ              byte
}

func archive(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		size := int64(len(e.body))
		if typ != tar.TypeReg {
			size = 0
		}
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: mode, Size: size, Typeflag: typ, Linkname: e.link}); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func readArchive(t *testing.T, r io.Reader) map[string]entry {
	t.Helper()
	out := map[string]entry{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = entry{name: h.Name, body: string(body), mode: h.Mode, typ: h.Typeflag}
	}
}

func TestTarRoundTrip(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	create(t, d, driver.CreateSpec{ID: "sbx_b"})
	tree := []entry{
		{name: "tree/", typ: tar.TypeDir, mode: 0o755},
		{name: "tree/hello.txt", body: "hello", mode: 0o640},
		{name: "tree/bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "tree/bin/run.sh", body: "#!/bin/sh\n", mode: 0o755},
	}
	if err := d.ImportTar(t.Context(), "sbx_a", driver.DefaultWorkdir, bytes.NewReader(archive(t, tree...))); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := d.ExportTar(t.Context(), "sbx_a", []string{driver.DefaultWorkdir + "/tree"}, &out); err != nil {
		t.Fatal(err)
	}
	exported := out.Bytes()
	got := readArchive(t, bytes.NewReader(exported))
	for _, want := range tree {
		name := want.name
		if want.typ == tar.TypeDir {
			name = name[:len(name)-1]
		}
		e, ok := got[name]
		if !ok {
			t.Fatalf("the export omits %q: %v", name, slices.Sorted(maps.Keys(got)))
		}
		if e.body != want.body || e.mode&0o777 != want.mode {
			t.Errorf("export %q: body %q mode %o, want %q %o", name, e.body, e.mode, want.body, want.mode)
		}
	}
	// The same archive lands in a second sandbox, which is the round trip the
	// contract asks for.
	if err := d.ImportTar(t.Context(), "sbx_b", driver.DefaultWorkdir, bytes.NewReader(exported)); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := d.ExportTar(t.Context(), "sbx_b", nil, &out); err != nil {
		t.Fatal(err)
	}
	if body := readArchive(t, &out)["tree/hello.txt"].body; body != "hello" {
		t.Fatalf("the whole-workspace export of the second sandbox = %q", body)
	}
	// Two paths become one archive.
	out.Reset()
	if err := d.ExportTar(t.Context(), "sbx_a", []string{driver.DefaultWorkdir + "/tree/hello.txt", driver.DefaultWorkdir + "/tree/bin"}, &out); err != nil {
		t.Fatal(err)
	}
	joined := readArchive(t, &out)
	if _, ok := joined["tree/hello.txt"]; !ok {
		t.Fatalf("the joined export = %v", slices.Sorted(maps.Keys(joined)))
	}
	if _, ok := joined["tree/bin/run.sh"]; !ok {
		t.Fatalf("the joined export = %v", slices.Sorted(maps.Keys(joined)))
	}
}

func TestFilesWorkWhileStopped(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	if err := d.Stop(t.Context(), "sbx_a"); err != nil {
		t.Fatal(err)
	}
	if err := d.ImportTar(t.Context(), "sbx_a", driver.DefaultWorkdir, bytes.NewReader(archive(t, entry{name: "stopped.txt", body: "while stopped"}))); err != nil {
		t.Fatalf("ImportTar while Stopped: %v", err)
	}
	var out bytes.Buffer
	if err := d.ExportTar(t.Context(), "sbx_a", []string{driver.DefaultWorkdir + "/stopped.txt"}, &out); err != nil {
		t.Fatalf("ExportTar while Stopped: %v", err)
	}
	if body := readArchive(t, &out)["stopped.txt"].body; body != "while stopped" {
		t.Fatalf("the export while Stopped = %q", body)
	}
}

func TestImportRefusals(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a", Workspace: driver.Workspace{Path: "/space"}})
	for name, bad := range map[string]entry{
		"traversal": {name: "../escape", body: "x"},
		"absolute":  {name: "/abs", body: "x"},
		"symlink":   {name: "link", typ: tar.TypeSymlink, link: "../../escape"},
		"hardlink":  {name: "hard", typ: tar.TypeLink, link: "hello.txt"},
		"fifo":      {name: "pipe", typ: tar.TypeFifo},
		"backslash": {name: "a\\b", body: "x"},
	} {
		t.Run(name, func(t *testing.T) {
			err := d.ImportTar(t.Context(), "sbx_a", "/space", bytes.NewReader(archive(t, bad)))
			if !errors.Is(err, driver.ErrInvalid) {
				t.Fatalf("ImportTar %q: %v, want ErrInvalid", bad.name, err)
			}
		})
	}
	if files := f.container(t, "sbx_a").files; len(files) != 0 {
		t.Fatalf("a refused entry reached the engine: %v", slices.Sorted(maps.Keys(files)))
	}
	for name, dest := range map[string]string{"outside": "/tmp", "traversal": "/space/../etc", "relative": "sub"} {
		t.Run("dest_"+name, func(t *testing.T) {
			if err := d.ImportTar(t.Context(), "sbx_a", dest, bytes.NewReader(archive(t))); !errors.Is(err, driver.ErrInvalid) {
				t.Fatalf("ImportTar into %q: %v, want ErrInvalid", dest, err)
			}
		})
	}
	if err := d.ImportTar(t.Context(), "sbx_absent", "/workspace", bytes.NewReader(archive(t))); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("ImportTar into an unknown sandbox: %v", err)
	}
	if err := d.ImportTar(t.Context(), "sbx_a", "/space", bytes.NewReader([]byte("not a tar at all"))); !errors.Is(err, driver.ErrInvalid) {
		t.Fatal("a body that is not an archive was accepted")
	}
	// An entry larger than the bound is refused before it is copied.
	big := &tar.Header{Name: "big", Mode: 0o644, Size: MaxArchiveBytes + 1, Typeflag: tar.TypeReg}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(big)
	if err := d.ImportTar(t.Context(), "sbx_a", "/space", bytes.NewReader(b.Bytes())); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("an oversized entry: %v", err)
	}
}

func TestExportRefusals(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	for name, p := range map[string]string{"outside": "/etc", "traversal": "/workspace/../etc", "relative": "sub"} {
		t.Run(name, func(t *testing.T) {
			if err := d.ExportTar(t.Context(), "sbx_a", []string{p}, io.Discard); !errors.Is(err, driver.ErrInvalid) {
				t.Fatalf("ExportTar %q: %v, want ErrInvalid", p, err)
			}
		})
	}
	if err := d.ExportTar(t.Context(), "sbx_a", []string{driver.DefaultWorkdir + "/absent"}, io.Discard); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("ExportTar of a path that is not there: %v", err)
	}
	if _, err := d.workspacePath(t.Context(), "sbx_absent"); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("the workspace of an unknown sandbox: %v", err)
	}
	// A special file inside the workspace is named rather than carried.
	f.mu.Lock()
	f.containers[containerName("sbx_a")].files["/workspace/sock"] = fakeFile{mode: 0o644}
	f.mu.Unlock()
	f.special = "/workspace/sock"
	if err := d.ExportTar(t.Context(), "sbx_a", nil, io.Discard); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("exporting a special file: %v", err)
	}
}

func TestFileTransferReportsEngineFaults(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_a"})
	f.fault("PUT "+compat+"/containers/cella-sbx_a/archive", http.StatusInternalServerError)
	if err := d.ImportTar(t.Context(), "sbx_a", driver.DefaultWorkdir, bytes.NewReader(archive(t, entry{name: "x", body: "y"}))); err == nil {
		t.Fatal("a refused import read as success")
	}
	f.fault("PUT "+compat+"/containers/cella-sbx_a/archive", http.StatusNotFound)
	if err := d.ImportTar(t.Context(), "sbx_a", driver.DefaultWorkdir, bytes.NewReader(archive(t, entry{name: "x", body: "y"}))); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("an import into a container that went away: %v", err)
	}
	f.fault("GET "+compat+"/containers/cella-sbx_a/archive", http.StatusInternalServerError)
	if err := d.ExportTar(t.Context(), "sbx_a", nil, io.Discard); err == nil {
		t.Fatal("a refused export read as success")
	}
}
