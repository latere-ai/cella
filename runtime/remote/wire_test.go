// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"encoding/json/v2"
	"testing"
	"time"

	"latere.ai/x/cella/runtime/remote"
)

// TestExecRequestTimeoutWire holds the timeout to integer nanoseconds, the
// form a worker built from any release reads.
func TestExecRequestTimeoutWire(t *testing.T) {
	got, err := json.Marshal(remote.ExecRequest{Command: []string{"ls"}, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"command":["ls"],"timeout":30000000000}`; string(got) != want {
		t.Fatalf("request is %s, want %s", got, want)
	}
	var back remote.ExecRequest
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatal(err)
	}
	if back.Timeout != 30*time.Second || len(back.Command) != 1 {
		t.Fatalf("request read back as %+v", back)
	}
	none, err := json.Marshal(remote.ExecRequest{})
	if err != nil || string(none) != `{}` {
		t.Fatalf("a zero request is %s (%v), want {}", none, err)
	}
}

// A control message whose error text carries a byte that is not UTF-8 is sent
// with U+FFFD in its place and not refused.
func TestEncodeMessageInvalidUTF8(t *testing.T) {
	raw, err := remote.EncodeMessage(string(make([]byte, remote.OperationIDLen)), remote.Message{
		Type: "result", Error: &remote.Error{Message: "open caf\xe9"},
	})
	if err != nil {
		t.Fatalf("the message was refused: %v", err)
	}
	_, _, payload, err := remote.DecodeFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	m, err := remote.DecodeMessage(payload)
	if err != nil {
		t.Fatal(err)
	}
	if m.Error == nil || m.Error.Message != "open caf�" {
		t.Fatalf("error is %+v", m.Error)
	}
}
