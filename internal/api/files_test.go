// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/cella/runtime"
)

// call sends one request with the method, media type and body a file route
// takes, and returns the status and what came back.
func (f *fixture) call(method, path, token, media, body string) (int, []byte) {
	f.t.Helper()
	req, err := http.NewRequest(method, f.url+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if media != "" {
		req.Header.Set("Content-Type", media)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	f.header = res.Header
	return res.StatusCode, out
}

// expect sends one request and holds it to a status.
func (f *fixture) expect(status int, method, path, token, media, body string) []byte {
	f.t.Helper()
	got, out := f.call(method, path, token, media, body)
	if got != status {
		f.t.Fatalf("%s %s answered %d, want %d: %s", method, path, got, status, out)
	}
	return out
}

// query is one path as a selector carries it, so a name with a space, a
// percent sign or a newline reaches the server as itself.
func query(path string) string { return url.QueryEscape(path) }

func filesFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := setup(t, nil)
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj); err != nil {
		t.Fatal(err)
	}
	return f, "/v1/sandboxes/" + obj.Status.ID + "/files"
}

// TestFilesRoutes drives every granular route of design 033 over real HTTP
// under a signed bearer, against the native driver.
func TestFilesRoutes(t *testing.T) {
	f, base := filesFixture(t)

	t.Run("WriteAndRead", func(t *testing.T) {
		f.expect(204, "PUT", base+"?path="+query("/workspace/dir/a.txt")+"&mode=0640", f.alice, "text/plain", "hello")
		body := f.expect(200, "GET", base+"/content?path="+query("/workspace/dir/a.txt"), f.alice, "", "")
		if string(body) != "hello" {
			t.Fatalf("the file reads %q", body)
		}
		if got := f.header.Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("the body came as %q", got)
		}
		if got := f.header.Get("Content-Length"); got != "5" {
			t.Errorf("the body declared %q bytes", got)
		}
		if f.header.Get("Last-Modified") == "" {
			t.Error("the body declared no modification time")
		}
	})

	t.Run("Stat", func(t *testing.T) {
		var got entry
		if err := json.Unmarshal(f.expect(200, "GET", base+"/stat?path="+query("/workspace/dir/a.txt"), f.alice, "", ""), &got); err != nil {
			t.Fatal(err)
		}
		if got.Name != "a.txt" || got.Path != "/workspace/dir/a.txt" || got.Size != 5 || got.Mode != "0640" || got.IsDir {
			t.Fatalf("stat is %+v", got)
		}
		if got.ModTime.IsZero() {
			t.Error("stat carries no modification time")
		}
	})

	t.Run("List", func(t *testing.T) {
		f.expect(204, "POST", base+"/mkdir", f.alice, "application/json", `{"path":"/workspace/dir/sub"}`)
		var listing struct {
			Items []entry `json:"items"`
			Next  string  `json:"next"`
		}
		if err := json.Unmarshal(f.expect(200, "GET", base+"/list?path="+query("/workspace/dir"), f.alice, "", ""), &listing); err != nil {
			t.Fatal(err)
		}
		if len(listing.Items) != 2 || listing.Items[0].Name != "a.txt" || !listing.Items[1].IsDir || listing.Next != "" {
			t.Fatalf("the listing is %+v", listing)
		}
	})

	t.Run("MoveAndRemove", func(t *testing.T) {
		f.expect(204, "POST", base+"/move", f.alice, "application/json",
			`{"from":"/workspace/dir/a.txt","to":"/workspace/dir/b.txt"}`)
		f.expect(404, "GET", base+"/content?path="+query("/workspace/dir/a.txt"), f.alice, "", "")
		f.expect(200, "GET", base+"/content?path="+query("/workspace/dir/b.txt"), f.alice, "", "")
		f.expect(204, "DELETE", base+"?path="+query("/workspace/dir/b.txt"), f.alice, "", "")
		f.expect(404, "GET", base+"/stat?path="+query("/workspace/dir/b.txt"), f.alice, "", "")
		// A path that names nothing is already gone.
		f.expect(204, "DELETE", base+"?path="+query("/workspace/dir/b.txt"), f.alice, "", "")
	})

	t.Run("AnExactName", func(t *testing.T) {
		const name = "/workspace/a file 100%25 done.txt"
		f.expect(204, "PUT", base+"?path="+query(name), f.alice, "application/octet-stream", "exact")
		var got entry
		if err := json.Unmarshal(f.expect(200, "GET", base+"/stat?path="+query(name), f.alice, "", ""), &got); err != nil {
			t.Fatal(err)
		}
		if got.Name != "a file 100%25 done.txt" {
			t.Fatalf("the name came back as %q", got.Name)
		}
		if body := f.expect(200, "GET", base+"/content?path="+query(name), f.alice, "", ""); string(body) != "exact" {
			t.Fatalf("the file reads %q", body)
		}
	})

	t.Run("TheArchiveRoutesStillWork", func(t *testing.T) {
		f.upload(base+"?dest=/workspace", f.alice, "application/x-tar", archive(t, "tarred.txt", "from a tar"), 204)
		if body := f.expect(200, "GET", base+"/content?path="+query("/workspace/tarred.txt"), f.alice, "", ""); string(body) != "from a tar" {
			t.Fatalf("the file the archive carried reads %q", body)
		}
	})

	t.Run("PathsOutsideTheWorkspace", func(t *testing.T) {
		for _, p := range []string{"/etc/hosts", "/workspace/../etc", "relative", ""} {
			f.expect(400, "GET", base+"/content?path="+query(p), f.alice, "", "")
			f.expect(400, "GET", base+"/stat?path="+query(p), f.alice, "", "")
			f.expect(400, "GET", base+"/list?path="+query(p), f.alice, "", "")
			f.expect(400, "PUT", base+"?path="+query(p), f.alice, "text/plain", "x")
			f.expect(400, "DELETE", base+"?path="+query(p), f.alice, "", "")
			f.expect(400, "POST", base+"/mkdir", f.alice, "application/json", `{"path":`+quoted(p)+`}`)
			f.expect(400, "POST", base+"/move", f.alice, "application/json",
				`{"from":"/workspace/dir","to":`+quoted(p)+`}`)
			f.expect(400, "POST", base+"/move", f.alice, "application/json",
				`{"from":`+quoted(p)+`,"to":"/workspace/dir"}`)
		}
	})

	t.Run("OnePathPerRequest", func(t *testing.T) {
		f.expect(400, "GET", base+"/content?path=/workspace/a&path=/workspace/b", f.alice, "", "")
		f.expect(400, "GET", base+"/stat", f.alice, "", "")
		f.expect(400, "PUT", base, f.alice, "text/plain", "x")
	})

	t.Run("OneWritePerRequest", func(t *testing.T) {
		out := f.expect(400, "PUT", base+"?dest=/workspace&path=/workspace/a.txt", f.alice, "text/plain", "x")
		if !strings.Contains(string(out), "exclusive_fields") {
			t.Fatalf("a write naming both selectors answered %s", out)
		}
		out = f.expect(415, "PUT", base+"?dest=/workspace", f.alice, "application/json", "{}")
		if !strings.Contains(string(out), "unsupported_media_type") {
			t.Fatalf("an archive that is not one answered %s", out)
		}
	})

	t.Run("TheWrongKindOfPath", func(t *testing.T) {
		f.expect(400, "GET", base+"/list?path="+query("/workspace/tarred.txt"), f.alice, "", "")
		f.expect(400, "GET", base+"/content?path=/workspace", f.alice, "", "")
		f.expect(404, "GET", base+"/list?path="+query("/workspace/absent"), f.alice, "", "")
		f.expect(400, "POST", base+"/move", f.alice, "application/json",
			`{"from":"/workspace/tarred.txt","to":"/workspace/dir"}`)
		f.expect(400, "DELETE", base+"?path=/workspace", f.alice, "", "")
	})

	t.Run("AModeThatIsNotOne", func(t *testing.T) {
		for _, mode := range []string{"999", "abc", "1777", "0o644"} {
			f.expect(400, "PUT", base+"?path="+query("/workspace/mode.txt")+"&mode="+mode, f.alice, "text/plain", "x")
		}
	})

	t.Run("ABodyThatIsNotTheRoutes", func(t *testing.T) {
		f.expect(400, "POST", base+"/mkdir", f.alice, "application/json", `{"path":"/workspace/x","extra":1}`)
		f.expect(400, "POST", base+"/mkdir", f.alice, "application/json", `not json`)
		f.expect(400, "POST", base+"/move", f.alice, "application/json", `{"from":"/workspace/a"}`)
		f.expect(400, "POST", base+"/mkdir", f.alice, "application/json", `{"path":"/workspace/a"}{"path":"/workspace/b"}`)
	})

	t.Run("ACallerWhoDoesNotOwnIt", func(t *testing.T) {
		f.expect(403, "GET", base+"/content?path="+query("/workspace/tarred.txt"), f.bob, "", "")
		f.expect(403, "GET", base+"/stat?path="+query("/workspace/tarred.txt"), f.bob, "", "")
		f.expect(403, "GET", base+"/list?path=/workspace", f.bob, "", "")
		f.expect(403, "PUT", base+"?path="+query("/workspace/evil.txt"), f.bob, "text/plain", "x")
		f.expect(403, "DELETE", base+"?path="+query("/workspace/tarred.txt"), f.bob, "", "")
		f.expect(403, "POST", base+"/mkdir", f.bob, "application/json", `{"path":"/workspace/evil"}`)
		f.expect(403, "POST", base+"/move", f.bob, "application/json", `{"from":"/workspace/tarred.txt","to":"/workspace/evil"}`)
		f.expect(401, "GET", base+"/stat?path=/workspace", "", "", "")
	})

	t.Run("ABodyPastTheBound", func(t *testing.T) {
		f.expect(204, "PUT", base+"?path="+query("/workspace/bounded.txt"), f.alice, "text/plain", "before")
		f.h.(*handler).MaxUploadBytes = 8
		defer func() { f.h.(*handler).MaxUploadBytes = 1 << 30 }()
		f.expect(413, "PUT", base+"?path="+query("/workspace/bounded.txt"), f.alice, "text/plain", strings.Repeat("x", 9))
		if body := f.expect(200, "GET", base+"/content?path="+query("/workspace/bounded.txt"), f.alice, "", ""); string(body) != "before" {
			t.Fatalf("a refused write left %q", body)
		}
		f.expect(204, "PUT", base+"?path="+query("/workspace/bounded.txt"), f.alice, "text/plain", "12345678")
	})

	t.Run("ASandboxThatIsNotThere", func(t *testing.T) {
		f.expect(404, "GET", "/v1/sandboxes/sbx_absent/files/stat?path=/workspace", f.alice, "", "")
	})
}

