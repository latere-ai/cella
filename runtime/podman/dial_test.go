// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package podman

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"

	driver "latere.ai/x/cella/runtime"
)

// TestPodmanPublishesDeclaredPorts: a create asks the engine to publish every
// declared port on loopback at a host port the engine picks, and one that
// declares none asks for nothing.
func TestPodmanPublishesDeclaredPorts(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_web", Name: "web", Owner: "alice",
		Ports: []driver.Port{{Name: "web", Port: 8080}, {Name: "debug", Port: 9222, Expose: driver.ExposeNone}}})
	want := []portMapping{
		{ContainerPort: 8080, HostIP: "127.0.0.1", Protocol: "tcp"},
		{ContainerPort: 9222, HostIP: "127.0.0.1", Protocol: "tcp"},
	}
	if got := f.container(t, "sbx_web").ports; !reflect.DeepEqual(got, want) {
		t.Fatalf("the create asked to publish %+v, want %+v", got, want)
	}
	create(t, d, driver.CreateSpec{ID: "sbx_plain", Name: "plain", Owner: "alice"})
	if got := f.container(t, "sbx_plain").ports; got != nil {
		t.Fatalf("a sandbox that declared no port asked to publish %+v", got)
	}
}

// TestPodmanDial: a declared port of a running sandbox is reached through the
// host port the engine reports, and everything else is refused with the
// contract's errors.
func TestPodmanDial(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_web", Name: "web", Owner: "alice",
		Ports: []driver.Port{{Name: "web", Port: 8080}}})

	conn, err := d.Dial(t.Context(), "sbx_web", 8080)
	if err != nil {
		t.Fatalf("Dial of a declared port: %v", err)
	}
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || line != "hello\n" {
		t.Fatalf("read back %q, %v", line, err)
	}
	_ = conn.Close()

	for _, tc := range []struct {
		name string
		id   string
		port int
		want error
	}{
		{"an undeclared port", "sbx_web", 9090, driver.ErrNotFound},
		{"a port below the range", "sbx_web", 0, driver.ErrInvalid},
		{"a port above the range", "sbx_web", 70000, driver.ErrInvalid},
		{"an id outside the syntax", "../x", 8080, driver.ErrInvalid},
		{"an absent sandbox", "sbx_absent", 8080, driver.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := d.Dial(t.Context(), tc.id, tc.port)
			if conn != nil {
				_ = conn.Close()
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Dial answered %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("an engine that fails the read", func(t *testing.T) {
		f.fault("GET "+libpod+"/containers/cella-sbx_web/json", http.StatusInternalServerError)
		conn, err := d.Dial(t.Context(), "sbx_web", 8080)
		if conn != nil {
			_ = conn.Close()
		}
		if err == nil || errors.Is(err, driver.ErrNotFound) || errors.Is(err, driver.ErrNotRunning) {
			t.Fatalf("a failed read answered %v", err)
		}
	})

	t.Run("a published port nothing answers on", func(t *testing.T) {
		c := f.container(t, "sbx_web")
		f.mu.Lock()
		_ = c.published[8080].Close()
		f.mu.Unlock()
		conn, err := d.Dial(t.Context(), "sbx_web", 8080)
		if conn != nil {
			_ = conn.Close()
		}
		var opErr *net.OpError
		if !errors.As(err, &opErr) {
			t.Fatalf("a closed host port answered %v, want the refused dial", err)
		}
	})

	if err = d.Stop(t.Context(), "sbx_web"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Dial(t.Context(), "sbx_web", 8080); !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("a stopped sandbox answered %v, want ErrNotRunning", err)
	}
	// A stopped sandbox whose container is gone keeps its volumes, and is
	// still a sandbox that is not running rather than one that is absent.
	f.mu.Lock()
	delete(f.containers, containerName("sbx_web"))
	f.mu.Unlock()
	if _, err = d.Dial(t.Context(), "sbx_web", 8080); !errors.Is(err, driver.ErrNotRunning) {
		t.Fatalf("a sandbox with no container answered %v, want ErrNotRunning", err)
	}
}

// TestPublishedPortsHostDefault: an engine that reports the wildcard or no
// host address for a binding is dialed on loopback, where it was asked to
// publish.
func TestPublishedPortsHostDefault(t *testing.T) {
	f := newFake(t)
	d := f.driver(t)
	create(t, d, driver.CreateSpec{ID: "sbx_wild", Name: "wild", Owner: "alice",
		Ports: []driver.Port{{Name: "web", Port: 8080}}})
	c := f.container(t, "sbx_wild")
	f.mu.Lock()
	old := c.published[8080]
	_, port, _ := net.SplitHostPort(old.Addr().String())
	_ = old.Close()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	f.mu.Unlock()
	if err != nil {
		t.Skipf("the host port was taken back before it could be rebound: %v", err)
	}
	f.mu.Lock()
	c.published[8080] = wildcard{ln}
	f.mu.Unlock()
	go echoAll(ln)
	conn, err := d.Dial(t.Context(), "sbx_wild", 8080)
	if err != nil {
		t.Fatalf("a wildcard binding was not dialed on loopback: %v", err)
	}
	_ = conn.Close()
}

// wildcard reports its address as the engine does for a binding on every
// interface.
type wildcard struct{ net.Listener }

func (w wildcard) Addr() net.Addr {
	addr := *w.Listener.Addr().(*net.TCPAddr)
	addr.IP = net.IPv4zero
	return &addr
}
