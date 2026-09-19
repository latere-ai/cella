// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

func archive(t *testing.T, name, body string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func (f *fixture) upload(path, token, media string, body []byte, status int) {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPut, f.url+path, bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", media)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != status {
		f.t.Fatalf("upload status %d want %d: %s", res.StatusCode, status, b)
	}
}
func TestWorkspaceFilesEndToEnd(t *testing.T) {
	f := setup(t, nil)
	var obj v1.Sandbox
	_ = json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj)
	base := "/v1/sandboxes/" + obj.Status.ID
	f.upload(base+"/files?dest=/workspace", f.alice, "application/x-tar", archive(t, "input.txt", "hello"), 204)
	f.request("POST", base+"/stop", f.alice, "", 200)
	b := f.request("GET", base+"/files?path=/workspace/input.txt", f.alice, "", 200)
	tr := tar.NewReader(bytes.NewReader(b))
	header, err := tr.Next()
	if err != nil || header.Name != "input.txt" {
		t.Fatal(header, err)
	}
	content, err := io.ReadAll(tr)
	if err != nil || string(content) != "hello" {
		t.Fatal(string(content), err)
	}
	f.upload(base+"/files?dest=/workspace", f.alice, "application/x-tar", archive(t, "input.txt", "updated"), 204)
	f.request("POST", base+"/start", f.alice, "", 200)
	b = f.request("POST", base+"/exec?wait=1", f.alice, `{"command":["cat","input.txt"]}`, 200)
	if !bytes.Contains(b, []byte("updated")) {
		t.Fatal(string(b))
	}
	f.upload(base+"/files?dest=/workspace", f.bob, "application/x-tar", archive(t, "input.txt", "evil"), 403)
	f.request("GET", base+"/files", f.bob, "", 403)
	f.request("GET", base+"/logs", f.bob, "", 403)
	f.upload(base+"/files?dest=/workspace", f.alice, "application/json", archive(t, "input.txt", "wrong"), 415)
	for _, dest := range []string{"/etc", "/workspace/../etc", "relative", ""} {
		f.upload(base+"/files?dest="+dest, f.alice, "application/x-tar", archive(t, "input.txt", "wrong"), 400)
	}
	for _, name := range []string{"../escape", "/absolute"} {
		f.upload(base+"/files?dest=/workspace", f.alice, "application/x-tar", archive(t, name, "wrong"), 400)
	}
	f.upload(base+"/files?dest=/workspace", f.alice, "application/x-tar", []byte("broken"), 400)
	for _, path := range []string{"/etc", "/workspace/../etc"} {
		f.request("GET", base+"/files?path="+path, f.alice, "", 400)
	}
	f.request("GET", base+"/files?path=/workspace/missing", f.alice, "", 404)
	// Reject the complete raw request before extraction, even when extra bytes
	// follow a valid tar terminator that the runtime would otherwise ignore.
	f.h.(*handler).MaxUploadBytes = 2048
	data := append(archive(t, "input.txt", "wrong"), []byte(strings.Repeat("x", 2049))...)
	f.upload(base+"/files?dest=/workspace", f.alice, "application/x-tar", data, 413)
	b = f.request("POST", base+"/exec?wait=1", f.alice, `{"command":["cat","input.txt"]}`, 200)
	if !bytes.Contains(b, []byte("updated")) {
		t.Fatal("oversized upload changed existing file", string(b))
	}
}
func TestLogSelectorValidation(t *testing.T) {
	f := setup(t, nil)
	var obj v1.Sandbox
	_ = json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj)
	base := "/v1/sandboxes/" + obj.Status.ID + "/logs"
	for _, q := range []string{"?follow=bad", "?since=bad", "?tail=-1", "?tail=100001", "?tail=bad"} {
		f.request("GET", base+q, f.alice, "", 400)
	}
}
func TestStreamErrors(t *testing.T) {
	w := httptest.NewRecorder()
	s := newStream(w, "application/x-tar")
	s.fail(runtime.ErrInvalid)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_field") || w.Header().Get("Trailer") != "" {
		t.Fatal(w)
	}
	for _, tc := range []struct {
		err  error
		want string
	}{{runtime.ErrInvalid, "invalid_field"}, {errors.New("lost stream"), "driver_unavailable"}} {
		w = httptest.NewRecorder()
		s = newStream(w, "application/x-tar")
		if _, err := s.Write([]byte("partial")); err != nil {
			t.Fatal(err)
		}
		s.fail(tc.err)
		if w.Code != 200 || w.Result().Trailer.Get("X-Cella-Error") != tc.want {
			t.Fatal(w.Result())
		}
	}
}