// quoted renders one path as a JSON string.
func quoted(p string) string {
	b, _ := json.Marshal(p)
	return string(b)
}

// brokenOpen answers a stream that fails on its first read, which is what a
// transfer that died between the entry and its body looks like.
type brokenOpen struct{ runtime.Driver }

func (d brokenOpen) Capabilities() runtime.Capabilities { return runtime.Capabilities{Files: true} }

func (d brokenOpen) Stat(ctx context.Context, id, path string) (runtime.FileInfo, error) {
	return runtime.FileInfo{Name: "a.txt", Path: path, Size: 5, ModTime: time.Now()}, nil
}

func (d brokenOpen) ReadDir(context.Context, string, string) ([]runtime.FileInfo, error) {
	return nil, nil
}

func (d brokenOpen) Open(ctx context.Context, id, path string) (io.ReadCloser, runtime.FileInfo, error) {
	info, _ := d.Stat(ctx, id, path)
	return io.NopCloser(failingBody{}), info, nil
}

func (d brokenOpen) Write(context.Context, string, runtime.WriteRequest) (int64, error) {
	return 0, nil
}
func (d brokenOpen) Mkdir(context.Context, string, string) error        { return nil }
func (d brokenOpen) Remove(context.Context, string, string) error       { return nil }
func (d brokenOpen) Move(context.Context, string, string, string) error { return nil }

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("the transfer died") }

