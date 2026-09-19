// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	driver "latere.ai/x/cella/runtime"
)

func TestArchiveRoundTripAndContainment(t *testing.T) {
	ctx := context.Background()
	d, root := fresh(t)
	create(t, d, "one")
	create(t, d, "two")
	check(t, d.ImportTar(ctx, "one", "/workspace", bytes.NewReader(archive(t, "folder/", "", tar.TypeDir))))
	check(t, d.ImportTar(ctx, "one", "/workspace/folder", bytes.NewReader(archive(t, "data", "payload", tar.TypeReg))))
	var b bytes.Buffer
	check(t, d.ExportTar(ctx, "one", nil, &b))
	check(t, d.ImportTar(ctx, "two", "/workspace", &b))
	got, err := os.ReadFile(filepath.Join(root, "two", "workspace", "folder", "data"))
	check(t, err)
	if string(got) != "payload" {
		t.Fatal(string(got))
	}
	for _, p := range []string{"/tmp", "relative", "/workspace/../escape", "/workspace\x00evil"} {
		if err = d.ImportTar(ctx, "one", p, strings.NewReader("")); !errors.Is(err, driver.ErrInvalid) {
			t.Fatal(p, err)
		}
		if err = d.ExportTar(ctx, "one", []string{p}, io.Discard); !errors.Is(err, driver.ErrInvalid) {
			t.Fatal(p, err)
		}
	}
	for _, name := range []string{"../escape", "/absolute", "folder/../../escape", "bad\\name"} {
		err = d.ImportTar(ctx, "one", "/workspace", bytes.NewReader(archive(t, name, "payload", tar.TypeReg)))
		if !errors.Is(err, driver.ErrInvalid) {
			t.Fatal(name, err)
		}
	}
	for _, typ := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeFifo} {
		err = d.ImportTar(ctx, "one", "/workspace", bytes.NewReader(archive(t, "link", "", typ)))
		if !errors.Is(err, driver.ErrInvalid) {
			t.Fatal(typ, err)
		}
	}
	outside := t.TempDir()
	check(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600))
	check(t, os.Symlink(outside, filepath.Join(root, "one", "workspace", "escape")))
	if err = d.ImportTar(ctx, "one", "/workspace/escape", bytes.NewReader(archive(t, "secret", "overwrite", tar.TypeReg))); err == nil {
		t.Fatal("escaped import destination")
	}
	if err = d.ImportTar(ctx, "one", "/workspace", bytes.NewReader(archive(t, "escape/secret", "overwrite", tar.TypeReg))); err == nil {
		t.Fatal("escaped import entry")
	}
	if err = d.ExportTar(ctx, "one", []string{"/workspace/escape"}, io.Discard); err == nil {
		t.Fatal("exported link")
	}
	if err = d.ExportTar(ctx, "one", []string{"/workspace/escape/secret"}, io.Discard); err == nil {
		t.Fatal("exported escaped link target")
	}
	got, err = os.ReadFile(filepath.Join(outside, "secret"))
	check(t, err)
	if string(got) != "secret" {
		t.Fatal("outside mutated")
	}
}
func TestArchiveFailuresPreserveDestination(t *testing.T) {
	ctx := context.Background()
	d, root := fresh(t)
	create(t, d, "one")
	check(t, d.ImportTar(ctx, "one", "/workspace", bytes.NewReader(archive(t, "existing", "original", tar.TypeReg))))
	payload := archive(t, "existing", "replacement", tar.TypeReg)
	if err := d.ImportTar(ctx, "one", "/workspace", bytes.NewReader(payload[:515])); err == nil {
		t.Fatal("accepted truncated payload")
	}
	got, err := os.ReadFile(filepath.Join(root, "one", "workspace", "existing"))
	check(t, err)
	if string(got) != "original" {
		t.Fatal("partial write replaced destination")
	}
	entries, err := os.ReadDir(filepath.Join(root, "one", "workspace"))
	check(t, err)
	if len(entries) != 1 {
		t.Fatal("temporary file leaked", entries)
	}
	if err = d.ImportTar(ctx, "one", "/workspace", strings.NewReader("corrupt")); err == nil {
		t.Fatal("accepted corrupt archive")
	}
	if err = d.ImportTar(ctx, "one", "/workspace/existing/sub", strings.NewReader("")); err == nil {
		t.Fatal("accepted file parent")
	}
	if err = d.ImportTar(ctx, "one", "/workspace", bytes.NewReader(archive(t, "existing/sub", "", tar.TypeDir))); err == nil {
		t.Fatal("accepted file parent directory")
	}
	if err = d.ImportTar(ctx, "one", "/workspace", bytes.NewReader(archive(t, "existing/sub", "data", tar.TypeReg))); err == nil {
		t.Fatal("accepted file parent regular")
	}
	if err = d.ImportTar(ctx, "absent", "/workspace", strings.NewReader("")); !errors.Is(err, driver.ErrNotFound) {
		t.Fatal(err)
	}
	if err = d.ExportTar(ctx, "one", []string{"/workspace/missing"}, io.Discard); err == nil {
		t.Fatal("accepted missing path")
	}
	if err = d.ExportTar(ctx, "one", nil, brokenWriter{}); err == nil {
		t.Fatal("ignored destination failure")
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	check(t, tw.WriteHeader(&tar.Header{Name: "huge", Mode: 0600, Size: MaxArchiveBytes + 1}))
	if err = d.ImportTar(ctx, "one", "/workspace", &b); !errors.Is(err, driver.ErrInvalid) {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	r := &contextReader{ctx: cancelled, r: strings.NewReader("body")}
	if _, err = r.Read(make([]byte, 8)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("destination failed") }
func TestCorruptRecordAndFilesystemErrors(t *testing.T) {
	if _, err := New(""); !errors.Is(err, driver.ErrInvalid) {
		t.Fatal(err)
	}
	d, root := fresh(t)
	create(t, d, "one")
	ctx := context.Background()
	if _, err := New(filepath.Join(root, "one", "record.json")); err == nil {
		t.Fatal("file root accepted")
	}
	recordPath := filepath.Join(root, "one", "record.json")
	check(t, os.WriteFile(recordPath, []byte("invalid"), 0600))
	if _, err := d.Inspect(ctx, "one"); err == nil {
		t.Fatal("accepted corrupt record")
	}
	if _, err := d.List(ctx, driver.Filter{}); err == nil {
		t.Fatal("list ignored corrupt record")
	}
	check(t, os.Remove(recordPath))
	check(t, os.Mkdir(recordPath, 0700))
	if _, err := d.Inspect(ctx, "one"); err == nil {
		t.Fatal("record directory accepted")
	}
	check(t, os.RemoveAll(root))
	if err := d.Ready(ctx); err == nil {
		t.Fatal("missing root accepted")
	}
	if _, err := d.List(ctx, driver.Filter{}); err == nil {
		t.Fatal("missing root listed")
	}
	if _, err := d.Create(ctx, driver.CreateSpec{ID: "two"}); err == nil {
		t.Fatal("missing root create")
	}
}
