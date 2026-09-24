// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/client"
)

// TestEveryFileRouteIsTheOneDesign008Names holds each granular call to its
// method, its route and its selector.
func TestEveryFileRouteIsTheOneDesign008Names(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/files/list"):
			_, _ = w.Write([]byte(`{"items":[{"name":"a.txt","path":"/workspace/a.txt","size":3,"mode":"0644","modTime":"2026-09-20T10:00:00Z","isDir":false}],"next":""}`))
		case strings.HasSuffix(r.URL.Path, "/files/stat"):
			_, _ = w.Write([]byte(`{"name":"a.txt","path":"/workspace/a.txt","size":3,"mode":"0600","modTime":"2026-09-20T10:00:00Z","isDir":false}`))
		case strings.HasSuffix(r.URL.Path, "/files/content"):
			_, _ = w.Write([]byte("abc"))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	c := f.client(client.Config{})
	ctx := t.Context()

	entries, raw, err := c.FileList(ctx, "dev", "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "a.txt" || entries[0].Mode != "0644" || entries[0].Size != 3 {
		t.Fatalf("the listing is %+v", entries)
	}
	if entries[0].ModTime.UTC().Format(time.RFC3339) != "2026-09-20T10:00:00Z" {
		t.Errorf("the entry's time is %v", entries[0].ModTime)
	}
	if !strings.Contains(string(raw), "a.txt") {
		t.Errorf("the answer's own bytes are %q", raw)
	}
	if seen := f.last(); seen.Method != "GET" || seen.Path != "/v1/sandboxes/dev/files/list" || seen.Query.Get("path") != "/workspace" {
		t.Fatalf("the listing called %s %s?%s", seen.Method, seen.Path, seen.Query.Encode())
	}

	entry, _, err := c.FileStat(ctx, "dev", "/workspace/a.txt")
	if err != nil || entry.Mode != "0600" {
		t.Fatalf("stat: %+v, %v", entry, err)
	}
	if seen := f.last(); seen.Path != "/v1/sandboxes/dev/files/stat" {
		t.Fatalf("stat called %s", seen.Path)
	}

	body, err := c.FileGet(ctx, "dev", "/workspace/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(body)
	if err != nil || string(content) != "abc" {
		t.Fatalf("the file holds %q, %v", content, err)
	}
	_ = body.Close()
	if seen := f.last(); seen.Path != "/v1/sandboxes/dev/files/content" {
		t.Fatalf("the read called %s", seen.Path)
	}

	if err = c.FilePut(ctx, "dev", "/workspace/b.txt", "0700", strings.NewReader("written")); err != nil {
		t.Fatal(err)
	}
	seen := f.last()
	if seen.Method != "PUT" || seen.Path != "/v1/sandboxes/dev/files" || seen.Query.Get("path") != "/workspace/b.txt" {
		t.Fatalf("the write called %s %s?%s", seen.Method, seen.Path, seen.Query.Encode())
	}
	if seen.Query.Get("mode") != "0700" || seen.Body != "written" {
		t.Fatalf("the write sent mode %q and %q", seen.Query.Get("mode"), seen.Body)
	}
	if seen.Query.Has("dest") {
		t.Error("the one-file write named dest, which is the archive selector")
	}
	// No mode named leaves the server's default rather than sending zero.
	if err = c.FilePut(ctx, "dev", "/workspace/b.txt", "", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if f.last().Query.Has("mode") {
		t.Error("a write with no mode named one")
	}

	if err = c.FileRemove(ctx, "dev", "/workspace/b.txt"); err != nil {
		t.Fatal(err)
	}
	if seen = f.last(); seen.Method != "DELETE" || seen.Query.Get("path") != "/workspace/b.txt" {
		t.Fatalf("the remove called %s %s?%s", seen.Method, seen.Path, seen.Query.Encode())
	}

	if err = c.FileMkdir(ctx, "dev", "/workspace/sub"); err != nil {
		t.Fatal(err)
	}
	if seen = f.last(); seen.Method != "POST" || seen.Path != "/v1/sandboxes/dev/files/mkdir" || seen.Body != `{"path":"/workspace/sub"}` {
		t.Fatalf("mkdir called %s %s with %s", seen.Method, seen.Path, seen.Body)
	}

	if err = c.FileMove(ctx, "dev", "/workspace/a.txt", "/workspace/sub/a.txt"); err != nil {
		t.Fatal(err)
	}
	if seen = f.last(); seen.Path != "/v1/sandboxes/dev/files/move" || !strings.Contains(seen.Body, `"from":"/workspace/a.txt"`) {
		t.Fatalf("move called %s with %s", seen.Path, seen.Body)
	}
}

// TestARefusalOnAFileRouteReachesTheCaller: a 204 route that refuses still
// answers the envelope, and the client reports it rather than reporting
// success.
func TestARefusalOnAFileRouteReachesTheCaller(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, 400, "invalid_field", "A field has a value it cannot take.", nil)
	})
	c := f.client(client.Config{})
	ctx := t.Context()
	for name, run := range map[string]func() error{
		"write":  func() error { return c.FilePut(ctx, "dev", "/etc/passwd", "", strings.NewReader("x")) },
		"remove": func() error { return c.FileRemove(ctx, "dev", "/etc") },
		"mkdir":  func() error { return c.FileMkdir(ctx, "dev", "/etc/sub") },
		"move":   func() error { return c.FileMove(ctx, "dev", "/etc", "/workspace") },
		"list":   func() error { _, _, err := c.FileList(ctx, "dev", "/etc"); return err },
		"stat":   func() error { _, _, err := c.FileStat(ctx, "dev", "/etc"); return err },
		"read":   func() error { _, err := c.FileGet(ctx, "dev", "/etc/passwd"); return err },
		"import": func() error { return c.ImportTar(ctx, "dev", "/etc", strings.NewReader("")) },
		"export": func() error { _, err := c.ExportTar(ctx, "dev", []string{"/etc"}); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if got := client.CodeOf(run()); got != "invalid_field" {
				t.Fatalf("the refusal became %q", got)
			}
		})
	}
}