// TestFileContentThatFailsBeforeItsFirstByte: the length the route announced
// is not the length of the envelope that replaces it, so both the length and
// the trailer go before the error is written.
func TestFileContentThatFailsBeforeItsFirstByte(t *testing.T) {
	f := setupDriver(t, nil, func(d runtime.Driver) runtime.Driver { return brokenOpen{d} })
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj); err != nil {
		t.Fatal(err)
	}
	out := f.expect(503, "GET", "/v1/sandboxes/"+obj.Status.ID+"/files/content?path=/workspace/a.txt", f.alice, "", "")
	var envelope struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("the answer is not an envelope: %q: %v", out, err)
	}
	if envelope.Error.Code != "driver_unavailable" {
		t.Fatalf("the answer is %s", out)
	}
	if got := f.header.Get("Content-Length"); got != "" && got != strconv.Itoa(len(out)) {
		t.Fatalf("the envelope came under Content-Length %q, and is %d bytes", got, len(out))
	}
}

// TestFilesWhileStopped: the native driver reaches the workspace whether or
// not the sandbox runs, which is what the Files capability declares.
func TestFilesWhileStopped(t *testing.T) {
	f, base := filesFixture(t)
	sandbox := strings.TrimSuffix(base, "/files")
	f.expect(204, "PUT", base+"?path="+query("/workspace/before.txt"), f.alice, "text/plain", "before")
	f.request("POST", sandbox+"/stop", f.alice, "", 200)
	f.expect(204, "PUT", base+"?path="+query("/workspace/during.txt"), f.alice, "text/plain", "during")
	if body := f.expect(200, "GET", base+"/content?path="+query("/workspace/before.txt"), f.alice, "", ""); string(body) != "before" {
		t.Fatalf("a stopped sandbox reads %q", body)
	}
	f.request("POST", sandbox+"/start", f.alice, "", 200)
	out := f.request("POST", sandbox+"/exec?wait=1", f.alice, `{"command":["cat","during.txt"]}`, 200)
	if !strings.Contains(string(out), "during") {
		t.Fatalf("the restarted sandbox does not hold what was written: %s", out)
	}
}

