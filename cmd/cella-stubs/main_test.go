// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// buffer is an output a test reads while the binary is still serving.
type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// loopbackArgs binds every role on a port the kernel chooses, so two runs
// never collide and no test holds a fixed port.
var loopbackArgs = []string{
	"-issuer", "127.0.0.1:0", "-authorizer", "127.0.0.1:0",
	"-admission", "127.0.0.1:0", "-sink", "127.0.0.1:0",
}

// serve runs the binary until the test ends and returns the URL of each
// role it printed.
func serve(t *testing.T, args ...string) (map[string]string, *buffer) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	stdout, stderr := &buffer{}, &buffer{}
	done := make(chan int, 1)
	go func() { done <- run(ctx, append(loopbackArgs, args...), stdout, stderr) }()
	t.Cleanup(func() {
		stop()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("the binary exited %d: %s", code, stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Error("the binary did not stop when its context was cancelled")
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		roles := parseRoles(stdout.String())
		if len(roles) == 4 {
			return roles, stderr
		}
		if time.Now().After(deadline) {
			t.Fatalf("the binary printed %q and %q", stdout.String(), stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// parseRoles reads the "<role> <url>" lines the binary prints.
func parseRoles(out string) map[string]string {
	roles := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		role, url, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && strings.HasPrefix(url, "http://") {
			roles[role] = url
		}
	}
	return roles
}

// TestTheBinaryServesEveryRoleAndStops: the four addresses are printed,
// they answer, and the context that a signal cancels stops the process.
func TestTheBinaryServesEveryRoleAndStops(t *testing.T) {
	roles, stderr := serve(t)
	for _, role := range []string{"issuer", "authorizer", "admission", "sink"} {
		if roles[role] == "" {
			t.Fatalf("the binary printed no address for %s", role)
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, roles["issuer"]+"/.well-known/openid-configuration", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the issuer did not answer: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the issuer answered %d", resp.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stderr.String(), "issuer GET /.well-known/openid-configuration 200") {
		if time.Now().After(deadline) {
			t.Fatalf("the request was not logged; the log is %q", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTheBinaryTakesItsFlags: a role turned off is not started, and the
// flags of the table reach the roles.
func TestTheBinaryTakesItsFlags(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	stdout, stderr := &buffer{}, &buffer{}
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{
			"-issuer", "127.0.0.1:0", "-issuer-alg", "es256", "-issuer-url", "http://issuer.example",
			"-authorizer", "", "-admission", "",
			"-sink", "127.0.0.1:0", "-sink-secret", "one", "-sink-secret", "two",
			"-admission-default", "image=registry.example/base:1",
		}, stdout, stderr)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(parseRoles(stdout.String())) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the binary printed %q and %q", stdout.String(), stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	roles := parseRoles(stdout.String())
	if len(roles) != 2 || roles["issuer"] == "" || roles["sink"] == "" {
		t.Errorf("the binary started %v, and two roles were asked for", roles)
	}
	stop()
	if code := <-done; code != 0 {
		t.Errorf("the binary exited %d: %s", code, stderr.String())
	}
}

// TestTheBinaryRefusesWhatItCannotRun: a bad flag is a usage error and a
// role that cannot be built is a failure to start. Neither is a process
// that serves half an installation.
func TestTheBinaryRefusesWhatItCannotRun(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		code int
	}{
		"an unknown flag":        {args: []string{"-nonsense"}, code: 2},
		"an argument":            {args: []string{"apply"}, code: 2},
		"a default with no name": {args: []string{"-admission-default", "=value"}, code: 2},
		"an empty deny":          {args: []string{"-authorizer-deny", " "}, code: 2},
		"an unknown algorithm":   {args: []string{"-issuer-alg", "hs256"}, code: 1},
		"a bad failure mode":     {args: []string{"-authorizer-fail", "explode"}, code: 1},
		"every role turned off": {args: []string{
			"-issuer", "", "-authorizer", "", "-admission", "", "-sink", "",
		}, code: 1},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			args := append(append([]string{}, loopbackArgs...), tc.args...)
			if got := run(t.Context(), args, stdout, stderr); got != tc.code {
				t.Fatalf("the exit code is %d, want %d; the output is %q", got, tc.code, stderr.String())
			}
			if stderr.Len() == 0 {
				t.Error("the refusal said nothing about what is wrong")
			}
		})
	}
}

// TestTheBinaryPrintsItsFlags: -h is an exit of zero with the table on
// the output a reader asked for it on.
func TestTheBinaryPrintsItsFlags(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if code := run(t.Context(), []string{"-h"}, stdout, stderr); code != 0 {
		t.Fatalf("-h exited %d", code)
	}
	for _, flag := range []string{"-issuer", "-authorizer-deny", "-admission-rewrite", "-sink-fail-first"} {
		if !strings.Contains(stderr.String(), flag) {
			t.Errorf("the usage does not name %s", flag)
		}
	}
}

// TestThePairFlagReadsItsForm: the repeatable field=value flags report
// what they hold and refuse what is not a pair.
func TestThePairFlagReadsItsForm(t *testing.T) {
	p := pairs{}
	if err := p.Set("resources.cpu=500m"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if p["resources.cpu"] != "500m" {
		t.Errorf("the flag holds %v", p)
	}
	if err := p.Set("no-equals-sign"); err == nil {
		t.Error("a value that is not <field>=<value> was accepted")
	}
	if got := p.String(); got != "resources.cpu=500m" {
		t.Errorf("the flag prints %q", got)
	}
	var l list
	if err := l.Set("one"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := l.Set("two"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := l.String(); got != "one,two" {
		t.Errorf("the flag prints %q", got)
	}
}
