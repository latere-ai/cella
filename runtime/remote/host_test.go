// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"
	driver "latere.ai/x/cella/runtime"
	"latere.ai/x/cella/runtime/remote"
	"latere.ai/x/cella/runtime/runtimetest"
)

// TestAHeldCreateReachesTheControlPlane is the held frame of spec 082 across
// the seam: a worker that holds a create while its host decides tells the
// control plane, which names the sandbox the create is for from its own
// record. The create then answers as the host decided.
func TestAHeldCreateReachesTheControlPlane(t *testing.T) {
	release := make(chan struct{})
	admit := func(_ context.Context, opType string, _ remote.Request, hold func() error) error {
		if opType != remote.OpCreate {
			return nil
		}
		if err := hold(); err != nil {
			return err
		}
		<-release
		return nil
	}
	s := openSeamOver(t, runtimetest.Nop{}, remote.HubOptions{Offline: time.Minute},
		remote.ServerOptions{ReportInterval: 50 * time.Millisecond, Admit: admit}, seamWrap{})
	held := make(chan [2]string, 4)
	s.hub.OnHeld(func(environment, sandbox string) { held <- [2]string{environment, sandbox} })

	created := make(chan error, 1)
	go func() {
		_, err := s.driver.Create(context.Background(), driver.CreateSpec{ID: "sbx_held", Name: "held"})
		created <- err
	}()
	select {
	case got := <-held:
		if got != [2]string{"env_test", "sbx_held"} {
			t.Fatalf("the held create named %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the held frame never reached the control plane")
	}
	close(release)
	if err := <-created; err != nil {
		t.Fatalf("the create the host allowed answered %v", err)
	}
	// A hold the hub has no create for, such as one after the create was
	// answered, is dropped rather than passed on.
	s.hub.OnHeld(nil)
}

// TestARefusalBeforeTheDriverReachesAStreamingCaller: an operation the
// worker's Admit refuses never reaches the driver, and a caller of an
// operation that waits for its start learns the refusal rather than waiting
// on a start that never comes.
func TestARefusalBeforeTheDriverReachesAStreamingCaller(t *testing.T) {
	refusal := errors.New("not on this host")
	var mu sync.Mutex
	var answered []string
	s := openSeamOver(t, runtimetest.Nop{}, remote.HubOptions{Offline: time.Minute}, remote.ServerOptions{
		ReportInterval: 50 * time.Millisecond,
		Admit: func(_ context.Context, opType string, _ remote.Request, _ func() error) error {
			if opType == remote.OpExec || opType == remote.OpStop {
				return refusal
			}
			return nil
		},
		Answered: func(opType string, _ remote.Request, _ remote.Response, err error) {
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				answered = append(answered, opType)
			}
		},
	}, seamWrap{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.driver.Exec(ctx, "sbx_any", driver.ExecRequest{Command: []string{"true"}}); err == nil ||
		!errContains(err, "not on this host") {
		t.Errorf("a refused exec answered %v", err)
	}
	if err := s.driver.Stop(ctx, "sbx_any"); err == nil || !errContains(err, "not on this host") {
		t.Errorf("a refused stop answered %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(answered)
		mu.Unlock()
		if n >= 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("the refusals were not handed to Answered: %v", answered)
}

func errContains(err error, text string) bool { return err != nil && contains(err.Error(), text) }

// TestHostRefusalsCrossTheSeam: the three refusals of a host keep their
// meaning across the wire, so the control plane tells host_refused,
// host_timeout and workspace_not_granted apart.
func TestHostRefusalsCrossTheSeam(t *testing.T) {
	for _, sentinel := range []error{driver.ErrHostRefused, driver.ErrHostTimeout, driver.ErrWorkspaceNotGranted} {
		crossed := remote.EncodeError(errors.Join(errors.New("the host said so"), sentinel)).Err()
		if !errors.Is(crossed, sentinel) {
			t.Errorf("%v did not survive the seam: %v", sentinel, crossed)
		}
	}
}

// TestTheHubKeepsEachWorkersHost: a registration carrying the host is kept
// per worker and read back with the workers, a copy the caller cannot write
// through; one carrying none reports none.
func TestTheHubKeepsEachWorkersHost(t *testing.T) {
	hub := remote.NewHub(remote.HubOptions{Offline: time.Minute})
	host := v1.HostInfo{OS: "darwin", Arch: "arm64", Grants: []v1.HostGrant{{Path: "/Users/dev/site", Write: true}},
		Network: v1.HostNetwork{Mode: "allowlist", Hosts: []string{"github.com"}}}
	with, err := hub.Register("env_host", remote.Registration{Driver: "local", Isolation: "process", Host: &host,
		Capabilities: driver.Capabilities{HostWorkspace: true, Files: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = hub.Register("env_host", remote.Registration{Driver: "local", Isolation: "process",
		Capabilities: driver.Capabilities{Files: true}}); err != nil {
		t.Fatal(err)
	}
	host.Grants[0].Path = "/elsewhere"
	var reported, bare int
	for _, w := range hub.Workers("env_host") {
		switch {
		case w.Worker == with.Worker && w.Host != nil && w.Host.Grants[0].Path == "/Users/dev/site" && w.Host.OS == "darwin":
			reported++
			w.Host.Grants[0].Path = "/written-through"
		case w.Host == nil:
			bare++
		}
	}
	if reported != 1 || bare != 1 {
		t.Fatalf("the workers read back as %+v", hub.Workers("env_host"))
	}
	if again := hub.Workers("env_host"); again[0].Host != nil && again[0].Host.Grants[0].Path == "/written-through" ||
		len(again) > 1 && again[1].Host != nil && again[1].Host.Grants[0].Path == "/written-through" {
		t.Error("a caller wrote through the hub's own record of a host")
	}
	// What every worker can do is what the environment can do, the host
	// workspace among it.
	r, _ := hub.Transport("env_host").Registration()
	if r.Capabilities.HostWorkspace {
		t.Error("an environment one of whose workers binds no host folder declares HostWorkspace")
	}
}

// closingConn records how a link ended it.
type closingConn struct {
	*pipeConn
	mu     sync.Mutex
	code   int
	reason string
}

func (c *closingConn) CloseWith(code int, reason string) error {
	c.mu.Lock()
	c.code, c.reason = code, reason
	c.mu.Unlock()
	return c.Close()
}

// TestALinkClosesWithItsRefusal: a link shut down with a CloseError ends a
// connection that can carry a close code with that code and reason, and one
// ended any other way closes plainly.
func TestALinkClosesWithItsRefusal(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = b.Close() }()
	conn := &closingConn{pipeConn: &pipeConn{conn: a}}
	link := remote.NewLink(conn, remote.LinkOptions{})
	refusal := &remote.CloseError{Code: remote.ClosePolicy, Reason: remote.ReasonKeyRevoked}
	link.Shutdown(refusal)
	conn.mu.Lock()
	code, reason := conn.code, conn.reason
	conn.mu.Unlock()
	if code != remote.ClosePolicy || reason != remote.ReasonKeyRevoked {
		t.Errorf("the link closed with %d %q", code, reason)
	}
	if !errors.Is(link.Err(), refusal) || !contains(refusal.Error(), "key_revoked") {
		t.Errorf("the link ended with %v", link.Err())
	}
	plain := &closingConn{pipeConn: &pipeConn{conn: b}}
	remote.NewLink(plain, remote.LinkOptions{}).Shutdown(errors.New("gone"))
	if plain.code != 0 {
		t.Errorf("a link ended without a refusal closed with %d", plain.code)
	}
}
