// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/cella/runtime"
)

// readFrames reads design 008's frames until the stream ends, and returns what
// each output channel carried and the frame that closed it.
func readFrames(t *testing.T, body io.Reader) (stdout, stderr []byte, last byte, payload []byte) {
	t.Helper()
	header := make([]byte, 5)
	for {
		if _, err := io.ReadFull(body, header); err != nil {
			t.Fatalf("the stream ended with no closing frame: %v", err)
		}
		length := binary.BigEndian.Uint32(header[1:])
		if length > execFrameBytes {
			t.Fatalf("a frame of %d bytes is past the cap", length)
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(body, buf); err != nil {
			t.Fatalf("the payload the length named did not arrive: %v", err)
		}
		switch header[0] {
		case channelStdout:
			stdout = append(stdout, buf...)
		case channelStderr:
			stderr = append(stderr, buf...)
		case channelExit, channelError:
			return stdout, stderr, header[0], buf
		default:
			t.Fatalf("a frame on channel %d", header[0])
		}
	}
}

// open sends one request and returns the live response, for a case that reads
// a stream rather than a body.
func (f *fixture) openStream(method, path, token, body string) *http.Response {
	f.t.Helper()
	req, err := http.NewRequest(method, f.url+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// TestExecStreamFrames proves the unbounded form of design 008: the content
// type, one byte of channel, four bytes of length, the payload on the channel
// the output came from, and an exit frame carrying the code as decimal text.
func TestExecStreamFrames(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("work")
	res := f.openStream("POST", "/v1/sandboxes/"+obj.Status.ID+"/exec", f.alice,
		`{"command":["/bin/sh","-c","printf out; printf err >&2; exit 5"]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the stream answered %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != execStreamType {
		t.Fatalf("the stream answered Content-Type %q", got)
	}
	stdout, stderr, last, payload := readFrames(t, res.Body)
	if last != channelExit {
		t.Fatalf("the stream closed on channel %d carrying %q", last, payload)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(payload)))
	if err != nil || code != 5 {
		t.Fatalf("the exit frame carries %q", payload)
	}
	if string(stdout) != "out" || string(stderr) != "err" {
		t.Fatalf("channel 1 carried %q and channel 2 carried %q", stdout, stderr)
	}
}

// TestExecStreamCarriesMoreThanOneFrame proves output larger than one frame
// arrives whole and in frames the cap bounds, which is what lets a caller read
// a long run without the server holding it.
func TestExecStreamCarriesMoreThanOneFrame(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("work")
	const lines = 4096
	res := f.openStream("POST", "/v1/sandboxes/"+obj.Status.ID+"/exec", f.alice,
		`{"command":["/bin/sh","-c","i=0; while [ $i -lt `+strconv.Itoa(lines)+` ]; do printf '0123456789abcdef\n'; i=$((i+1)); done"]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the stream answered %d", res.StatusCode)
	}
	stdout, _, last, payload := readFrames(t, res.Body)
	if last != channelExit || strings.TrimSpace(string(payload)) != "0" {
		t.Fatalf("the stream closed on channel %d carrying %q", last, payload)
	}
	if want := lines * len("0123456789abcdef\n"); len(stdout) != want {
		t.Fatalf("channel 1 carried %d bytes, want %d", len(stdout), want)
	}
	if !bytes.HasPrefix(stdout, []byte("0123456789abcdef\n")) {
		t.Fatal("the output is not what the command wrote")
	}
}

// TestExecStreamTimeout proves a command the timeout ended closes with 124,
// which is the code the bounded form reports for the same end.
func TestExecStreamTimeout(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("work")
	res := f.openStream("POST", "/v1/sandboxes/"+obj.Status.ID+"/exec", f.alice,
		`{"command":["/bin/sh","-c","sleep 30"],"timeout":"200ms"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the stream answered %d", res.StatusCode)
	}
	_, _, last, payload := readFrames(t, res.Body)
	if last != channelExit || strings.TrimSpace(string(payload)) != "124" {
		t.Fatalf("the stream closed on channel %d carrying %q", last, payload)
	}
}

// TestExecStreamErrorFrame proves a failure after the first byte is the error
// frame of design 008 and not a status: the status is already sent, and the
// envelope the frame carries is the same one an early refusal would have
// written.
func TestExecStreamErrorFrame(t *testing.T) {
	execution := newBrokenExec()
	f := setupDriver(t, nil, func(d runtime.Driver) runtime.Driver { return failingExecDriver{d, execution} })
	obj := f.sandbox("work")
	res := f.openStream("POST", "/v1/sandboxes/"+obj.Status.ID+"/exec", f.alice, `{"command":["true"]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the stream answered %d before the first byte", res.StatusCode)
	}
	stdout, _, last, payload := readFrames(t, res.Body)
	if last != channelError {
		t.Fatalf("the stream closed on channel %d carrying %q", last, payload)
	}
	var frame errorFrame
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatalf("the error frame is not the envelope: %q", payload)
	}
	if frame.Error.Code == "" || frame.Error.Message == "" {
		t.Fatalf("the error frame carries %+v", frame.Error)
	}
	if frame.Error.Details["request_id"] == "" {
		t.Error("the error frame carries no request id")
	}
	// What the command had already written reached the caller before the
	// failure did.
	if string(stdout) != "partial" {
		t.Errorf("channel 1 carried %q", stdout)
	}
	if !execution.closed() {
		t.Error("the failed session was not closed")
	}
}

// TestExecStreamRefusesBeforeTheFirstByte proves a driver that will not start
// the command is still an HTTP status: nothing has been written, so the
// status can still carry the refusal.
func TestExecStreamRefusesBeforeTheFirstByte(t *testing.T) {
	f := setup(t, nil)
	obj := f.sandbox("work")
	f.request("POST", "/v1/sandboxes/"+obj.Status.ID+"/stop", f.alice, "", 200)
	body := f.request("POST", "/v1/sandboxes/"+obj.Status.ID+"/exec", f.alice, `{"command":["true"]}`, 409)
	if !strings.Contains(string(body), "phase_conflict") {
		t.Fatalf("a stopped sandbox answered %q", body)
	}
}
