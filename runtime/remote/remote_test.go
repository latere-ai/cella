// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/runtime/native"
	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/runtime/runtimetest"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
)

// pipeConn is one end of a stream over a net.Pipe: length-prefixed frames, so
// the protocol above is proven without a listener and without a WebSocket.
type pipeConn struct {
	conn net.Conn
	mu   sync.Mutex
}

func (p *pipeConn) ReadFrame() ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(p.conn, header[:]); err != nil {
		return nil, err
	}
	size := int(header[0])<<24 | int(header[1])<<16 | int(header[2])<<8 | int(header[3])
	if size < 0 || size > remote.MaxFrameBytes+remote.OperationIDLen+1 {
		return nil, errors.New("pipe: a frame past the bound")
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(p.conn, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (p *pipeConn) WriteFrame(raw []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	size := len(raw)
	header := [4]byte{byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size)}
	if _, err := p.conn.Write(header[:]); err != nil {
		return err
	}
	_, err := p.conn.Write(raw)
	return err
}

func (p *pipeConn) Close() error { return p.conn.Close() }

// seam is one environment: a hub, a worker running the driver under it, and
// the remote driver over the stream between them.
type seam struct {
	hub    *remote.Hub
	driver *remote.Driver
	worker driver.Driver
	server *remote.Server
	cancel context.CancelFunc
	done   chan struct{}
	// control and workerSide are the two ends of the connection, which a
	// test closes to take the stream away.
	control, workerSide net.Conn
}

// openSeam stands one worker up against one hub over a pipe and returns the
// remote driver of that environment, connected and registered.
func openSeam(t *testing.T, host driver.Driver) *seam {
	t.Helper()
	return openSeamWith(t, host, remote.HubOptions{Offline: time.Minute})
}

// openSeamWith is openSeam over hub options a test chooses, for the rows that
// are about what the hub records rather than what the driver does.
func openSeamWith(t *testing.T, host driver.Driver, o remote.HubOptions) *seam {
	t.Helper()
	return openSeamOver(t, host, o, remote.ServerOptions{ReportInterval: 50 * time.Millisecond}, seamWrap{})
}

// seamWrap puts something between each side and its end of the connection,
// which is how a test watches what crosses it or stands in for a peer of
// another release. A nil field leaves that side's end as it is.
type seamWrap struct {
	control, worker func(remote.FrameConn) remote.FrameConn
}

func (w seamWrap) apply(wrap func(remote.FrameConn) remote.FrameConn, conn remote.FrameConn) remote.FrameConn {
	if wrap == nil {
		return conn
	}
	return wrap(conn)
}

// openSeamOver is openSeam over the options of both sides, for the rows about
// the stream itself: its window on either side, and how often the worker
// reports. The worker's id and driver are filled in here.
func openSeamOver(t *testing.T, host driver.Driver, o remote.HubOptions, so remote.ServerOptions, wrap seamWrap) *seam {
	t.Helper()
	hub := remote.NewHub(o)
	registered, err := hub.Register("env_test", remote.Registration{
		Driver: host.Name(), Isolation: host.Isolation(), Capabilities: host.Capabilities(),
	})
	if err != nil {
		t.Fatalf("the worker did not register: %v", err)
	}
	control, workerSide := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	s := &seam{hub: hub, worker: host, cancel: cancel, done: make(chan struct{}, 2),
		control: control, workerSide: workerSide}
	so.Worker, so.Driver = registered.Worker, host
	s.server = remote.NewServer(wrap.apply(wrap.worker, &pipeConn{conn: workerSide}), so)
	go func() {
		defer func() { s.done <- struct{}{} }()
		_ = hub.Serve(ctx, "env_test", wrap.apply(wrap.control, &pipeConn{conn: control}))
	}()
	go func() {
		defer func() { s.done <- struct{}{} }()
		_ = s.server.Run(ctx)
	}()
	s.driver, err = remote.New(remote.Options{Environment: "env_test", Transport: hub.Transport("env_test")})
	if err != nil {
		t.Fatalf("the remote driver was not built: %v", err)
	}
	// The stream is up once the driver answers Ready, which is the worker's
	// hello having reached the hub.
	waitReady(t, s.driver)
	t.Cleanup(func() {
		cancel()
		_ = control.Close()
		_ = workerSide.Close()
		<-s.done
		<-s.done
	})
	return s
}

func waitReady(t *testing.T, d *remote.Driver) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := d.Ready(context.Background()); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no worker became ready inside the deadline")
}

