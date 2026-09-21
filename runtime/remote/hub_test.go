// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/runtime/runtimetest"
)

// recordingQueue is the operations table of design 010 as the hub uses it:
// the row per operation and the answer that ends it.
type recordingQueue struct {
	mu         sync.Mutex
	enqueued   []string
	acked      map[string][]byte
	enqueueErr error
	ackErr     error
}

func newQueue() *recordingQueue { return &recordingQueue{acked: map[string][]byte{}} }

func (q *recordingQueue) Enqueue(_ context.Context, _, id, _, opType string, payload []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.enqueueErr != nil {
		return q.enqueueErr
	}
	if len(payload) == 0 {
		return errors.New("an operation row carries its arguments")
	}
	q.enqueued = append(q.enqueued, id+" "+opType)
	return nil
}

func (q *recordingQueue) Acknowledge(_ context.Context, id string, result []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ackErr != nil {
		return q.ackErr
	}
	q.acked[id] = result
	return nil
}

func (q *recordingQueue) rows() ([]string, map[string][]byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	acked := make(map[string][]byte, len(q.acked))
	maps.Copy(acked, q.acked)
	return append([]string(nil), q.enqueued...), acked
}

// TestHubRecordsEveryOperation holds that the table is the record: one row per
// operation, with the `op_` id of design 001, and the answer written back.
func TestHubRecordsEveryOperation(t *testing.T) {
	host, err := native.New(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatalf("the worker's own driver did not open: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	queue := newQueue()
	s := openSeamWith(t, host, remote.HubOptions{Offline: time.Minute, Queue: queue})

	ref, err := s.driver.Create(t.Context(), driver.CreateSpec{
		ID: "sbx_recorded", Name: "recorded", Owner: "ops", Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("the create failed: %v", err)
	}
	t.Cleanup(func() { _ = s.driver.Delete(context.Background(), ref.ID) })

	enqueued, acked := queue.rows()
	if len(enqueued) == 0 {
		t.Fatalf("the create wrote no row")
	}
	for _, row := range enqueued {
		if len(row) < len(remote.OperationIDPrefix) || row[:len(remote.OperationIDPrefix)] != remote.OperationIDPrefix {
			t.Errorf("the row %q does not carry the op_ prefix of design 001", row)
		}
	}
	if len(acked) == 0 {
		t.Fatalf("no row was answered")
	}
	for id, result := range acked {
		var answer struct {
			OK  bool `json:"ok"`
			Res *struct {
				Ref *driver.Ref `json:"ref"`
			} `json:"response"`
		}
		if err = json.Unmarshal(result, &answer); err != nil {
			t.Fatalf("the answer of %s did not decode: %v", id, err)
		}
		if !answer.OK {
			t.Errorf("the answer of %s is a refusal: %s", id, result)
		}
	}
}

// TestHubRefusesWhenTheRowCannotBeWritten holds that an operation the table
// refused is never sent: the record and the work commit together.
func TestHubRefusesWhenTheRowCannotBeWritten(t *testing.T) {
	host, err := native.New(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatalf("the worker's own driver did not open: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	queue := newQueue()
	queue.enqueueErr = errors.New("the database is gone")
	s := openSeamWith(t, host, remote.HubOptions{Offline: time.Minute, Queue: queue})

	if err = s.driver.Start(t.Context(), "sbx_1"); err == nil || !contains(err.Error(), "the database is gone") {
		t.Errorf("an operation whose row could not be written answered %v", err)
	}
	// An acknowledgement that cannot be written is logged and never returned:
	// the operation the caller asked for has already happened.
	queue.enqueueErr, queue.ackErr = nil, errors.New("the database is gone")
	ref, err := s.driver.Create(t.Context(), driver.CreateSpec{
		ID: "sbx_unacked", Name: "unacked", Owner: "ops", Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Errorf("a create whose row could not be answered was refused: %v", err)
	} else {
		_ = s.driver.Delete(context.Background(), ref.ID)
	}
}

// TestHubReportsItsWorkers holds what an environment's phase is computed
// from: the registrations, their heartbeats, and whether each holds a stream.
func TestHubReportsItsWorkers(t *testing.T) {
	hub := remote.NewHub(remote.HubOptions{Offline: time.Minute})
	if hub.Workers("env_absent") != nil {
		t.Errorf("an environment nobody registered on reports workers")
	}
	first, err := hub.Register("env_a", remote.Registration{Driver: "podman", Isolation: v1.IsolationContainer})
	if err != nil {
		t.Fatal(err)
	}
	second, err := hub.Register("env_a", remote.Registration{Driver: "podman", Isolation: v1.IsolationContainer})
	if err != nil {
		t.Fatal(err)
	}
	workers := hub.Workers("env_a")
	if len(workers) != 2 {
		t.Fatalf("the environment reports %d workers, want the two that registered", len(workers))
	}
	for _, w := range workers {
		if w.Worker != first.Worker && w.Worker != second.Worker {
			t.Errorf("the environment reports the worker %q, which never registered", w.Worker)
		}
		if w.Connected {
			t.Errorf("a worker that registered and opened no stream reports as connected")
		}
		if w.LastHeartbeat.IsZero() {
			t.Errorf("a registration stamped no heartbeat")
		}
	}

	// A clean disconnect forgets the registration, so the phase moves at
	// once rather than at the end of a lease nobody will renew.
	hub.Forget("env_a", first.Worker)
	if len(hub.Workers("env_a")) != 1 {
		t.Errorf("forgetting one worker left %d", len(hub.Workers("env_a")))
	}
	hub.Forget("env_absent", "wrk_nobody") // an environment nobody holds is not an error
	hub.Forget("env_a", "wrk_nobody")

	// Releasing the environment drops everything it held, which is what a
	// delete of the object writes.
	hub.Release("env_a")
	if len(hub.Workers("env_a")) != 0 {
		t.Errorf("releasing the environment left workers behind")
	}
	if _, held := hub.Transport("env_a").Registration(); held {
		t.Errorf("releasing the environment left its registration behind")
	}
	hub.Release("env_absent") // releasing one nobody holds is not an error
}

// TestReleaseEndsTheStream holds that deleting an environment ends the
// connections its workers hold rather than leaving them talking to nothing.
func TestReleaseEndsTheStream(t *testing.T) {
	host, err := native.New(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatalf("the worker's own driver did not open: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	s := openSeam(t, host)
	s.hub.Release("env_test")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err = s.driver.Ready(t.Context()); errors.Is(err, remote.ErrNoWorker) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the environment is still ready after it was released: %v", err)
}

// TestHubRefusesAFrameThatBelongsTheOtherWay holds that a worker sending a
// message only the control plane sends ends the connection: a side that
// cannot read a frame cannot know what it missed.
func TestHubRefusesAFrameThatBelongsTheOtherWay(t *testing.T) {
	hub := remote.NewHub(remote.HubOptions{Offline: time.Minute})
	registered, err := hub.Register("env_a", remote.Registration{Driver: "native", Isolation: v1.IsolationNone})
	if err != nil {
		t.Fatal(err)
	}
	control, workerSide := net.Pipe()
	t.Cleanup(func() { _ = workerSide.Close() })
	client := &pipeConn{conn: workerSide}
	go func() {
		hello, encodeErr := remote.EncodeMessage(remote.NoOperation,
			remote.Message{Type: remote.MessageHello, Worker: registered.Worker})
		if encodeErr != nil {
			return
		}
		if writeErr := client.WriteFrame(hello); writeErr != nil {
			return
		}
		wrong, encodeErr := remote.EncodeMessage(remote.NoOperation, remote.Message{Type: remote.MessageOperation})
		if encodeErr == nil {
			_ = client.WriteFrame(wrong)
		}
	}()
	err = hub.Serve(t.Context(), "env_a", &pipeConn{conn: control})
	if err == nil {
		t.Errorf("a frame that belongs the other way left the stream open")
	}
}

// TestNewRefusesWithoutATransport holds that a driver is never built over
// nothing, because every call on it would be a call into a nil transport.
func TestNewRefusesWithoutATransport(t *testing.T) {
	if _, err := remote.New(remote.Options{Environment: "env_a"}); err == nil {
		t.Errorf("a remote driver was built with no transport")
	}
}

// TestLinkEndsEverySubStream holds that a connection that drops releases what
// is blocked on it: a reader waiting on a sub-stream learns the stream ended
// rather than waiting forever.
func TestLinkEndsEverySubStream(t *testing.T) {
	control, workerSide := net.Pipe()
	link := remote.NewLink(&pipeConn{conn: control}, remote.LinkOptions{})
	channel := link.Open(remote.NewOperationID(), true)
	if link.Err() != nil {
		t.Errorf("an open link reports the error %v", link.Err())
	}
	reader := channel.Up(remote.StreamStdout)
	read := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := reader.Read(buf)
		read <- err
	}()
	_ = workerSide.Close()
	link.Shutdown(errors.New("the connection dropped"))
	select {
	case err := <-read:
		if err == nil {
			t.Errorf("the reader was released without an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("a reader blocked on a sub-stream was never released")
	}
	if link.Err() == nil {
		t.Errorf("a link that ended reports no error")
	}
	// Every call on a link that ended is refused rather than queued for a
	// connection that is gone.
	if err := link.Send(remote.NoOperation, remote.Message{Type: remote.MessageHeartbeat}); err == nil {
		t.Errorf("a message was queued on a link that ended")
	}
	if _, err := channel.Result(t.Context()); err == nil {
		t.Errorf("a result was awaited on a link that ended")
	}
	if err := channel.Accepted(t.Context()); err == nil {
		t.Errorf("an acceptance was awaited on a link that ended")
	}
	// Close on an operation whose link is gone is not an error: the
	// operation is over either way.
	if err := channel.Close(); err != nil {
		t.Errorf("closing an operation on a link that ended is %v", err)
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// TestOptionalInterfacesCrossTheSeam holds what a worker whose driver
// declares no optional interface answers: every call of that interface
// crosses and comes back as the contract's own ErrUnsupported, which is what
// the API reports as the capability the environment lacks.
//
// The remote driver embeds every optional interface unconditionally, so this
// is the rule that keeps a caller gated on what the worker actually provides
// rather than on what the type happens to embed.
func TestOptionalInterfacesCrossTheSeam(t *testing.T) {
	s := openSeam(t, runtimetest.Nop{})
	ctx := t.Context()

	if caps := s.driver.Capabilities(); caps.Files || caps.Attach {
		t.Fatalf("a worker declaring nothing reported %+v", caps)
	}
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Stat", func() error { _, err := s.driver.Stat(ctx, "sbx_1", "/workspace/a"); return err }},
		{"ReadDir", func() error { _, err := s.driver.ReadDir(ctx, "sbx_1", "/workspace"); return err }},
		{"Open", func() error { _, _, err := s.driver.Open(ctx, "sbx_1", "/workspace/a"); return err }},
		{"Write", func() error {
			_, err := s.driver.Write(ctx, "sbx_1", driver.WriteRequest{
				Path: "/workspace/a", Body: strings.NewReader("x"),
			})
			return err
		}},
		{"Mkdir", func() error { return s.driver.Mkdir(ctx, "sbx_1", "/workspace/d") }},
		{"Remove", func() error { return s.driver.Remove(ctx, "sbx_1", "/workspace/a") }},
		{"Move", func() error { return s.driver.Move(ctx, "sbx_1", "/workspace/a", "/workspace/b") }},
		{"Attach", func() error { _, err := s.driver.Attach(ctx, "sbx_1", driver.AttachRequest{}); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, driver.ErrUnsupported) {
				t.Errorf("%s on a worker that declares none is %v, want ErrUnsupported", tc.name, err)
			}
		})
	}
}

// TestWholeTreeTransfersCrossTheSeam holds the two archive operations of the
// contract: the bytes travel on the operation's own sub-stream and the answer
// says the transfer landed.
func TestWholeTreeTransfersCrossTheSeam(t *testing.T) {
	host, err := native.New(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatalf("the worker's own driver did not open: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	s := openSeam(t, host)
	ctx := t.Context()

	ref, err := s.driver.Create(ctx, driver.CreateSpec{
		ID: "sbx_archive", Name: "archive", Owner: "ops", Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("the create failed: %v", err)
	}
	t.Cleanup(func() { _ = s.driver.Delete(context.Background(), ref.ID) })

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	body := []byte("what crossed the seam\n")
	if err = tw.WriteHeader(&tar.Header{Name: "crossed.txt", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err = tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.driver.ImportTar(ctx, ref.ID, "/workspace", bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatalf("the import over the stream failed: %v", err)
	}
	var out bytes.Buffer
	if err = s.driver.ExportTar(ctx, ref.ID, []string{"/workspace/crossed.txt"}, &out); err != nil {
		t.Fatalf("the export over the stream failed: %v", err)
	}
	tr := tar.NewReader(&out)
	header, err := tr.Next()
	if err != nil {
		t.Fatalf("the exported archive has no entry: %v", err)
	}
	got, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("the exported entry did not read: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("the entry %q carries %q, want %q", header.Name, got, body)
	}
	// An archive of a sandbox the worker does not have is the driver's own
	// refusal, carried across the seam.
	if err = s.driver.ExportTar(ctx, "sbx_absent", []string{"/workspace"}, io.Discard); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("exporting an unknown sandbox is %v, want ErrNotFound", err)
	}
	if err = s.driver.ImportTar(ctx, "sbx_absent", "/workspace", bytes.NewReader(archive.Bytes())); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("importing into an unknown sandbox is %v, want ErrNotFound", err)
	}
}

// TestLogsCrossTheSeam holds the one sub-stream a follow carries, and the
// refusal that reaches the caller of Logs rather than the caller of Read.
func TestLogsCrossTheSeam(t *testing.T) {
	host, err := native.New(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatalf("the worker's own driver did not open: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	s := openSeam(t, host)
	ctx := t.Context()

	ref, err := s.driver.Create(ctx, driver.CreateSpec{
		ID: "sbx_logs", Name: "logs", Owner: "ops",
		Command: []string{"sh", "-c", "echo the main process spoke; sleep 30"},
	})
	if err != nil {
		t.Fatalf("the create failed: %v", err)
	}
	t.Cleanup(func() { _ = s.driver.Delete(context.Background(), ref.ID) })

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		reader, logsErr := s.driver.Logs(ctx, ref.ID, driver.LogsRequest{})
		if logsErr != nil {
			t.Fatalf("the logs over the stream failed: %v", logsErr)
		}
		got, readErr := io.ReadAll(reader)
		_ = reader.Close()
		if readErr != nil {
			t.Fatalf("the log stream did not read: %v", readErr)
		}
		if strings.Contains(string(got), "the main process spoke") {
			// A follow the caller stopped wanting ends the operation rather
			// than holding the connection.
			follow, followErr := s.driver.Logs(ctx, ref.ID, driver.LogsRequest{Follow: true})
			if followErr != nil {
				t.Fatalf("the follow failed: %v", followErr)
			}
			if err = follow.Close(); err != nil {
				t.Errorf("closing a follow is %v", err)
			}
			if _, err = s.driver.Logs(ctx, "sbx_absent", driver.LogsRequest{}); !errors.Is(err, driver.ErrNotFound) {
				t.Errorf("the logs of an unknown sandbox are %v, want ErrNotFound", err)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the main process's output never crossed the seam")
}

// TestReadAnswersBeforeItStreams holds the ordering the protocol needs: the
// far side waits for a file read's answer before it reads the bytes, and one
// connection carries both, so an answer queued behind bytes nobody is
// draining yet would hold the whole stream. This is the shape that hung
// under load before the answer was sent first.
func TestReadAnswersBeforeItStreams(t *testing.T) {
	host, err := native.New(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatalf("the worker's own driver did not open: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	s := openSeam(t, host)
	ctx := t.Context()

	ref, err := s.driver.Create(ctx, driver.CreateSpec{
		ID: "sbx_ordered", Name: "ordered", Owner: "ops", Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("the create failed: %v", err)
	}
	t.Cleanup(func() { _ = s.driver.Delete(context.Background(), ref.ID) })

	// A body of many frames: the copy on the worker's side is well ahead of
	// the reader on this side by the time the answer is asked for.
	body := strings.Repeat("the bytes that arrive before anybody reads them\n", 4096)
	if _, err = s.driver.Write(ctx, ref.ID, driver.WriteRequest{
		Path: "/workspace/ordered.txt", Body: strings.NewReader(body),
	}); err != nil {
		t.Fatalf("the write failed: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		reader, info, openErr := s.driver.Open(ctx, ref.ID, "/workspace/ordered.txt")
		if openErr != nil {
			done <- openErr
			return
		}
		defer func() { _ = reader.Close() }()
		if info.Size != int64(len(body)) {
			done <- errors.New("the entry reports a size the file does not have")
			return
		}
		got, readErr := io.ReadAll(reader)
		if readErr != nil {
			done <- readErr
			return
		}
		if string(got) != body {
			done <- errors.New("the file read back as something else")
			return
		}
		done <- nil
	}()
	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("the read failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the read never answered, which is the answer queued behind its own bytes")
	}
}

// TestABodyThatFailedLeavesTheFileWhole holds what a cut-short write costs:
// the sub-stream's zero-length frame is what says a body is whole, so a body
// that failed never sends one and the worker discards what it had staged.
func TestABodyThatFailedLeavesTheFileWhole(t *testing.T) {
	host, err := native.New(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatalf("the worker's own driver did not open: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	s := openSeam(t, host)
	ctx := t.Context()

	ref, err := s.driver.Create(ctx, driver.CreateSpec{
		ID: "sbx_cutshort", Name: "cutshort", Owner: "ops", Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("the create failed: %v", err)
	}
	t.Cleanup(func() { _ = s.driver.Delete(context.Background(), ref.ID) })

	const path = "/workspace/cutshort.txt"
	if _, err = s.driver.Write(ctx, ref.ID, driver.WriteRequest{
		Path: path, Body: strings.NewReader("what was there before"),
	}); err != nil {
		t.Fatalf("the first write failed: %v", err)
	}
	cut := io.MultiReader(strings.NewReader("half"), failingReader{})
	if _, err = s.driver.Write(ctx, ref.ID, driver.WriteRequest{Path: path, Body: cut}); err == nil {
		t.Errorf("a body that failed part way reported no error")
	}
	reader, _, err := s.driver.Open(ctx, ref.ID, path)
	if err != nil {
		t.Fatalf("the read failed: %v", err)
	}
	defer func() { _ = reader.Close() }()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("the file did not read: %v", err)
	}
	if string(got) != "what was there before" {
		t.Errorf("a write whose body failed left %q", got)
	}
}

// failingReader is a body cut short: it fails on the first read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the body was cut short") }

// TestAShortWindowDoesNotOutrunTheHeartbeat holds the floor the lease puts
// under the liveness window. A worker heartbeats every HeartbeatInterval, so
// a window below the lease would answer that no worker holds the stream
// between two heartbeats and refuse every operation issued there.
func TestAShortWindowDoesNotOutrunTheHeartbeat(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}
	hub := remote.NewHub(remote.HubOptions{Offline: time.Second, Now: clock})
	registered, err := hub.Register("env_a", remote.Registration{Driver: "native", Isolation: v1.IsolationNone})
	if err != nil {
		t.Fatal(err)
	}
	control, workerSide := net.Pipe()
	t.Cleanup(func() { _ = workerSide.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = hub.Serve(ctx, "env_a", &pipeConn{conn: control}) }()
	// The worker says hello and then holds its stream open without sending
	// anything else, which is an idle worker between two heartbeats.
	hello, err := remote.EncodeMessage(remote.NoOperation,
		remote.Message{Type: remote.MessageHello, Worker: registered.Worker})
	if err != nil {
		t.Fatal(err)
	}
	if err = (&pipeConn{conn: workerSide}).WriteFrame(hello); err != nil {
		t.Fatal(err)
	}
	transport := hub.Transport("env_a")
	deadline := time.Now().Add(5 * time.Second)
	for !transport.Live() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !transport.Live() {
		t.Fatal("the worker never became live")
	}
	advance(remote.HeartbeatInterval + time.Second)
	if !transport.Live() {
		t.Error("a worker one heartbeat old is not live under a short window")
	}
	// Past the lease it is gone, whatever the window said.
	advance(remote.HeartbeatTimeout)
	if transport.Live() {
		t.Error("a worker past the lease is still live")
	}
}
