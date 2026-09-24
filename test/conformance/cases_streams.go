// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	cellaclient "latere.ai/x/cella/client"
)

// streamCases prove every stream of design 008's table: the bounded exec,
// the framed exec stream, the two sockets with their frame protocol, the
// archive both ways, the granular file routes with their containment
// refusals, the log stream, and the dial route's gate.
func streamCases() []Case {
	return []Case{
		{"streams", "case008ExecWait", case008ExecWait},
		{"streams", "case008ExecStream", case008ExecStream},
		{"streams", "case008ExecSocket", case008ExecSocket},
		{"streams", "case008AttachSocket", case008AttachSocket},
		{"streams", "case008FilesTar", case008FilesTar},
		{"streams", "case008FileRoutes", case008FileRoutes},
		{"streams", "case008Logs", case008Logs},
		{"streams", "case008Dial", case008Dial},
	}
}

// case008ExecWait: the bounded form answers one JSON result with the exit
// code, both output channels, and the timeout's own code.
func case008ExecWait(ctx context.Context, e *Env) error {
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	result, x, err := e.exec(ctx, e.caller, obj.Status.ID, "/bin/sh", "-c", "printf out; printf err >&2; exit 7")
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	if result.ExitCode != 7 {
		return x.disagree("exitCode 7", fmt.Sprintf("exitCode %d", result.ExitCode))
	}
	if result.Stdout != "out" || result.Stderr != "err" {
		return x.disagree("stdout out and stderr err", fmt.Sprintf("stdout %q and stderr %q", result.Stdout, result.Stderr))
	}
	if result.Truncated {
		return x.disagree("truncated false for a short output", "truncated true")
	}
	body := mustJSON(map[string]any{"command": []string{"/bin/sh", "-c", "sleep 30"}, "timeout": "1s"})
	x, err = e.caller.post(ctx, "/v1/sandboxes/"+obj.Status.ID+"/exec?wait=1", body)
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	var timedOut execResult
	if err := json.Unmarshal(x.Body, &timedOut); err != nil {
		return x.disagree("an exec result as JSON", err.Error())
	}
	if timedOut.ExitCode != 124 {
		return x.disagree("exit code 124 from a command the timeout ended", fmt.Sprintf("exitCode %d", timedOut.ExitCode))
	}
	return nil
}