// TestWorkerConformance is spec 004's row for the remote driver: every method
// the contract names, issued through runtime/remote, executes on a worker
// running native and returns what the direct call returns.
func TestWorkerConformance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the native driver runs host processes and this suite needs a POSIX shell")
	}
	runtimetest.Run(t, func(t *testing.T) driver.Driver {
		host, err := native.New(filepath.Join(t.TempDir(), "native"))
		if err != nil {
			t.Fatalf("the worker's own driver did not open: %v", err)
		}
		t.Cleanup(func() { _ = host.Close() })
		return openSeam(t, host).driver
	}, runtimetest.Options{})
}

// TestRemoteReportsTheWorkersDeclaration holds that the four calls a
// registration answers never reach the worker, and say what the worker said.
func TestRemoteReportsTheWorkersDeclaration(t *testing.T) {
	host, err := native.New(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatalf("the worker's own driver did not open: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	s := openSeam(t, host)

	if s.driver.Name() != remote.DriverName {
		t.Errorf("the driver names itself %q, want %q", s.driver.Name(), remote.DriverName)
	}
	if s.driver.Isolation() != host.Isolation() {
		t.Errorf("the environment declares %q isolation, want the worker's %q", s.driver.Isolation(), host.Isolation())
	}
	if !reflect.DeepEqual(s.driver.Capabilities(), host.Capabilities()) {
		t.Errorf("the environment declares %+v, want the worker's %+v", s.driver.Capabilities(), host.Capabilities())
	}
	if err = s.driver.Preflight(context.Background()); err != nil {
		t.Errorf("a registered environment failed preflight: %v", err)
	}
}

// TestRemoteWithoutAWorker holds what an environment nobody serves answers: it
// declares nothing, it is not ready, and every operation is refused rather
// than enqueued for a worker that will never claim it.
func TestRemoteWithoutAWorker(t *testing.T) {
	hub := remote.NewHub(remote.HubOptions{})
	d, err := remote.New(remote.Options{Environment: "env_none", Transport: hub.Transport("env_none")})
	if err != nil {
		t.Fatalf("the remote driver was not built: %v", err)
	}
	ctx := t.Context()
	if d.Isolation() != driver.IsolationNone {
		t.Errorf("an environment with no registration declares %q isolation", d.Isolation())
	}
	if !reflect.DeepEqual(d.Capabilities(), driver.Capabilities{}) {
		t.Errorf("an environment with no registration declares %+v", d.Capabilities())
	}
	if err = d.Preflight(ctx); !errors.Is(err, driver.ErrNotRunning) {
		t.Errorf("preflight on an environment with no registration is %v, want ErrNotRunning", err)
	}
	if err = d.Ready(ctx); !errors.Is(err, remote.ErrNoWorker) {
		t.Errorf("ready on an environment with no worker is %v, want ErrNoWorker", err)
	}
	if _, err = d.Inspect(ctx, "sbx_1"); !errors.Is(err, remote.ErrNoWorker) {
		t.Errorf("an operation on an environment with no worker is %v, want ErrNoWorker", err)
	}
	if err = d.Start(ctx, "sbx_1"); !errors.Is(err, remote.ErrNoWorker) {
		t.Errorf("a start on an environment with no worker is %v, want ErrNoWorker", err)
	}
}

// TestRegistrationMismatch holds spec 021's refusal: one environment is one
// data plane, so a worker reporting another driver or another isolation class
// is refused rather than admitted beside the first.
func TestRegistrationMismatch(t *testing.T) {
	hub := remote.NewHub(remote.HubOptions{})
	first := remote.Registration{Driver: "podman", Isolation: v1.IsolationContainer,
		Capabilities: driver.Capabilities{Files: true, Attach: true}}
	if _, err := hub.Register("env_a", first); err != nil {
		t.Fatalf("the first registration was refused: %v", err)
	}
	for _, tc := range []struct {
		name string
		r    remote.Registration
	}{
		{"another driver", remote.Registration{Driver: "k8s", Isolation: v1.IsolationContainer}},
		{"another isolation class", remote.Registration{Driver: "podman", Isolation: v1.IsolationVM}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := hub.Register("env_a", tc.r); !errors.Is(err, remote.ErrRegistrationMismatch) {
				t.Errorf("the registration was answered %v, want ErrRegistrationMismatch", err)
			}
		})
	}
	// A registration that matches joins, and what the environment can do is
	// what every worker on it can do.
	second := remote.Registration{Driver: "podman", Isolation: v1.IsolationContainer,
		Capabilities: driver.Capabilities{Files: true}}
	if _, err := hub.Register("env_a", second); err != nil {
		t.Fatalf("a matching registration was refused: %v", err)
	}
	d, err := remote.New(remote.Options{Environment: "env_a", Transport: hub.Transport("env_a")})
	if err != nil {
		t.Fatalf("the remote driver was not built: %v", err)
	}
	if caps := d.Capabilities(); !caps.Files || caps.Attach {
		t.Errorf("the environment declares %+v, want the intersection: files without attach", caps)
	}
	// A registration naming no driver is a defect on the worker's side.
	if _, err = hub.Register("env_a", remote.Registration{}); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a registration naming no driver is %v, want ErrInvalid", err)
	}
	if _, err = hub.Register("", first); !errors.Is(err, driver.ErrInvalid) {
		t.Errorf("a registration naming no environment is %v, want ErrInvalid", err)
	}
}