// noFilesDriver declares no Files. It embeds the interface, not the native
// driver, so the concrete FileStore methods are not promoted either.
type noFilesDriver struct{ runtime.Driver }

func (noFilesDriver) Capabilities() runtime.Capabilities { return runtime.Capabilities{} }

// filesOnlyInName declares Files over a driver that has no per-file half, so
// the controller's assertion is the one that refuses.
type filesOnlyInName struct{ runtime.Driver }

func (filesOnlyInName) Capabilities() runtime.Capabilities {
	return runtime.Capabilities{Files: true}
}

// TestFilesCapabilityGate: an environment without the capability answers 422
// before any driver call, whether the declaration or the interface is what it
// lacks.
func TestFilesCapabilityGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(runtime.Driver) runtime.Driver
	}{
		{"Undeclared", func(d runtime.Driver) runtime.Driver { return noFilesDriver{d} }},
		{"DeclaredWithoutTheInterface", func(d runtime.Driver) runtime.Driver { return filesOnlyInName{d} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupDriver(t, nil, tc.wrap)
			var obj v1.Sandbox
			if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj); err != nil {
				t.Fatal(err)
			}
			base := "/v1/sandboxes/" + obj.Status.ID + "/files"
			for _, call := range []struct{ method, path, media, body string }{
				{"GET", base + "/content?path=/workspace/a", "", ""},
				{"GET", base + "/stat?path=/workspace", "", ""},
				{"GET", base + "/list?path=/workspace", "", ""},
				{"PUT", base + "?path=/workspace/a", "text/plain", "x"},
				{"DELETE", base + "?path=/workspace/a", "", ""},
				{"POST", base + "/mkdir", "application/json", `{"path":"/workspace/a"}`},
				{"POST", base + "/move", "application/json", `{"from":"/workspace/a","to":"/workspace/b"}`},
			} {
				out := f.expect(422, call.method, call.path, f.alice, call.media, call.body)
				if !strings.Contains(string(out), "capability_unsupported") {
					t.Errorf("%s %s answered %s", call.method, call.path, out)
				}
			}
		})
	}
}

// TestFilesRecords: every file route writes one record naming the operation
// and the bytes, and no record carries a name's content.
func TestFilesRecords(t *testing.T) {
	const canary = "a-file-body-nobody-should-see"
	f := setupRecorded(t)
	var obj v1.Sandbox
	if err := json.Unmarshal(f.request("POST", "/v1/sandboxes", f.alice, createBody, 201), &obj); err != nil {
		t.Fatal(err)
	}
	base := "/v1/sandboxes/" + obj.Status.ID + "/files"
	f.expect(204, "PUT", base+"?path="+query("/workspace/one.txt"), f.alice, "text/plain", canary)
	f.expect(200, "GET", base+"/stat?path="+query("/workspace/one.txt"), f.alice, "", "")
	f.expect(200, "GET", base+"/list?path=/workspace", f.alice, "", "")
	f.expect(200, "GET", base+"/content?path="+query("/workspace/one.txt"), f.alice, "", "")
	f.expect(204, "POST", base+"/mkdir", f.alice, "application/json", `{"path":"/workspace/two"}`)
	f.expect(204, "POST", base+"/move", f.alice, "application/json",
		`{"from":"/workspace/one.txt","to":"/workspace/two/one.txt"}`)
	f.expect(204, "DELETE", base+"?path="+query("/workspace/two"), f.alice, "", "")

	var operations []string
	var bytesWritten, bytesRead int64
	for _, r := range f.records(obj.Status.ID) {
		if r.Type != "sandbox.files" {
			continue
		}
		var data struct {
			Operation string   `json:"operation"`
			Paths     []string `json:"paths"`
			Bytes     int64    `json:"bytes"`
		}
		if err := json.Unmarshal(r.Data, &data); err != nil {
			t.Fatal(err)
		}
		operations = append(operations, data.Operation)
		switch data.Operation {
		case "write":
			bytesWritten = data.Bytes
		case "read":
			bytesRead = data.Bytes
		}
		if strings.Contains(string(r.Data), canary) {
			t.Errorf("the %s record carries the body: %s", data.Operation, r.Data)
		}
		if len(data.Paths) == 0 {
			t.Errorf("the %s record names no path", data.Operation)
		}
	}
	want := []string{"write", "stat", "list", "read", "mkdir", "move", "remove"}
	if strings.Join(operations, ",") != strings.Join(want, ",") {
		t.Fatalf("the journal holds %v, want %v", operations, want)
	}
	if bytesWritten != int64(len(canary)) || bytesRead != int64(len(canary)) {
		t.Errorf("the records report %d written and %d read, for %d bytes", bytesWritten, bytesRead, len(canary))
	}
}