// case008ExecStream: the unbounded form is the framed stream of design 008:
// one byte of channel, four bytes of length, the payload, and an exit frame
// that closes it.
func case008ExecStream(ctx context.Context, e *Env) error {
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	body := mustJSON(map[string]any{"command": []string{"/bin/sh", "-c", "printf out; printf err >&2; exit 5"}})
	resp, err := e.caller.open(ctx, http.MethodPost, "/v1/sandboxes/"+obj.Status.ID+"/exec", request{
		Body: body, ContentType: "application/json",
	})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	x := &exchange{Method: http.MethodPost, Path: "/v1/sandboxes/" + obj.Status.ID + "/exec", Status: resp.StatusCode, Header: resp.Header}
	if resp.StatusCode != http.StatusOK {
		x.Body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return x.disagree("status 200 and the exec stream", fmt.Sprintf("status %d", resp.StatusCode))
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/vnd.cella.exec-stream") {
		return x.disagree("Content-Type application/vnd.cella.exec-stream", "Content-Type "+got)
	}
	channels := map[byte]*bytes.Buffer{1: {}, 2: {}}
	header := make([]byte, 5)
	for {
		if _, err := io.ReadFull(resp.Body, header); err != nil {
			return x.disagree("an exit frame before the stream ends", "the stream ended: "+err.Error())
		}
		length := binary.BigEndian.Uint32(header[1:])
		if length > 1<<20 {
			return x.disagree("a frame of at most 1 MiB", fmt.Sprintf("a frame of %d bytes", length))
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(resp.Body, payload); err != nil {
			return x.disagree("the payload the frame's length names", err.Error())
		}
		switch header[0] {
		case 1, 2:
			channels[header[0]].Write(payload)
		case 3:
			code, err := strconv.Atoi(strings.TrimSpace(string(payload)))
			if err != nil {
				return x.disagree("an exit frame carrying the code as decimal text", fmt.Sprintf("%q", payload))
			}
			if code != 5 {
				return x.disagree("exit 5", fmt.Sprintf("exit %d", code))
			}
			if channels[1].String() != "out" || channels[2].String() != "err" {
				return x.disagree("out on channel 1 and err on channel 2",
					fmt.Sprintf("%q on channel 1 and %q on channel 2", channels[1], channels[2]))
			}
			return nil
		case 4:
			return x.disagree("an exit frame", "an error frame: "+string(payload))
		default:
			return x.disagree("a channel of 1, 2, 3 or 4", fmt.Sprintf("channel %d", header[0]))
		}
	}
}

// case008ExecSocket: the exec socket carries bytes both ways and ends with
// the exit code in a text frame before the close.
func case008ExecSocket(ctx context.Context, e *Env) error {
	if err := e.need("attach"); err != nil {
		return err
	}
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	c, err := e.socketClient()
	if err != nil {
		return err
	}
	// The protocol has no half close, so the command ends on its own once it
	// has read the line the case writes.
	session, err := c.ExecSession(ctx, obj.Status.ID, cellaclient.ExecRequest{
		Command: []string{"/bin/sh", "-c", "read line; printf 'got %s' \"$line\"; exit 3"},
	})
	if err != nil {
		return fmt.Errorf("opening the exec socket: %w", err)
	}
	defer func() { _ = session.Close() }()
	if _, err := io.WriteString(session, "hello\n"); err != nil {
		return fmt.Errorf("writing to the exec socket: %w", err)
	}
	out, err := io.ReadAll(session)
	if err != nil {
		return fmt.Errorf("reading the exec socket: %w", err)
	}
	code, err := session.Wait()
	if err != nil {
		return fmt.Errorf("waiting for the exec socket's exit: %w", err)
	}
	if code != 3 {
		return fmt.Errorf("the exec socket reported exit %d, want 3", code)
	}
	if !strings.Contains(string(out), "got hello") {
		return fmt.Errorf("the exec socket returned %q, want the bytes the command read back", out)
	}
	return nil
}

// case008AttachSocket: attach carries a terminal, a resize reaches it, and
// the exit arrives before the close.
func case008AttachSocket(ctx context.Context, e *Env) error {
	if err := e.need("attach"); err != nil {
		return err
	}
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	c, err := e.socketClient()
	if err != nil {
		return err
	}
	session, err := c.AttachSession(ctx, obj.Status.ID, cellaclient.ExecRequest{
		Command: []string{"/bin/sh"}, Cols: 80, Rows: 24,
	})
	if err != nil {
		return fmt.Errorf("opening the attach socket: %w", err)
	}
	defer func() { _ = session.Close() }()
	if err := session.Resize(120, 40); err != nil {
		return fmt.Errorf("resizing the terminal: %w", err)
	}
	if _, err := io.WriteString(session, "exit 4\n"); err != nil {
		return fmt.Errorf("writing to the attach socket: %w", err)
	}
	if _, err := io.Copy(io.Discard, session); err != nil {
		return fmt.Errorf("reading the attach socket: %w", err)
	}
	code, err := session.Wait()
	if err != nil {
		return fmt.Errorf("waiting for the attach socket's exit: %w", err)
	}
	if code != 4 {
		return fmt.Errorf("the attach socket reported exit %d, want 4", code)
	}
	return nil
}

// case008FilesTar: an archive extracted under a destination comes back as an
// archive with the same bytes.
func case008FilesTar(ctx context.Context, e *Env) error {
	if err := e.need("files"); err != nil {
		return err
	}
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	content := "the conformance suite wrote this"
	archive := tarOf("suite.txt", content)
	base := "/v1/sandboxes/" + obj.Status.ID + "/files"
	x, err := e.caller.put(ctx, base+"?dest=/workspace", archive, "application/x-tar")
	if err != nil {
		return err
	}
	if err := x.status(http.StatusNoContent); err != nil {
		return err
	}
	x, err = e.caller.get(ctx, base+"?path=/workspace/suite.txt")
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	if got := x.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/x-tar") {
		return x.disagree("Content-Type application/x-tar", "Content-Type "+got)
	}
	found, err := tarEntry(x.Body, "suite.txt")
	if err != nil {
		return x.disagree("an archive holding suite.txt", err.Error())
	}
	if found != content {
		return x.disagree("the bytes that were written", fmt.Sprintf("%q", found))
	}
	return nil
}

