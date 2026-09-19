// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"

	driver "latere.ai/x/cella/runtime"
)

// logStream is the reader Logs hands back. Closing it ends the request as well
// as the reader, so a Follow that nobody reads any more stops costing a
// connection.
type logStream struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (l *logStream) Close() error {
	l.cancel()
	return l.PipeReader.Close()
}

// Logs streams the container's output, both streams interleaved in the order
// the engine recorded them. Follow runs until the container ends or the
// reader is closed; Since drops what was written before an instant and
// TailLines bounds the snapshot to the last lines.
func (d *Driver) Logs(ctx context.Context, id string, req driver.LogsRequest) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.TailLines < 0 {
		return nil, driver.ErrInvalid
	}
	if err := d.exists(ctx, id); err != nil {
		return nil, err
	}
	q := []string{"stdout", "true", "stderr", "true"}
	if req.Follow {
		q = append(q, "follow", "true")
	}
	if !req.Since.IsZero() {
		q = append(q, "since", strconv.FormatInt(req.Since.Unix(), 10))
	}
	if req.TailLines > 0 {
		q = append(q, "tail", strconv.Itoa(req.TailLines))
	}
	streamCtx, cancel := context.WithCancel(ctx)
	resp, err := d.client().do(streamCtx, http.MethodGet, d.client().libpodURL("/containers/"+containerName(id)+"/logs?"+query(q...)), nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("podman: reading the logs of %s: %w", id, err)
	}
	if serr := statusErr(resp); serr != nil {
		_ = resp.Body.Close()
		cancel()
		if notFound(serr) {
			return nil, driver.ErrNotFound
		}
		return nil, fmt.Errorf("podman: reading the logs of %s: %w", id, serr)
	}
	pr, pw := io.Pipe()
	body := resp.Body
	go func() {
		// The body is closed here rather than by the caller: the stream is
		// what the reader reads, and closing the reader ends the request.
		defer func() { _ = body.Close() }()
		defer cancel()
		_ = pw.CloseWithError(demux(body, pw, pw))
	}()
	return &logStream{PipeReader: pr, cancel: cancel}, nil
}
