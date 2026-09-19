// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// MaxLogTailBytes bounds memory retained for a requested initial log tail.
// Larger selected tails fail; untailed streaming has no total-size limit.
const MaxLogTailBytes = 1 << 20

const maxLogRecordBytes = 256 << 10

type logReader struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (r *logReader) Close() error { r.cancel(); return r.PipeReader.Close() }

// Logs streams combined main-process stdout and stderr. Since selects chunks by
// capture time. TailLines applies to the initial snapshot; Follow then emits new
// chunks until the main process ends or the reader closes. Logs survive restarts.
func (d *Driver) Logs(ctx context.Context, id string, req driver.LogsRequest) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.TailLines < 0 {
		return nil, driver.ErrInvalid
	}
	d.mu.Lock()
	_, err := d.load(id)
	main := d.mains[id]
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(d.dir(id), "logs.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	reader := &logReader{PipeReader: pr, cancel: cancel}
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			select {
			case <-finished:
				return
			default:
				_ = pr.CloseWithError(ctx.Err())
			}
		case <-finished:
		}
	}()
	go func() {
		defer func() { _ = f.Close() }()
		defer cancel()
		defer close(finished)
		err := streamLogs(ctx, f, pw, req, info.Size(), main)
		_ = pw.CloseWithError(err)
	}()
	return reader, nil
}
func streamLogs(ctx context.Context, f *os.File, dst io.Writer, req driver.LogsRequest, size int64, main *mainProcess) error {
	// A limited reader freezes the snapshot boundary while the process appends.
	// Records are appended by one writer, but a snapshot may end mid-record: the
	// decoder consumes that partial chunk again from its original byte offset.
	offset := int64(0)
	var tail []byte
	for offset < size {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, n, err := readChunk(f, offset, size-offset)
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			if mainFinished(main) {
				return io.ErrUnexpectedEOF
			}
			break
		}
		if err != nil {
			return err
		}
		offset += n
		if chunk.At.Before(req.Since) {
			continue
		}
		if req.TailLines == 0 {
			if _, err = dst.Write(chunk.Data); err != nil {
				return err
			}
		} else {
			tail = append(tail, chunk.Data...)
			tail = lastLines(tail, req.TailLines)
			if len(tail) > MaxLogTailBytes {
				return fmt.Errorf("%w: log tail exceeds %d bytes; use untailed streaming", driver.ErrInvalid, MaxLogTailBytes)
			}
		}
	}
	if len(tail) > 0 {
		if _, err := dst.Write(tail); err != nil {
			return err
		}
	}
	if !req.Follow || main == nil {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		for offset < info.Size() {
			chunk, n, err := readChunk(f, offset, info.Size()-offset)
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				if mainFinished(main) {
					return io.ErrUnexpectedEOF
				}
				break
			}
			if err != nil {
				return err
			}
			offset += n
			if !chunk.At.Before(req.Since) {
				if _, err = dst.Write(chunk.Data); err != nil {
					return err
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-main.done:
			// The supervisor closes done after the final append, so drain once more.
			info, err = f.Stat()
			if err != nil {
				return err
			}
			if offset >= info.Size() {
				return nil
			}
		case <-ticker.C:
		}
	}
}
func readChunk(f *os.File, offset, size int64) (logChunk, int64, error) {
	var chunk logChunk
	line, err := bufio.NewReader(io.NewSectionReader(f, offset, min(size, maxLogRecordBytes))).ReadBytes('\n')
	if err != nil && len(line) >= maxLogRecordBytes {
		return chunk, 0, fmt.Errorf("%w: log record exceeds size limit", driver.ErrInvalid)
	}
	if err != nil {
		return chunk, 0, err
	}
	err = json.Unmarshal(line, &chunk)
	return chunk, int64(len(line)), err
}
func lastLines(b []byte, n int) []byte {
	end := len(b)
	if end > 0 && b[end-1] == '\n' {
		end--
	}
	for i := end - 1; i >= 0; i-- {
		if b[i] == '\n' {
			n--
			if n == 0 {
				return b[i+1:]
			}
		}
	}
	return b
}

func mainFinished(main *mainProcess) bool {
	if main == nil {
		return true
	}
	select {
	case <-main.done:
		return true
	default:
		return false
	}
}