// case008FileRoutes: each granular route answers its shape, and each
// containment rule refuses with the code of the table.
func case008FileRoutes(ctx context.Context, e *Env) error {
	if err := e.need("files"); err != nil {
		return err
	}
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	base := "/v1/sandboxes/" + obj.Status.ID + "/files"
	dir := "/workspace/suite"
	x, err := e.caller.post(ctx, base+"/mkdir", mustJSON(map[string]string{"path": dir}))
	if err != nil {
		return err
	}
	if err := x.status(http.StatusNoContent); err != nil {
		return err
	}
	file := dir + "/one.txt"
	x, err = e.caller.send(ctx, http.MethodPut, base+"?path="+file+"&mode=0644", request{
		Body: []byte("hello"), ContentType: "text/plain",
	})
	if err != nil {
		return err
	}
	if err := x.status(http.StatusNoContent); err != nil {
		return err
	}
	x, err = e.caller.get(ctx, base+"/stat?path="+file)
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	var entry struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		Size  int64  `json:"size"`
		Mode  string `json:"mode"`
		IsDir bool   `json:"isDir"`
	}
	if err := json.Unmarshal(x.Body, &entry); err != nil {
		return x.disagree("one entry as JSON", err.Error())
	}
	if entry.Size != 5 || entry.IsDir {
		return x.disagree("a file of 5 bytes", fmt.Sprintf("size %d, isDir %v", entry.Size, entry.IsDir))
	}
	if entry.Mode != "0644" {
		return x.disagree("mode as octal text, 0644", "mode "+entry.Mode)
	}
	x, err = e.caller.get(ctx, base+"/list?path="+dir)
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	var listing struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
		Next string `json:"next"`
	}
	if err := json.Unmarshal(x.Body, &listing); err != nil {
		return x.disagree("a listing with items and next", err.Error())
	}
	if len(listing.Items) != 1 || listing.Items[0].Name != "one.txt" {
		return x.disagree("one entry named one.txt", fmt.Sprintf("%v", listing.Items))
	}
	x, err = e.caller.get(ctx, base+"/content?path="+file)
	if err != nil {
		return err
	}
	if err := x.status(http.StatusOK); err != nil {
		return err
	}
	if string(x.Body) != "hello" {
		return x.disagree("the bytes that were written", fmt.Sprintf("%q", x.Body))
	}
	moved := dir + "/two.txt"
	x, err = e.caller.post(ctx, base+"/move", mustJSON(map[string]string{"from": file, "to": moved}))
	if err != nil {
		return err
	}
	if err := x.status(http.StatusNoContent); err != nil {
		return err
	}
	for range 2 {
		x, err = e.caller.del(ctx, base+"?path="+dir)
		if err != nil {
			return err
		}
		if err := x.status(http.StatusNoContent); err != nil {
			return fmt.Errorf("a remove is 204 and 204 again when it is already gone: %w", err)
		}
	}
	refusals := []struct {
		path, code string
	}{
		{base + "/stat?path=/etc/passwd", "invalid_field"},
		{base + "/stat?path=/workspace/../etc/passwd", "invalid_field"},
		{base + "/stat?path=/workspace/none", "not_found"},
		{base + "/stat?path=/workspace/a&path=/workspace/b", "invalid_field"},
	}
	for _, refusal := range refusals {
		x, err = e.caller.get(ctx, refusal.path)
		if err != nil {
			return err
		}
		if err := x.refusal(refusal.code); err != nil {
			return err
		}
	}
	return nil
}

