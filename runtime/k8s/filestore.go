// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"sync"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/internal/fileshell"
)

var _ driver.FileStore = (*Driver)(nil)

// Stat describes one entry of the workspace.
func (d *Driver) Stat(ctx context.Context, id, p string) (driver.FileInfo, error) {
	session, err := d.fileSession(ctx, id, p)
	if err != nil {
		return driver.FileInfo{}, err
	}
	defer session.release()
	return session.stat(ctx, p)
}

// ReadDir lists the immediate entries of a directory, sorted by name.
func (d *Driver) ReadDir(ctx context.Context, id, p string) ([]driver.FileInfo, error) {
	session, err := d.fileSession(ctx, id, p)
	if err != nil {
		return nil, err
	}
	defer session.release()
	out, err := session.run(ctx, fileshell.ReadDir(session.root, p), nil, p)
	if err != nil {
		return nil, err
	}
	entries, err := fileshell.Parse(out, p)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b driver.FileInfo) int { return strings.Compare(a.Name, b.Name) })
	return entries, nil
}

// Open streams one file. The session lives as long as the stream, so the
// container the bytes come from is there until the caller closes it.
func (d *Driver) Open(ctx context.Context, id, p string) (io.ReadCloser, driver.FileInfo, error) {
	session, err := d.fileSession(ctx, id, p)
	if err != nil {
		return nil, driver.FileInfo{}, err
	}
	info, err := session.stat(ctx, p)
	if err == nil && info.IsDir {
		err = fmt.Errorf("%w: %s is a directory", driver.ErrInvalid, p)
	}
	if err != nil {
		session.release()
		return nil, driver.FileInfo{}, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	go func() {
		defer session.release()
		defer cancel()
		var diagnostic tail
		err := d.stream.stream(streamCtx, session.pod, execOpts{argv: fileshell.Cat(session.root, p)}, nil, pw, &diagnostic)
		_ = pw.CloseWithError(session.failure(err, p, &diagnostic))
	}()
	return &streamedFile{reader: pr, cancel: cancel}, info, nil
}

// streamedFile ends the command inside the container when the caller stops
// reading, rather than leaving it to write into a pipe nobody drains.
type streamedFile struct {
	reader *io.PipeReader
	cancel context.CancelFunc
}

func (f *streamedFile) Read(p []byte) (int, error) { return f.reader.Read(p) }

func (f *streamedFile) Close() error {
	f.cancel()
	return f.reader.Close()
}

// Write stages the body inside the container and commits it in a second
// command, both in one session. The commit runs only once the caller's own
// copy of the body has finished, which is what leaves the previous file whole
// when a body ends early.
func (d *Driver) Write(ctx context.Context, id string, req driver.WriteRequest) (int64, error) {
	session, err := d.fileSession(ctx, id, req.Path)
	if err != nil {
		return 0, err
	}
	defer session.release()
	mode := req.Mode.Perm()
	if mode == 0 {
		mode = driver.DefaultFileMode
	}
	staged := fileshell.Staged(req.Path, rand.Text())
	body := &recordingReader{r: req.Body}
	if body.r == nil {
		body.r = strings.NewReader("")
	}
	out, err := session.run(ctx, fileshell.WriteBody(session.root, staged, req.MaxBytes), body, req.Path)
	if err == nil {
		err = body.err()
	}
	var n int64
	if err == nil {
		n, err = fileshell.Size(out)
	}
	if err == nil {
		_, err = session.run(ctx, fileshell.Commit(session.root, staged, req.Path, mode), nil, req.Path)
	}
	if err != nil {
		clean := context.WithoutCancel(ctx)
		_, _ = session.run(clean, fileshell.Discard(session.root, staged), nil, staged)
		return 0, err
	}
	return n, nil
}

// Mkdir creates a directory and the missing parents.
func (d *Driver) Mkdir(ctx context.Context, id, p string) error {
	return d.act(ctx, id, p, fileshell.Mkdir)
}

// Remove deletes a file or a directory tree.
func (d *Driver) Remove(ctx context.Context, id, p string) error {
	return d.act(ctx, id, p, fileshell.Remove)
}

// act runs one program that names one path and answers nothing.
func (d *Driver) act(ctx context.Context, id, p string, build func(root, path string) []string) error {
	session, err := d.fileSession(ctx, id, p)
	if err != nil {
		return err
	}
	defer session.release()
	if path.Clean(p) == session.root {
		return fmt.Errorf("%w: the workspace itself is not the subject of this operation", driver.ErrInvalid)
	}
	_, err = session.run(ctx, build(session.root, p), nil, p)
	return err
}

// Move renames from onto the exact path to.
func (d *Driver) Move(ctx context.Context, id, from, to string) error {
	session, err := d.fileSession(ctx, id, from)
	if err != nil {
		return err
	}
	defer session.release()
	if _, err := relativeTo(to, session.root); err != nil {
		return err
	}
	if path.Clean(from) == session.root || path.Clean(to) == session.root {
		return fmt.Errorf("%w: the workspace itself is not movable", driver.ErrInvalid)
	}
	_, err = session.run(ctx, fileshell.Move(session.root, from, to), nil, from)
	return err
}

// fileSession is the container one run of file programs happens in: the
// sandbox's own while it runs, and the helper Pod over the same claim while
// it does not, which is what the Files capability declares.
type fileSession struct {
	d       *Driver
	pod     string
	root    string
	release func()
}

// fileSession reads the sandbox, holds the path to the lexical half of the
// containment rule, and opens the container the programs run in. The half
// that resolves symbolic links runs inside, where the filesystem is.
func (d *Driver) fileSession(ctx context.Context, id, p string) (*fileSession, error) {
	if d.stream == nil {
		return nil, fmt.Errorf("%w: this driver was built without a cluster connection", driver.ErrUnsupported)
	}
	spec, err := d.transferSpec(ctx, id)
	if err != nil {
		return nil, err
	}
	root := workspacePath(spec)
	if _, err := relativeTo(p, root); err != nil {
		return nil, err
	}
	pod, release, err := d.transferPod(ctx, id, spec)
	if err != nil {
		return nil, err
	}
	return &fileSession{d: d, pod: pod, root: root, release: release}, nil
}

// run carries one program into the container and reports what it printed, or
// the contract's error for the code it exited with.
func (s *fileSession) run(ctx context.Context, argv []string, stdin io.Reader, p string) (string, error) {
	var out strings.Builder
	var diagnostic tail
	err := s.d.stream.stream(ctx, s.pod, execOpts{argv: argv, stdin: stdin != nil}, stdin, &out, &diagnostic)
	return out.String(), s.failure(err, p, &diagnostic)
}

// failure is how one program's end reads: a fixed exit code is the contract's
// error, and anything else is the cluster's own failure with what the program
// said about it.
func (s *fileSession) failure(err error, p string, diagnostic *tail) error {
	if err == nil {
		return nil
	}
	if code, ok := exitCode(err); ok {
		return fileshell.Classify(code, p, diagnostic.String())
	}
	return s.d.wrapExec(err, diagnostic)
}

// stat is the read Open and Stat share.
func (s *fileSession) stat(ctx context.Context, p string) (driver.FileInfo, error) {
	out, err := s.run(ctx, fileshell.Stat(s.root, p), nil, p)
	if err != nil {
		return driver.FileInfo{}, err
	}
	return fileshell.One(out, p)
}

// recordingReader keeps the reason a body ended, which the copy into the
// container does not report back. The copy runs on the transport's goroutine,
// so the reason crosses one and is guarded.
type recordingReader struct {
	r  io.Reader
	mu sync.Mutex
	// failure is why the body ended, if it did not end of its own accord.
	failure error
}

func (r *recordingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && err != io.EOF {
		r.mu.Lock()
		r.failure = err
		r.mu.Unlock()
	}
	return n, err
}

// err is why the body ended, read once the copy of it has finished.
func (r *recordingReader) err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failure
}