// TestUnknownWorkerIsRefused holds that a stream is only ever served to a
// worker the control plane minted an id for, so a key alone opens nothing.
func TestUnknownWorkerIsRefused(t *testing.T) {
	hub := remote.NewHub(remote.HubOptions{})
	control, workerSide := net.Pipe()
	t.Cleanup(func() { _ = workerSide.Close() })
	go func() {
		client := &pipeConn{conn: workerSide}
		raw, encodeErr := remote.EncodeMessage(remote.NoOperation,
			remote.Message{Type: remote.MessageHello, Worker: "wrk_nobody"})
		if encodeErr == nil {
			_ = client.WriteFrame(raw)
		}
	}()
	err := hub.Serve(t.Context(), "env_a", &pipeConn{conn: control})
	if !errors.Is(err, remote.ErrUnknownWorker) {
		t.Errorf("a stream from an unregistered worker is %v, want ErrUnknownWorker", err)
	}
}

// TestObservedStateAnswersWithoutTheWorker holds that a read of a sandbox
// costs no operation: the worker reports what its driver holds, and Inspect
// and List answer from that report.
func TestObservedStateAnswersWithoutTheWorker(t *testing.T) {
	host, err := native.New(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatalf("the worker's own driver did not open: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	s := openSeam(t, host)
	ctx := t.Context()

	ref, err := s.driver.Create(ctx, driver.CreateSpec{
		ID: "sbx_observed", Name: "observed", Owner: "ops", Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("the create on the worker failed: %v", err)
	}
	// The worker reports after every operation that changes what it holds,
	// so the sandbox is readable without another operation.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		states, listErr := s.driver.List(ctx, driver.Filter{})
		if listErr != nil {
			t.Fatalf("the list failed: %v", listErr)
		}
		if len(states) == 1 && states[0].ID == ref.ID {
			state, inspectErr := s.driver.Inspect(ctx, ref.ID)
			if inspectErr != nil {
				t.Fatalf("the inspect failed: %v", inspectErr)
			}
			if state.Owner != "ops" || state.Name != "observed" {
				t.Errorf("the reported state is %+v, want the identity the create stamped", state)
			}
			// A filter narrows the reported states rather than the worker's.
			narrowed, filterErr := s.driver.List(ctx, driver.Filter{Owner: "someone-else"})
			if filterErr != nil {
				t.Fatalf("the filtered list failed: %v", filterErr)
			}
			if len(narrowed) != 0 {
				t.Errorf("the filter admitted %d states of another owner", len(narrowed))
			}
			_ = s.driver.Delete(ctx, ref.ID)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the worker never reported the sandbox it created")
}
