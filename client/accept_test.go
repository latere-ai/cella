// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/cella/client"
)

// TestTheSyntaxACallerNamesIsPassedThrough: a read in the syntax the caller
// names sends that Accept and answers the server's own bytes, one object or
// one page with the selectors; a refusal is the envelope's.
func TestTheSyntaxACallerNamesIsPassedThrough(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/missing") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"There is no such object."}}`)
			return
		}
		w.Header().Set("Content-Type", r.Header.Get("Accept"))
		_, _ = io.WriteString(w, "accept: "+r.Header.Get("Accept")+"\nat: "+r.URL.RequestURI()+"\n")
	}))
	t.Cleanup(server.Close)
	c, err := client.New(client.Config{URL: server.URL, Token: client.StaticToken("caller-token"), UserAgent: "cella-test"})
	if err != nil {
		t.Fatal(err)
	}
	one, err := c.GetAs(t.Context(), client.KindSandbox, "dev", "application/yaml")
	if err != nil || string(one) != "accept: application/yaml\nat: /v1/sandboxes/dev\n" {
		t.Fatalf("one object read as %q, %v", one, err)
	}
	page, err := c.ListAs(t.Context(), client.KindSandbox, client.ListOptions{Phase: "Running", Limit: 5}, "text/yaml")
	if err != nil || !strings.HasPrefix(string(page), "accept: text/yaml\nat: /v1/sandboxes?") || !strings.Contains(string(page), "phase=Running") {
		t.Fatalf("one page read as %q, %v", page, err)
	}
	if _, err = c.GetAs(t.Context(), client.KindSandbox, "missing", "application/yaml"); client.CodeOf(err) != "not_found" {
		t.Fatalf("a refusal read as %v", err)
	}
}

// TestTheErrorsSayWhatWentWrong: the local failures name what to fix, and
// unwrap to their cause.
func TestTheErrorsSayWhatWentWrong(t *testing.T) {
	unreadable := &client.NoBearer{Path: "/run/cella/token", Err: fs.ErrPermission}
	if !strings.Contains(unreadable.Error(), fs.ErrPermission.Error()) || !errors.Is(unreadable, fs.ErrPermission) {
		t.Errorf("an unreadable token file reads %q", unreadable.Error())
	}
	empty := &client.NoBearer{Path: "/run/cella/token"}
	if !strings.Contains(empty.Error(), "/run/cella/token holds none") {
		t.Errorf("an empty token file reads %q", empty.Error())
	}
	cut := &client.StreamError{Err: io.ErrUnexpectedEOF}
	if !strings.Contains(cut.Error(), io.ErrUnexpectedEOF.Error()) || !errors.Is(cut, io.ErrUnexpectedEOF) {
		t.Errorf("a transfer cut short reads %q", cut.Error())
	}
	coded := &client.StreamError{Code: "driver_unavailable"}
	if !strings.HasSuffix(coded.Error(), "driver_unavailable") {
		t.Errorf("a transfer the server ended reads %q", coded.Error())
	}
	if got := (&client.Error{Status: 418}).Error(); got != "the server answered 418" {
		t.Errorf("a refusal with no sentence reads %q", got)
	}
}
