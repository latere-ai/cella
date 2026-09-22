// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"latere.ai/x/cella/internal/events"
	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// The channels of design 008's exec stream. One byte says which, four bytes
// big-endian say how long the payload is, and the payload follows.
const (
	channelStdout byte = 1
	channelStderr byte = 2
	channelExit   byte = 3
	channelError  byte = 4
)

const (
	// execFrameBytes is the longest payload one frame carries, which design
	// 008 fixes at one mebibyte. A read larger than this is written as more
	// than one frame.
	execFrameBytes = 1 << 20
	// execStreamType is the content type design 008 gives the framed stream.
	// It is the route's own and is never negotiated.
	execStreamType = "application/vnd.cella.exec-stream"
)

// execStream serves POST /v1/sandboxes/{id}/exec without ?wait=1: the frames
// of design 008, written as the command produces them.
//
// The status is written once the driver has the command, so a refusal the
// driver made is still an HTTP status. After that byte the status can no
// longer change, which is what the error frame carries instead.
func (h *handler) execStream(w http.ResponseWriter, r *http.Request, obj v1.Sandbox, req execRequest, timeout time.Duration) {
	h.touch(r, obj)
	started := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	running, err := h.Controller.Exec(ctx, obj.Status.ID, driver.ExecRequest{
		Command: req.Command, Env: req.Env, Workdir: req.Workdir, Timeout: timeout,
	})
	if err != nil {
		respondError(w, err)
		return
	}
	defer func() { _ = running.Close() }()
	requestID := w.Header().Get(RequestIDHeader)
	w.Header().Set("Content-Type", execStreamType)
	w.WriteHeader(http.StatusOK)
	frames := &execFrames{w: w}

	copied := make(chan error, 2)
	go func() { copied <- frames.copy(channelStdout, running.Stdout()) }()
	go func() { copied <- frames.copy(channelStderr, running.Stderr()) }()
	var copyErr error
	for range 2 {
		if failure := <-copied; failure != nil {
			copyErr = errors.Join(copyErr, failure)
			_ = running.Close()
		}
	}
	code, err := running.Wait(ctx)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// A command the timeout ended reports 124, which is the code the
		// bounded form reports for the same end.
		code, err = 124, nil
	}
	if err == nil {
		err = copyErr
	}
	if err != nil {
		frames.fail(err, requestID)
		return
	}
	elapsed := time.Since(started).Milliseconds()
	_ = frames.write(channelExit, []byte(strconv.Itoa(code)))
	h.emit(r, obj, events.TypeExec, events.Exec{ExitCode: code, DurationMS: elapsed})
}

// execFrames writes design 008's frames to one response body. Two output
// channels reach it from two goroutines, so the lock is what keeps one
// frame's header, length and payload together on the wire.
type execFrames struct {
	mu sync.Mutex
	w  http.ResponseWriter
}

// write puts one frame on the wire and flushes it, so a caller reads output
// as the command produces it rather than when the response ends.
func (f *execFrames) write(channel byte, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var header [5]byte
	header[0] = channel
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := f.w.Write(header[:]); err != nil {
		return err
	}
	if _, err := f.w.Write(payload); err != nil {
		return err
	}
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// fail writes the error frame: the envelope of design 008 as JSON, which is
// what a failure says once the status has been sent.
func (f *execFrames) fail(err error, requestID string) {
	_, envelope := errorEnvelope(err, requestID)
	body, marshalErr := json.Marshal(errorFrame{Error: envelope})
	if marshalErr != nil {
		// The envelope is two strings and a map of strings, so this is
		// unreachable; an empty frame still ends the stream on channel 4,
		// which is what a reader acts on.
		body = nil
	}
	_ = f.write(channelError, body)
}

// copy reads one output channel and writes a frame per read, bounded at the
// frame cap so a fast producer never asks for a frame larger than design 008
// allows. An end of the reader is the channel's end and not a failure.
func (f *execFrames) copy(channel byte, src io.Reader) error {
	buf := make([]byte, execFrameBytes)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if failure := f.write(channel, buf[:n]); failure != nil {
				return failure
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
