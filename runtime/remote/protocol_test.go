// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/runtime/remote"

	driver "latere.ai/x/cella/runtime"
)

// TestStreamFraming is spec 021's frame: a 26-byte operation id, one byte of
// sub-stream, and the payload, with the bounds that make a frame parseable
// without a length.
func TestStreamFraming(t *testing.T) {
	id := remote.NewOperationID()
	if len(id) != remote.OperationIDLen {
		t.Fatalf("an operation id is %d characters, not %d", remote.OperationIDLen, len(id))
	}
	raw, err := remote.EncodeFrame(id, remote.StreamStdout, []byte("hello"))
	if err != nil {
		t.Fatalf("the frame did not encode: %v", err)
	}
	if len(raw) != remote.OperationIDLen+1+5 {
		t.Errorf("the frame is %d bytes, want the id, the sub-stream and the payload", len(raw))
	}
	operation, stream, payload, err := remote.DecodeFrame(raw)
	if err != nil {
		t.Fatalf("the frame did not decode: %v", err)
	}
	if operation != id || stream != remote.StreamStdout || string(payload) != "hello" {
		t.Errorf("the frame read back as %q/%d/%q", operation, stream, payload)
	}
	// A zero-length payload is the frame that ends a sub-stream, and it is a
	// whole frame rather than a short one.
	raw, err = remote.EncodeFrame(id, remote.StreamStdin, nil)
	if err != nil {
		t.Fatalf("the closing frame did not encode: %v", err)
	}
	if _, _, payload, err = remote.DecodeFrame(raw); err != nil || len(payload) != 0 {
		t.Errorf("the closing frame read back as %q (%v)", payload, err)
	}

	for _, tc := range []struct {
		name      string
		operation string
		payload   []byte
	}{
		{"an id of another width", "short", nil},
		{"an empty id", "", nil},
		{"a payload past the bound", id, make([]byte, remote.MaxFrameBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := remote.EncodeFrame(tc.operation, remote.StreamControl, tc.payload); !errors.Is(err, remote.ErrFrame) {
				t.Errorf("this frame encoded: %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"a frame shorter than its header", []byte("too short")},
		{"an empty frame", nil},
		{"a frame past the bound", make([]byte, remote.MaxFrameBytes+remote.OperationIDLen+2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := remote.DecodeFrame(tc.raw); !errors.Is(err, remote.ErrFrame) {
				t.Errorf("this frame decoded: %v", err)
			}
		})
	}
}

// TestControlMessages holds the control sub-stream: JSON, one type per
// message, and a message that names no type refused rather than read as one.
func TestControlMessages(t *testing.T) {
	id := remote.NewOperationID()
	raw, err := remote.EncodeMessage(id, remote.Message{Type: remote.MessageOperation, Operation: id})
	if err != nil {
		t.Fatalf("the message did not encode: %v", err)
	}
	operation, stream, payload, err := remote.DecodeFrame(raw)
	if err != nil {
		t.Fatalf("the frame did not decode: %v", err)
	}
	if operation != id || stream != remote.StreamControl {
		t.Errorf("a control message rode on sub-stream %d of %q", stream, operation)
	}
	m, err := remote.DecodeMessage(payload)
	if err != nil {
		t.Fatalf("the message did not decode: %v", err)
	}
	if m.Type != remote.MessageOperation {
		t.Errorf("the message read back as %q", m.Type)
	}
	if _, err = remote.DecodeMessage([]byte(`{}`)); !errors.Is(err, remote.ErrFrame) {
		t.Errorf("a message naming no type decoded: %v", err)
	}
	if _, err = remote.DecodeMessage([]byte(`not json`)); !errors.Is(err, remote.ErrFrame) {
		t.Errorf("a payload that is not JSON decoded: %v", err)
	}
	if _, err = remote.EncodeMessage("short", remote.Message{Type: remote.MessageHeartbeat}); !errors.Is(err, remote.ErrFrame) {
		t.Errorf("a message on an id of another width encoded: %v", err)
	}
}

// TestErrorCrossesTheSeam holds that a driver's refusal reaches the control
// plane in the driver's own vocabulary: a caller reads the same sentinel it
// would from a driver in the same process.
func TestErrorCrossesTheSeam(t *testing.T) {
	for _, sentinel := range []error{
		driver.ErrNotFound, driver.ErrAlreadyExists, driver.ErrNotRunning,
		driver.ErrUnsupported, driver.ErrInvalid, driver.ErrTooLarge,
		context.DeadlineExceeded, context.Canceled,
	} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			// Bare, as a driver returns it.
			if got := remote.EncodeError(sentinel).Err(); !errors.Is(got, sentinel) {
				t.Errorf("the bare error crossed as %v", got)
			}
			// Wrapped, as a driver that added a sentence returns it.
			wrapped := fmt.Errorf("%w: the sandbox sbx_1", sentinel)
			got := remote.EncodeError(wrapped).Err()
			if !errors.Is(got, sentinel) {
				t.Errorf("the wrapped error crossed as %v", got)
			}
			if !strings.Contains(got.Error(), "sbx_1") {
				t.Errorf("the driver's own sentence was lost: %q", got)
			}
		})
	}
	if remote.EncodeError(nil) != nil {
		t.Errorf("a nil error encoded to something")
	}
	var absent *remote.Error
	if absent.Err() != nil {
		t.Errorf("an absent error decoded to something")
	}
	// An error the contract names no sentinel for crosses as its message.
	other := remote.EncodeError(errors.New("the engine is out of disk"))
	if other.Kind != "" || other.Err().Error() != "the engine is out of disk" {
		t.Errorf("an unclassified error crossed as %+v", other)
	}
}

// TestStreamingOperations holds which operations carry a sub-stream for their
// whole life, which is what decides whether one can be redelivered.
func TestStreamingOperations(t *testing.T) {
	for _, op := range []string{remote.OpExec, remote.OpAttach, remote.OpLogs,
		remote.OpExportTar, remote.OpImportTar, remote.OpOpen, remote.OpWrite} {
		if !remote.Streaming(op) {
			t.Errorf("%s streams and is not marked as such", op)
		}
	}
	for _, op := range []string{remote.OpCreate, remote.OpStart, remote.OpStop,
		remote.OpDelete, remote.OpUpdate, remote.OpInspect, remote.OpList, remote.OpTouch,
		remote.OpStat, remote.OpReadDir, remote.OpMkdir, remote.OpRemove, remote.OpMove} {
		if remote.Streaming(op) {
			t.Errorf("%s answers at once and is marked as streaming", op)
		}
	}
}

// TestCancelCrossesTheSeam is spec 021's row: a caller that went away cancels
// the operation on the worker rather than leaving it running for nobody.
func TestCancelCrossesTheSeam(t *testing.T) {
	host, err := native.New(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatalf("the worker's own driver did not open: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	s := openSeam(t, host)

	ref, err := s.driver.Create(context.Background(), driver.CreateSpec{
		ID: "sbx_cancel", Name: "cancel", Owner: "ops", Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("the create failed: %v", err)
	}
	t.Cleanup(func() { _ = s.driver.Delete(context.Background(), ref.ID) })

	ctx, cancel := context.WithCancel(context.Background())
	exec, err := s.driver.Exec(ctx, ref.ID, driver.ExecRequest{Command: []string{"sh", "-c", "sleep 30"}})
	if err != nil {
		t.Fatalf("the exec failed: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })
	cancel()
	// The wait is under a context of its own, so what ends it is the
	// cancellation crossing the seam and not this call's own deadline.
	waitCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	if _, err = exec.Wait(waitCtx); !errors.Is(err, context.Canceled) {
		t.Errorf("the wait ended with %v, want the caller's own cancellation", err)
	}
}

// TestExecStreamsAcrossTheSeam holds the sub-streams of one command: stdin
// down, stdout and stderr up, and the exit code as the operation's result.
func TestExecStreamsAcrossTheSeam(t *testing.T) {
	host, err := native.New(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatalf("the worker's own driver did not open: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	s := openSeam(t, host)
	ctx := context.Background()

	ref, err := s.driver.Create(ctx, driver.CreateSpec{
		ID: "sbx_streams", Name: "streams", Owner: "ops", Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("the create failed: %v", err)
	}
	t.Cleanup(func() { _ = s.driver.Delete(ctx, ref.ID) })

	exec, err := s.driver.Exec(ctx, ref.ID, driver.ExecRequest{
		Command: []string{"sh", "-c", "cat; echo to-stderr >&2; exit 7"},
		Stdin:   strings.NewReader("typed in"),
	})
	if err != nil {
		t.Fatalf("the exec failed: %v", err)
	}
	defer func() { _ = exec.Close() }()
	var out, errs bytes.Buffer
	done := make(chan struct{}, 2)
	go func() { _, _ = out.ReadFrom(exec.Stdout()); done <- struct{}{} }()
	go func() { _, _ = errs.ReadFrom(exec.Stderr()); done <- struct{}{} }()
	code, err := exec.Wait(ctx)
	<-done
	<-done
	if err != nil {
		t.Fatalf("the wait failed: %v", err)
	}
	if code != 7 {
		t.Errorf("the exit code crossed as %d, want 7", code)
	}
	if out.String() != "typed in" {
		t.Errorf("stdout carried %q, want what was typed in", out.String())
	}
	if !strings.Contains(errs.String(), "to-stderr") {
		t.Errorf("stderr carried %q", errs.String())
	}
	// An exec of an unknown sandbox is refused by the driver on the far side
	// and reaches this side as the driver's own sentinel, at the call rather
	// than at the wait.
	if _, err = s.driver.Exec(ctx, "sbx_absent", driver.ExecRequest{Command: []string{"true"}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("an exec of an unknown sandbox is %v, want ErrNotFound", err)
	}
}