func TestMainProcessLogsEndToEnd(t *testing.T) {
	f := setup(t, nil)
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"app"},"spec":{"command":["sh"],"args":["-c","printf 'first\\n'; sleep 0.1; printf 'second\\n'"]}}`
	var obj v1.Sandbox
	_ = json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, body, 201), &obj)
	base := "/v1/sandboxes/" + obj.Status.ID
	logs := f.request("GET", base+"/logs?follow=1", f.alice, "", 200)
	if string(logs) != "first\nsecond\n" {
		t.Fatalf("logs %q", logs)
	}
	_ = json.Unmarshal(f.request("GET", base, f.alice, "", 200), &obj)
	if obj.Status.Phase != "Stopped" || obj.Status.ExitCode == nil || *obj.Status.ExitCode != 0 || obj.Status.Reason != "Exited" {
		t.Fatal(obj.Status)
	}
	if logs = f.request("GET", base+"/logs?tail=1&follow=0", f.alice, "", 200); string(logs) != "second\n" {
		t.Fatalf("tail %q", logs)
	}
	if logs = f.request("GET", base+"/logs?since=2999-01-01T00:00:00Z", f.alice, "", 200); len(logs) != 0 {
		t.Fatalf("future logs %q", logs)
	}
	f.request("POST", base+"/start", f.alice, "", 200)
	f.request("GET", base+"/logs?follow=1", f.alice, "", 200)
	f.request("DELETE", base, f.alice, "", 202)
}

// The design's default is 64 KiB; with the previous 1 MiB default the
// oversized valid manifest below created a workspace instead of returning 413.
func TestDefaultManifestBodyLimit(t *testing.T) {
	f := setup(t, nil)
	padded := createBody + strings.Repeat(" ", 65537-len(createBody))
	f.request("POST", "/v1/sandboxes", f.alice, padded, 413)
	if len(f.c.List()) != 0 {
		t.Fatal("oversized body created workspace")
	}
	f.request("POST", "/v1/sandboxes", f.alice, padded[:65536], 201)
}

func TestFollowLogsFlushesBeforeProcessExits(t *testing.T) {
	f := setup(t, nil)
	body := `{"apiVersion":"cella.latere.ai/v1beta1","kind":"Sandbox","metadata":{"name":"stream"},"spec":{"command":["sh","-c","printf ready; sleep 30"]}}`
	var obj v1.Sandbox
	_ = json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, body, 201), &obj)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url+"/v1/sandboxes/"+obj.Status.ID+"/logs?follow=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.alice)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("log output was not flushed while process remained running", err)
	}
	data := make([]byte, 5)
	_, err = io.ReadFull(res.Body, data)
	_ = res.Body.Close()
	cancel()
	if err != nil || string(data) != "ready" {
		t.Fatal(string(data), err)
	}
	f.request("POST", "/v1/sandboxes/"+obj.Status.ID+"/stop", f.alice, "", 200)
}

// touchDriver counts activity stamps and refuses them, so a handler that
// stamps is observable and a refused stamp is proved not to fail its request.
type touchDriver struct {
	runtime.Driver
	mu  sync.Mutex
	ids []string
}

func (d *touchDriver) Touch(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ids = append(d.ids, id)
	return errors.New("runtime outage")
}
func (d *touchDriver) stamped() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.ids)
}
func TestActivityIsStamped(t *testing.T) {
	stamps := &touchDriver{}
	f := setupDriver(t, nil, func(d runtime.Driver) runtime.Driver {
		stamps.Driver = d
		return stamps
	})
	create := func(name string) v1.Sandbox {
		t.Helper()
		var obj v1.Sandbox
		body := strings.Replace(createBody, `"name":"work"`, `"name":"`+name+`"`, 1)
		if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, body, 201), &obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}
	executed, transferred := create("executed"), create("transferred")
	// Each request answers although every stamp is refused.
	f.request("POST", "/v1/sandboxes/"+executed.Status.ID+"/exec?wait=1", f.alice, `{"command":["true"]}`, 200)
	f.request("GET", "/v1/sandboxes/"+transferred.Status.ID+"/files?path=/workspace", f.alice, "", 200)
	if got := stamps.stamped(); !slices.Equal(got, []string{executed.Status.ID, transferred.Status.ID}) {
		t.Fatalf("exec and files stamped %v", got)
	}
	// Reading logs is watching a sandbox, not using it.
	f.request("GET", "/v1/sandboxes/"+executed.Status.ID+"/logs", f.alice, "", 200)
	if got := stamps.stamped(); len(got) != 2 {
		t.Fatalf("a log read stamped activity: %v", got)
	}
}
