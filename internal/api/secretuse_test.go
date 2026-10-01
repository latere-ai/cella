// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/egress"
	v1 "latere.ai/x/cella/manifest/v1"
)

// useWriter is a controller's use seam as the hub sees it: it keeps what it
// was handed, holds its first write until the test releases it, and fails the
// secret a test names.
type useWriter struct {
	mu      sync.Mutex
	written []egress.Use
	release chan struct{}
	failing string
}

func (w *useWriter) write(_ context.Context, sandboxID, secretID string, at time.Time) error {
	w.mu.Lock()
	w.written = append(w.written, egress.Use{Principal: egress.Principal(sandboxID), Secret: secretID, At: at})
	first := len(w.written) == 1
	w.mu.Unlock()
	if first {
		<-w.release
	}
	if secretID == w.failing {
		return errors.New("the store is gone")
	}
	return nil
}

func (w *useWriter) taken() []egress.Use {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.written)
}

func useFrame(principal, secret string, at time.Time) egress.Frame {
	return egress.Frame{Type: egress.FrameUse, Use: &egress.Use{Principal: principal, Secret: secret, At: at}}
}

// TestTheHubHandsUsesToTheController: a use reaches the writer with its
// sandbox, its secret and its time in UTC; a writer that holds, as a
// controller whose lock a driver call holds does, holds neither the read loop
// nor the acknowledgment a create waits on, and a full queue drops rather
// than waits; a use the contract refuses never reaches the writer; and a
// write that fails leaves the stream serving.
func TestTheHubHandsUsesToTheController(t *testing.T) {
	f := newHub(t, 5*time.Second, 0)
	w := &useWriter{release: make(chan struct{}), failing: "sec_failing"}
	f.used = w.write
	g := f.connect(t, "default", "", false)
	waitUntil(t, func() bool { return f.hub.Connected() == 1 }, "the gateway to connect")

	at := time.Date(2026, 10, 1, 14, 0, 3, 0, time.FixedZone("CEST", 2*60*60))
	g.send(t, useFrame(egress.Principal("sbx_a"), "sec_github", at))
	waitUntil(t, func() bool { return len(w.taken()) == 1 }, "the first use to reach the writer")
	if got := w.taken()[0]; got.Principal != egress.Principal("sbx_a") || got.Secret != "sec_github" ||
		!got.At.Equal(at) || got.At.Location() != time.UTC {
		t.Fatalf("the writer was handed %+v", got)
	}

	// The writer is holding its first write. More uses than the queue
	// holds arrive, and a create's map is still acknowledged.
	for range useQueue + 10 {
		g.send(t, useFrame(egress.Principal("sbx_a"), "sec_queued", at))
	}
	sent := time.Now()
	if err := f.hub.Send(t.Context(), boundary("sbx_a", 1)); err != nil {
		t.Fatalf("a create's map behind a held use writer: %v", err)
	}
	if waited := time.Since(sent); waited > 2*time.Second {
		t.Fatalf("the acknowledgment waited %v behind the use writer", waited)
	}
	close(w.release)
	waitUntil(t, func() bool { return len(w.taken()) == 1+useQueue }, "the queued uses to drain")

	g.send(t, useFrame("environment:default", "sec_github", at))
	g.send(t, useFrame(egress.Principal("sbx_a"), "github", at))
	g.send(t, useFrame(egress.Principal("sbx_a"), "sec_failing", at))
	g.send(t, useFrame(egress.Principal("sbx_a"), "sec_after", at))
	waitUntil(t, func() bool {
		taken := w.taken()
		return taken[len(taken)-1].Secret == "sec_after"
	}, "the use after a failed write")
	for _, u := range w.taken() {
		if u.Principal == "environment:default" || u.Secret == "github" {
			t.Fatalf("a use the contract refuses reached the writer: %+v", u)
		}
	}
	if err := f.hub.Send(t.Context(), boundary("sbx_b", 1)); err != nil {
		t.Fatalf("the stream after a failed write: %v", err)
	}

	t.Run("aStreamWithNoWriterDropsEveryUse", func(t *testing.T) {
		f := newHub(t, 5*time.Second, 0)
		g := f.connect(t, "default", "", false)
		waitUntil(t, func() bool { return f.hub.Connected() == 1 }, "the gateway to connect")
		g.send(t, useFrame(egress.Principal("sbx_a"), "sec_github", at))
		if err := f.hub.Send(t.Context(), boundary("sbx_a", 1)); err != nil {
			t.Fatalf("the stream after a use with no writer: %v", err)
		}
	})
}

// TestASecretNeverUsedHasNoLastUse: a read and a list of a secret no gateway
// has substituted carry no lastUsedAt at all, not a zero time; once a use is
// written, both carry it as an RFC 3339 time in UTC to the second.
func TestASecretNeverUsedHasNoLastUse(t *testing.T) {
	f := setupSealed(t, nil)
	created := decodeSecret(t, f.request(http.MethodPost, "/v1/secrets", f.alice,
		secretBody("github", "api.github.com", "ghp_canary"), http.StatusCreated))
	for _, path := range []string{"/v1/secrets/github", "/v1/secrets"} {
		if body := f.request(http.MethodGet, path, f.alice, "", http.StatusOK); strings.Contains(string(body), "lastUsedAt") {
			t.Fatalf("%s answers a secret never used with a stamp: %s", path, body)
		}
	}

	sandbox := decodeSandbox(t, f.request(http.MethodPost, "/v1/sandboxes?wait=1", f.alice,
		`{"apiVersion":"`+v1.APIVersion+`","kind":"Sandbox","metadata":{"name":"work"},`+
			`"spec":{"command":["/bin/sh","-c","sleep 30"],"secrets":[{"name":"github","env":"GITHUB_TOKEN"}]}}`,
		http.StatusCreated))
	t.Cleanup(func() {
		f.request(http.MethodDelete, "/v1/sandboxes/"+sandbox.Status.ID, f.alice, "", http.StatusAccepted)
	})
	at := time.Now().Add(-time.Minute).In(time.FixedZone("CEST", 2*60*60))
	if err := f.c.SecretUsed(t.Context(), sandbox.Status.ID, created.Status.ID, at); err != nil {
		t.Fatal(err)
	}
	want := `"lastUsedAt":"` + at.UTC().Truncate(time.Second).Format(time.RFC3339) + `"`
	for _, path := range []string{"/v1/secrets/github", "/v1/secrets"} {
		if body := f.request(http.MethodGet, path, f.alice, "", http.StatusOK); !strings.Contains(string(body), want) {
			t.Fatalf("%s answers %s, want %s", path, body, want)
		}
	}
}
