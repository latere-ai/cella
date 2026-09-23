// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/runtime/remote"
)

// nativeSandbox opens a native driver for the worker and a seam over it with
// the options given, and creates one sandbox to run in.
func nativeSandbox(t *testing.T, id string, o remote.HubOptions, so remote.ServerOptions, wrap seamWrap) (*seam, string) {
	t.Helper()
	host, err := native.New(t.TempDir())
	if err != nil {
		t.Fatalf("the worker's own driver did not open: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	s := openSeamOver(t, host, o, so, wrap)
	ref, err := s.driver.Create(t.Context(), driver.CreateSpec{ID: id, Name: "streams", Owner: "ops", Command: []string{"sleep", "60"}})
	if err != nil {
		t.Fatalf("the create failed: %v", err)
	}
	t.Cleanup(func() { _ = s.driver.Delete(context.Background(), ref.ID) })
	return s, ref.ID
}

// zeros is an exec of n bytes of output.
func zeros(n int) driver.ExecRequest {
	return driver.ExecRequest{Command: []string{"head", "-c", strconv.Itoa(n), "/dev/zero"}}
}

// drain reads one exec to its end and returns its stdout's size, what its
// stderr said, and its exit code.
func drain(ctx context.Context, e driver.Exec, chunk int, pause time.Duration) (int64, string, int, error) {
	var stderr bytes.Buffer
	errDone := make(chan error, 1)
	go func() { _, err := stderr.ReadFrom(e.Stderr()); errDone <- err }()
	var n int64
	buf := make([]byte, chunk)
	for {
		read, err := e.Stdout().Read(buf)
		n += int64(read)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return n, "", 0, err
		}
		if pause > 0 {
			time.Sleep(pause)
		}
	}
	if err := <-errDone; err != nil {
		return n, "", 0, err
	}
	code, err := e.Wait(ctx)
	return n, stderr.String(), code, err
}

// TestAStalledReaderHoldsOnlyItsOwnStream is the row the window exists for.
// A caller that stops reading one exec's output holds that exec and nothing
// else: another exec on the same connection runs to its end meanwhile, and
// the stalled one completes whole once its reader returns. Before the window,
// the stalled output held the connection's read pump, and every other
// operation on it waited behind bytes nobody was reading.
func TestAStalledReaderHoldsOnlyItsOwnStream(t *testing.T) {
	s, id := nativeSandbox(t, "sbx_stalled", remote.HubOptions{Offline: time.Minute},
		remote.ServerOptions{ReportInterval: time.Minute}, seamWrap{})
	ctx := t.Context()
	const size = 4 * remote.DefaultWindow
	stalled, err := s.driver.Exec(ctx, id, zeros(size))
	if err != nil {
		t.Fatalf("the exec failed: %v", err)
	}
	defer func() { _ = stalled.Close() }()
	// The control plane holds a whole window of the stalled output: the
	// worker has spent its credit and waits.
	waitFor(t, "a window of the stalled output held", func() bool { return s.hub.Peak("env_test") >= remote.DefaultWindow })

	other := make(chan error, 1)
	go func() {
		e, execErr := s.driver.Exec(ctx, id, driver.ExecRequest{Command: []string{"sh", "-c", "echo beside; exit 3"}})
		if execErr != nil {
			other <- execErr
			return
		}
		defer func() { _ = e.Close() }()
		out, readErr := io.ReadAll(e.Stdout())
		if readErr != nil {
			other <- readErr
			return
		}
		code, waitErr := e.Wait(ctx)
		switch {
		case waitErr != nil:
			other <- waitErr
		case code != 3 || strings.TrimSpace(string(out)) != "beside":
			other <- errors.New("the other exec answered " + string(out) + " with exit " + strconv.Itoa(code))
		default:
			other <- nil
		}
	}()
	select {
	case err = <-other:
		if err != nil {
			t.Fatalf("the other exec failed while one reader stalled: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("an exec on the same connection waited behind a reader that stopped reading")
	}
	// A lifecycle call and a file operation move too.
	if err = s.driver.Touch(ctx, id); err != nil {
		t.Errorf("a touch beside the stalled reader failed: %v", err)
	}
	if _, err = s.driver.Stat(ctx, id, "/workspace"); err != nil {
		t.Errorf("a stat beside the stalled reader failed: %v", err)
	}

	n, stderr, code, err := drain(ctx, stalled, 32<<10, 0)
	if err != nil || code != 0 || stderr != "" {
		t.Fatalf("the stalled exec ended with exit %d, stderr %q, err %v", code, stderr, err)
	}
	if n != size {
		t.Errorf("the stalled exec carried %d bytes once read, want %d", n, size)
	}
	if peak := s.hub.Peak("env_test"); peak > remote.DefaultWindow {
		t.Errorf("the control plane held %d bytes of one sub-stream, past the window of %d", peak, remote.DefaultWindow)
	}
}

// TestRemoteStreams is design 021's row over one connection: an exec of
// 64 MiB of output, an attach with a resize, a tar both ways, and a file read
// past the window, the last four at once. No sub-stream on either side ever
// holds more than the window, which the link measures; the exec's reader
// stalls first until the window is full, so the bound is reached and not
// merely unchallenged.
func TestRemoteStreams(t *testing.T) {
	s, id := nativeSandbox(t, "sbx_remote_streams", remote.HubOptions{Offline: time.Minute},
		remote.ServerOptions{ReportInterval: time.Minute}, seamWrap{})
	ctx := t.Context()
	if !s.hub.Credited("env_test") || !s.server.Credited() {
		t.Fatalf("the stream runs without credit")
	}
	const output = 64 << 20
	exec, err := s.driver.Exec(ctx, id, zeros(output))
	if err != nil {
		t.Fatalf("the exec failed: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })
	waitFor(t, "the exec's output fills the window", func() bool { return s.hub.Peak("env_test") >= remote.DefaultWindow })

	// A body past the window, not a repetition of one byte, so a transfer
	// that dropped or reordered a frame reads back as something else.
	pattern := make([]byte, 251)
	for i := range pattern {
		pattern[i] = byte(i * 7)
	}
	body := bytes.Repeat(pattern, (3*remote.DefaultWindow)/len(pattern)+1)
	want := sha256.Sum256(body)

	t.Run("at once", func(t *testing.T) {
		t.Run("the exec of 64 MiB", func(t *testing.T) {
			t.Parallel()
			// Small reads with a pause now and then: the window is refilled
			// by credits a few bytes at a time as well as a frame at a time.
			n, stderr, code, err := drain(ctx, exec, 256<<10, time.Millisecond)
			if err != nil || code != 0 || stderr != "" {
				t.Fatalf("the exec ended with exit %d, stderr %q, err %v", code, stderr, err)
			}
			if n != output {
				t.Errorf("the exec carried %d bytes, want %d", n, output)
			}
		})
		t.Run("an attach with a resize", func(t *testing.T) {
			t.Parallel()
			session, err := s.driver.Attach(ctx, id, driver.AttachRequest{Cols: 80, Rows: 24})
			if err != nil {
				t.Fatalf("the attach failed: %v", err)
			}
			defer func() { _ = session.Close() }()
			screen := &terminal{}
			go screen.pump(session)
			if _, err = session.Write([]byte("stty size\n")); err != nil {
				t.Fatal(err)
			}
			screen.await(t, "24 80")
			if err = session.Resize(120, 40); err != nil {
				t.Fatalf("the resize failed: %v", err)
			}
			screen.awaitAsking(t, "40 120", func() error {
				_, err := session.Write([]byte("stty size\n"))
				return err
			})
			if _, err = session.Write([]byte("exit 5\n")); err != nil {
				t.Fatal(err)
			}
			if code, waitErr := session.Wait(ctx); waitErr != nil || code != 5 {
				t.Errorf("the session ended with exit %d, err %v", code, waitErr)
			}
		})
		t.Run("a tar both ways", func(t *testing.T) {
			t.Parallel()
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			if err := tw.WriteHeader(&tar.Header{Name: "carried.bin", Mode: 0o644, Size: int64(len(body))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.driver.ImportTar(ctx, id, "/workspace/tar", &archive); err != nil {
				t.Fatalf("the import failed: %v", err)
			}
			var exported bytes.Buffer
			if err := s.driver.ExportTar(ctx, id, []string{"/workspace/tar/carried.bin"}, &exported); err != nil {
				t.Fatalf("the export failed: %v", err)
			}
			tr := tar.NewReader(&exported)
			if _, err := tr.Next(); err != nil {
				t.Fatalf("the exported archive has no entry: %v", err)
			}
			got, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			if sha256.Sum256(got) != want {
				t.Errorf("the file came back as %d bytes that are not the %d sent", len(got), len(body))
			}
		})
		t.Run("a file read past the window", func(t *testing.T) {
			t.Parallel()
			if _, err := s.driver.Write(ctx, id, driver.WriteRequest{Path: "/workspace/read.bin", Body: bytes.NewReader(body)}); err != nil {
				t.Fatalf("the write failed: %v", err)
			}
			reader, info, err := s.driver.Open(ctx, id, "/workspace/read.bin")
			if err != nil {
				t.Fatalf("the open failed: %v", err)
			}
			defer func() { _ = reader.Close() }()
			if info.Size != int64(len(body)) {
				t.Errorf("the entry reports %d bytes, want %d", info.Size, len(body))
			}
			got, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("the read failed: %v", err)
			}
			if sha256.Sum256(got) != want {
				t.Errorf("the file read back as %d bytes that are not the %d written", len(got), len(body))
			}
		})
	})

	control, worker := s.hub.Peak("env_test"), s.server.Peak()
	t.Logf("the most one sub-stream held: %d bytes on the control plane, %d on the worker, of a window of %d",
		control, worker, remote.DefaultWindow)
	if control > remote.DefaultWindow {
		t.Errorf("the control plane held %d bytes of one sub-stream, past the window of %d", control, remote.DefaultWindow)
	}
	if worker > remote.DefaultWindow {
		t.Errorf("the worker held %d bytes of one sub-stream, past the window of %d", worker, remote.DefaultWindow)
	}
}

// terminal is what a session printed so far, read beside the test.
type terminal struct {
	mu  sync.Mutex
	out bytes.Buffer
}

func (s *terminal) pump(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		s.mu.Lock()
		s.out.Write(buf[:n])
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// awaitAsking writes what prints the text again until it appears. A resize
// reaches the terminal beside the input rather than in line with it, as it
// does for the exec of any container engine, so the first command typed
// after one may still read the old window; the window is applied, and a
// later command reads it.
func (s *terminal) awaitAsking(t *testing.T, text string, ask func() error) {
	t.Helper()
	var next time.Time
	waitFor(t, "the terminal printing "+text, func() bool {
		if now := time.Now(); now.After(next) {
			if err := ask(); err != nil {
				t.Fatal(err)
			}
			next = now.Add(200 * time.Millisecond)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return strings.Contains(s.out.String(), text)
	})
}

func (s *terminal) await(t *testing.T, text string) {
	t.Helper()
	waitFor(t, "the terminal printing "+text, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return strings.Contains(s.out.String(), text)
	})
}

// TestACancelReleasesTheBodyReader holds that a caller who went away releases
// the worker's driver at once. The driver is blocked reading a body the
// caller never finished; the cancel ends that read with the cancellation, the
// operation ends on the worker, and the file keeps what it had. Before, the
// read waited until the whole connection ended.
func TestACancelReleasesTheBodyReader(t *testing.T) {
	s, id := nativeSandbox(t, "sbx_released", remote.HubOptions{Offline: time.Minute},
		remote.ServerOptions{ReportInterval: time.Minute}, seamWrap{})
	const path = "/workspace/kept.txt"
	if _, err := s.driver.Write(t.Context(), id, driver.WriteRequest{Path: path, Body: strings.NewReader("what was there")}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the worker idle", func() bool { return s.server.Running() == 0 })

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := s.driver.Write(ctx, id, driver.WriteRequest{Path: path, Body: &unfinishedBody{ctx: ctx}})
		done <- err
	}()
	waitFor(t, "the first half on the worker", func() bool { return s.server.Peak() >= int64(len("half")) })
	waitFor(t, "the write running on the worker", func() bool { return s.server.Running() == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("the caller's write ended with %v, want its own cancellation", err)
	}
	waitFor(t, "the worker's driver released from the body", func() bool { return s.server.Running() == 0 })

	reader, _, err := s.driver.Open(t.Context(), id, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	if got, _ := io.ReadAll(reader); string(got) != "what was there" {
		t.Errorf("a write the caller abandoned left %q", got)
	}
}

// unfinishedBody sends half a body and then nothing until its caller gives
// up.
type unfinishedBody struct {
	ctx  context.Context
	sent bool
}

func (b *unfinishedBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "half"), nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

// TestAPeerWithoutCreditKeepsThePreviousStream is a worker and a control plane
// of different releases. The worker's hello loses its window and its watch on
// the way, which is the hello of a worker before credit and what a control
// plane before credit reads of a hello of this release. Neither side then
// sends the other anything that release does not know: no hello in answer, no
// credit, no Watch and no event. Exec output and an archive of several
// windows still cross, and the control plane holds no more of a stalled
// sub-stream than its window and one frame, which is the back pressure of
// before with room for a window.
func TestAPeerWithoutCreditKeepsThePreviousStream(t *testing.T) {
	const window = 64 << 10
	worker := &tap{stripHello: true}
	control := &tap{}
	s, id := nativeSandbox(t, "sbx_skew", remote.HubOptions{Offline: time.Minute, Window: window},
		remote.ServerOptions{Window: window, ReportInterval: time.Minute},
		seamWrap{
			control: func(c remote.FrameConn) remote.FrameConn { control.FrameConn = c; return control },
			worker:  func(c remote.FrameConn) remote.FrameConn { worker.FrameConn = c; return worker },
		})
	ctx := t.Context()
	if s.hub.Credited("env_test") || s.server.Credited() {
		t.Fatalf("credit was agreed with a peer that announced none")
	}

	const output = 16 * window
	stalled, err := s.driver.Exec(ctx, id, zeros(output))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stalled.Close() }()
	waitFor(t, "the stalled output at the window", func() bool { return s.hub.Peak("env_test") >= window })
	n, stderr, code, err := drain(ctx, stalled, 4096, 0)
	if err != nil || code != 0 || stderr != "" || n != output {
		t.Fatalf("the exec without credit carried %d bytes with exit %d, stderr %q, err %v", n, code, stderr, err)
	}
	if peak := s.hub.Peak("env_test"); peak > window+remote.MaxFrameBytes {
		t.Errorf("the control plane held %d bytes of one sub-stream without credit, past a window and a frame", peak)
	}

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	body := bytes.Repeat([]byte{7}, 8*window)
	if err = tw.WriteHeader(&tar.Header{Name: "skew.bin", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err = tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.driver.ImportTar(ctx, id, "/workspace", &archive); err != nil {
		t.Fatalf("an archive of several windows did not cross without credit: %v", err)
	}

	received, sent := worker.seen()
	for _, kind := range []string{remote.MessageHello, remote.MessageCredit, remote.MessageOperation + ":" + remote.OpWatch} {
		if slices.Contains(received, kind) {
			t.Errorf("the control plane sent %s to a worker that announced no window", kind)
		}
	}
	for _, kind := range []string{remote.MessageCredit, remote.MessageEvent} {
		if slices.Contains(sent, kind) {
			t.Errorf("the worker sent %s to a control plane that answered no window", kind)
		}
	}
	fromWorker, _ := control.seen()
	if !slices.Contains(fromWorker, remote.MessageHello) || !slices.Contains(received, remote.MessageOperation+":"+remote.OpExec) {
		t.Fatalf("the taps saw nothing cross, so they prove nothing: %v, %v", fromWorker, received)
	}
}
