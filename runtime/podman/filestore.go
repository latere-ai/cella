// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

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

// diagnosticBytes is how much of a failed program's error stream is kept.
const diagnosticBytes = 2048

// Stat describes one entry of the workspace.
func (d *Driver) Stat(ctx context.Context, id, p string) (driver.FileInfo, error) {
	root, err := d.filePath(ctx, id, p)
	if err != nil {
		return driver.FileInfo{}, err
	}
	out, err := d.text(ctx, id, fileshell.Stat(root, p), p)
	if err != nil {
		return driver.FileInfo{}, err
	}
	return fileshell.One(out, p)
}

// ReadDir lists the immediate entries of a directory, sorted by name.
func (d *Driver) ReadDir(ctx context.Context, id, p string) ([]driver.FileInfo, error) {
	root, err := d.filePath(ctx, id, p)
	if err != nil {
		return nil, err
	}
	out, err := d.text(ctx, id, fileshell.ReadDir(root, p), p)
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

// Open streams one file. The entry is read first, so a caller learns the size
// and the mode before the body and a directory is refused rather than
// answered with the error of a utility.
func (d *Driver) Open(ctx context.Context, id, p string) (io.ReadCloser, driver.FileInfo, error) {
	info, err := d.Stat(ctx, id, p)
	if err != nil {
		return nil, driver.FileInfo{}, err
	}
	if info.IsDir {
		return nil, driver.FileInfo{}, fmt.Errorf("%w: %s is a directory", driver.ErrInvalid, p)
	}
	root, err := d.filePath(ctx, id, p)
	if err != nil {
		return nil, driver.FileInfo{}, err
	}
	running, err := d.Exec(ctx, id, driver.ExecRequest{Command: fileshell.Cat(root, p)})
	if err != nil {
		return nil, driver.FileInfo{}, err
	}
	// The error stream is drained as the body is read, so a program that
	// prints a diagnostic does not block on a full pipe.
	diagnostic := &boundedBuffer{}
	go func() { _, _ = io.Copy(diagnostic, running.Stderr()) }()
	return &fileStream{ctx: ctx, exec: running, path: p, diagnostic: diagnostic}, info, nil
}

// fileStream is one file's body with the program that produced it behind it.
// The end of the body is where the program's exit is read, so a transfer that
// ended early reports why instead of looking complete.
type fileStream struct {
	ctx        context.Context
	exec       driver.Exec
	path       string
	diagnostic *boundedBuffer
	done       bool
}

func (s *fileStream) Read(p []byte) (int, error) {
	n, err := s.exec.Stdout().Read(p)
	if err == io.EOF && !s.done {
		s.done = true
		code, waitErr := s.exec.Wait(s.ctx)
		if waitErr != nil {
			return n, waitErr
		}
		if failed := fileshell.Classify(code, s.path, s.diagnostic.String()); failed != nil {
			return n, failed
		}
	}
	return n, err
}

func (s *fileStream) Close() error { return s.exec.Close() }

// Write stages the body inside the sandbox, then commits it in a second call
// once the whole body has arrived. The two steps are what leaves the previous
// file whole when a body ends early: nothing is renamed onto the destination
// until the caller's own copy has finished.
func (d *Driver) Write(ctx context.Context, id string, req driver.WriteRequest) (int64, error) {
	root, err := d.filePath(ctx, id, req.Path)
	if err != nil {
		return 0, err
	}
	mode := req.Mode.Perm()
	if mode == 0 {
		mode = driver.DefaultFileMode
	}
	staged := fileshell.Staged(req.Path, rand.Text())
	body := &recordingReader{r: req.Body}
	if body.r == nil {
		body.r = strings.NewReader("")
	}
	out, err := d.run(ctx, id, fileshell.WriteBody(root, staged, req.MaxBytes), body, req.Path)
	if err == nil {
		err = body.err
	}
	if err != nil {
		d.discard(ctx, id, root, staged)
		return 0, err
	}
	n, err := fileshell.Size(out)
	if err != nil {
		d.discard(ctx, id, root, staged)
		return 0, err
	}
	if _, err := d.run(ctx, id, fileshell.Commit(root, staged, req.Path, mode), nil, req.Path); err != nil {
		d.discard(ctx, id, root, staged)
		return 0, err
	}
	return n, nil
}

// discard drops a staged body the write is not going to commit. It runs
// outside the caller's cancellation, because a cancelled write still leaves
// nothing behind, and a failure to clean up is not the caller's answer.
func (d *Driver) discard(ctx context.Context, id, root, staged string) {
	clean := context.WithoutCancel(ctx)
	_, _ = d.run(clean, id, fileshell.Discard(root, staged), nil, staged)
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
	root, err := d.filePath(ctx, id, p)
	if err != nil {
		return err
	}
	if path.Clean(p) == root {
		return fmt.Errorf("%w: the workspace itself is not the subject of this operation", driver.ErrInvalid)
	}
	_, err = d.run(ctx, id, build(root, p), nil, p)
	return err
}

// Move renames from onto the exact path to.
func (d *Driver) Move(ctx context.Context, id, from, to string) error {
	root, err := d.filePath(ctx, id, from)
	if err != nil {
		return err
	}
	if _, err := relative(root, to); err != nil {
		return err
	}
	if path.Clean(from) == root || path.Clean(to) == root {
		return fmt.Errorf("%w: the workspace itself is not movable", driver.ErrInvalid)
	}
	_, err = d.run(ctx, id, fileshell.Move(root, from, to), nil, from)
	return err
}

// filePath reads where the sandbox's files live and holds the path to the
// lexical half of the containment rule. The half that resolves symbolic links
// runs inside the sandbox, where the filesystem is.
func (d *Driver) filePath(ctx context.Context, id, p string) (string, error) {
	root, err := d.workspacePath(ctx, id)
	if err != nil {
		return "", err
	}
	if _, err := relative(root, p); err != nil {
		return "", err
	}
	return root, nil
}

// text runs one program and returns what it printed.
func (d *Driver) text(ctx context.Context, id string, argv []string, p string) (string, error) {
	return d.run(ctx, id, argv, nil, p)
}

// run carries one program into the sandbox and reports what it printed, or
// the contract's error for the code it exited with. A stopped sandbox has no
// exec session, which is how this driver answers ErrNotRunning.
func (d *Driver) run(ctx context.Context, id string, argv []string, stdin io.Reader, p string) (string, error) {
	running, err := d.Exec(ctx, id, driver.ExecRequest{Command: argv, Stdin: stdin})
	if err != nil {
		return "", err
	}
	defer func() { _ = running.Close() }()
	var out strings.Builder
	diagnostic := &boundedBuffer{}
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = io.Copy(&out, running.Stdout()) })
	wg.Go(func() { _, _ = io.Copy(diagnostic, running.Stderr()) })
	wg.Wait()
	code, err := running.Wait(ctx)
	if err != nil {
		return "", err
	}
	return out.String(), fileshell.Classify(code, p, diagnostic.String())
}

// recordingReader keeps the reason a body ended, which the engine's own copy
// of it does not report back.
type recordingReader struct {
	r   io.Reader
	err error
}

func (r *recordingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

// boundedBuffer keeps the head of a program's diagnostic output, bounded, so
// a failure says what the program said without holding the stream.
type boundedBuffer struct {
	mu   sync.Mutex
	text []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := diagnosticBytes - len(b.text); room > 0 {
		b.text = append(b.text, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.text)
}