// TestAnArchiveTravelsBothWaysAsTar is design 008's whole-tree transfer: the
// export is a tar body and the import is one under the tar media type,
// streamed with no temporary file between.
func TestAnArchiveTravelsBothWaysAsTar(t *testing.T) {
	archive := tarOf(t, map[string]string{"a.txt": "one", "sub/b.txt": "two"})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/x-tar")
			w.Header().Set("Trailer", "X-Cella-Error")
			_, _ = w.Write(archive)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c := f.client(client.Config{})
	stream, err := c.ExportTar(t.Context(), "dev", []string{"/workspace/a.txt", "/workspace/sub"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Err(); err != nil {
		t.Fatalf("the trailer reported %v on a whole transfer", err)
	}
	_ = stream.Close()
	if !bytes.Equal(got, archive) {
		t.Fatalf("the archive read back %d bytes, want %d", len(got), len(archive))
	}
	if seen := f.last(); strings.Join(seen.Query["path"], ",") != "/workspace/a.txt,/workspace/sub" {
		t.Fatalf("the export named %v", seen.Query["path"])
	}

	if err = c.ImportTar(t.Context(), "dev", "/workspace", bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	seen := f.last()
	if seen.Method != "PUT" || seen.Query.Get("dest") != "/workspace" {
		t.Fatalf("the import called %s with dest %q", seen.Method, seen.Query.Get("dest"))
	}
	if got := seen.Header.Get("Content-Type"); got != "application/x-tar" {
		t.Fatalf("the import sent %q, and the archive write is told apart by it", got)
	}
	if seen.Body != string(archive) {
		t.Error("the archive that arrived is not the archive that was sent")
	}
}

// TestATransferThatFailedPartWayIsTheTrailer: once a body has begun the API
// can no longer change the status, so design 008 puts the code in the
// trailer and the client reads it there.
func TestATransferThatFailedPartWayIsTheTrailer(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "X-Cella-Error")
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write([]byte("half an archive"))
		w.Header().Set("X-Cella-Error", "driver_unavailable")
	})
	stream, err := f.client(client.Config{}).ExportTar(t.Context(), "dev", []string{"/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(stream); err != nil {
		t.Fatal(err)
	}
	err = stream.Err()
	if err == nil || !strings.Contains(err.Error(), "driver_unavailable") {
		t.Fatalf("the trailer reported %v", err)
	}
	_ = stream.Close()
}

// TestLogsCarryTheirSelectors: follow, since and tail reach the route as
// design 008 spells them, and the body is read as it arrives.
func TestLogsCarryTheirSelectors(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("first line\nsecond line\n"))
	})
	c := f.client(client.Config{})
	since := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	body, err := c.Logs(t.Context(), "dev", client.LogOptions{Follow: true, Since: since, Tail: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	lines, err := io.ReadAll(body)
	if err != nil || !strings.Contains(string(lines), "second line") {
		t.Fatalf("the log holds %q, %v", lines, err)
	}
	seen := f.last()
	if seen.Path != "/v1/sandboxes/dev/logs" {
		t.Fatalf("the logs called %s", seen.Path)
	}
	for key, want := range map[string]string{"follow": "1", "since": "2026-09-20T10:00:00Z", "tail": "10"} {
		if got := seen.Query.Get(key); got != want {
			t.Errorf("the %s selector is %q, want %q", key, got, want)
		}
	}
	// Nothing asked for is nothing sent, so the server's defaults stand.
	if _, err = c.Logs(t.Context(), "dev", client.LogOptions{}); err != nil {
		t.Fatal(err)
	}
	if q := f.last().Query; q.Has("follow") || q.Has("since") || q.Has("tail") {
		t.Errorf("a plain log call carried %v", q)
	}
}

// tarOf builds one archive, which is the shape both file transfers carry.
func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for name, body := range files {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