// case008Logs: the main process's output is what the log route answers, with
// tail and follow honored.
func case008Logs(ctx context.Context, e *Env) error {
	obj, err := e.sandbox(ctx, e.caller, func(body map[string]any) {
		spec, _ := body["spec"].(map[string]any)
		spec["command"] = []any{"/bin/sh", "-c", "echo conformance-line; sleep 300"}
	})
	if err != nil {
		return err
	}
	base := "/v1/sandboxes/" + obj.Status.ID + "/logs"
	deadline := time.Now().Add(30 * time.Second)
	var x *exchange
	for {
		if x, err = e.caller.get(ctx, base+"?tail=10"); err != nil {
			return err
		}
		if err := x.status(http.StatusOK); err != nil {
			return err
		}
		if strings.Contains(string(x.Body), "conformance-line") {
			break
		}
		if time.Now().After(deadline) {
			return x.disagree("the main process's output", fmt.Sprintf("%q", x.Body))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	if got := x.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		return x.disagree("Content-Type text/plain", "Content-Type "+got)
	}
	follow, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := e.caller.open(follow, http.MethodGet, base+"?follow=1", request{})
	if err != nil {
		return fmt.Errorf("following the log: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return x.disagree("status 200 from a followed log", fmt.Sprintf("status %d", resp.StatusCode))
	}
	buf := make([]byte, len("conformance-line"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		return fmt.Errorf("a followed log carries the output already written: %w", err)
	}
	if !strings.Contains(string(buf), "conformance") {
		return fmt.Errorf("a followed log opened with %q", buf)
	}
	return nil
}

// case008Dial: the dial route carries raw bytes to a port inside, and needs
// the capability the environment declares for it.
func case008Dial(ctx context.Context, e *Env) error {
	if err := e.need("dial"); err != nil {
		return err
	}
	obj, err := e.sandbox(ctx, e.caller)
	if err != nil {
		return err
	}
	upgrade := map[string]string{
		"Connection":             "Upgrade",
		"Upgrade":                "websocket",
		"Sec-WebSocket-Version":  "13",
		"Sec-WebSocket-Key":      "dGhlIHNhbXBsZSBub25jZQ==",
		"Sec-WebSocket-Protocol": "cella.dial.v1",
	}
	path := "/v1/sandboxes/" + obj.Status.ID + "/dial/8080"
	resp, err := e.caller.open(ctx, http.MethodGet, path, request{Header: upgrade})
	if err != nil {
		return err
	}
	// An open socket lasts as long as the port inside does, which on a host
	// where something holds 8080 is until the server's idle bound. The case
	// reads the status and closes, rather than reading the socket to its end.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if err != nil {
			return fmt.Errorf("GET %s: reading the answer: %w", path, err)
		}
		x := &exchange{Method: http.MethodGet, Path: path, Status: resp.StatusCode, Header: resp.Header, Body: body}
		return x.disagree("status 101 and the dial socket", fmt.Sprintf("status %d", x.Status))
	}
	return nil
}

// socketClient is the typed client the two socket cases speak, which is the
// client every caller of those streams speaks.
func (e *Env) socketClient() (*cellaclient.Client, error) {
	return cellaclient.New(cellaclient.Config{URL: e.caller.base, Token: cellaclient.StaticToken(e.caller.token), UserAgent: "cella-conformance"})
}

// tarOf is one file as an archive. It writes into memory, where a write does
// not fail, so a failure here is a defect in the suite and not an answer.
func tarOf(name, content string) []byte {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}
	if err := w.WriteHeader(header); err != nil {
		panic("the suite built an archive it cannot write: " + err.Error())
	}
	if _, err := io.WriteString(w, content); err != nil {
		panic("the suite built an archive it cannot write: " + err.Error())
	}
	if err := w.Close(); err != nil {
		panic("the suite built an archive it cannot write: " + err.Error())
	}
	return buf.Bytes()
}

// tarEntry reads one file out of an archive.
func tarEntry(archive []byte, name string) (string, error) {
	r := tar.NewReader(bytes.NewReader(archive))
	var seen []string
	for {
		header, err := r.Next()
		if err == io.EOF {
			return "", fmt.Errorf("no entry named %s in the archive; it holds %v", name, seen)
		}
		if err != nil {
			return "", err
		}
		seen = append(seen, header.Name)
		if strings.TrimPrefix(header.Name, "./") == name || strings.HasSuffix(header.Name, "/"+name) {
			content, err := io.ReadAll(r)
			return string(content), err
		}
	}
}
